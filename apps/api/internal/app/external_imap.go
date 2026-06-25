package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
)

const (
	externalIMAPStorageLocal  = "local"
	externalIMAPStorageRemote = "remote"
	externalIMAPTLS           = "tls"
	externalIMAPStartTLS      = "starttls"
	externalIMAPPlain         = "plain"
	externalIMAPMaxFetch      = 30
	externalIMAPAuthPassword  = "password"
	externalIMAPAuthOAuth2    = "oauth2"
	externalIMAPOAuthGmail    = "gmail"
	externalIMAPOAuthOutlook  = "outlook"
)

type externalIMAPClientFactory interface {
	openExternalIMAPClient(ctx context.Context, account externalIMAPAccountRecord) (externalIMAPClient, error)
}

type externalIMAPClient interface {
	Close() error
	ListFolders(ctx context.Context) ([]externalIMAPRemoteFolder, error)
	FetchSummaries(ctx context.Context, folder string, query string, cursor string, limit int) ([]externalIMAPRemoteMessage, string, error)
	FetchNew(ctx context.Context, folder string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error)
	FetchRaw(ctx context.Context, folder string, uid uint32) ([]byte, externalIMAPRemoteMessage, error)
	FetchAttachments(ctx context.Context, folder string, uid uint32) ([]Attachment, error)
	FetchPart(ctx context.Context, folder string, uid uint32, partID string) ([]byte, Attachment, error)
	SetRead(ctx context.Context, folder string, uid uint32, read bool) error
}

type externalIMAPAccountRecord struct {
	ExternalIMAPAccount
	PasswordCiphertext          string
	OAuthAccessTokenCiphertext  string
	OAuthRefreshTokenCiphertext string
}

type externalIMAPRemoteFolder struct {
	Name        string
	Role        string
	UnreadCount int
	TotalCount  int
	UIDValidity uint32
}

type externalIMAPRemoteMessage struct {
	UID         uint32
	UIDValidity uint32
	Folder      string
	MessageID   string
	Subject     string
	From        string
	FromName    string
	To          []string
	CC          []string
	SentAt      time.Time
	ReceivedAt  time.Time
	Snippet     string
	IsRead      bool
	SizeBytes   int64
	Raw         []byte
	Attachments []Attachment
}

type externalIMAPAttachmentPart struct {
	Attachment
	Encoding string
}

type externalIMAPPayload struct {
	MailboxID     string `json:"mailboxId"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	TLSMode       string `json:"tlsMode"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	StorageMode   string `json:"storageMode"`
	SyncReadState *bool  `json:"syncReadState"`
	Enabled       *bool  `json:"enabled"`
}

type externalIMAPOAuthStartPayload struct {
	MailboxID     string `json:"mailboxId"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	StorageMode   string `json:"storageMode"`
	SyncReadState *bool  `json:"syncReadState"`
	Enabled       *bool  `json:"enabled"`
}

type externalIMAPOAuthState struct {
	UserID        string `json:"userId"`
	MailboxID     string `json:"mailboxId"`
	Provider      string `json:"provider"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	StorageMode   string `json:"storageMode"`
	SyncReadState bool   `json:"syncReadState"`
	Enabled       bool   `json:"enabled"`
	Nonce         string `json:"nonce"`
	ExpiresAt     int64  `json:"expiresAt"`
}

func (a *App) externalIMAPWorker(ctx context.Context) {
	interval := time.Duration(a.cfg.ExternalIMAPSyncSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.syncDueExternalIMAPAccounts(ctx)
		}
	}
}

func (a *App) syncDueExternalIMAPAccounts(ctx context.Context) {
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM external_imap_accounts WHERE enabled=1 AND storage_mode=? ORDER BY COALESCE(last_sync_at, created_at) ASC LIMIT 10`, externalIMAPStorageLocal)
	if err != nil {
		a.log.Warn("failed to list external imap accounts", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, _ = a.syncExternalIMAPAccount(runCtx, id)
		cancel()
	}
}

func (a *App) handleListExternalIMAPAccounts(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	args := []any{user.ID}
	where := "user_id=?"
	if mailboxID != "" {
		if _, err := a.mailboxForUserByID(r.Context(), user.ID, mailboxID); err != nil {
			respondError(w, http.StatusNotFound, "mailbox not found")
			return
		}
		where += " AND mailbox_id=?"
		args = append(args, mailboxID)
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_sync_at,last_status,last_error,created_at,updated_at FROM external_imap_accounts WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external accounts")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPAccount{}
	for rows.Next() {
		item, err := scanExternalIMAPAccount(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan external accounts")
			return
		}
		items = append(items, item.ExternalIMAPAccount)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req externalIMAPPayload
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	req.MailboxID = mb.ID
	if strings.TrimSpace(req.Password) == "" {
		badRequest(w, errors.New("password is required"))
		return
	}
	normalized, err := a.normalizeExternalIMAPPayload(r.Context(), req, true)
	if err != nil {
		badRequest(w, err)
		return
	}
	ciphertext, err := a.encryptExternalIMAPPassword(normalized.Password)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	id := newID("ximap")
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, user.ID, normalized.MailboxID, normalized.Name, normalized.Host, normalized.Port, normalized.TLSMode, normalized.Username, ciphertext, normalized.StorageMode, boolInt(*normalized.SyncReadState), boolInt(*normalized.Enabled), "idle", now, now); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create external account")
		return
	}
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external account")
		return
	}
	respondJSON(w, http.StatusCreated, account.ExternalIMAPAccount)
}

func (a *App) handleUpdateExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	current, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	var req externalIMAPPayload
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(req.MailboxID) == "" {
		req.MailboxID = current.MailboxID
	}
	if _, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID)); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	normalized, err := a.normalizeExternalIMAPPayload(r.Context(), req, false)
	if err != nil {
		badRequest(w, err)
		return
	}
	ciphertext := current.PasswordCiphertext
	if strings.TrimSpace(normalized.Password) != "" {
		ciphertext, err = a.encryptExternalIMAPPassword(normalized.Password)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `UPDATE external_imap_accounts SET mailbox_id=?,name=?,host=?,port=?,tls_mode=?,username=?,password_ciphertext=?,storage_mode=?,sync_read_state=?,enabled=?,updated_at=? WHERE id=? AND user_id=?`,
		normalized.MailboxID, normalized.Name, normalized.Host, normalized.Port, normalized.TLSMode, normalized.Username, ciphertext, normalized.StorageMode, boolInt(*normalized.SyncReadState), boolInt(*normalized.Enabled), now, id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update external account")
		return
	}
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external account")
		return
	}
	respondJSON(w, http.StatusOK, account.ExternalIMAPAccount)
}

func (a *App) handleDeleteExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM external_imap_accounts WHERE id=? AND user_id=?`, chi.URLParam(r, "id"), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete external account")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleTestExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		a.updateExternalIMAPStatus(r.Context(), account.ID, "error", err.Error())
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	folders, err := client.ListFolders(r.Context())
	if err != nil {
		a.updateExternalIMAPStatus(r.Context(), account.ID, "error", err.Error())
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	a.updateExternalIMAPStatus(r.Context(), account.ID, "ok", "")
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders)})
}

