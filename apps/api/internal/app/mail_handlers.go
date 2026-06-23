package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// mailMessagesPageSize is the max number of messages returned per page in mail listing.
const mailMessagesPageSize = 30

type AttachmentInput struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
}

type storedMessage struct {
	MailboxID     string
	FolderID      string
	RecipientAddr string
	MessageUID    string
	MessageID     string
	Subject       string
	From          string
	FromName      string
	To            []string
	CC            []string
	BCC           []string
	SentAt        time.Time
	ReceivedAt    time.Time
	Snippet       string
	BodyText      string
	BodyHTML      string
	IsRead        bool
	IsStarred     bool
	RawPath       string
}

func (a *App) handleMyMailboxes(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT mb.id,mb.user_id,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at
		FROM mailboxes mb
		JOIN domains d ON d.id=mb.domain_id
		WHERE mb.user_id=? AND mb.status='active' AND d.status='active'
		ORDER BY mb.address`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailboxes")
		return
	}
	defer rows.Close()
	items := []Mailbox{}
	for rows.Next() {
		var m Mailbox
		var created string
		if err := rows.Scan(&m.ID, &m.UserID, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan mailboxes")
			return
		}
		m.UserEmail = user.Email
		m.CreatedAt = parseTime(created)
		items = append(items, m)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleMailFolders(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT f.id,f.name,f.role,
		COALESCE(SUM(CASE WHEN m.is_read=0 THEN 1 ELSE 0 END),0) AS unread,
		COUNT(m.id) AS total
		FROM folders f LEFT JOIN messages m ON m.folder_id=f.id
		WHERE f.mailbox_id=? GROUP BY f.id,f.name,f.role
		ORDER BY CASE f.role WHEN 'inbox' THEN 1 WHEN 'sent' THEN 2 WHEN 'drafts' THEN 3 WHEN 'archive' THEN 4 WHEN 'spam' THEN 5 WHEN 'trash' THEN 6 ELSE 99 END, f.name`, mb.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load folders")
		return
	}
	defer rows.Close()
	items := []MailFolder{}
	for rows.Next() {
		var f MailFolder
		if err := rows.Scan(&f.ID, &f.Name, &f.Role, &f.UnreadCount, &f.TotalCount); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan folders")
			return
		}
		items = append(items, f)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleMailMessages(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	if labelID := strings.TrimSpace(r.URL.Query().Get("labelId")); labelID != "" {
		if !a.labelBelongsToMailbox(r.Context(), labelID, mb.ID) {
			respondError(w, http.StatusNotFound, "label not found")
			return
		}
		a.respondMailMessageList(w, r, `m.mailbox_id=? AND EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id=m.id AND ml.label_id=?)`, []any{mb.ID, labelID})
		return
	}
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = "Inbox"
	}
	folderID, err := a.ensureFolder(r.Context(), mb.ID, folder)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load folder")
		return
	}
	a.respondMailMessageList(w, r, `m.mailbox_id=? AND m.folder_id=?`, []any{mb.ID, folderID})
}

func (a *App) handleStarredMessages(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	a.respondMailMessageList(w, r, `m.mailbox_id=? AND m.is_starred=1`, []any{mb.ID})
}

func (a *App) respondMailMessageList(w http.ResponseWriter, r *http.Request, where string, args []any) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	if offset < 0 {
		offset = 0
	}
	limit := mailMessagesPageSize

	if q != "" {
		where += ` AND (m.subject LIKE ? OR m.from_addr LIKE ? OR m.from_name LIKE ? OR m.snippet LIKE ? OR m.body_text LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like)
	}
	args = append(args, limit+1, offset)
	query := `SELECT m.id,m.mailbox_id,m.folder_id,COALESCE(f.name,''),m.message_uid,m.message_id,m.subject,m.from_addr,COALESCE(m.from_name,''),m.to_addrs,m.cc_addrs,m.bcc_addrs,m.sent_at,m.received_at,m.snippet,m.is_read,m.is_starred,m.has_attachments,m.size_bytes
		FROM messages m LEFT JOIN folders f ON f.id=m.folder_id WHERE ` + where + ` ORDER BY m.received_at DESC LIMIT ? OFFSET ?`
	rows, err := a.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	defer rows.Close()
	items := []MailMessage{}
	for rows.Next() {
		msg, err := scanMessageSummary(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan messages")
			return
		}
		items = append(items, msg)
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.Itoa(offset + limit)
	}
	if err := a.attachLabelsToMessages(r.Context(), items); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load labels")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleMailLabels(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	labels, err := a.labelsForMailbox(r.Context(), mb.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load labels")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": labels})
}

func (a *App) handleCreateMailLabel(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	label, err := a.ensureLabel(r.Context(), mb.ID, req.Name, req.Color)
	if err != nil {
		badRequest(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, label)
}

func (a *App) handleDeleteMailLabel(w http.ResponseWriter, r *http.Request) {
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	labelID := chi.URLParam(r, "id")
	if labelID == "" {
		badRequest(w, fmt.Errorf("label id is required"))
		return
	}
	ctx := r.Context()
	if !a.labelBelongsToMailbox(ctx, labelID, mb.ID) {
		respondError(w, http.StatusNotFound, "label not found")
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_labels WHERE label_id = ?", labelID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to remove label associations")
		return
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM mail_labels WHERE id=? AND mailbox_id=?`, labelID, mb.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete label")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "label not found")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to commit transaction")
		return
	}
	labels, err := a.labelsForMailbox(ctx, mb.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load labels")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"labels": labels})
}

func (a *App) handleAddMessageLabel(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	label, err := a.ensureLabel(r.Context(), msg.MailboxID, req.Name, req.Color)
	if err != nil {
		badRequest(w, err)
		return
	}
	_, err = a.db.ExecContext(r.Context(), `INSERT OR IGNORE INTO message_labels(message_id,label_id,created_at) VALUES(?,?,?)`, msg.ID, label.ID, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to add label")
		return
	}
	labels, err := a.labelsForMessage(r.Context(), msg.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load labels")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"labels": labels})
}

func (a *App) handleRemoveMessageLabel(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	labelID := strings.TrimSpace(chi.URLParam(r, "labelID"))
	if !a.labelBelongsToMailbox(r.Context(), labelID, msg.MailboxID) {
		respondError(w, http.StatusNotFound, "label not found")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM message_labels WHERE message_id=? AND label_id=?`, msg.ID, labelID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to remove label")
		return
	}
	labels, err := a.labelsForMessage(r.Context(), msg.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load labels")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"labels": labels})
}

func (a *App) handleMailMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	if r.URL.Query().Get("markRead") != "0" && !msg.IsRead && userHasPermission(currentUser(r), PermissionMailOrganize) {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE messages SET is_read=1, updated_at=? WHERE id=?`, a.now().UTC().Format(time.RFC3339Nano), msg.ID)
		msg.IsRead = true
	}
	respondJSON(w, http.StatusOK, msg)
}

