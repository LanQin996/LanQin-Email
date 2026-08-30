package app

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type AdminDeliveryQueueItem struct {
	ID            string `json:"id"`
	QueueType     string `json:"queueType"`
	Status        string `json:"status"`
	AttemptCount  int    `json:"attemptCount"`
	MaxAttempts   int    `json:"maxAttempts"`
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
	DeliveredAt   string `json:"deliveredAt,omitempty"`
}

var (
	adminQueueEmailPattern = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	adminQueueURLPattern   = regexp.MustCompile(`(?i)https?://\S+`)
	adminQueueTokenPattern = regexp.MustCompile(`\b[0-9]{5,}:[A-Za-z0-9_-]{20,}\b`)
)

func (a *App) handleAdminDeliveryQueue(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		} else {
			badRequest(w, errors.New("invalid limit"))
			return
		}
	}
	page := 1
	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		} else {
			badRequest(w, errors.New("invalid page"))
			return
		}
	}
	typeFilter := strings.TrimSpace(r.URL.Query().Get("queueType"))
	if typeFilter == "" {
		typeFilter = "all"
	}
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	if statusFilter == "" {
		statusFilter = "all"
	}
	validTypes := map[string]bool{"all": true, "send": true, "webhook": true, "telegram": true}
	validStatuses := map[string]bool{"all": true, "pending": true, "failed": true, "delivered": true, "canceled": true}
	if !validTypes[typeFilter] || !validStatuses[statusFilter] {
		badRequest(w, errors.New("invalid queue filter"))
		return
	}

	queries := []string{
		`SELECT id,'send' AS queue_type,
			CASE WHEN delivered_at IS NOT NULL OR status='delivered' THEN 'delivered' WHEN status='canceled' THEN 'canceled' WHEN status='failed' THEN 'failed' ELSE 'pending' END AS queue_status,
			attempt_count,max_attempts,next_attempt_at,last_error,created_at,updated_at,COALESCE(delivered_at,'') AS delivered_at
			FROM send_queue`,
		`SELECT id,'webhook' AS queue_type,
			CASE WHEN delivered_at IS NOT NULL THEN 'delivered' WHEN attempt_count>=10 THEN 'failed' ELSE 'pending' END AS queue_status,
			attempt_count,10 AS max_attempts,next_attempt_at,last_error,created_at,updated_at,COALESCE(delivered_at,'') AS delivered_at
			FROM status_webhook_outbox`,
		`SELECT id,'telegram' AS queue_type,
			CASE WHEN delivered_at IS NOT NULL THEN 'delivered' WHEN attempt_count>=10 THEN 'failed' ELSE 'pending' END AS queue_status,
			attempt_count,10 AS max_attempts,next_attempt_at,last_error,created_at,updated_at,COALESCE(delivered_at,'') AS delivered_at
			FROM telegram_notification_outbox`,
	}
	if typeFilter != "all" {
		idx := map[string]int{"send": 0, "webhook": 1, "telegram": 2}[typeFilter]
		queries = []string{queries[idx]}
	}
	union := strings.Join(queries, " UNION ALL ")
	where := []string{"1=1"}
	args := []any{}
	if typeFilter != "all" {
		where = append(where, "queue_type=?")
		args = append(args, typeFilter)
	}
	if statusFilter != "all" {
		where = append(where, "queue_status=?")
		args = append(args, statusFilter)
	}
	base := "SELECT id,queue_type,queue_status,attempt_count,max_attempts,next_attempt_at,last_error,created_at,updated_at,delivered_at FROM (" + union + ") q WHERE " + strings.Join(where, " AND ")
	var total int
	if err := a.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM ("+union+") q WHERE "+strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to count delivery queue")
		return
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := a.db.QueryContext(r.Context(), base+" ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list delivery queue")
		return
	}
	defer rows.Close()
	items := []AdminDeliveryQueueItem{}
	for rows.Next() {
		var item AdminDeliveryQueueItem
		if err := rows.Scan(&item.ID, &item.QueueType, &item.Status, &item.AttemptCount, &item.MaxAttempts, &item.NextAttemptAt, &item.LastError, &item.CreatedAt, &item.UpdatedAt, &item.DeliveredAt); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan delivery queue")
			return
		}
		item.LastError = sanitizeAdminQueueError(item.LastError)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list delivery queue")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "limit": limit, "totalCount": total})
}

func sanitizeAdminQueueError(value string) string {
	value = adminQueueTokenPattern.ReplaceAllString(value, "[token redacted]")
	value = adminQueueURLPattern.ReplaceAllString(value, "[url redacted]")
	value = adminQueueEmailPattern.ReplaceAllString(value, "[email redacted]")
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}

func (a *App) handleAdminDeliveryQueueRetry(w http.ResponseWriter, r *http.Request) {
	a.adminDeliveryQueueMutation(w, r, true)
}

func (a *App) handleAdminDeliveryQueueCancel(w http.ResponseWriter, r *http.Request) {
	a.adminDeliveryQueueMutation(w, r, false)
}

func (a *App) adminDeliveryQueueMutation(w http.ResponseWriter, r *http.Request, retry bool) {
	queueType := strings.TrimSpace(chi.URLParam(r, "queueType"))
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" || (queueType != "send" && queueType != "webhook" && queueType != "telegram") {
		badRequest(w, errors.New("invalid delivery queue item"))
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	lease := "(lease_until IS NULL OR lease_until<=?)"
	var (
		res sql.Result
		err error
	)
	switch queueType {
	case "send":
		if retry {
			res, err = a.db.ExecContext(r.Context(), `UPDATE send_queue SET status='queued',attempt_count=0,next_attempt_at=?,last_error='',updated_at=?,delivered_at=NULL,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND status='failed' AND delivered_at IS NULL AND `+lease, now, now, id, now)
		} else {
			res, err = a.db.ExecContext(r.Context(), `UPDATE send_queue SET status='canceled',last_error='',updated_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND status IN ('queued','failed') AND delivered_at IS NULL AND `+lease, now, id, now)
		}
	case "webhook", "telegram":
		table := "status_webhook_outbox"
		if queueType == "telegram" {
			table = "telegram_notification_outbox"
		}
		if retry {
			res, err = a.db.ExecContext(r.Context(), `UPDATE `+table+` SET attempt_count=0,next_attempt_at=?,last_error='',updated_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND delivered_at IS NULL AND attempt_count>=10 AND `+lease, now, now, id, now)
		} else {
			res, err = a.db.ExecContext(r.Context(), `DELETE FROM `+table+` WHERE id=? AND delivered_at IS NULL AND `+lease, id, now)
		}
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update delivery queue item")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusConflict, "delivery queue item is no longer eligible")
		return
	}
	a.log.Info("admin delivery queue mutation", "actor", currentUser(r).ID, "queue_type", queueType, "item_id", id, "action", map[bool]string{true: "retry", false: "cancel"}[retry])
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}
