package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

func (a *App) handleOpenAPIListDomains(w http.ResponseWriter, r *http.Request) {
	limit := parseOpenAPILimit(r, 50, 100)
	sortValue, cursorID, err := parseOpenAPIListCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		badRequest(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,status,dkim_selector,dkim_public_key,dns_status,dns_checked_at,created_at FROM domains
		WHERE (?='' OR name>? OR (name=? AND id>?)) ORDER BY name,id LIMIT ?`, sortValue, sortValue, sortValue, cursorID, limit+1)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list domains")
		return
	}
	defer rows.Close()
	items := []Domain{}
	for rows.Next() {
		item, err := scanDomain(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan domains")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list domains")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeOpenAPIListCursor(last.Name, last.ID)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleOpenAPICreateDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	id, err := a.createDomainTx(r.Context(), nil, req.Name)
	if err != nil {
		badRequest(w, err)
		return
	}
	domain, err := a.domainByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load domain")
		return
	}
	respondJSON(w, http.StatusCreated, domain)
}

func (a *App) handleOpenAPIGetDomain(w http.ResponseWriter, r *http.Request) {
	domain, err := a.domainByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	respondJSON(w, http.StatusOK, domain)
}

func (a *App) handleOpenAPIUpdateDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "active" && status != "disabled" {
		badRequest(w, errors.New("invalid status"))
		return
	}
	id := chi.URLParam(r, "id")
	res, err := a.db.ExecContext(r.Context(), `UPDATE domains SET status=?, updated_at=? WHERE id=?`, status, a.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update domain")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	domain, err := a.domainByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load domain")
		return
	}
	respondJSON(w, http.StatusOK, domain)
}

func (a *App) handleOpenAPIDeleteDomain(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var count int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailboxes WHERE domain_id=?`, id).Scan(&count); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check domain")
		return
	}
	if count > 0 {
		badRequest(w, errors.New("domain still has mailboxes"))
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM domains WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete domain")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleOpenAPIListMailboxes(w http.ResponseWriter, r *http.Request) {
	limit := parseOpenAPILimit(r, 50, 100)
	sortValue, cursorID, err := parseOpenAPIListCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		badRequest(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT mb.id,mb.user_id,u.email,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at
		FROM mailboxes mb JOIN users u ON u.id=mb.user_id
		WHERE (?='' OR mb.address>? OR (mb.address=? AND mb.id>?)) ORDER BY mb.address,mb.id LIMIT ?`, sortValue, sortValue, sortValue, cursorID, limit+1)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list mailboxes")
		return
	}
	defer rows.Close()
	items := []Mailbox{}
	for rows.Next() {
		item, err := scanMailbox(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan mailboxes")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list mailboxes")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeOpenAPIListCursor(last.Address, last.ID)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleOpenAPICreateMailbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DomainID    string `json:"domainId"`
		LocalPart   string `json:"localPart"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		QuotaMB     int    `json:"quotaMb"`
		OwnerEmail  string `json:"ownerEmail"`
		UserID      string `json:"userId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := requireString("domainId", req.DomainID); err != nil {
		badRequest(w, err)
		return
	}
	if err := requireString("localPart", req.LocalPart); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("password must be at least 8 characters"))
		return
	}
	domain, err := a.domainByID(r.Context(), req.DomainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	localPart := normalizeLocalPart(req.LocalPart)
	if localPart == "" {
		badRequest(w, errors.New("localPart is required"))
		return
	}
	address := localPart + "@" + domain.Name
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = address
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	userID, err := a.resolveMailboxOwnerTx(r.Context(), tx, req.UserID, req.OwnerEmail, address, displayName, string(passwordHash))
	if err != nil {
		respondMailboxOwnerError(w, err)
		return
	}
	mailboxID, err := a.createMailboxWithPasswordHashTx(r.Context(), tx, userID, req.DomainID, localPart, displayName, string(passwordHash), req.QuotaMB, "active")
	if err != nil {
		badRequest(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create mailbox")
		return
	}
	mailbox, err := a.mailboxByID(r.Context(), mailboxID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	respondJSON(w, http.StatusCreated, mailbox)
}

func (a *App) handleOpenAPIGetMailbox(w http.ResponseWriter, r *http.Request) {
	mailbox, err := a.mailboxByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	respondJSON(w, http.StatusOK, mailbox)
}

func (a *App) handleOpenAPIUpdateMailbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current, err := a.mailboxByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	var req struct {
		DisplayName string `json:"displayName"`
		QuotaMB     int    `json:"quotaMb"`
		Status      string `json:"status"`
		UserID      string `json:"userId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = current.DisplayName
	}
	quotaMB := req.QuotaMB
	if quotaMB <= 0 {
		quotaMB = current.QuotaMB
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = current.Status
	}
	if status != "active" && status != "disabled" {
		badRequest(w, errors.New("invalid status"))
		return
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		userID = current.UserID
	}
	if err := a.ensureActiveUserExists(r.Context(), userID); err != nil {
		respondMailboxOwnerError(w, err)
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE mailboxes SET user_id=?,display_name=?,quota_mb=?,status=?,updated_at=? WHERE id=?`,
		userID, displayName, quotaMB, status, a.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	mailbox, err := a.mailboxByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	respondJSON(w, http.StatusOK, mailbox)
}

func (a *App) handleOpenAPIDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var owner string
	if err := a.db.QueryRowContext(r.Context(), `SELECT user_id FROM mailboxes WHERE id=?`, id).Scan(&owner); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	current := currentUser(r)
	if current != nil && owner == current.ID {
		var count int
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailboxes WHERE user_id=?`, owner).Scan(&count); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to check mailbox")
			return
		}
		if count <= 1 {
			badRequest(w, errors.New("cannot delete your last mailbox"))
			return
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id FROM messages WHERE mailbox_id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox messages")
		return
	}
	messageIDs := []string{}
	for rows.Next() {
		var messageID string
		if rows.Scan(&messageID) == nil {
			messageIDs = append(messageIDs, messageID)
		}
	}
	rows.Close()
	for _, messageID := range messageIDs {
		a.deleteMessage(r.Context(), messageID)
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM mailboxes WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete mailbox")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleOpenAPIResetMailboxPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("password must be at least 8 characters"))
		return
	}
	var userID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT user_id FROM mailboxes WHERE id=?`, chi.URLParam(r, "id")).Scan(&userID); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `UPDATE users SET password_hash=?,updated_at=? WHERE id=?`, string(hash), now, userID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}
	res, err := tx.ExecContext(r.Context(), `UPDATE mailboxes SET password_hash=?,updated_at=? WHERE user_id=?`, string(hash), now, userID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset mailbox passwords")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save password")
		return
	}
	affected, _ := res.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "affectedMailboxes": affected})
}

func (a *App) handleOpenAPISendMail(w http.ResponseWriter, r *http.Request) {
	var req mailComposeInput
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, req.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	requestJSON, _ := json.Marshal(req)
	requestSum := sha256.Sum256(requestJSON)
	requestHash := hex.EncodeToString(requestSum[:])
	if idempotencyKey != "" {
		if len(idempotencyKey) > 128 || strings.ContainsAny(idempotencyKey, "\r\n") {
			badRequest(w, errors.New("invalid Idempotency-Key"))
			return
		}
		status, replayed, err := a.reserveOpenAPISendIdempotency(r.Context(), currentUser(r).ID, idempotencyKey, requestHash)
		if err != nil {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		if replayed {
			w.Header().Set("Idempotency-Replayed", "true")
			respondJSON(w, http.StatusOK, status)
			return
		}
	}
	msg, err := a.sendMailWithSource(r.Context(), currentUser(r), mb, req, sendSourceOpenAPI)
	if err != nil {
		if idempotencyKey != "" {
			_, _ = a.db.ExecContext(r.Context(), `DELETE FROM send_idempotency_keys WHERE user_id=? AND idempotency_key=? AND sent_message_id=''`, currentUser(r).ID, idempotencyKey)
		}
		respondSendError(w, err)
		return
	}
	status := openAPISendStatusFromMessage(msg, mb.Address)
	if msg.SendQueueID != "" {
		if item, err := a.loadSendQueueEntryForUser(r.Context(), msg.SendQueueID, mb.UserID); err == nil {
			status = openAPISendStatusFromQueue(item, mb.Address)
		}
	} else {
		item, err := a.loadLatestSendQueueForMailboxMessage(r.Context(), msg.ID, mb.ID)
		if err == nil {
			status = openAPISendStatusFromQueue(item, mb.Address)
		}
	}
	a.applyDeliveryStatus(r.Context(), &status)
	if idempotencyKey != "" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE send_idempotency_keys SET sent_message_id=?,queue_id=? WHERE user_id=? AND idempotency_key=?`, status.MessageID, status.QueueID, currentUser(r).ID, idempotencyKey)
	}
	respondJSON(w, http.StatusCreated, status)
}