type mailComposeInput struct {
	MailboxID   string            `json:"mailboxId"`
	To          []string          `json:"to"`
	CC          []string          `json:"cc"`
	BCC         []string          `json:"bcc"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Attachments []AttachmentInput `json:"attachments"`
}

type mailDraftInput struct {
	MailboxID   string             `json:"mailboxId"`
	To          []string           `json:"to"`
	CC          []string           `json:"cc"`
	BCC         []string           `json:"bcc"`
	Subject     string             `json:"subject"`
	Text        string             `json:"text"`
	HTML        string             `json:"html"`
	Attachments *[]AttachmentInput `json:"attachments"`
}

type scheduledSendPayload struct {
	MailboxID   string            `json:"mailboxId"`
	To          []string          `json:"to"`
	CC          []string          `json:"cc"`
	BCC         []string          `json:"bcc"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Attachments []AttachmentInput `json:"attachments"`
	DraftID     string            `json:"draftId,omitempty"`
}

type ScheduledSend struct {
	ID        string     `json:"id"`
	MailboxID string     `json:"mailboxId"`
	DraftID   string     `json:"draftId,omitempty"`
	Subject   string     `json:"subject"`
	To        []string   `json:"to"`
	Snippet   string     `json:"snippet"`
	SendAt    time.Time  `json:"sendAt"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
}

func (a *App) handleMailSend(w http.ResponseWriter, r *http.Request) {
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
	msg, err := a.sendMailNow(r.Context(), currentUser(r), mb, req)
	if err != nil {
		if errors.Is(err, errNoRecipients) {
			badRequest(w, err)
			return
		}
		if errors.Is(err, errInvalidMIME) {
			badRequest(w, err)
			return
		}
		if errors.Is(err, errAttachmentTooLarge) {
			badRequest(w, err)
			return
		}
		if errors.Is(err, errSMTPRateLimited) {
			respondError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if strings.HasPrefix(err.Error(), "smtp delivery failed:") {
			respondError(w, http.StatusBadGateway, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, msg)
}

var errNoRecipients = errors.New("at least one recipient is required")
var errInvalidMIME = errors.New("invalid mime message")
var errAttachmentTooLarge = errors.New("attachment size exceeds permission limit")
var errSMTPRateLimited = errors.New("smtp send rate limit exceeded")

func (a *App) sendMailNow(ctx context.Context, user *User, mb *Mailbox, req mailComposeInput) (*MailMessage, error) {
	if err := validateAttachmentLimit(req.Attachments, userLimits(user)); err != nil {
		return nil, err
	}
	req.To, req.CC, req.BCC = dedupeEmails(req.To), dedupeEmails(req.CC), dedupeEmails(req.BCC)
	allRecipients := append(append([]string{}, req.To...), append(req.CC, req.BCC...)...)
	if len(allRecipients) == 0 {
		return nil, errNoRecipients
	}
	if strings.TrimSpace(req.Subject) == "" {
		req.Subject = "(no subject)"
	}
	req.HTML = a.policy.Sanitize(req.HTML)
	if strings.TrimSpace(req.Text) == "" {
		req.Text = stripTags(req.HTML)
	}
	if strings.TrimSpace(req.HTML) == "" {
		req.HTML = "<p>" + htmlEscape(req.Text) + "</p>"
	}

	now := a.now().UTC()
	messageID := fmt.Sprintf("<%s@%s>", newID("msg"), strings.Split(mb.Address, "@")[1])
	mimeBytes, err := BuildMIME(MIMEMessage{
		From: mb.Address, FromName: mb.DisplayName, To: req.To, CC: req.CC, BCC: req.BCC, Subject: req.Subject, Text: req.Text, HTML: req.HTML, MessageID: messageID, Date: now, Attachments: req.Attachments,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidMIME, err)
	}
	if err := a.recordSMTPRate(ctx, user, mb); err != nil {
		return nil, err
	}
	if a.cfg.SMTPHost != "" {
		if err := a.sendSMTP(mb.Address, allRecipients, mimeBytes); err != nil {
			return nil, fmt.Errorf("smtp delivery failed: %w", err)
		}
	}

	sentFolderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		return nil, fmt.Errorf("failed to load sent folder: %w", err)
	}
	base := storedMessage{MailboxID: mb.ID, FolderID: sentFolderID, MessageUID: newID("uid"), MessageID: messageID, Subject: req.Subject, From: mb.Address, FromName: mb.DisplayName, To: req.To, CC: req.CC, BCC: req.BCC, SentAt: now, ReceivedAt: now, Snippet: snippetFrom(req.Text, req.HTML), BodyText: req.Text, BodyHTML: req.HTML, IsRead: true}
	sentID, err := a.insertMessage(ctx, base, req.Attachments)
	if err != nil {
		return nil, fmt.Errorf("failed to store sent message: %w", err)
	}

	// Development/local-domain delivery: known local recipients go to their Inbox.
	// When catch-all is enabled, unknown local recipients are stored as unregistered
	// messages visible only in the admin "全部邮件" view.
	localRecipients := append(req.To, req.CC...)
	localRecipients = append(localRecipients, req.BCC...)
	for _, rcpt := range localRecipients {
		rcptMailbox, err := a.mailboxByAddress(ctx, rcpt)
		if err != nil {
			if !a.cfg.CatchAllEnabled || !a.isLocalDomainAddress(ctx, rcpt) {
				continue
			}
			copyMsg := base
			copyMsg.MailboxID = ""
			copyMsg.FolderID = ""
			copyMsg.RecipientAddr = normalizeEmail(rcpt)
			copyMsg.MessageUID = newID("uid")
			copyMsg.IsRead = false
			_, _ = a.insertMessage(ctx, copyMsg, req.Attachments)
			continue
		}
		if rcptMailbox.Status != "active" {
			if a.cfg.CatchAllEnabled && a.isLocalDomainAddress(ctx, rcpt) {
				copyMsg := base
				copyMsg.MailboxID = ""
				copyMsg.FolderID = ""
				copyMsg.RecipientAddr = normalizeEmail(rcpt)
				copyMsg.MessageUID = newID("uid")
				copyMsg.IsRead = false
				_, _ = a.insertMessage(ctx, copyMsg, req.Attachments)
			}
			continue
		}
		inboxID, err := a.ensureFolder(ctx, rcptMailbox.ID, "Inbox")
		if err != nil {
			continue
		}
		copyMsg := base
		copyMsg.MailboxID = rcptMailbox.ID
		copyMsg.FolderID = inboxID
		copyMsg.MessageUID = newID("uid")
		copyMsg.IsRead = false
		if inboxMsgID, err := a.insertMessage(ctx, copyMsg, req.Attachments); err == nil {
			a.applyInboundControls(ctx, inboxMsgID, rcptMailbox.ID, copyMsg.From, copyMsg.Subject)
		}
	}

	msg, _ := a.messageByID(ctx, sentID, true)
	return msg, nil
}

func userLimits(user *User) PermissionLimits {
	if user == nil {
		return defaultPermissionLimits()
	}
	return user.Limits
}

func validateAttachmentLimit(attachments []AttachmentInput, limits PermissionLimits) error {
	limitBytes := attachmentLimitBytes(limits)
	if limitBytes == 0 {
		return nil
	}
	for _, att := range attachments {
		decodedLen, err := decodedBase64Len(att.ContentBase64)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalidMIME, err)
		}
		if decodedLen > limitBytes {
			return fmt.Errorf("%w: max %d MB", errAttachmentTooLarge, limits.MaxAttachmentMB)
		}
	}
	return nil
}

func decodedBase64Len(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

var errIMAPRateLimited = errors.New("imap rate limit exceeded")
var errPOP3RateLimited = errors.New("pop3 rate limit exceeded")

func (a *App) checkAndRecordProtocolRate(ctx context.Context, user *User, mb *Mailbox, table string, dailyLimit, minuteLimit int) error {
	if dailyLimit == 0 && minuteLimit == 0 {
		return nil
	}
	now := a.now().UTC()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if dailyLimit > 0 {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE user_id=? AND created_at>=?", user.ID, now.Add(-24*time.Hour).Format(time.RFC3339Nano)).Scan(&count); err != nil {
			return err
		}
		if count >= dailyLimit {
			return fmt.Errorf("daily limit %d", dailyLimit)
		}
	}
	if minuteLimit > 0 {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE user_id=? AND created_at>=?", user.ID, now.Add(-time.Minute).Format(time.RFC3339Nano)).Scan(&count); err != nil {
			return err
		}
		if count >= minuteLimit {
			return fmt.Errorf("per-minute limit %d", minuteLimit)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+"(id,user_id,mailbox_id,created_at) VALUES(?,?,?,?)", newID("evt"), user.ID, mb.ID, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) handleAuthPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login        string `json:"login"`
		Protocol     string `json:"protocol"`
		Username     string `json:"username"`
		IP           string `json:"ip"`
		Remote       string `json:"remote"`
		Success      *bool  `json:"success"`
		PolicyReject *bool  `json:"policy_reject"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondJSON(w, http.StatusOK, map[string]int{"status": 0})
		return
	}
	var user *User
	var mailbox *Mailbox
	login := normalizeEmail(req.Login)
	if login == "" {
		login = normalizeEmail(req.Username)
	}
	if login != "" {
		if mb, err := a.mailboxByAddress(r.Context(), login); err == nil {
			mailbox = mb
			user, _ = a.userByID(r.Context(), mb.UserID)
		}
		if user == nil {
			var passHash string
			user, passHash, _ = a.userByEmail(r.Context(), login)
			_ = passHash
		}
	}
	if user == nil || user.Disabled {
		respondJSON(w, http.StatusOK, map[string]any{"status": -1, "msg": "user not found or disabled"})
		return
	}
	if user.Role == "admin" {
		respondJSON(w, http.StatusOK, map[string]int{"status": 0})
		return
	}
	limits := user.Limits
	var err error
	switch strings.ToLower(req.Protocol) {
	case "imap":
		if limits.IMAPMinuteLimit > 0 {
			if mailbox == nil {
				err = errors.New("mailbox not found")
			} else {
				err = a.checkAndRecordProtocolRate(r.Context(), user, mailbox, "imap_events", 0, limits.IMAPMinuteLimit)
			}
		}
	case "pop3":
		if limits.POP3MinuteLimit > 0 {
			if mailbox == nil {
				err = errors.New("mailbox not found")
			} else {
				err = a.checkAndRecordProtocolRate(r.Context(), user, mailbox, "pop3_events", 0, limits.POP3MinuteLimit)
			}
		}
	}
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]any{"status": -1, "msg": err.Error()})
	} else {
		respondJSON(w, http.StatusOK, map[string]int{"status": 0})
	}
}

