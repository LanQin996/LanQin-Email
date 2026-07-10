package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const deliveryWebhookMaxAge = 5 * time.Minute

type deliveryWebhookEvent struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	QueueID      string `json:"queueId"`
	MessageID    string `json:"messageId"`
	RFCMessageID string `json:"rfcMessageId"`
	Recipient    string `json:"recipient"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	OccurredAt   string `json:"occurredAt"`
}

func (a *App) handleOpenAPIDeliveryWebhook(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSpace(a.cfg.DeliveryWebhookSecret)
	if secret == "" {
		respondError(w, http.StatusServiceUnavailable, "delivery webhook is not configured")
		return
	}
	timestamp := strings.TrimSpace(r.Header.Get("X-LanQin-Timestamp"))
	signature := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("X-LanQin-Signature")), "sha256=")
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || signature == "" {
		respondError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	signedAt := time.Unix(unix, 0)
	if delta := a.now().UTC().Sub(signedAt); delta < -deliveryWebhookMaxAge || delta > deliveryWebhookMaxAge {
		respondError(w, http.StatusUnauthorized, "webhook timestamp is outside the allowed window")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		badRequest(w, errors.New("invalid webhook body"))
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	expected, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(mac.Sum(nil), expected) {
		respondError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	var payload struct {
		Events []deliveryWebhookEvent `json:"events"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil || len(payload.Events) == 0 || len(payload.Events) > 100 {
		badRequest(w, errors.New("events must contain between 1 and 100 items"))
		return
	}
	accepted := 0
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start delivery event transaction")
		return
	}
	defer tx.Rollback()
	for _, event := range payload.Events {
		inserted, err := a.storeDeliveryEvent(r, tx, event)
		if err != nil {
			badRequest(w, err)
			return
		}
		if inserted {
			accepted++
		}
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to store delivery events")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": accepted, "duplicates": len(payload.Events) - accepted})
}

