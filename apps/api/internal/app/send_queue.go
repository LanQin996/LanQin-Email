package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	sendQueueStatusQueued    = "queued"
	sendQueueStatusSending   = "sending"
	sendQueueStatusDelivered = "delivered"
	sendQueueStatusFailed    = "failed"

	sendAuditAccepted  = "accepted"
	sendAuditQueued    = "queued"
	sendAuditDelivered = "delivered"
	sendAuditFailed    = "failed"
	sendAuditRetry     = "retry"

	sendSourceWebmail    = "webmail"
	sendSourceSubmission = "submission"

	sendQueueStaleAfter = 15 * time.Minute
)

type sendQueueInput struct {
	UserID        string
	MailboxID     string
	SentMessageID string
	MessageID     string
	Source        string
	MailFrom      string
	HeaderFrom    string
	Recipients    []string
	MIMEBytes     []byte
	Now           time.Time
}

type sendQueueItem struct {
	ID            string
	UserID        string
	MailboxID     string
	SentMessageID string
	MessageID     string
	Source        string
	MailFrom      string
	HeaderFrom    string
	Recipients    []string
	MIMEBytes     []byte
	AttemptCount  int
	MaxAttempts   int
}

func (a *App) enqueueSend(ctx context.Context, in sendQueueInput) (string, error) {
	if strings.TrimSpace(a.cfg.SMTPHost) == "" {
		return "", nil
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = a.now().UTC()
	}
	id := newID("snd")
	messageID := strings.TrimSpace(in.MessageID)
	mimeBase64 := base64.StdEncoding.EncodeToString(in.MIMEBytes)
	recipientsJSON := jsonEncode(dedupeEmails(in.Recipients))
	_, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO send_queue(id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.UserID, in.MailboxID, in.SentMessageID, messageID, in.Source, normalizeEmail(in.MailFrom), normalizeEmail(in.HeaderFrom), recipientsJSON, mimeBase64, sendQueueStatusQueued, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(messageID) != "" {
		var existingID, status string
		var attemptCount, maxAttempts int
		if err := a.db.QueryRowContext(ctx, `SELECT id,status,attempt_count,max_attempts FROM send_queue WHERE mailbox_id=? AND source=? AND message_id=?`, in.MailboxID, in.Source, messageID).Scan(&existingID, &status, &attemptCount, &maxAttempts); err != nil {
			return "", err
		}
		if existingID != id {
			if status == sendQueueStatusFailed && attemptCount >= maxAttempts {
				_, err := a.db.ExecContext(ctx, `UPDATE send_queue SET user_id=?,sent_message_id=?,mail_from=?,header_from=?,recipients_json=?,mime_base64=?,status=?,attempt_count=0,next_attempt_at=?,last_error='',updated_at=?,delivered_at=NULL WHERE id=? AND status=? AND attempt_count>=max_attempts`,
					in.UserID, in.SentMessageID, normalizeEmail(in.MailFrom), normalizeEmail(in.HeaderFrom), recipientsJSON, mimeBase64, sendQueueStatusQueued, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), existingID, sendQueueStatusFailed)
				if err != nil {
					return "", err
				}
				a.recordSendAudit(ctx, sendAuditQueued, sendQueueStatusQueued, sendAuditInput{
					QueueID:       existingID,
					UserID:        in.UserID,
					MailboxID:     in.MailboxID,
					SentMessageID: in.SentMessageID,
					Source:        in.Source,
					MailFrom:      in.MailFrom,
					HeaderFrom:    in.HeaderFrom,
					Recipients:    in.Recipients,
				})
			}
			return existingID, nil
		}
	}
	a.recordSendAudit(ctx, sendAuditQueued, sendQueueStatusQueued, sendAuditInput{
		QueueID:       id,
		UserID:        in.UserID,
		MailboxID:     in.MailboxID,
		SentMessageID: in.SentMessageID,
		Source:        in.Source,
		MailFrom:      in.MailFrom,
		HeaderFrom:    in.HeaderFrom,
		Recipients:    in.Recipients,
	})
	return id, nil
}