func (a *App) recordSMTPRate(ctx context.Context, user *User, mb *Mailbox) error {
	if user == nil || mb == nil || user.Role == "admin" {
		return nil
	}
	limits := user.Limits
	if limits.SMTPDailyLimit == 0 && limits.SMTPMinuteLimit == 0 {
		return nil
	}
	now := a.now().UTC()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if limits.SMTPDailyLimit > 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM smtp_send_events WHERE user_id=? AND created_at>=?`, user.ID, now.Add(-24*time.Hour).Format(time.RFC3339Nano)).Scan(&count); err != nil {
			return err
		}
		if count >= limits.SMTPDailyLimit {
			return fmt.Errorf("%w: daily limit %d", errSMTPRateLimited, limits.SMTPDailyLimit)
		}
	}
	if limits.SMTPMinuteLimit > 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM smtp_send_events WHERE user_id=? AND created_at>=?`, user.ID, now.Add(-time.Minute).Format(time.RFC3339Nano)).Scan(&count); err != nil {
			return err
		}
		if count >= limits.SMTPMinuteLimit {
			return fmt.Errorf("%w: per-minute limit %d", errSMTPRateLimited, limits.SMTPMinuteLimit)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO smtp_send_events(id,user_id,mailbox_id,created_at) VALUES(?,?,?,?)`, newID("smtp"), user.ID, mb.ID, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	var req mailDraftInput
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, req.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	if req.Attachments != nil {
		if err := validateAttachmentLimit(*req.Attachments, userLimits(currentUser(r))); err != nil {
			if errors.Is(err, errAttachmentTooLarge) || errors.Is(err, errInvalidMIME) {
				badRequest(w, err)
				return
			}
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	compose := mailComposeInput{MailboxID: req.MailboxID, To: req.To, CC: req.CC, BCC: req.BCC, Subject: req.Subject, Text: req.Text, HTML: req.HTML}
	compose.To, compose.CC, compose.BCC = dedupeEmails(compose.To), dedupeEmails(compose.CC), dedupeEmails(compose.BCC)
	subject := strings.TrimSpace(compose.Subject)
	if subject == "" {
		subject = "(无主题)"
	}
	compose.HTML = a.policy.Sanitize(compose.HTML)
	if strings.TrimSpace(compose.Text) == "" {
		compose.Text = stripTags(compose.HTML)
	}
	if strings.TrimSpace(compose.HTML) == "" && strings.TrimSpace(compose.Text) != "" {
		compose.HTML = "<p>" + htmlEscape(compose.Text) + "</p>"
	}

	now := a.now().UTC()
	draftID := strings.TrimSpace(chi.URLParam(r, "id"))
	draftsFolderID, err := a.ensureFolder(r.Context(), mb.ID, "Drafts")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load drafts folder")
		return
	}
	if draftID == "" {
		messageID := fmt.Sprintf("<%s@%s>", newID("draft"), strings.Split(mb.Address, "@")[1])
		attachments := []AttachmentInput{}
		if req.Attachments != nil {
			attachments = *req.Attachments
		}
		stored := storedMessage{MailboxID: mb.ID, FolderID: draftsFolderID, MessageUID: newID("uid"), MessageID: messageID, Subject: subject, From: mb.Address, FromName: mb.DisplayName, To: compose.To, CC: compose.CC, BCC: compose.BCC, SentAt: now, ReceivedAt: now, Snippet: snippetFrom(compose.Text, compose.HTML), BodyText: compose.Text, BodyHTML: compose.HTML, IsRead: true}
		draftID, err = a.insertMessage(r.Context(), stored, attachments)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save draft")
			return
		}
		msg, _ := a.messageByID(r.Context(), draftID, true)
		respondJSON(w, http.StatusCreated, msg)
		return
	}

	existing, err := a.loadMessageForRequest(r, draftID, false)
	if err != nil || !strings.EqualFold(existing.Folder, "Drafts") || existing.MailboxID != mb.ID {
		respondError(w, http.StatusNotFound, "draft not found")
		return
	}
	size := int64(len(compose.Text) + len(compose.HTML))
	hasAttachments := existing.HasAttachments
	if req.Attachments != nil {
		a.deleteMessageFiles(r.Context(), draftID)
		if _, err := a.db.ExecContext(r.Context(), `DELETE FROM attachments WHERE message_id=?`, draftID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to replace draft attachments")
			return
		}
		hasAttachments = len(*req.Attachments) > 0
		for _, att := range *req.Attachments {
			if decoded, err := base64.StdEncoding.DecodeString(att.ContentBase64); err == nil {
				size += int64(len(decoded))
			}
		}
	} else {
		var attachmentBytes int64
		_ = a.db.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(size_bytes),0) FROM attachments WHERE message_id=?`, draftID).Scan(&attachmentBytes)
		size += attachmentBytes
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE messages SET subject=?,to_addrs=?,cc_addrs=?,bcc_addrs=?,sent_at=?,received_at=?,snippet=?,body_text=?,body_html=?,is_read=1,has_attachments=?,size_bytes=?,updated_at=? WHERE id=?`,
		subject, jsonEncode(compose.To), jsonEncode(compose.CC), jsonEncode(compose.BCC), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), snippetFrom(compose.Text, compose.HTML), compose.Text, compose.HTML, boolInt(hasAttachments), size, now.Format(time.RFC3339Nano), draftID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update draft")
		return
	}
	if req.Attachments != nil {
		for _, att := range *req.Attachments {
			if err := a.storeAttachment(r.Context(), draftID, att); err != nil {
				respondError(w, http.StatusInternalServerError, "failed to store draft attachment")
				return
			}
		}
	}
	msg, _ := a.messageByID(r.Context(), draftID, true)
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleDeleteDraft(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil || !strings.EqualFold(msg.Folder, "Drafts") {
		respondError(w, http.StatusNotFound, "draft not found")
		return
	}
	a.deleteMessageFiles(r.Context(), msg.ID)
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM messages WHERE id=?`, msg.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete draft")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleScheduledSends(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mb, err := a.mailboxForCurrentUser(r)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,mailbox_id,draft_id,payload_json,send_at,status,error,created_at,updated_at,sent_at
		FROM scheduled_sends
		WHERE user_id=? AND mailbox_id=? AND status IN ('pending','sending','failed')
		ORDER BY send_at ASC, created_at DESC`, user.ID, mb.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load scheduled sends")
		return
	}
	defer rows.Close()
	items := []ScheduledSend{}
	for rows.Next() {
		var item ScheduledSend
		var draftID, errorText, sentAt sql.NullString
		var payloadJSON, sendAt, createdAt, updatedAt string
		if err := rows.Scan(&item.ID, &item.MailboxID, &draftID, &payloadJSON, &sendAt, &item.Status, &errorText, &createdAt, &updatedAt, &sentAt); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan scheduled sends")
			return
		}
		if draftID.Valid {
			item.DraftID = draftID.String
		}
		if errorText.Valid {
			item.Error = errorText.String
		}
		item.SendAt = parseTime(sendAt)
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		item.SentAt = nullableTime(sentAt)
		applyScheduledSendPreview(&item, payloadJSON)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load scheduled sends")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleScheduleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MailboxID   string            `json:"mailboxId"`
		To          []string          `json:"to"`
		CC          []string          `json:"cc"`
		BCC         []string          `json:"bcc"`
		Subject     string            `json:"subject"`
		Text        string            `json:"text"`
		HTML        string            `json:"html"`
		Attachments []AttachmentInput `json:"attachments"`
		DraftID     string            `json:"draftId"`
		SendAt      string            `json:"sendAt"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, req.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	sendAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(req.SendAt))
	if err != nil {
		badRequest(w, errors.New("sendAt is required"))
		return
	}
	if !sendAt.After(a.now().Add(30 * time.Second)) {
		badRequest(w, errors.New("sendAt must be in the future"))
		return
	}
	compose := mailComposeInput{MailboxID: req.MailboxID, To: req.To, CC: req.CC, BCC: req.BCC, Subject: req.Subject, Text: req.Text, HTML: req.HTML, Attachments: req.Attachments}
	if err := validateAttachmentLimit(compose.Attachments, userLimits(currentUser(r))); err != nil {
		if errors.Is(err, errAttachmentTooLarge) || errors.Is(err, errInvalidMIME) {
			badRequest(w, err)
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	compose.To, compose.CC, compose.BCC = dedupeEmails(compose.To), dedupeEmails(compose.CC), dedupeEmails(compose.BCC)
	if len(append(append([]string{}, compose.To...), append(compose.CC, compose.BCC...)...)) == 0 {
		badRequest(w, errNoRecipients)
		return
	}
	compose.HTML = a.policy.Sanitize(compose.HTML)
	if strings.TrimSpace(compose.Text) == "" {
		compose.Text = stripTags(compose.HTML)
	}
	if strings.TrimSpace(compose.HTML) == "" {
		compose.HTML = "<p>" + htmlEscape(compose.Text) + "</p>"
	}
	if strings.TrimSpace(compose.Subject) == "" {
		compose.Subject = "(no subject)"
	}
	draftID := strings.TrimSpace(req.DraftID)
	if draftID != "" {
		msg, err := a.loadMessageForRequest(r, draftID, false)
		if err != nil || !strings.EqualFold(msg.Folder, "Drafts") || msg.MailboxID != mb.ID {
			respondError(w, http.StatusNotFound, "draft not found")
			return
		}
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	payload := scheduledSendPayload{MailboxID: compose.MailboxID, To: compose.To, CC: compose.CC, BCC: compose.BCC, Subject: compose.Subject, Text: compose.Text, HTML: compose.HTML, Attachments: compose.Attachments, DraftID: draftID}
	item := ScheduledSend{ID: newID("sched"), MailboxID: mb.ID, DraftID: draftID, Subject: payload.Subject, To: payload.To, Snippet: snippetFrom(payload.Text, payload.HTML), SendAt: sendAt.UTC(), Status: "pending", CreatedAt: parseTime(now), UpdatedAt: parseTime(now)}
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO scheduled_sends(id,user_id,mailbox_id,draft_id,payload_json,send_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, item.ID, currentUser(r).ID, mb.ID, nullableString(draftID), jsonEncode(payload), item.SendAt.Format(time.RFC3339Nano), item.Status, now, now); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to schedule send")
		return
	}
	respondJSON(w, http.StatusCreated, item)
}

func applyScheduledSendPreview(item *ScheduledSend, payloadJSON string) {
	var payload scheduledSendPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		item.Subject = "(no subject)"
		return
	}
	item.Subject = strings.TrimSpace(payload.Subject)
	if item.Subject == "" {
		item.Subject = "(no subject)"
	}
	item.To = payload.To
	item.Snippet = snippetFrom(payload.Text, payload.HTML)
}