func (a *App) handleSyncExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	if account.StorageMode != externalIMAPStorageLocal {
		badRequest(w, errors.New("remote storage accounts do not sync into local mailbox"))
		return
	}
	run, err := a.syncExternalIMAPAccount(r.Context(), account.ID)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (a *App) handleExternalIMAPSyncRuns(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,account_id,folder,status,imported,skipped,failed,error,started_at,finished_at FROM external_imap_sync_runs WHERE account_id=? ORDER BY started_at DESC LIMIT 20`, account.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load sync runs")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPSyncRun{}
	for rows.Next() {
		run, err := scanExternalIMAPSyncRun(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan sync runs")
			return
		}
		items = append(items, run)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleSyncExternalIMAPFolder(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	if account.StorageMode != externalIMAPStorageLocal {
		badRequest(w, errors.New("remote storage accounts do not sync into local mailbox"))
		return
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	folder := strings.TrimSpace(req.Folder)
	if folder == "" {
		badRequest(w, errors.New("folder is required"))
		return
	}
	run, err := a.syncExternalIMAPAccountFolder(r.Context(), account.ID, folder)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (a *App) handleStartExternalIMAPOAuth(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	provider := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	conf, profile, err := a.externalIMAPOAuthConfig(provider)
	if err != nil {
		badRequest(w, err)
		return
	}
	var req externalIMAPOAuthStartPayload
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	email := normalizeEmail(req.Email)
	if email != "" && !strings.Contains(email, "@") {
		badRequest(w, errors.New("invalid oauth email"))
		return
	}
	req.StorageMode = strings.ToLower(strings.TrimSpace(req.StorageMode))
	if req.StorageMode == "" {
		req.StorageMode = externalIMAPStorageLocal
	}
	if req.StorageMode != externalIMAPStorageLocal && req.StorageMode != externalIMAPStorageRemote {
		badRequest(w, errors.New("invalid storage mode"))
		return
	}
	syncRead := true
	if req.SyncReadState != nil {
		syncRead = *req.SyncReadState
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	state := externalIMAPOAuthState{
		UserID:        user.ID,
		MailboxID:     mb.ID,
		Provider:      provider,
		Name:          strings.Join(strings.Fields(req.Name), " "),
		Email:         email,
		StorageMode:   req.StorageMode,
		SyncReadState: syncRead,
		Enabled:       enabled,
		Nonce:         newID("oauth"),
		ExpiresAt:     a.now().Add(10 * time.Minute).Unix(),
	}
	if state.Name == "" {
		state.Name = profile.Name
	}
	stateValue, err := a.encryptExternalIMAPOAuthState(state)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"url": conf.AuthCodeURL(stateValue, oauth2.AccessTypeOffline)})
}

func (a *App) handleExternalIMAPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	conf, profile, err := a.externalIMAPOAuthConfig(provider)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	state, err := a.decryptExternalIMAPOAuthState(r.URL.Query().Get("state"))
	if err != nil || state.Provider != provider || state.ExpiresAt < a.now().Unix() {
		respondError(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	if oauthErr := strings.TrimSpace(r.URL.Query().Get("error")); oauthErr != "" {
		respondError(w, http.StatusBadRequest, "oauth failed: "+oauthErr)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		respondError(w, http.StatusBadRequest, "missing oauth code")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := conf.Exchange(ctx, code)
	if err != nil {
		respondError(w, http.StatusBadRequest, "oauth exchange failed")
		return
	}
	accessCipher, err := a.encryptExternalIMAPPassword(token.AccessToken)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	refreshCipher := ""
	if token.RefreshToken != "" {
		refreshCipher, err = a.encryptExternalIMAPPassword(token.RefreshToken)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	name := state.Name
	if name == "" {
		name = profile.Name
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	expiry := ""
	if !token.Expiry.IsZero() {
		expiry = token.Expiry.UTC().Format(time.RFC3339Nano)
	}
	authorizedEmail, err := externalIMAPOAuthEmail(provider, token)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if state.Email != "" && !strings.EqualFold(normalizeEmail(state.Email), authorizedEmail) {
		respondError(w, http.StatusBadRequest, "oauth account email does not match requested external email")
		return
	}
	id := newID("ximap")
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, state.UserID, state.MailboxID, name, profile.Host, profile.Port, externalIMAPTLS, authorizedEmail, "", externalIMAPAuthOAuth2, provider, authorizedEmail, accessCipher, refreshCipher, expiry, state.StorageMode, boolInt(state.SyncReadState), boolInt(state.Enabled), "idle", now, now)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save oauth account")
		return
	}
	http.Redirect(w, r, strings.TrimRight(a.cfg.PublicBaseURL, "/")+"/profile?tab=mailboxes", http.StatusFound)
}

func (a *App) handleMailExternalAccounts(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_sync_at,last_status,last_error,created_at,updated_at FROM external_imap_accounts WHERE user_id=? AND enabled=1 ORDER BY name`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external accounts")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPAccount{}
	for rows.Next() {
		item, err := scanExternalIMAPAccount(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan external accounts")
			return
		}
		items = append(items, item.ExternalIMAPAccount)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleExternalIMAPFolders(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	folders, err := client.ListFolders(r.Context())
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to list folders")
		return
	}
	items := make([]ExternalIMAPFolder, 0, len(folders))
	for _, folder := range folders {
		items = append(items, ExternalIMAPFolder{Name: folder.Name, Role: folder.Role, UnreadCount: folder.UnreadCount, TotalCount: folder.TotalCount})
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleExternalIMAPMessages(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	if folder == "" {
		folder = "INBOX"
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	remote, next, err := client.FetchSummaries(r.Context(), folder, query, cursor, externalIMAPMaxFetch)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote messages")
		return
	}
	items := make([]MailMessage, 0, len(remote))
	for _, msg := range remote {
		items = append(items, externalRemoteMessageToMailMessage(account, msg, false))
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleExternalIMAPMessage(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	stored, attachments, err := a.parseMaildirMessage(raw, account.Username)
	msg := externalRemoteMessageToMailMessage(account, remote, true)
	if err == nil {
		msg.BodyText = stored.BodyText
		msg.BodyHTML = stored.BodyHTML
		msg.Snippet = stored.Snippet
		if len(attachments) > 0 {
			msg.HasAttachments = true
		}
	}
	if parts, err := client.FetchAttachments(r.Context(), folder, uid); err == nil {
		for i := range parts {
			parts[i].MessageID = msg.ID
		}
		msg.Attachments = parts
		msg.HasAttachments = len(parts) > 0
	}
	if len(msg.Attachments) == 0 {
		msg.Attachments = []Attachment{{ID: "raw", MessageID: msg.ID, Filename: safeExternalEMLFilename(msg.Subject), ContentType: "message/rfc822", SizeBytes: int64(len(raw)), CreatedAt: a.now().UTC()}}
	}
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleExternalIMAPAttachment(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	partID := chi.URLParam(r, "partId")
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	if partID != "raw" {
		data, att, err := client.FetchPart(r.Context(), folder, uid, partID)
		if err != nil {
			respondError(w, http.StatusNotFound, "attachment not found")
			return
		}
		w.Header().Set("Content-Type", att.ContentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+escapeDownloadFilename(att.Filename)+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return
	}
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeExternalEMLFilename(remote.Subject)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = w.Write(raw)
}

func (a *App) handleExternalIMAPMarkRead(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	if !account.SyncReadState {
		badRequest(w, errors.New("read state sync is disabled"))
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	var req struct {
		Read *bool `json:"read"`
	}
	_ = decodeJSON(r, &req)
	read := true
	if req.Read != nil {
		read = *req.Read
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	if err := client.SetRead(r.Context(), folder, uid, read); err != nil {
		respondError(w, http.StatusBadRequest, "failed to update remote message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "read": read})
}

func (a *App) normalizeExternalIMAPPayload(ctx context.Context, req externalIMAPPayload, create bool) (externalIMAPPayload, error) {
	req.Name = strings.Join(strings.Fields(req.Name), " ")
	if req.Name == "" {
		req.Name = req.Username
	}
	if req.Name == "" {
		req.Name = req.Host
	}
	if len([]rune(req.Name)) > 80 {
		return req, errors.New("name is too long")
	}
	req.Host = strings.ToLower(strings.TrimSpace(req.Host))
	if req.Host == "" {
		return req, errors.New("host is required")
	}
	if err := a.validateExternalIMAPHost(ctx, req.Host); err != nil {
		return req, err
	}
	req.TLSMode = strings.ToLower(strings.TrimSpace(req.TLSMode))
	if req.TLSMode == "" {
		req.TLSMode = externalIMAPTLS
	}
	if req.TLSMode != externalIMAPTLS && req.TLSMode != externalIMAPStartTLS && req.TLSMode != externalIMAPPlain {
		return req, errors.New("invalid TLS mode")
	}
	if req.Port <= 0 {
		if req.TLSMode == externalIMAPTLS {
			req.Port = 993
		} else {
			req.Port = 143
		}
	}
	if req.Port <= 0 || req.Port > 65535 {
		return req, errors.New("invalid port")
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		return req, errors.New("username is required")
	}
	req.StorageMode = strings.ToLower(strings.TrimSpace(req.StorageMode))
	if req.StorageMode == "" {
		req.StorageMode = externalIMAPStorageLocal
	}
	if req.StorageMode != externalIMAPStorageLocal && req.StorageMode != externalIMAPStorageRemote {
		return req, errors.New("invalid storage mode")
	}
	if req.SyncReadState == nil {
		v := true
		req.SyncReadState = &v
	}
	if req.Enabled == nil {
		v := true
		req.Enabled = &v
	}
	return req, nil
}

func (a *App) validateExternalIMAPHost(ctx context.Context, host string) error {
	if a.cfg.ExternalIMAPAllowPrivateHosts {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("localhost is not allowed")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			return fmt.Errorf("failed to resolve host: %w", err)
		}
	}
	for _, ip := range ips {
		if !isPublicExternalIMAPIP(ip) {
			return errors.New("private or local IMAP hosts are not allowed")
		}
	}
	return nil
}

func isPublicExternalIMAPIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}

func (a *App) encryptExternalIMAPPassword(password string) (string, error) {
	key, err := a.externalIMAPKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(nonce, gcm.Seal(nil, nonce, []byte(password), nil)...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *App) decryptExternalIMAPPassword(ciphertext string) (string, error) {
	key, err := a.externalIMAPKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted password")
	}
	nonce, data := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (a *App) externalIMAPKey() ([]byte, error) {
	secret := strings.TrimSpace(a.cfg.ExternalIMAPSecretKey)
	if secret == "" {
		return nil, errors.New("LANQIN_EXTERNAL_IMAP_SECRET_KEY is required")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

type externalIMAPOAuthProvider struct {
	Name string
	Host string
	Port int
}

func (a *App) externalIMAPOAuthConfig(provider string) (*oauth2.Config, externalIMAPOAuthProvider, error) {
	callback := strings.TrimRight(a.cfg.PublicBaseURL, "/") + "/api/external-imap-oauth/" + provider + "/callback"
	switch provider {
	case externalIMAPOAuthGmail:
		if a.cfg.ExternalIMAPGmailClientID == "" || a.cfg.ExternalIMAPGmailClientSecret == "" {
			return nil, externalIMAPOAuthProvider{}, errors.New("gmail oauth is not configured")
		}
		return &oauth2.Config{
			ClientID:     a.cfg.ExternalIMAPGmailClientID,
			ClientSecret: a.cfg.ExternalIMAPGmailClientSecret,
			RedirectURL:  callback,
			Scopes:       []string{"openid", "email", "profile", "https://mail.google.com/"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL: "https://oauth2.googleapis.com/token",
			},
		}, externalIMAPOAuthProvider{Name: "Gmail", Host: "imap.gmail.com", Port: 993}, nil
	case externalIMAPOAuthOutlook:
		if a.cfg.ExternalIMAPOutlookClientID == "" || a.cfg.ExternalIMAPOutlookClientSecret == "" {
			return nil, externalIMAPOAuthProvider{}, errors.New("outlook oauth is not configured")
		}
		return &oauth2.Config{
			ClientID:     a.cfg.ExternalIMAPOutlookClientID,
			ClientSecret: a.cfg.ExternalIMAPOutlookClientSecret,
			RedirectURL:  callback,
			Scopes:       []string{"openid", "email", "profile", "offline_access", "https://outlook.office.com/IMAP.AccessAsUser.All"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
				TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			},
		}, externalIMAPOAuthProvider{Name: "Outlook", Host: "outlook.office365.com", Port: 993}, nil
	default:
		return nil, externalIMAPOAuthProvider{}, errors.New("unsupported oauth provider")
	}
}

func externalIMAPOAuthEmail(provider string, token *oauth2.Token) (string, error) {
	if token == nil {
		return "", errors.New("oauth token is missing")
	}
	idToken, _ := token.Extra("id_token").(string)
	claims, err := externalIMAPOIDCClaims(idToken)
	if err != nil {
		return "", err
	}
	fields := []string{"email"}
	if provider == externalIMAPOAuthOutlook {
		fields = []string{"email", "preferred_username", "upn"}
	}
	for _, field := range fields {
		value, _ := claims[field].(string)
		email := normalizeEmail(value)
		if email != "" && strings.Contains(email, "@") {
			return email, nil
		}
	}
	return "", errors.New("oauth provider did not return an email address")
}

func externalIMAPOIDCClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, errors.New("oauth provider did not return an id token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid oauth id token")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("invalid oauth id token")
	}
	return claims, nil
}

func (a *App) encryptExternalIMAPOAuthState(state externalIMAPOAuthState) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	ciphertext, err := a.encryptExternalIMAPPassword(string(raw))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString([]byte(ciphertext)), nil
}

func (a *App) decryptExternalIMAPOAuthState(value string) (externalIMAPOAuthState, error) {
	var state externalIMAPOAuthState
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return state, err
	}
	plain, err := a.decryptExternalIMAPPassword(string(raw))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(plain), &state)
	return state, err
}

func (a *App) externalIMAPAccountForUser(ctx context.Context, userID, id string) (externalIMAPAccountRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_sync_at,last_status,last_error,created_at,updated_at FROM external_imap_accounts WHERE id=? AND user_id=?`, id, userID)
	return scanExternalIMAPAccount(row)
}

func (a *App) externalIMAPAccountForMailRequest(w http.ResponseWriter, r *http.Request) (externalIMAPAccountRecord, bool) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil || !account.Enabled {
		respondError(w, http.StatusNotFound, "external account not found")
		return externalIMAPAccountRecord{}, false
	}
	return account, true
}

type externalIMAPScanner interface {
	Scan(dest ...any) error
}

func scanExternalIMAPAccount(row externalIMAPScanner) (externalIMAPAccountRecord, error) {
	var item externalIMAPAccountRecord
	var syncRead, enabled int
	var lastSync, oauthExpiry sql.NullString
	var created, updated string
	err := row.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.Name, &item.Host, &item.Port, &item.TLSMode, &item.Username, &item.PasswordCiphertext, &item.AuthMode, &item.OAuthProvider, &item.OAuthEmail, &item.OAuthAccessTokenCiphertext, &item.OAuthRefreshTokenCiphertext, &oauthExpiry, &item.StorageMode, &syncRead, &enabled, &lastSync, &item.LastStatus, &item.LastError, &created, &updated)
	if err != nil {
		return item, err
	}
	if item.AuthMode == "" {
		item.AuthMode = externalIMAPAuthPassword
	}
	item.OAuthConfigured = item.AuthMode == externalIMAPAuthOAuth2 && item.OAuthAccessTokenCiphertext != ""
	item.SyncReadState = syncRead != 0
	item.Enabled = enabled != 0
	if lastSync.Valid && strings.TrimSpace(lastSync.String) != "" {
		t := parseTime(lastSync.String)
		item.LastSyncAt = &t
	}
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (a *App) updateExternalIMAPStatus(ctx context.Context, accountID, status, errText string) {
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET last_status=?,last_error=?,updated_at=? WHERE id=?`, status, trimExternalIMAPError(errText), a.now().UTC().Format(time.RFC3339Nano), accountID)
}

func (a *App) migrateExternalIMAP(ctx context.Context) error {
	columns := []struct {
		table string
		name  string
		sql   string
	}{
		{"external_imap_sync_runs", "folder", `ALTER TABLE external_imap_sync_runs ADD COLUMN folder TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "auth_mode", `ALTER TABLE external_imap_accounts ADD COLUMN auth_mode TEXT NOT NULL DEFAULT 'password'`},
		{"external_imap_accounts", "oauth_provider", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_provider TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_email", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_email TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_access_token_ciphertext", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_access_token_ciphertext TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_refresh_token_ciphertext", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_refresh_token_ciphertext TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_expiry", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_expiry TEXT`},
	}
	for _, column := range columns {
		if err := a.ensureTableColumn(ctx, column.table, column.name, column.sql); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) syncExternalIMAPAccount(ctx context.Context, accountID string) (ExternalIMAPSyncRun, error) {
	account, err := a.externalIMAPAccountByID(ctx, accountID)
	if err != nil {
		return ExternalIMAPSyncRun{}, err
	}
	if account.StorageMode != externalIMAPStorageLocal {
		return ExternalIMAPSyncRun{}, errors.New("account is not configured for local storage")
	}
	run := ExternalIMAPSyncRun{ID: newID("ximrun"), AccountID: account.ID, Status: "running", StartedAt: a.now().UTC()}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO external_imap_sync_runs(id,account_id,folder,status,started_at) VALUES(?,?,?,?,?)`, run.ID, run.AccountID, run.Folder, run.Status, run.StartedAt.Format(time.RFC3339Nano))
	client, err := a.externalIMAP.openExternalIMAPClient(ctx, account)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	defer client.Close()
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	for _, folder := range folders {
		imported, skipped, failed, err := a.syncExternalIMAPFolder(ctx, account, client, folder)
		run.Imported += imported
		run.Skipped += skipped
		run.Failed += failed
		if err != nil {
			run.Failed++
			a.log.Warn("external imap folder sync failed", "account", account.ID, "folder", folder.Name, "error", err)
		}
	}
	status := "ok"
	if run.Failed > 0 {
		status = "partial"
	}
	return a.finishExternalIMAPRun(ctx, run, status, nil)
}

func (a *App) syncExternalIMAPAccountFolder(ctx context.Context, accountID, folderName string) (ExternalIMAPSyncRun, error) {
	account, err := a.externalIMAPAccountByID(ctx, accountID)
	if err != nil {
		return ExternalIMAPSyncRun{}, err
	}
	if account.StorageMode != externalIMAPStorageLocal {
		return ExternalIMAPSyncRun{}, errors.New("account is not configured for local storage")
	}
	run := ExternalIMAPSyncRun{ID: newID("ximrun"), AccountID: account.ID, Folder: folderName, Status: "running", StartedAt: a.now().UTC()}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO external_imap_sync_runs(id,account_id,folder,status,started_at) VALUES(?,?,?,?,?)`, run.ID, run.AccountID, run.Folder, run.Status, run.StartedAt.Format(time.RFC3339Nano))
	client, err := a.externalIMAP.openExternalIMAPClient(ctx, account)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	defer client.Close()
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	var selected externalIMAPRemoteFolder
	for _, folder := range folders {
		if strings.EqualFold(folder.Name, folderName) {
			selected = folder
			break
		}
	}
	if strings.TrimSpace(selected.Name) == "" {
		return a.finishExternalIMAPRun(ctx, run, "failed", errors.New("remote folder not found"))
	}
	imported, skipped, failed, err := a.syncExternalIMAPFolder(ctx, account, client, selected)
	run.Imported = imported
	run.Skipped = skipped
	run.Failed = failed
	status := "ok"
	if failed > 0 {
		status = "partial"
	}
	if err != nil {
		status = "failed"
	}
	return a.finishExternalIMAPRun(ctx, run, status, err)
}

func (a *App) externalIMAPAccountByID(ctx context.Context, id string) (externalIMAPAccountRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_sync_at,last_status,last_error,created_at,updated_at FROM external_imap_accounts WHERE id=? AND enabled=1`, id)
	return scanExternalIMAPAccount(row)
}

func (a *App) syncExternalIMAPFolder(ctx context.Context, account externalIMAPAccountRecord, client externalIMAPClient, folder externalIMAPRemoteFolder) (int, int, int, error) {
	localFolderName := normalizeExternalIMAPFolderName(folder.Name)
	localFolderID, err := a.ensureFolder(ctx, account.MailboxID, localFolderName)
	if err != nil {
		return 0, 0, 0, err
	}
	state := a.loadExternalIMAPFolderState(ctx, account.ID, folder.Name)
	remote, err := client.FetchNew(ctx, folder.Name, state.LastUID, 100)
	if err != nil {
		return 0, 0, 0, err
	}
	imported, skipped, failed := 0, 0, 0
	maxUID := state.LastUID
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, item := range remote {
		if item.UID > maxUID {
			maxUID = item.UID
		}
		if a.externalIMAPRemoteMessageExists(ctx, account.ID, folder.Name, item.UIDValidity, item.UID) {
			skipped++
			continue
		}
		raw, item, err := client.FetchRaw(ctx, folder.Name, item.UID)
		if err != nil {
			failed++
			continue
		}
		stored, attachments, err := a.parseMaildirMessage(raw, account.Username)
		if err != nil {
			failed++
			continue
		}
		stored.MailboxID = account.MailboxID
		stored.FolderID = localFolderID
		stored.RecipientAddr = account.Username
		stored.IsRead = item.IsRead
		msgID, err := a.insertExternalIMAPMessageOnce(ctx, account, folder.Name, item, stored, attachments)
		if err != nil {
			failed++
			continue
		}
		if msgID != "" {
			if err := a.writeStoredMessageToMaildir(ctx, msgID, stored, attachments); err != nil {
				a.log.Warn("failed to write external imap message to maildir", "message", msgID, "error", err)
			}
			imported++
		} else {
			skipped++
		}
	}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO external_imap_folder_states(account_id,remote_folder,local_folder_id,uid_validity,last_uid,last_sync_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id,remote_folder) DO UPDATE SET local_folder_id=excluded.local_folder_id,uid_validity=excluded.uid_validity,last_uid=MAX(last_uid,excluded.last_uid),last_sync_at=excluded.last_sync_at,updated_at=excluded.updated_at`,
		account.ID, folder.Name, localFolderID, folder.UIDValidity, maxUID, now, now, now)
	return imported, skipped, failed, nil
}

type externalIMAPFolderState struct {
	LastUID uint32
}

func (a *App) loadExternalIMAPFolderState(ctx context.Context, accountID, folder string) externalIMAPFolderState {
	var state externalIMAPFolderState
	_ = a.db.QueryRowContext(ctx, `SELECT last_uid FROM external_imap_folder_states WHERE account_id=? AND remote_folder=?`, accountID, folder).Scan(&state.LastUID)
	return state
}

func (a *App) externalIMAPRemoteMessageExists(ctx context.Context, accountID, folder string, uidValidity, uid uint32) bool {
	var exists int
	_ = a.db.QueryRowContext(ctx, `SELECT 1 FROM external_imap_messages WHERE account_id=? AND remote_folder=? AND uid_validity=? AND uid=?`, accountID, folder, uidValidity, uid).Scan(&exists)
	return exists == 1
}

func (a *App) insertExternalIMAPMessageOnce(ctx context.Context, account externalIMAPAccountRecord, folder string, item externalIMAPRemoteMessage, msg storedMessage, attachments []AttachmentInput) (string, error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO external_imap_messages(account_id,remote_folder,uid_validity,uid,message_id,is_read,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, account.ID, folder, item.UIDValidity, item.UID, msg.MessageID, boolInt(item.IsRead), now, now)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", tx.Commit()
	}
	if strings.TrimSpace(msg.MessageID) != "" {
		var existing string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=? LIMIT 1`, msg.MailboxID, msg.FolderID, msg.MessageID).Scan(&existing); err == nil {
			_, _ = tx.ExecContext(ctx, `UPDATE external_imap_messages SET local_message_id=?,updated_at=? WHERE account_id=? AND remote_folder=? AND uid_validity=? AND uid=?`, existing, now, account.ID, folder, item.UIDValidity, item.UID)
			return "", tx.Commit()
		}
	}
	id, err := a.insertMessageWithDB(ctx, tx, msg, attachments)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE external_imap_messages SET local_message_id=?,updated_at=? WHERE account_id=? AND remote_folder=? AND uid_validity=? AND uid=?`, id, now, account.ID, folder, item.UIDValidity, item.UID)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (a *App) finishExternalIMAPRun(ctx context.Context, run ExternalIMAPSyncRun, status string, err error) (ExternalIMAPSyncRun, error) {
	run.Status = status
	if err != nil {
		run.Error = trimExternalIMAPError(err.Error())
	}
	finished := a.now().UTC()
	run.FinishedAt = &finished
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_sync_runs SET status=?,imported=?,skipped=?,failed=?,error=?,finished_at=? WHERE id=?`,
		run.Status, run.Imported, run.Skipped, run.Failed, run.Error, finished.Format(time.RFC3339Nano), run.ID)
	lastStatus := status
	if status == "failed" {
		lastStatus = "error"
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET last_sync_at=?,last_status=?,last_error=?,updated_at=? WHERE id=?`,
		finished.Format(time.RFC3339Nano), lastStatus, run.Error, finished.Format(time.RFC3339Nano), run.AccountID)
	return run, err
}

func scanExternalIMAPSyncRun(row externalIMAPScanner) (ExternalIMAPSyncRun, error) {
	var run ExternalIMAPSyncRun
	var started, finished sql.NullString
	if err := row.Scan(&run.ID, &run.AccountID, &run.Folder, &run.Status, &run.Imported, &run.Skipped, &run.Failed, &run.Error, &started, &finished); err != nil {
		return run, err
	}
	if started.Valid && strings.TrimSpace(started.String) != "" {
		run.StartedAt = parseTime(started.String)
	}
	if finished.Valid && strings.TrimSpace(finished.String) != "" {
		t := parseTime(finished.String)
		run.FinishedAt = &t
	}
	return run, nil
}

func trimExternalIMAPError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func normalizeExternalIMAPFolderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "inbox":
		return "Inbox"
	case "sent", "sent items", "sent messages":
		return "Sent"
	case "drafts", "draft":
		return "Drafts"
	case "archive", "archives":
		return "Archive"
	case "spam", "junk", "junk email":
		return "Spam"
	case "trash", "deleted", "deleted items":
		return "Trash"
	default:
		folder, err := normalizeCustomFolderName(name)
		if err != nil {
			return "Imported"
		}
		return folder
	}
}

func encodeExternalRemoteID(folder string, uid uint32) string {
	return base64.RawURLEncoding.EncodeToString([]byte(folder + "\x00" + strconv.FormatUint(uint64(uid), 10)))
}

func decodeExternalRemoteID(w http.ResponseWriter, raw string) (string, uint32, bool) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	parts := strings.SplitN(string(data), "\x00", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	uid, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || uid == 0 {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	return parts[0], uint32(uid), true
}

func externalRemoteMessageToMailMessage(account externalIMAPAccountRecord, msg externalIMAPRemoteMessage, includeBody bool) MailMessage {
	id := encodeExternalRemoteID(msg.Folder, msg.UID)
	messageID := msg.MessageID
	if messageID != "" && !strings.HasPrefix(messageID, "<") {
		messageID = "<" + messageID + ">"
	}
	return MailMessage{
		ID:                id,
		MailboxID:         account.MailboxID,
		MailboxAddress:    account.Name,
		FolderID:          msg.Folder,
		Folder:            msg.Folder,
		MessageUID:        id,
		IMAPUID:           int64(msg.UID),
		MessageID:         messageID,
		Subject:           msg.Subject,
		From:              msg.From,
		FromName:          msg.FromName,
		To:                msg.To,
		CC:                msg.CC,
		SentAt:            msg.SentAt,
		ReceivedAt:        msg.ReceivedAt,
		Snippet:           msg.Snippet,
		IsRead:            msg.IsRead,
		SizeBytes:         msg.SizeBytes,
		HasAttachments:    includeBody,
		ExternalAccountID: account.ID,
	}
}

func safeExternalEMLFilename(subject string) string {
	name := strings.TrimSpace(subject)
	if name == "" {
		name = "message"
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '-'
		}
		return r
	}, name)
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	return name + ".eml"
}

func externalIMAPAttachmentsFromBodyStructure(body imap.BodyStructure) []Attachment {
	parts := externalIMAPAttachmentPartsFromBodyStructure(body)
	items := make([]Attachment, 0, len(parts))
	for _, part := range parts {
		items = append(items, part.Attachment)
	}
	return items
}

func externalIMAPAttachmentPartsFromBodyStructure(body imap.BodyStructure) []externalIMAPAttachmentPart {
	now := time.Now().UTC()
	items := []externalIMAPAttachmentPart{}
	body.Walk(func(path []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		filename := strings.TrimSpace(single.Filename())
		disposition := ""
		if disp := single.Disposition(); disp != nil {
			disposition = strings.ToLower(strings.TrimSpace(disp.Value))
		}
		if filename == "" && disposition != "attachment" {
			return true
		}
		if filename == "" {
			filename = "attachment"
		}
		contentType := single.MediaType()
		if contentType == "/" || contentType == "" {
			contentType = "application/octet-stream"
		}
		items = append(items, externalIMAPAttachmentPart{
			Attachment: Attachment{
				ID:          encodeExternalIMAPPartID(path),
				Filename:    filename,
				ContentType: contentType,
				SizeBytes:   int64(single.Size),
				CreatedAt:   now,
			},
			Encoding: strings.ToLower(strings.TrimSpace(single.Encoding)),
		})
		return true
	})
	return items
}

func encodeExternalIMAPPartID(path []int) string {
	parts := make([]string, 0, len(path))
	for _, n := range path {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ".")
}

func decodeExternalIMAPPartID(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "raw" {
		return nil, errors.New("invalid part id")
	}
	rawParts := strings.Split(value, ".")
	path := make([]int, 0, len(rawParts))
	for _, part := range rawParts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, errors.New("invalid part id")
		}
		path = append(path, n)
	}
	return path, nil
}