func (a *App) sendQueueWorker(ctx context.Context) {
	a.log.Info("send queue worker started")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.processDueSendQueue(ctx); err != nil {
			a.log.Warn("send queue worker failed", "error", err)
		}
		select {
		case <-ctx.Done():
			a.log.Info("send queue worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func (a *App) processDueSendQueue(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.SMTPHost) == "" {
		return nil
	}
	if err := a.recoverStaleSendQueueItems(ctx); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM send_queue WHERE (status=? OR (status=? AND attempt_count<max_attempts)) AND next_attempt_at<=? ORDER BY next_attempt_at, created_at LIMIT 20`, sendQueueStatusQueued, sendQueueStatusFailed, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		a.processSendQueueItem(ctx, id)
	}
	return nil
}

func (a *App) recoverStaleSendQueueItems(ctx context.Context) error {
	cutoff := a.now().UTC().Add(-sendQueueStaleAfter).Format(time.RFC3339Nano)
	rows, err := a.db.QueryContext(ctx, `SELECT id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,attempt_count,max_attempts FROM send_queue WHERE status=? AND updated_at<=? AND attempt_count<max_attempts LIMIT 20`, sendQueueStatusSending, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []sendQueueItem
	for rows.Next() {
		var item sendQueueItem
		var recipientsJSON, mimeBase64 string
		if err := rows.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.SentMessageID, &item.MessageID, &item.Source, &item.MailFrom, &item.HeaderFrom, &recipientsJSON, &mimeBase64, &item.AttemptCount, &item.MaxAttempts); err != nil {
			return err
		}
		item.Recipients = jsonDecodeSlice(recipientsJSON)
		mimeBytes, err := base64.StdEncoding.DecodeString(mimeBase64)
		if err != nil {
			return err
		}
		item.MIMEBytes = mimeBytes
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, item := range items {
		res, err := a.db.ExecContext(ctx, `UPDATE send_queue SET status=?,next_attempt_at=?,last_error=?,updated_at=? WHERE id=? AND status=?`, sendQueueStatusFailed, now, "send attempt interrupted", now, item.ID, sendQueueStatusSending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			a.recordSendAudit(ctx, sendAuditRetry, sendQueueStatusFailed, sendAuditInputFromQueue(item, "send attempt interrupted"))
		}
	}
	return nil
}

func (a *App) processSendQueueItem(ctx context.Context, id string) {
	item, err := a.claimSendQueueItem(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Warn("failed to claim send queue item", "id", id, "error", err)
		}
		return
	}
	if err := a.sendSMTP(item.MailFrom, item.Recipients, item.MIMEBytes); err != nil {
		a.markSendQueueFailed(ctx, item, err)
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `UPDATE send_queue SET status=?,delivered_at=?,updated_at=?,last_error='',mime_base64='' WHERE id=?`, sendQueueStatusDelivered, now, now, item.ID); err != nil {
		a.log.Warn("failed to mark send queue delivered", "id", item.ID, "error", err)
		return
	}
	a.recordSendAudit(ctx, sendAuditDelivered, sendQueueStatusDelivered, sendAuditInputFromQueue(item, ""))
}

func (a *App) claimSendQueueItem(ctx context.Context, id string) (sendQueueItem, error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	res, err := a.db.ExecContext(ctx, `UPDATE send_queue SET status=?,attempt_count=attempt_count+1,updated_at=? WHERE id=? AND (status=? OR (status=? AND attempt_count<max_attempts)) AND next_attempt_at<=?`, sendQueueStatusSending, now, id, sendQueueStatusQueued, sendQueueStatusFailed, now)
	if err != nil {
		return sendQueueItem{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sendQueueItem{}, sql.ErrNoRows
	}
	var item sendQueueItem
	var recipientsJSON, mimeBase64 string
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,attempt_count,max_attempts FROM send_queue WHERE id=?`, id)
	if err := row.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.SentMessageID, &item.MessageID, &item.Source, &item.MailFrom, &item.HeaderFrom, &recipientsJSON, &mimeBase64, &item.AttemptCount, &item.MaxAttempts); err != nil {
		return sendQueueItem{}, err
	}
	item.Recipients = jsonDecodeSlice(recipientsJSON)
	mimeBytes, err := base64.StdEncoding.DecodeString(mimeBase64)
	if err != nil {
		return sendQueueItem{}, err
	}
	item.MIMEBytes = mimeBytes
	return item, nil
}