func (a *App) handleCancelScheduledSend(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	var status string
	if err := a.db.QueryRowContext(r.Context(), `SELECT status FROM scheduled_sends WHERE id=? AND user_id=?`, id, user.ID).Scan(&status); err != nil {
		respondError(w, http.StatusNotFound, "scheduled send not found")
		return
	}
	if status != "pending" && status != "failed" {
		badRequest(w, errors.New("scheduled send is not pending"))
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE scheduled_sends SET status='cancelled',updated_at=? WHERE id=? AND user_id=?`, a.now().UTC().Format(time.RFC3339Nano), id, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to cancel scheduled send")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) scheduledSendWorker(ctx context.Context) {
	a.log.Info("scheduled send worker started")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.processDueScheduledSends(ctx); err != nil {
			a.log.Warn("scheduled send worker failed", "error", err)
		}
		select {
		case <-ctx.Done():
			a.log.Info("scheduled send worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func (a *App) smtpEventsCleanupWorker(ctx context.Context) {
	a.log.Info("smtp events cleanup worker started")
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Info("smtp events cleanup worker stopped")
			return
		case <-ticker.C:
			a.cleanupStaleEvents(ctx)
		}
	}
}

func (a *App) cleanupStaleEvents(ctx context.Context) {
	cutoff := a.now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for _, table := range []string{"smtp_send_events", "imap_events", "pop3_events"} {
		result, err := a.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE created_at<?", cutoff)
		if err != nil {
			a.log.Warn("event cleanup failed", "table", table, "error", err)
		} else if n, _ := result.RowsAffected(); n > 0 {
			a.log.Debug("event cleanup deleted rows", "table", table, "count", n)
		}
	}
}

func (a *App) processDueScheduledSends(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,mailbox_id,draft_id,payload_json FROM scheduled_sends WHERE status='pending' AND send_at<=? ORDER BY send_at LIMIT 20`, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer rows.Close()
	type dueItem struct {
		id, mailboxID, draftID, payloadJSON string
	}
	items := []dueItem{}
	for rows.Next() {
		var item dueItem
		var draftID sql.NullString
		if err := rows.Scan(&item.id, &item.mailboxID, &draftID, &item.payloadJSON); err != nil {
			return err
		}
		if draftID.Valid {
			item.draftID = draftID.String
		}
		items = append(items, item)
	}
	for _, item := range items {
		a.processScheduledSend(ctx, item.id, item.mailboxID, item.draftID, item.payloadJSON)
	}
	return rows.Err()
}

func (a *App) processScheduledSend(ctx context.Context, id, mailboxID, draftID, payloadJSON string) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	res, err := a.db.ExecContext(ctx, `UPDATE scheduled_sends SET status='sending',updated_at=? WHERE id=? AND status='pending'`, now, id)
	if err != nil {
		a.log.Warn("failed to claim scheduled send", "id", id, "error", err)
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return
	}
	var payload scheduledSendPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		a.markScheduledSendFailed(ctx, id, "invalid scheduled payload")
		return
	}
	mb, err := a.mailboxByID(ctx, mailboxID)
	if err != nil || mb.Status != "active" {
		a.markScheduledSendFailed(ctx, id, "mailbox not found")
		return
	}
	user, err := a.userByID(ctx, mb.UserID)
	if err != nil || !userHasPermission(user, PermissionMailSchedule) || !userHasPermission(user, PermissionMailSend) {
		a.markScheduledSendFailed(ctx, id, "mail send permission revoked")
		return
	}
	compose := mailComposeInput{MailboxID: payload.MailboxID, To: payload.To, CC: payload.CC, BCC: payload.BCC, Subject: payload.Subject, Text: payload.Text, HTML: payload.HTML, Attachments: payload.Attachments}
	if _, err := a.sendMailNow(ctx, user, mb, compose); err != nil {
		a.markScheduledSendFailed(ctx, id, err.Error())
		return
	}
	if draftID != "" {
		a.deleteMessageFiles(ctx, draftID)
		_, _ = a.db.ExecContext(ctx, `DELETE FROM messages WHERE id=?`, draftID)
	}
	sentAt := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `UPDATE scheduled_sends SET status='sent',sent_at=?,updated_at=?,error='' WHERE id=?`, sentAt, sentAt, id); err != nil {
		a.log.Warn("failed to mark scheduled send sent", "id", id, "error", err)
	}
}