func decodeExternalIMAPPartData(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		compact := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(string(data))
		decoded := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
		n, err := base64.StdEncoding.Decode(decoded, []byte(compact))
		if err != nil {
			return nil, err
		}
		return decoded[:n], nil
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(data))))
	default:
		return data, nil
	}
}

func escapeDownloadFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "attachment"
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == '\\' {
			return '-'
		}
		return r
	}, name)
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	return mime.QEncoding.Encode("utf-8", name)
}

func (a *App) openExternalIMAPClient(ctx context.Context, account externalIMAPAccountRecord) (externalIMAPClient, error) {
	if err := a.validateExternalIMAPHost(ctx, account.Host); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(account.Host, strconv.Itoa(account.Port))
	options := &imapclient.Options{
		Dialer:    &net.Dialer{Timeout: 10 * time.Second},
		TLSConfig: &tls.Config{ServerName: account.Host, MinVersion: tls.VersionTLS12},
	}
	var c *imapclient.Client
	var err error
	switch account.TLSMode {
	case externalIMAPTLS:
		c, err = imapclient.DialTLS(addr, options)
	case externalIMAPStartTLS:
		c, err = imapclient.DialStartTLS(addr, options)
	default:
		c, err = imapclient.DialInsecure(addr, options)
	}
	if err != nil {
		return nil, err
	}
	if account.AuthMode == externalIMAPAuthOAuth2 {
		token, err := a.externalIMAPOAuthAccessToken(ctx, account)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := c.Authenticate(sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{Username: account.Username, Token: token, Host: account.Host, Port: account.Port})); err != nil {
			c.Close()
			return nil, err
		}
	} else {
		password, err := a.decryptExternalIMAPPassword(account.PasswordCiphertext)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := c.Login(account.Username, password).Wait(); err != nil {
			c.Close()
			return nil, err
		}
	}
	return &goExternalIMAPClient{client: c}, nil
}