func (a *App) handleOpenAPISendStatus(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	item, err := a.loadSendQueueEntryForUser(r.Context(), id, user.ID)
	if err != nil {
		item, err = a.loadSendQueueEntryForSentMessage(r.Context(), id, user.ID)
	}
	if err == nil {
		mailboxAddress := ""
		if mb, mbErr := a.mailboxByID(r.Context(), item.MailboxID); mbErr == nil {
			mailboxAddress = mb.Address
		}
		status := openAPISendStatusFromQueue(item, mailboxAddress)
		a.applyDeliveryStatus(r.Context(), &status)
		respondJSON(w, http.StatusOK, status)
		return
	}
	msg, err := a.loadOpenAPISentMessageForUser(r.Context(), id, user.ID)
	if err != nil {
		respondError(w, http.StatusNotFound, "send item not found")
		return
	}
	mailboxAddress := ""
	if mb, mbErr := a.mailboxByID(r.Context(), msg.MailboxID); mbErr == nil {
		mailboxAddress = mb.Address
	}
	respondJSON(w, http.StatusOK, openAPISendStatusFromMessage(msg, mailboxAddress))
}

func (a *App) handleOpenAPIMailboxMessages(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mailboxID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, err := a.mailboxForUserByID(r.Context(), user.ID, mailboxID); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	limit := parseOpenAPILimit(r, 30, 100)
	cursorReceivedAt, cursorID, offset, err := parseOpenAPIMessageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		badRequest(w, err)
		return
	}
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	if folder == "" {
		folder = "Inbox"
	}
	where := "m.mailbox_id=?"
	args := []any{mailboxID}
	if folder != "" && !strings.EqualFold(folder, "all") {
		where += " AND lower(f.name)=lower(?)"
		args = append(args, folder)
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		where += " AND (m.subject LIKE ? OR m.from_addr LIKE ? OR m.from_name LIKE ? OR m.to_addrs LIKE ? OR m.snippet LIKE ? OR m.body_text LIKE ?)"
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like, like)
	}
	if cursorReceivedAt != "" {
		where += " AND (m.received_at<? OR (m.received_at=? AND m.id<?))"
		args = append(args, cursorReceivedAt, cursorReceivedAt, cursorID)
	}
	args = append(args, limit+1)
	query := `SELECT m.id,m.mailbox_id,m.folder_id,f.name,m.message_uid,m.imap_uid,m.imap_modseq,m.message_id,m.subject,m.from_addr,COALESCE(m.from_name,''),m.to_addrs,m.cc_addrs,m.bcc_addrs,m.sent_at,m.received_at,m.snippet,m.is_read,m.is_starred,m.has_attachments,m.size_bytes
		FROM messages m JOIN folders f ON f.id=m.folder_id
		WHERE ` + where + `
		ORDER BY m.received_at DESC,m.id DESC LIMIT ?`
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}
	rows, err := a.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	defer rows.Close()
	items := []MailMessage{}
	for rows.Next() {
		item, err := scanMessageSummary(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan messages")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		nextCursor = encodeOpenAPIMessageCursor(last.ReceivedAt, last.ID)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nextCursor})
}

type domainScanner interface{ Scan(dest ...any) error }

func scanDomain(row domainScanner) (Domain, error) {
	var item Domain
	var checked sql.NullString
	var created string
	err := row.Scan(&item.ID, &item.Name, &item.Status, &item.DKIMSelector, &item.DKIMPublicKey, &item.DNSStatus, &checked, &created)
	if err != nil {
		return item, err
	}
	item.DNSCheckedAt = nullableTime(checked)
	item.CreatedAt = parseTime(created)
	return item, nil
}

type mailboxScanner interface{ Scan(dest ...any) error }

func scanMailbox(row mailboxScanner) (Mailbox, error) {
	var item Mailbox
	var created string
	err := row.Scan(&item.ID, &item.UserID, &item.UserEmail, &item.DomainID, &item.LocalPart, &item.Address, &item.DisplayName, &item.QuotaMB, &item.Status, &created)
	if err != nil {
		return item, err
	}
	item.CreatedAt = parseTime(created)
	return item, nil
}

type openAPISendStatus struct {
	ID                string                   `json:"id"`
	QueueID           string                   `json:"queueId,omitempty"`
	Status            string                   `json:"status"`
	QueueStatus       string                   `json:"queueStatus,omitempty"`
	MessageID         string                   `json:"messageId"`
	RFCMessageID      string                   `json:"rfcMessageId"`
	MailboxID         string                   `json:"mailboxId"`
	MailboxAddress    string                   `json:"mailboxAddress,omitempty"`
	Subject           string                   `json:"subject,omitempty"`
	Recipients        []string                 `json:"recipients,omitempty"`
	AttemptCount      int                      `json:"attemptCount,omitempty"`
	MaxAttempts       int                      `json:"maxAttempts,omitempty"`
	NextAttemptAt     *time.Time               `json:"nextAttemptAt,omitempty"`
	LastError         string                   `json:"lastError,omitempty"`
	CreatedAt         time.Time                `json:"createdAt"`
	UpdatedAt         *time.Time               `json:"updatedAt,omitempty"`
	DeliveredAt       *time.Time               `json:"deliveredAt,omitempty"`
	RecipientStatuses []openAPIRecipientStatus `json:"recipientStatuses,omitempty"`
}

type openAPIRecipientStatus struct {
	Recipient  string    `json:"recipient"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

func openAPISendStatusFromQueue(item SendQueueEntry, mailboxAddress string) openAPISendStatus {
	status := item.Status
	if status == sendQueueStatusDelivered {
		status = "relayed"
	}
	return openAPISendStatus{
		ID:             firstNonEmpty(item.SentMessageID, item.ID),
		QueueID:        item.ID,
		Status:         status,
		QueueStatus:    item.Status,
		MessageID:      item.SentMessageID,
		RFCMessageID:   item.MessageID,
		MailboxID:      item.MailboxID,
		MailboxAddress: mailboxAddress,
		Subject:        item.Subject,
		Recipients:     item.Recipients,
		AttemptCount:   item.AttemptCount,
		MaxAttempts:    item.MaxAttempts,
		NextAttemptAt:  timePtr(item.NextAttemptAt),
		LastError:      item.LastError,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      timePtr(item.UpdatedAt),
		DeliveredAt:    item.DeliveredAt,
	}
}

func (a *App) reserveOpenAPISendIdempotency(ctx context.Context, userID, key, requestHash string) (openAPISendStatus, bool, error) {
	_, _ = a.db.ExecContext(ctx, `DELETE FROM send_idempotency_keys WHERE created_at<?`, a.now().UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))
	res, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO send_idempotency_keys(user_id,idempotency_key,request_hash,created_at) VALUES(?,?,?,?)`, userID, key, requestHash, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return openAPISendStatus{}, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return openAPISendStatus{}, false, nil
	}
	var storedHash, sentMessageID, queueID string
	if err := a.db.QueryRowContext(ctx, `SELECT request_hash,sent_message_id,queue_id FROM send_idempotency_keys WHERE user_id=? AND idempotency_key=?`, userID, key).Scan(&storedHash, &sentMessageID, &queueID); err != nil {
		return openAPISendStatus{}, false, err
	}
	if storedHash != requestHash {
		return openAPISendStatus{}, false, errors.New("Idempotency-Key was already used with a different request")
	}
	if sentMessageID == "" {
		return openAPISendStatus{}, false, errors.New("a request with this Idempotency-Key is still processing")
	}
	item, err := a.loadSendQueueEntryForUser(ctx, queueID, userID)
	if err != nil {
		return openAPISendStatus{}, false, err
	}
	status := openAPISendStatusFromQueue(item, item.MailFrom)
	a.applyDeliveryStatus(ctx, &status)
	return status, true, nil
}