func (a *App) markScheduledSendFailed(ctx context.Context, id, message string) {
	if _, err := a.db.ExecContext(ctx, `UPDATE scheduled_sends SET status='failed',error=?,updated_at=? WHERE id=?`, message, a.now().UTC().Format(time.RFC3339Nano), id); err != nil {
		a.log.Warn("failed to mark scheduled send failed", "id", id, "error", err)
	}
}

func (a *App) isLocalDomainAddress(ctx context.Context, address string) bool {
	parts := strings.Split(normalizeEmail(address), "@")
	if len(parts) != 2 || parts[1] == "" {
		return false
	}
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM domains WHERE name=? AND status='active'`, parts[1]).Scan(&count)
	return count > 0
}

func (a *App) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
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
	_, err = a.db.ExecContext(r.Context(), `UPDATE messages SET is_read=?, updated_at=? WHERE id=?`, boolInt(read), a.now().UTC().Format(time.RFC3339Nano), msg.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "read": read})
}

func (a *App) handleStar(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	var req struct {
		Starred *bool `json:"starred"`
	}
	_ = decodeJSON(r, &req)
	starred := !msg.IsStarred
	if req.Starred != nil {
		starred = *req.Starred
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE messages SET is_starred=?, updated_at=? WHERE id=?`, boolInt(starred), a.now().UTC().Format(time.RFC3339Nano), msg.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "starred": starred})
}