func (a *App) storeDeliveryEvent(r *http.Request, tx *sql.Tx, event deliveryWebhookEvent) (bool, error) {
	event.ID = strings.TrimSpace(event.ID)
	event.Provider = strings.ToLower(strings.TrimSpace(event.Provider))
	event.QueueID = strings.TrimSpace(event.QueueID)
	event.MessageID = strings.TrimSpace(event.MessageID)
	event.RFCMessageID = strings.TrimSpace(event.RFCMessageID)
	event.Recipient = normalizeEmail(event.Recipient)
	event.Status = strings.ToLower(strings.TrimSpace(event.Status))
	if event.ID == "" || len(event.ID) > 200 || event.Provider == "" || len(event.Provider) > 80 || event.Recipient == "" || len(event.Recipient) > 320 || len(event.Reason) > 2000 || !validDeliveryEventStatus(event.Status) {
		return false, errors.New("invalid delivery event")
	}
	if event.QueueID == "" && event.MessageID == "" && event.RFCMessageID == "" {
		return false, errors.New("queueId, messageId, or rfcMessageId is required")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
	if err != nil {
		return false, errors.New("occurredAt must be an RFC3339 timestamp")
	}
	var queueID, sentMessageID, rfcMessageID string
	err = tx.QueryRowContext(r.Context(), `SELECT id,sent_message_id,message_id FROM send_queue
		WHERE (?<>'' AND id=?) OR (?<>'' AND sent_message_id=?) OR (?<>'' AND message_id=?)
		ORDER BY created_at DESC LIMIT 1`, event.QueueID, event.QueueID, event.MessageID, event.MessageID, event.RFCMessageID, event.RFCMessageID).Scan(&queueID, &sentMessageID, &rfcMessageID)
	if err != nil {
		return false, errors.New("send item not found")
	}
	if (event.QueueID != "" && event.QueueID != queueID) || (event.MessageID != "" && event.MessageID != sentMessageID) || (event.RFCMessageID != "" && event.RFCMessageID != rfcMessageID) {
		return false, errors.New("delivery event identifiers do not refer to the same send item")
	}
	var recipientsJSON string
	if err := tx.QueryRowContext(r.Context(), `SELECT recipients_json FROM send_queue WHERE id=?`, queueID).Scan(&recipientsJSON); err != nil {
		return false, errors.New("send item not found")
	}
	foundRecipient := false
	for _, recipient := range jsonDecodeSlice(recipientsJSON) {
		if normalizeEmail(recipient) == event.Recipient {
			foundRecipient = true
			break
		}
	}
	if !foundRecipient {
		return false, errors.New("delivery event recipient does not belong to the send item")
	}
	id := newID("dev")
	createdAt := a.now().UTC()
	reason := strings.TrimSpace(event.Reason)
	res, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO delivery_events(id,external_id,provider,queue_id,sent_message_id,rfc_message_id,recipient,status,reason,occurred_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, event.ID, event.Provider, queueID, sentMessageID, rfcMessageID, event.Recipient, event.Status, reason, occurredAt.UTC().Format(time.RFC3339Nano), createdAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		item := DeliveryEvent{ID: id, ExternalID: event.ID, Provider: event.Provider, QueueID: queueID, MessageID: sentMessageID, RFCMessageID: rfcMessageID, Recipient: event.Recipient, Status: event.Status, Reason: reason, OccurredAt: occurredAt.UTC(), CreatedAt: createdAt}
		var mailboxID string
		if err := tx.QueryRowContext(r.Context(), `SELECT mailbox_id FROM send_queue WHERE id=?`, queueID).Scan(&mailboxID); err != nil {
			return false, err
		}
		if err := a.enqueueStatusWebhook(r.Context(), tx, "delivery:"+event.Provider+":"+event.ID, "delivery."+event.Status, mailboxID, item); err != nil {
			return false, err
		}
	}
	return n > 0, nil
}

func validDeliveryEventStatus(status string) bool {
	switch status {
	case "delivered", "bounced", "complained", "rejected", "deferred":
		return true
	default:
		return false
	}
}

func (a *App) handleOpenAPIListSends(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	limit := parseOpenAPILimit(r, 30, 100)
	where := "mb.user_id=?"
	args := []any{user.ID}
	if mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId")); mailboxID != "" {
		where += " AND sq.mailbox_id=?"
		args = append(args, mailboxID)
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		if !validSendQueueStatus(status) {
			badRequest(w, errors.New("invalid send queue status"))
			return
		}
		where += " AND sq.status=?"
		args = append(args, status)
	}
	cursorCreatedAt, cursorID, _, err := parseSendQueueCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		badRequest(w, err)
		return
	}
	if cursorCreatedAt != "" {
		where += " AND (sq.created_at<? OR (sq.created_at=? AND sq.id<?))"
		args = append(args, cursorCreatedAt, cursorCreatedAt, cursorID)
	}
	args = append(args, limit+1)
	rows, err := a.db.QueryContext(r.Context(), `SELECT sq.id,sq.mailbox_id,sq.sent_message_id,sq.message_id,COALESCE(m.subject,''),sq.source,sq.mail_from,sq.header_from,sq.recipients_json,sq.status,sq.attempt_count,sq.max_attempts,sq.next_attempt_at,sq.last_error,sq.created_at,sq.updated_at,sq.delivered_at
		FROM send_queue sq JOIN mailboxes mb ON mb.id=sq.mailbox_id LEFT JOIN messages m ON m.id=sq.sent_message_id
		WHERE `+where+` ORDER BY sq.created_at DESC,sq.id DESC LIMIT ?`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list sends")
		return
	}
	defer rows.Close()
	items := []openAPISendStatus{}
	for rows.Next() {
		item, err := scanSendQueueEntry(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan sends")
			return
		}
		status := openAPISendStatusFromQueue(item, item.MailFrom)
		a.applyDeliveryStatus(r.Context(), &status)
		items = append(items, status)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list sends")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeSendQueueCursor(last.CreatedAt, last.QueueID)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleOpenAPISendEvents(w http.ResponseWriter, r *http.Request) {
	item, err := a.resolveOpenAPISendQueue(r, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "send item not found")
		return
	}
	audit, err := a.sendAuditEvents(r, item.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load send events")
		return
	}
	delivery, err := a.deliveryEvents(r, item.SentMessageID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load delivery events")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"auditEvents": audit, "deliveryEvents": delivery})
}

func (a *App) handleOpenAPIRetrySend(w http.ResponseWriter, r *http.Request) {
	item, err := a.resolveOpenAPISendQueue(r, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "send item not found")
		return
	}
	if item.Status != sendQueueStatusFailed {
		badRequest(w, errors.New("send item is not failed"))
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	res, err := a.db.ExecContext(r.Context(), `UPDATE send_queue SET status=?,attempt_count=0,next_attempt_at=?,last_error='',updated_at=?,delivered_at=NULL WHERE id=? AND status=?`, sendQueueStatusQueued, now, now, item.ID, sendQueueStatusFailed)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to retry send item")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusConflict, "send item status changed")
		return
	}
	a.recordSendAudit(r.Context(), sendAuditRetry, sendQueueStatusQueued, sendAuditInputFromEntry(item, currentUser(r).ID, ""))
	updated, _ := a.loadSendQueueEntryForUser(r.Context(), item.ID, currentUser(r).ID)
	respondJSON(w, http.StatusOK, openAPISendStatusFromQueue(updated, updated.MailFrom))
}