func (a *App) applyDeliveryStatus(ctx context.Context, status *openAPISendStatus) {
	rows, err := a.db.QueryContext(ctx, `SELECT recipient,status,reason,provider,occurred_at FROM delivery_events
		WHERE sent_message_id=? ORDER BY occurred_at DESC,id DESC`, status.MessageID)
	if err != nil {
		return
	}
	defer rows.Close()
	seen := map[string]bool{}
	counts := map[string]int{}
	for rows.Next() {
		var item openAPIRecipientStatus
		var occurredAt string
		if rows.Scan(&item.Recipient, &item.Status, &item.Reason, &item.Provider, &occurredAt) != nil || seen[item.Recipient] {
			continue
		}
		seen[item.Recipient] = true
		item.OccurredAt = parseTime(occurredAt)
		status.RecipientStatuses = append(status.RecipientStatuses, item)
		counts[item.Status]++
	}
	if len(status.RecipientStatuses) == 0 {
		return
	}
	if len(status.RecipientStatuses) < len(status.Recipients) || len(counts) > 1 {
		status.Status = "partial"
		return
	}
	for _, value := range []string{"complained", "bounced", "rejected", "deferred", "delivered"} {
		if counts[value] > 0 {
			status.Status = value
			return
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type openAPIMessageCursor struct {
	ReceivedAt string `json:"receivedAt"`
	ID         string `json:"id"`
}

type openAPIListCursor struct {
	Sort string `json:"sort"`
	ID   string `json:"id"`
}

func encodeOpenAPIMessageCursor(receivedAt time.Time, id string) string {
	payload, _ := json.Marshal(openAPIMessageCursor{ReceivedAt: receivedAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func parseOpenAPIMessageCursor(raw string) (receivedAt, id string, offset int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, nil
	}
	if n, convErr := strconv.Atoi(raw); convErr == nil {
		if n < 0 {
			return "", "", 0, errors.New("invalid cursor")
		}
		return "", "", n, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", 0, errors.New("invalid cursor")
	}
	var cursor openAPIMessageCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ReceivedAt == "" || cursor.ID == "" {
		return "", "", 0, errors.New("invalid cursor")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.ReceivedAt); err != nil {
		return "", "", 0, errors.New("invalid cursor")
	}
	return cursor.ReceivedAt, cursor.ID, 0, nil
}

func encodeOpenAPIListCursor(sortValue, id string) string {
	payload, _ := json.Marshal(openAPIListCursor{Sort: sortValue, ID: id})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func parseOpenAPIListCursor(raw string) (sortValue, id string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", errors.New("invalid cursor")
	}
	var cursor openAPIListCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Sort == "" || cursor.ID == "" {
		return "", "", errors.New("invalid cursor")
	}
	return cursor.Sort, cursor.ID, nil
}

func openAPISendStatusFromMessage(msg *MailMessage, mailboxAddress string) openAPISendStatus {
	recipients := append(append([]string{}, msg.To...), msg.CC...)
	recipients = append(recipients, msg.BCC...)
	return openAPISendStatus{
		ID:             msg.ID,
		Status:         sendAuditAccepted,
		MessageID:      msg.ID,
		RFCMessageID:   msg.MessageID,
		MailboxID:      msg.MailboxID,
		MailboxAddress: mailboxAddress,
		Subject:        msg.Subject,
		Recipients:     dedupeEmails(recipients),
		CreatedAt:      msg.ReceivedAt,
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *App) resolveMailboxOwnerTx(ctx context.Context, tx *sql.Tx, userID, ownerEmail, address, displayName, passwordHash string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID != "" {
		var disabled int
		if err := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", errNotFound
			}
			return "", err
		}
		if intBool(disabled) {
			return "", errors.New("owner user is disabled")
		}
		return userID, nil
	}
	email := normalizeEmail(ownerEmail)
	if email == "" {
		email = address
	}
	if !strings.Contains(email, "@") {
		return "", errors.New("invalid owner email")
	}
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND disabled=0`, email).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	userID = newID("usr")
	now := a.now().UTC().Format(time.RFC3339Nano)
	if displayName == "" {
		displayName = email
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, email, displayName, "user", passwordHash, 0, now, now)
	return userID, err
}

func (a *App) ensureActiveUserExists(ctx context.Context, userID string) error {
	var disabled int
	if err := a.db.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if intBool(disabled) {
		return errors.New("owner user is disabled")
	}
	return nil
}

func respondMailboxOwnerError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		respondError(w, http.StatusNotFound, "owner user not found")
		return
	}
	if err != nil {
		badRequest(w, err)
		return
	}
}

func respondSendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoRecipients), errors.Is(err, errInvalidMIME), errors.Is(err, errAttachmentTooLarge):
		badRequest(w, err)
	case errors.Is(err, errSMTPRateLimited):
		respondError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errSenderNotAuthorized):
		respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, errMailboxQuotaExceeded):
		respondError(w, http.StatusInsufficientStorage, err.Error())
	default:
		respondError(w, http.StatusInternalServerError, err.Error())
	}
}

func (a *App) loadLatestSendQueueForMessage(ctx context.Context, sentMessageID, userID string) (SendQueueEntry, error) {
	row := a.db.QueryRowContext(ctx, `SELECT sq.id,sq.mailbox_id,sq.sent_message_id,sq.message_id,COALESCE(m.subject,''),sq.source,sq.mail_from,sq.header_from,sq.recipients_json,sq.status,sq.attempt_count,sq.max_attempts,sq.next_attempt_at,sq.last_error,sq.created_at,sq.updated_at,sq.delivered_at
		FROM send_queue sq JOIN mailboxes mb ON mb.id=sq.mailbox_id LEFT JOIN messages m ON m.id=sq.sent_message_id
		WHERE sq.sent_message_id=? AND mb.user_id=? ORDER BY sq.created_at DESC, sq.id DESC LIMIT 1`, sentMessageID, userID)
	return scanSendQueueEntry(row)
}

func (a *App) loadLatestSendQueueForMailboxMessage(ctx context.Context, sentMessageID, mailboxID string) (SendQueueEntry, error) {
	row := a.db.QueryRowContext(ctx, `SELECT sq.id,sq.mailbox_id,sq.sent_message_id,sq.message_id,COALESCE(m.subject,''),sq.source,sq.mail_from,sq.header_from,sq.recipients_json,sq.status,sq.attempt_count,sq.max_attempts,sq.next_attempt_at,sq.last_error,sq.created_at,sq.updated_at,sq.delivered_at
		FROM send_queue sq LEFT JOIN messages m ON m.id=sq.sent_message_id
		WHERE sq.sent_message_id=? AND sq.mailbox_id=? ORDER BY sq.created_at DESC, sq.id DESC LIMIT 1`, sentMessageID, mailboxID)
	return scanSendQueueEntry(row)
}

func (a *App) loadSendQueueEntryForSentMessage(ctx context.Context, sentMessageID, userID string) (SendQueueEntry, error) {
	return a.loadLatestSendQueueForMessage(ctx, sentMessageID, userID)
}

func (a *App) loadOpenAPISentMessageForUser(ctx context.Context, id, userID string) (*MailMessage, error) {
	var messageID string
	err := a.db.QueryRowContext(ctx, `SELECT m.id
		FROM messages m JOIN mailboxes mb ON mb.id=m.mailbox_id JOIN folders f ON f.id=m.folder_id
		WHERE (m.id=? OR m.message_id=?) AND mb.user_id=? AND lower(f.name)='sent'
		ORDER BY m.received_at DESC LIMIT 1`, id, id, userID).Scan(&messageID)
	if err != nil {
		return nil, err
	}
	return a.messageByID(ctx, messageID, false)
}

func parseOpenAPILimit(r *http.Request, defaultLimit, maxLimit int) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func parseOpenAPIOffset(r *http.Request) int {
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		return 0
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0
	}
	return offset
}