func (a *App) handleMove(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	folderID, err := a.ensureFolder(r.Context(), msg.MailboxID, req.Folder)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load folder")
		return
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, folderID, a.now().UTC().Format(time.RFC3339Nano), msg.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to move message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), false)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	if strings.EqualFold(msg.Folder, "Trash") {
		a.deleteMessageFiles(r.Context(), msg.ID)
		_, err = a.db.ExecContext(r.Context(), `DELETE FROM messages WHERE id=?`, msg.ID)
	} else {
		trashID, e := a.ensureFolder(r.Context(), msg.MailboxID, "Trash")
		if e != nil {
			err = e
		} else {
			_, err = a.db.ExecContext(r.Context(), `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, trashID, a.now().UTC().Format(time.RFC3339Nano), msg.ID)
		}
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAttachment(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	attID := chi.URLParam(r, "id")
	row := a.db.QueryRowContext(r.Context(), `SELECT a.filename,a.content_type,a.size_bytes,a.storage_path
		FROM attachments a JOIN messages m ON m.id=a.message_id JOIN mailboxes mb ON mb.id=m.mailbox_id
		WHERE a.id=? AND mb.user_id=?`, attID, user.ID)
	var filename, contentType, path string
	var size int64
	if err := row.Scan(&filename, &contentType, &size, &path); err != nil {
		respondError(w, http.StatusNotFound, "attachment not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		respondError(w, http.StatusNotFound, "attachment file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, f)
}

func (a *App) handleAdminAttachment(w http.ResponseWriter, r *http.Request) {
	attID := chi.URLParam(r, "id")
	row := a.db.QueryRowContext(r.Context(), `SELECT filename,content_type,size_bytes,storage_path FROM attachments WHERE id=?`, attID)
	var filename, contentType, path string
	var size int64
	if err := row.Scan(&filename, &contentType, &size, &path); err != nil {
		respondError(w, http.StatusNotFound, "attachment not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		respondError(w, http.StatusNotFound, "attachment file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, f)
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	fmt.Fprintf(w, "event: sync\ndata: {\"status\":\"connected\"}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-ticker.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"time\":\"%s\"}\n\n", t.UTC().Format(time.RFC3339))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (a *App) mailboxForCurrentUser(r *http.Request) (*Mailbox, error) {
	return a.mailboxForCurrentUserWithID(r, r.URL.Query().Get("mailboxId"))
}

func (a *App) mailboxForCurrentUserWithID(r *http.Request, mailboxID string) (*Mailbox, error) {
	user := currentUser(r)
	if user == nil {
		return nil, errors.New("no user")
	}
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID != "" {
		row := a.db.QueryRowContext(r.Context(), `SELECT id,user_id,domain_id,local_part,address,display_name,quota_mb,status,created_at
			FROM mailboxes WHERE id=? AND user_id=? AND status='active'`, mailboxID, user.ID)
		var m Mailbox
		var created string
		if err := row.Scan(&m.ID, &m.UserID, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
			return nil, err
		}
		m.UserEmail = user.Email
		m.CreatedAt = parseTime(created)
		return &m, nil
	}
	return a.mailboxForUser(r.Context(), user.ID)
}

func (a *App) mailboxByAddress(ctx context.Context, address string) (*Mailbox, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,domain_id,local_part,address,display_name,quota_mb,status,created_at FROM mailboxes WHERE address=? AND status='active'`, normalizeEmail(address))
	var m Mailbox
	var created string
	if err := row.Scan(&m.ID, &m.UserID, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	return &m, nil
}

func (a *App) loadMessageForRequest(r *http.Request, id string, includeBody bool) (*MailMessage, error) {
	user := currentUser(r)
	row := a.db.QueryRowContext(r.Context(), `SELECT m.id FROM messages m JOIN mailboxes mb ON mb.id=m.mailbox_id WHERE m.id=? AND mb.user_id=?`, id, user.ID)
	var messageID string
	if err := row.Scan(&messageID); err != nil {
		return nil, err
	}
	return a.messageByID(r.Context(), messageID, includeBody)
}

func (a *App) messageByID(ctx context.Context, id string, includeBody bool) (*MailMessage, error) {
	row := a.db.QueryRowContext(ctx, `SELECT m.id,COALESCE(m.mailbox_id,''),COALESCE(m.recipient_addr,''),COALESCE(m.folder_id,''),COALESCE(f.name,'Unregistered'),m.message_uid,m.message_id,m.subject,m.from_addr,COALESCE(m.from_name,''),m.to_addrs,m.cc_addrs,m.bcc_addrs,m.sent_at,m.received_at,m.snippet,m.body_text,m.body_html,m.is_read,m.is_starred,m.has_attachments,m.size_bytes
		FROM messages m LEFT JOIN folders f ON f.id=m.folder_id WHERE m.id=?`, id)
	msg, err := scanMessageFull(row, includeBody)
	if err != nil {
		return nil, err
	}
	if includeBody {
		atts, err := a.attachmentsForMessage(ctx, id)
		if err != nil {
			return nil, err
		}
		msg.Attachments = atts
	}
	labels, err := a.labelsForMessage(ctx, id)
	if err != nil {
		return nil, err
	}
	msg.Labels = labels
	return &msg, nil
}

func (a *App) insertMessage(ctx context.Context, msg storedMessage, attachments []AttachmentInput) (string, error) {
	id := newID("mail")
	now := a.now().UTC().Format(time.RFC3339Nano)
	hasAttachments := len(attachments) > 0
	size := int64(len(msg.BodyText) + len(msg.BodyHTML))
	for _, att := range attachments {
		if decoded, err := base64.StdEncoding.DecodeString(att.ContentBase64); err == nil {
			size += int64(len(decoded))
		}
	}
	var mailboxID, folderID any
	if strings.TrimSpace(msg.MailboxID) != "" {
		mailboxID = msg.MailboxID
	}
	if strings.TrimSpace(msg.FolderID) != "" {
		folderID = msg.FolderID
	}
	recipientAddr := normalizeEmail(msg.RecipientAddr)
	_, err := a.db.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,folder_id,recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,raw_path,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, mailboxID, folderID, recipientAddr, msg.MessageUID, msg.MessageID, msg.Subject, msg.From, msg.FromName, jsonEncode(msg.To), jsonEncode(msg.CC), jsonEncode(msg.BCC), msg.SentAt.Format(time.RFC3339Nano), msg.ReceivedAt.Format(time.RFC3339Nano), msg.Snippet, msg.BodyText, msg.BodyHTML, boolInt(msg.IsRead), boolInt(msg.IsStarred), boolInt(hasAttachments), size, msg.RawPath, now, now)
	if err != nil {
		return "", err
	}
	for _, att := range attachments {
		if err := a.storeAttachment(ctx, id, att); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (a *App) storeAttachment(ctx context.Context, messageID string, input AttachmentInput) error {
	filename := filepath.Base(strings.TrimSpace(input.Filename))
	if filename == "." || filename == "" {
		filename = "attachment.bin"
	}
	contentType := input.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	data, err := base64.StdEncoding.DecodeString(input.ContentBase64)
	if err != nil {
		return err
	}
	dir := filepath.Join(a.cfg.DataDir, "attachments", messageID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	id := newID("att")
	path := filepath.Join(dir, id+"_"+filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,storage_path,created_at) VALUES(?,?,?,?,?,?,?)`, id, messageID, filename, contentType, len(data), path, a.now().UTC().Format(time.RFC3339Nano))
	return err
}

func (a *App) attachmentsForMessage(ctx context.Context, messageID string) ([]Attachment, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id,message_id,filename,content_type,size_bytes,created_at FROM attachments WHERE message_id=? ORDER BY filename`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Attachment{}
	for rows.Next() {
		var item Attachment
		var created string
		if err := rows.Scan(&item.ID, &item.MessageID, &item.Filename, &item.ContentType, &item.SizeBytes, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(created)
		items = append(items, item)
	}
	return items, nil
}

func (a *App) labelsForMailbox(ctx context.Context, mailboxID string) ([]MailLabel, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT l.id,l.mailbox_id,l.name,l.color,COUNT(ml.message_id)
		FROM mail_labels l LEFT JOIN message_labels ml ON ml.label_id=l.id
		WHERE l.mailbox_id=?
		GROUP BY l.id,l.mailbox_id,l.name,l.color
		ORDER BY lower(l.name)`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []MailLabel{}
	for rows.Next() {
		var item MailLabel
		if err := rows.Scan(&item.ID, &item.MailboxID, &item.Name, &item.Color, &item.MessageCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *App) labelsForMessage(ctx context.Context, messageID string) ([]MailLabel, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT l.id,l.mailbox_id,l.name,l.color
		FROM mail_labels l JOIN message_labels ml ON ml.label_id=l.id
		WHERE ml.message_id=?
		ORDER BY lower(l.name)`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []MailLabel{}
	for rows.Next() {
		var item MailLabel
		if err := rows.Scan(&item.ID, &item.MailboxID, &item.Name, &item.Color); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *App) attachLabelsToMessages(ctx context.Context, items []MailMessage) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	index := make(map[string]int, len(items))
	args := make([]any, 0, len(items))
	for i := range items {
		ids = append(ids, "?")
		index[items[i].ID] = i
		args = append(args, items[i].ID)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT ml.message_id,l.id,l.mailbox_id,l.name,l.color
		FROM message_labels ml JOIN mail_labels l ON l.id=ml.label_id
		WHERE ml.message_id IN (`+strings.Join(ids, ",")+`)
		ORDER BY lower(l.name)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var label MailLabel
		if err := rows.Scan(&messageID, &label.ID, &label.MailboxID, &label.Name, &label.Color); err != nil {
			return err
		}
		if itemIndex, ok := index[messageID]; ok {
			items[itemIndex].Labels = append(items[itemIndex].Labels, label)
		}
	}
	return rows.Err()
}

func (a *App) ensureLabel(ctx context.Context, mailboxID, name, color string) (MailLabel, error) {
	name = normalizeLabelName(name)
	if name == "" {
		return MailLabel{}, errors.New("label name is required")
	}
	color = normalizeLabelColor(color)
	now := a.now().UTC().Format(time.RFC3339Nano)
	var existing MailLabel
	row := a.db.QueryRowContext(ctx, `SELECT id,mailbox_id,name,color FROM mail_labels WHERE mailbox_id=? AND lower(name)=lower(?)`, mailboxID, name)
	if err := row.Scan(&existing.ID, &existing.MailboxID, &existing.Name, &existing.Color); err == nil {
		if color != "" && color != existing.Color {
			if _, err := a.db.ExecContext(ctx, `UPDATE mail_labels SET color=?, updated_at=? WHERE id=?`, color, now, existing.ID); err != nil {
				return MailLabel{}, err
			}
			existing.Color = color
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MailLabel{}, err
	}
	id := newID("lbl")
	if color == "" {
		color = "#64748b"
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO mail_labels(id,mailbox_id,name,color,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, mailboxID, name, color, now, now)
	if err != nil {
		return MailLabel{}, err
	}
	return MailLabel{ID: id, MailboxID: mailboxID, Name: name, Color: color}, nil
}

func (a *App) labelBelongsToMailbox(ctx context.Context, labelID, mailboxID string) bool {
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM mail_labels WHERE id=? AND mailbox_id=?`, labelID, mailboxID).Scan(&count)
	return count > 0
}

func normalizeLabelName(name string) string {
	name = strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
	if len([]rune(name)) > 32 {
		name = string([]rune(name)[:32])
	}
	return name
}

func normalizeLabelColor(color string) string {
	color = strings.TrimSpace(color)
	if len(color) != 7 || !strings.HasPrefix(color, "#") {
		return ""
	}
	for _, r := range color[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(color)
}

func (a *App) deleteMessageFiles(ctx context.Context, messageID string) {
	rows, err := a.db.QueryContext(ctx, `SELECT storage_path FROM attachments WHERE message_id=?`, messageID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			_ = os.Remove(p)
		}
	}
	_ = os.RemoveAll(filepath.Join(a.cfg.DataDir, "attachments", messageID))
}

type messageSummaryScanner interface{ Scan(dest ...any) error }

func scanAdminMessageSummary(row messageSummaryScanner) (MailMessage, error) {
	var msg MailMessage
	var toJSON, ccJSON, bccJSON, sent, received string
	var read, starred, hasAtt int
	err := row.Scan(&msg.ID, &msg.MailboxID, &msg.MailboxAddress, &msg.OwnerEmail, &msg.RecipientAddr, &msg.FolderID, &msg.Folder, &msg.MessageUID, &msg.MessageID, &msg.Subject, &msg.From, &msg.FromName, &toJSON, &ccJSON, &bccJSON, &sent, &received, &msg.Snippet, &read, &starred, &hasAtt, &msg.SizeBytes)
	if err != nil {
		return msg, err
	}
	msg.To, msg.CC, msg.BCC = jsonDecodeSlice(toJSON), jsonDecodeSlice(ccJSON), jsonDecodeSlice(bccJSON)
	msg.SentAt, msg.ReceivedAt = parseTime(sent), parseTime(received)
	msg.IsRead, msg.IsStarred, msg.HasAttachments = intBool(read), intBool(starred), intBool(hasAtt)
	return msg, nil
}

func scanMessageSummary(row messageSummaryScanner) (MailMessage, error) {
	var msg MailMessage
	var toJSON, ccJSON, bccJSON, sent, received string
	var read, starred, hasAtt int
	err := row.Scan(&msg.ID, &msg.MailboxID, &msg.FolderID, &msg.Folder, &msg.MessageUID, &msg.MessageID, &msg.Subject, &msg.From, &msg.FromName, &toJSON, &ccJSON, &bccJSON, &sent, &received, &msg.Snippet, &read, &starred, &hasAtt, &msg.SizeBytes)
	if err != nil {
		return msg, err
	}
	msg.To, msg.CC, msg.BCC = jsonDecodeSlice(toJSON), jsonDecodeSlice(ccJSON), jsonDecodeSlice(bccJSON)
	msg.SentAt, msg.ReceivedAt = parseTime(sent), parseTime(received)
	msg.IsRead, msg.IsStarred, msg.HasAttachments = intBool(read), intBool(starred), intBool(hasAtt)
	return msg, nil
}

func scanMessageFull(row messageSummaryScanner, includeBody bool) (MailMessage, error) {
	var msg MailMessage
	var toJSON, ccJSON, bccJSON, sent, received string
	var read, starred, hasAtt int
	var bodyText, bodyHTML string
	err := row.Scan(&msg.ID, &msg.MailboxID, &msg.RecipientAddr, &msg.FolderID, &msg.Folder, &msg.MessageUID, &msg.MessageID, &msg.Subject, &msg.From, &msg.FromName, &toJSON, &ccJSON, &bccJSON, &sent, &received, &msg.Snippet, &bodyText, &bodyHTML, &read, &starred, &hasAtt, &msg.SizeBytes)
	if err != nil {
		return msg, err
	}
	msg.To, msg.CC, msg.BCC = jsonDecodeSlice(toJSON), jsonDecodeSlice(ccJSON), jsonDecodeSlice(bccJSON)
	msg.SentAt, msg.ReceivedAt = parseTime(sent), parseTime(received)
	msg.IsRead, msg.IsStarred, msg.HasAttachments = intBool(read), intBool(starred), intBool(hasAtt)
	if includeBody {
		msg.BodyText, msg.BodyHTML = bodyText, bodyHTML
	}
	return msg, nil
}