func (a *App) handleOpenAPICancelSend(w http.ResponseWriter, r *http.Request) {
	item, err := a.resolveOpenAPISendQueue(r, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "send item not found")
		return
	}
	if item.Status != sendQueueStatusQueued && item.Status != sendQueueStatusFailed {
		badRequest(w, errors.New("send item cannot be canceled"))
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	res, err := a.db.ExecContext(r.Context(), `UPDATE send_queue SET status=?,last_error='',updated_at=? WHERE id=? AND status IN (?,?)`, sendQueueStatusCanceled, now, item.ID, sendQueueStatusQueued, sendQueueStatusFailed)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to cancel send item")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusConflict, "send item status changed")
		return
	}
	a.recordSendAudit(r.Context(), sendAuditCanceled, sendQueueStatusCanceled, sendAuditInputFromEntry(item, currentUser(r).ID, ""))
	updated, _ := a.loadSendQueueEntryForUser(r.Context(), item.ID, currentUser(r).ID)
	respondJSON(w, http.StatusOK, openAPISendStatusFromQueue(updated, updated.MailFrom))
}

func sendAuditInputFromEntry(item SendQueueEntry, userID, errorText string) sendAuditInput {
	return sendAuditInput{QueueID: item.ID, UserID: userID, MailboxID: item.MailboxID, SentMessageID: item.SentMessageID, Source: item.Source, MailFrom: item.MailFrom, HeaderFrom: item.HeaderFrom, Recipients: item.Recipients, Error: errorText}
}

func (a *App) resolveOpenAPISendQueue(r *http.Request, id string) (SendQueueEntry, error) {
	user := currentUser(r)
	if item, err := a.loadSendQueueEntryForUser(r.Context(), strings.TrimSpace(id), user.ID); err == nil {
		return item, nil
	}
	return a.loadLatestSendQueueForMessage(r.Context(), strings.TrimSpace(id), user.ID)
}

func (a *App) handleOpenAPIMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleOpenAPIListAliases(w http.ResponseWriter, r *http.Request) {
	limit := parseOpenAPILimit(r, 50, 100)
	sortValue, cursorID, err := parseOpenAPIListCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		badRequest(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,domain_id,source,destination,enabled,created_at FROM aliases
		WHERE (?='' OR source>? OR (source=? AND id>?)) ORDER BY source,id LIMIT ?`, sortValue, sortValue, sortValue, cursorID, limit+1)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list aliases")
		return
	}
	defer rows.Close()
	items := []Alias{}
	for rows.Next() {
		item, err := scanOpenAPIAlias(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan aliases")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list aliases")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeOpenAPIListCursor(last.Source, last.ID)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleOpenAPIGetAlias(w http.ResponseWriter, r *http.Request) {
	item, err := scanOpenAPIAlias(a.db.QueryRowContext(r.Context(), `SELECT id,domain_id,source,destination,enabled,created_at FROM aliases WHERE id=?`, chi.URLParam(r, "id")))
	if err != nil {
		respondError(w, http.StatusNotFound, "alias not found")
		return
	}
	respondJSON(w, http.StatusOK, item)
}

type aliasScanner interface{ Scan(...any) error }

func scanOpenAPIAlias(row aliasScanner) (Alias, error) {
	var item Alias
	var enabled int
	var created string
	err := row.Scan(&item.ID, &item.DomainID, &item.Source, &item.Destination, &enabled, &created)
	item.Enabled = intBool(enabled)
	item.CreatedAt = parseTime(created)
	return item, err
}

func (a *App) sendAuditEvents(r *http.Request, queueID string) ([]SendAuditEvent, error) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,queue_id,mailbox_id,sent_message_id,source,event,status,mail_from,header_from,recipients_json,error,created_at FROM send_audit_events WHERE queue_id=? ORDER BY created_at,id`, queueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SendAuditEvent{}
	for rows.Next() {
		var item SendAuditEvent
		var recipientsJSON, createdAt string
		if err := rows.Scan(&item.ID, &item.QueueID, &item.MailboxID, &item.SentMessageID, &item.Source, &item.Event, &item.Status, &item.MailFrom, &item.HeaderFrom, &recipientsJSON, &item.Error, &createdAt); err != nil {
			return nil, err
		}
		item.Recipients = jsonDecodeSlice(recipientsJSON)
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *App) deliveryEvents(r *http.Request, sentMessageID string) ([]DeliveryEvent, error) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,external_id,provider,queue_id,sent_message_id,rfc_message_id,recipient,status,reason,occurred_at,created_at FROM delivery_events WHERE sent_message_id=? ORDER BY occurred_at,id`, sentMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []DeliveryEvent{}
	for rows.Next() {
		var item DeliveryEvent
		var occurredAt, createdAt string
		if err := rows.Scan(&item.ID, &item.ExternalID, &item.Provider, &item.QueueID, &item.MessageID, &item.RFCMessageID, &item.Recipient, &item.Status, &item.Reason, &occurredAt, &createdAt); err != nil {
			return nil, err
		}
		item.OccurredAt = parseTime(occurredAt)
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
}