func (a *App) externalIMAPOAuthAccessToken(ctx context.Context, account externalIMAPAccountRecord) (string, error) {
	access, err := a.decryptExternalIMAPPassword(account.OAuthAccessTokenCiphertext)
	if err != nil {
		return "", err
	}
	var expiry time.Time
	var expiryRaw sql.NullString
	_ = a.db.QueryRowContext(ctx, `SELECT oauth_expiry FROM external_imap_accounts WHERE id=?`, account.ID).Scan(&expiryRaw)
	if expiryRaw.Valid && strings.TrimSpace(expiryRaw.String) != "" {
		expiry = parseTime(expiryRaw.String)
	}
	if expiry.IsZero() || expiry.After(a.now().Add(2*time.Minute)) || account.OAuthRefreshTokenCiphertext == "" {
		return access, nil
	}
	refresh, err := a.decryptExternalIMAPPassword(account.OAuthRefreshTokenCiphertext)
	if err != nil {
		return "", err
	}
	conf, _, err := a.externalIMAPOAuthConfig(account.OAuthProvider)
	if err != nil {
		return "", err
	}
	token, err := conf.TokenSource(ctx, &oauth2.Token{AccessToken: access, RefreshToken: refresh, Expiry: expiry}).Token()
	if err != nil {
		return "", err
	}
	accessCipher, err := a.encryptExternalIMAPPassword(token.AccessToken)
	if err != nil {
		return "", err
	}
	refreshCipher := account.OAuthRefreshTokenCiphertext
	if token.RefreshToken != "" && token.RefreshToken != refresh {
		refreshCipher, err = a.encryptExternalIMAPPassword(token.RefreshToken)
		if err != nil {
			return "", err
		}
	}
	expiryText := ""
	if !token.Expiry.IsZero() {
		expiryText = token.Expiry.UTC().Format(time.RFC3339Nano)
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET oauth_access_token_ciphertext=?,oauth_refresh_token_ciphertext=?,oauth_expiry=?,updated_at=? WHERE id=?`, accessCipher, refreshCipher, expiryText, a.now().UTC().Format(time.RFC3339Nano), account.ID)
	return token.AccessToken, nil
}

type goExternalIMAPClient struct {
	client *imapclient.Client
}

func (c *goExternalIMAPClient) Close() error {
	if c.client == nil {
		return nil
	}
	_ = c.client.Logout().Wait()
	return c.client.Close()
}

func (c *goExternalIMAPClient) ListFolders(ctx context.Context) ([]externalIMAPRemoteFolder, error) {
	list, err := c.client.List("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true, NumUnseen: true}}).Collect()
	if err != nil {
		return nil, err
	}
	folders := []externalIMAPRemoteFolder{}
	for _, item := range list {
		if strings.TrimSpace(item.Mailbox) == "" || mailboxHasNoSelect(item.Attrs) {
			continue
		}
		f := externalIMAPRemoteFolder{Name: item.Mailbox, Role: normalizeExternalIMAPFolderName(item.Mailbox)}
		if item.Status != nil {
			if item.Status.NumMessages != nil {
				f.TotalCount = int(*item.Status.NumMessages)
			}
			if item.Status.NumUnseen != nil {
				f.UnreadCount = int(*item.Status.NumUnseen)
			}
		}
		if strings.EqualFold(item.Mailbox, "INBOX") {
			f.Name = "INBOX"
		}
		folders = append(folders, f)
	}
	if len(folders) == 0 {
		folders = append(folders, externalIMAPRemoteFolder{Name: "INBOX", Role: "Inbox"})
	}
	return folders, nil
}

func mailboxHasNoSelect(attrs []imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if strings.EqualFold(string(attr), `\Noselect`) {
			return true
		}
	}
	return false
}

func (c *goExternalIMAPClient) FetchSummaries(ctx context.Context, folder string, query string, cursor string, limit int) ([]externalIMAPRemoteMessage, string, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 100 {
		limit = externalIMAPMaxFetch
	}
	if selected.NumMessages == 0 {
		return nil, "", nil
	}
	maxUID := uint32(0)
	if strings.TrimSpace(cursor) != "" {
		parsed, _ := strconv.ParseUint(strings.TrimPrefix(cursor, "uid:"), 10, 32)
		maxUID = uint32(parsed)
	}
	if maxUID == 0 {
		if selected.UIDNext == 0 {
			return nil, "", nil
		}
		maxUID = uint32(selected.UIDNext - 1)
	}
	criteria := &imap.SearchCriteria{}
	var uidRange imap.UIDSet
	uidRange.AddRange(1, imap.UID(maxUID))
	criteria.UID = []imap.UIDSet{uidRange}
	if q := strings.TrimSpace(query); q != "" {
		criteria.Text = []string{q}
	}
	data, err := c.client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, "", err
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, "", nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > limit {
		uids = uids[:limit]
	}
	var set imap.UIDSet
	set.AddNum(uids...)
	bodySection := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	messages, err := c.client.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, "", err
	}
	out := []externalIMAPRemoteMessage{}
	for _, message := range messages {
		out = append(out, fetchBufferToExternalMessage(folder, selected.UIDValidity, message, nil))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID > out[j].UID })
	next := ""
	if len(uids) == limit {
		last := uint32(uids[len(uids)-1])
		if last > 1 {
			next = "uid:" + strconv.FormatUint(uint64(last-1), 10)
		}
	}
	return out, next, nil
}

func (c *goExternalIMAPClient) FetchNew(ctx context.Context, folder string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, err
	}
	if selected.NumMessages == 0 || selected.UIDNext <= imap.UID(afterUID+1) {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(afterUID+1), selected.UIDNext-1)
	bodySection := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	messages, err := c.client.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, err
	}
	out := []externalIMAPRemoteMessage{}
	for i := 0; i < len(messages) && len(out) < limit; i++ {
		out = append(out, fetchBufferToExternalMessage(folder, selected.UIDValidity, messages[i], nil))
	}
	return out, nil
}

func (c *goExternalIMAPClient) FetchRaw(ctx context.Context, folder string, uid uint32) ([]byte, externalIMAPRemoteMessage, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, externalIMAPRemoteMessage{}, err
	}
	bodySection := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, externalIMAPRemoteMessage{}, err
	}
	if len(messages) == 0 {
		return nil, externalIMAPRemoteMessage{}, sql.ErrNoRows
	}
	raw := messages[0].FindBodySection(bodySection)
	return raw, fetchBufferToExternalMessage(folder, selected.UIDValidity, messages[0], raw), nil
}

func (c *goExternalIMAPClient) FetchAttachments(ctx context.Context, folder string, uid uint32) ([]Attachment, error) {
	parts, err := c.fetchAttachmentParts(ctx, folder, uid)
	if err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.Attachment)
	}
	return out, nil
}

func (c *goExternalIMAPClient) fetchAttachmentParts(ctx context.Context, folder string, uid uint32) ([]externalIMAPAttachmentPart, error) {
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return nil, err
	}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 || messages[0].BodyStructure == nil {
		return nil, sql.ErrNoRows
	}
	return externalIMAPAttachmentPartsFromBodyStructure(messages[0].BodyStructure), nil
}

func (c *goExternalIMAPClient) FetchPart(ctx context.Context, folder string, uid uint32, partID string) ([]byte, Attachment, error) {
	path, err := decodeExternalIMAPPartID(partID)
	if err != nil {
		return nil, Attachment{}, err
	}
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return nil, Attachment{}, err
	}
	attachments, err := c.fetchAttachmentParts(ctx, folder, uid)
	if err != nil {
		return nil, Attachment{}, err
	}
	var att externalIMAPAttachmentPart
	for _, item := range attachments {
		if item.ID == partID {
			att = item
			break
		}
	}
	if att.ID == "" {
		return nil, Attachment{}, sql.ErrNoRows
	}
	bodySection := &imap.FetchItemBodySection{Part: path, Peek: true}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, Attachment{}, err
	}
	if len(messages) == 0 {
		return nil, Attachment{}, sql.ErrNoRows
	}
	data := messages[0].FindBodySection(bodySection)
	if data == nil {
		return nil, Attachment{}, sql.ErrNoRows
	}
	data, err = decodeExternalIMAPPartData(data, att.Encoding)
	if err != nil {
		return nil, Attachment{}, err
	}
	att.SizeBytes = int64(len(data))
	return data, att.Attachment, nil
}

func (c *goExternalIMAPClient) SetRead(ctx context.Context, folder string, uid uint32, read bool) error {
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return err
	}
	op := imap.StoreFlagsDel
	if read {
		op = imap.StoreFlagsAdd
	}
	return c.client.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}, nil).Close()
}

func fetchBufferToExternalMessage(folder string, uidValidity uint32, msg *imapclient.FetchMessageBuffer, raw []byte) externalIMAPRemoteMessage {
	out := externalIMAPRemoteMessage{Folder: folder, UIDValidity: uidValidity, UID: uint32(msg.UID), ReceivedAt: time.Now().UTC(), Raw: raw}
	if msg.Envelope != nil {
		out.MessageID = msg.Envelope.MessageID
		out.Subject = msg.Envelope.Subject
		out.SentAt = msg.Envelope.Date
		out.From, out.FromName = firstIMAPAddress(msg.Envelope.From)
		out.To = imapAddresses(msg.Envelope.To)
		out.CC = imapAddresses(msg.Envelope.Cc)
	}
	if out.SentAt.IsZero() {
		out.SentAt = out.ReceivedAt
	}
	if !msg.InternalDate.IsZero() {
		out.ReceivedAt = msg.InternalDate
	}
	out.SizeBytes = msg.RFC822Size
	for _, flag := range msg.Flags {
		if flag == imap.FlagSeen {
			out.IsRead = true
			break
		}
	}
	if len(raw) > 0 {
		if stored, _, err := parseExternalIMAPRawForSnippet(raw); err == nil {
			if out.Subject == "" {
				out.Subject = stored.Subject
			}
			if out.MessageID == "" {
				out.MessageID = strings.Trim(stored.MessageID, "<>")
			}
			out.Snippet = stored.Snippet
		}
	}
	return out
}

func parseExternalIMAPRawForSnippet(raw []byte) (storedMessage, []AttachmentInput, error) {
	tmp := &App{now: time.Now, policy: NewHTMLPolicy()}
	return tmp.parseMaildirMessage(raw, "")
}

func firstIMAPAddress(addrs []imap.Address) (string, string) {
	for _, addr := range addrs {
		if email := addr.Addr(); email != "" {
			return normalizeEmail(email), addr.Name
		}
	}
	return "", ""
}

func imapAddresses(addrs []imap.Address) []string {
	out := []string{}
	for _, addr := range addrs {
		if email := addr.Addr(); email != "" {
			out = append(out, normalizeEmail(email))
		}
	}
	return out
}
