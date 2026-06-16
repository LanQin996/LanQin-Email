package app

import (
	"context"
	"database/sql"
	"encoding/base64"
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
	limit := 30

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
	if r.URL.Query().Get("markRead") != "0" && !msg.IsRead {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE messages SET is_read=1, updated_at=? WHERE id=?`, a.now().UTC().Format(time.RFC3339Nano), msg.ID)
		msg.IsRead = true
	}
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleMailSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MailboxID   string            `json:"mailboxId"`
		To          []string          `json:"to"`
		CC          []string          `json:"cc"`
		BCC         []string          `json:"bcc"`
		Subject     string            `json:"subject"`
		Text        string            `json:"text"`
		HTML        string            `json:"html"`
		Attachments []AttachmentInput `json:"attachments"`
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
	req.To, req.CC, req.BCC = dedupeEmails(req.To), dedupeEmails(req.CC), dedupeEmails(req.BCC)
	allRecipients := append(append([]string{}, req.To...), append(req.CC, req.BCC...)...)
	if len(allRecipients) == 0 {
		badRequest(w, errors.New("at least one recipient is required"))
		return
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
		badRequest(w, err)
		return
	}
	if a.cfg.SMTPHost != "" {
		if err := a.sendSMTP(mb.Address, allRecipients, mimeBytes); err != nil {
			respondError(w, http.StatusBadGateway, "smtp delivery failed: "+err.Error())
			return
		}
	}

	sentFolderID, err := a.ensureFolder(r.Context(), mb.ID, "Sent")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load sent folder")
		return
	}
	base := storedMessage{MailboxID: mb.ID, FolderID: sentFolderID, MessageUID: newID("uid"), MessageID: messageID, Subject: req.Subject, From: mb.Address, FromName: mb.DisplayName, To: req.To, CC: req.CC, BCC: req.BCC, SentAt: now, ReceivedAt: now, Snippet: snippetFrom(req.Text, req.HTML), BodyText: req.Text, BodyHTML: req.HTML, IsRead: true}
	sentID, err := a.insertMessage(r.Context(), base, req.Attachments)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to store sent message")
		return
	}

	// Development/local-domain delivery: known local recipients go to their Inbox.
	// When catch-all is enabled, unknown local recipients are stored as unregistered
	// messages visible only in the admin "全部邮件" view.
	localRecipients := append(req.To, req.CC...)
	localRecipients = append(localRecipients, req.BCC...)
	for _, rcpt := range localRecipients {
		rcptMailbox, err := a.mailboxByAddress(r.Context(), rcpt)
		if err != nil {
			if !a.cfg.CatchAllEnabled || !a.isLocalDomainAddress(r.Context(), rcpt) {
				continue
			}
			copyMsg := base
			copyMsg.MailboxID = ""
			copyMsg.FolderID = ""
			copyMsg.RecipientAddr = normalizeEmail(rcpt)
			copyMsg.MessageUID = newID("uid")
			copyMsg.IsRead = false
			_, _ = a.insertMessage(r.Context(), copyMsg, req.Attachments)
			continue
		}
		if rcptMailbox.Status != "active" {
			if a.cfg.CatchAllEnabled && a.isLocalDomainAddress(r.Context(), rcpt) {
				copyMsg := base
				copyMsg.MailboxID = ""
				copyMsg.FolderID = ""
				copyMsg.RecipientAddr = normalizeEmail(rcpt)
				copyMsg.MessageUID = newID("uid")
				copyMsg.IsRead = false
				_, _ = a.insertMessage(r.Context(), copyMsg, req.Attachments)
			}
			continue
		}
		inboxID, err := a.ensureFolder(r.Context(), rcptMailbox.ID, "Inbox")
		if err != nil {
			continue
		}
		copyMsg := base
		copyMsg.MailboxID = rcptMailbox.ID
		copyMsg.FolderID = inboxID
		copyMsg.MessageUID = newID("uid")
		copyMsg.IsRead = false
		if inboxMsgID, err := a.insertMessage(r.Context(), copyMsg, req.Attachments); err == nil {
			a.applyInboundControls(r.Context(), inboxMsgID, rcptMailbox.ID, copyMsg.From, copyMsg.Subject)
		}
	}

	msg, _ := a.messageByID(r.Context(), sentID, true)
	respondJSON(w, http.StatusCreated, msg)
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
