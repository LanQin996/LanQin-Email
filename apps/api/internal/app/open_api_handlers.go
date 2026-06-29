package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

func (a *App) handleOpenAPIListDomains(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,status,dkim_selector,dkim_public_key,dns_status,dns_checked_at,created_at FROM domains ORDER BY name`)
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
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
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
	rows, err := a.db.QueryContext(r.Context(), `SELECT mb.id,mb.user_id,u.email,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at
		FROM mailboxes mb JOIN users u ON u.id=mb.user_id ORDER BY mb.address`)
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
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
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
	userID, err := a.resolveMailboxOwner(r, req.UserID, req.OwnerEmail, address, displayName, req.Password)
	if err != nil {
		respondMailboxOwnerError(w, err)
		return
	}
	mailboxID, err := a.createMailbox(r.Context(), userID, req.DomainID, localPart, displayName, req.Password, req.QuotaMB, "active")
	if err != nil {
		badRequest(w, err)
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
	msg, err := a.sendMailWithSource(r.Context(), currentUser(r), mb, req, sendSourceOpenAPI)
	if err != nil {
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
		respondJSON(w, http.StatusOK, openAPISendStatusFromQueue(item, mailboxAddress))
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
	offset := parseOpenAPIOffset(r)
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
	args = append(args, limit+1, offset)
	rows, err := a.db.QueryContext(r.Context(), `SELECT m.id,m.mailbox_id,m.folder_id,f.name,m.message_uid,m.imap_uid,m.imap_modseq,m.message_id,m.subject,m.from_addr,COALESCE(m.from_name,''),m.to_addrs,m.cc_addrs,m.bcc_addrs,m.sent_at,m.received_at,m.snippet,m.is_read,m.is_starred,m.has_attachments,m.size_bytes
		FROM messages m JOIN folders f ON f.id=m.folder_id
		WHERE `+where+`
		ORDER BY m.received_at DESC LIMIT ? OFFSET ?`, args...)
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
		nextCursor = strconv.Itoa(offset + limit)
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
	ID             string     `json:"id"`
	QueueID        string     `json:"queueId,omitempty"`
	Status         string     `json:"status"`
	MessageID      string     `json:"messageId"`
	RFCMessageID   string     `json:"rfcMessageId"`
	MailboxID      string     `json:"mailboxId"`
	MailboxAddress string     `json:"mailboxAddress,omitempty"`
	Subject        string     `json:"subject,omitempty"`
	Recipients     []string   `json:"recipients,omitempty"`
	AttemptCount   int        `json:"attemptCount,omitempty"`
	MaxAttempts    int        `json:"maxAttempts,omitempty"`
	NextAttemptAt  *time.Time `json:"nextAttemptAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
}

func openAPISendStatusFromQueue(item SendQueueEntry, mailboxAddress string) openAPISendStatus {
	return openAPISendStatus{
		ID:             item.ID,
		QueueID:        item.ID,
		Status:         item.Status,
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

func (a *App) resolveMailboxOwner(r *http.Request, userID, ownerEmail, address, displayName, password string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID != "" {
		if err := a.ensureActiveUserExists(r.Context(), userID); err != nil {
			return "", err
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
	err := a.db.QueryRowContext(r.Context(), `SELECT id FROM users WHERE email=? AND disabled=0`, email).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return a.createMailboxOwnerUser(r, email, displayName, password)
}

func (a *App) createMailboxOwnerUser(r *http.Request, email, displayName, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	userID := newID("usr")
	now := a.now().UTC().Format(time.RFC3339Nano)
	if displayName == "" {
		displayName = email
	}
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, email, displayName, "user", string(hash), 0, now, now)
	if err != nil {
		return "", err
	}
	return userID, nil
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