func (a *App) markSendQueueFailed(ctx context.Context, item sendQueueItem, sendErr error) {
	now := a.now().UTC()
	status := sendQueueStatusFailed
	nextAttempt := now.Add(sendRetryDelay(item.AttemptCount))
	if item.AttemptCount >= item.MaxAttempts {
		nextAttempt = now.Add(365 * 24 * time.Hour)
	}
	_, err := a.db.ExecContext(ctx, `UPDATE send_queue SET status=?,next_attempt_at=?,last_error=?,updated_at=? WHERE id=?`, status, nextAttempt.Format(time.RFC3339Nano), sendErr.Error(), now.Format(time.RFC3339Nano), item.ID)
	if err != nil {
		a.log.Warn("failed to mark send queue failed", "id", item.ID, "error", err)
	}
	event := sendAuditRetry
	if item.AttemptCount >= item.MaxAttempts {
		event = sendAuditFailed
	}
	a.recordSendAudit(ctx, event, status, sendAuditInputFromQueue(item, sendErr.Error()))
}

func sendRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delays := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}
	if attempt > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt-1]
}

type sendAuditInput struct {
	QueueID       string
	UserID        string
	MailboxID     string
	SentMessageID string
	Source        string
	MailFrom      string
	HeaderFrom    string
	Recipients    []string
	Error         string
}

func sendAuditInputFromQueue(item sendQueueItem, errorText string) sendAuditInput {
	return sendAuditInput{
		QueueID:       item.ID,
		UserID:        item.UserID,
		MailboxID:     item.MailboxID,
		SentMessageID: item.SentMessageID,
		Source:        item.Source,
		MailFrom:      item.MailFrom,
		HeaderFrom:    item.HeaderFrom,
		Recipients:    item.Recipients,
		Error:         errorText,
	}
}

func (a *App) recordSendAudit(ctx context.Context, event, status string, in sendAuditInput) {
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = "unknown"
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO send_audit_events(id,queue_id,user_id,mailbox_id,sent_message_id,source,event,status,mail_from,header_from,recipients_json,error,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID("audit"), in.QueueID, in.UserID, in.MailboxID, in.SentMessageID, source, event, status, normalizeEmail(in.MailFrom), normalizeEmail(in.HeaderFrom), jsonEncode(dedupeEmails(in.Recipients)), in.Error, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		a.log.Warn("failed to record send audit", "event", event, "error", err)
	}
}

func (a *App) authorizedSender(ctx context.Context, mb *Mailbox, from string) (string, string, error) {
	from = normalizeEmail(from)
	if from == "" {
		from = normalizeEmail(mb.Address)
	}
	if from == normalizeEmail(mb.Address) {
		return normalizeEmail(mb.Address), mb.DisplayName, nil
	}
	var displayName string
	var enabled int
	err := a.db.QueryRowContext(ctx, `SELECT display_name,enabled FROM send_as_grants WHERE mailbox_id=? AND address=?`, mb.ID, from).Scan(&displayName, &enabled)
	if err == nil {
		if enabled == 0 {
			return "", "", fmt.Errorf("send-as address is disabled")
		}
		return from, strings.TrimSpace(displayName), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	var aliasDestination string
	err = a.db.QueryRowContext(ctx, `SELECT destination FROM aliases WHERE source=? AND enabled=1`, from).Scan(&aliasDestination)
	if err == nil && normalizeEmail(aliasDestination) == normalizeEmail(mb.Address) {
		return from, mb.DisplayName, nil
	}
	return "", "", fmt.Errorf("send-as address is not authorized")
}
