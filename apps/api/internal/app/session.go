package app

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

func (a *App) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token := randomToken()
	sessionID := newID("ses")
	expires := a.now().UTC().Add(time.Duration(a.cfg.SessionTTLHours) * time.Hour)
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,?,?)`,
		sessionID, userID, hashToken(token), expires.Format(time.RFC3339Nano), a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     a.cfg.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   !a.cfg.AllowInsecureHTTP,
	})
	return nil
}

// revokeUserSessionsTx deletes a user's sessions, optionally sparing one.
//
// keepTokenHash is the hash of the session that should survive — pass "" to end
// every session, which is what an administrator-driven reset does.
func revokeUserSessionsTx(ctx context.Context, tx *sql.Tx, userID, keepTokenHash string) error {
	if keepTokenHash == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, userID, keepTokenHash)
	return err
}

const (
	// sessionCleanupInterval is deliberately coarse: expired sessions are already
	// rejected by authenticateRequest, so removal is housekeeping rather than
	// enforcement.
	sessionCleanupInterval = time.Hour
	// sessionRetentionAfterExpiry keeps rows briefly past expiry so support can still
	// see that a session existed.
	sessionRetentionAfterExpiry = 7 * 24 * time.Hour
	// mailboxCreationEventRetention must exceed the rolling window the per-day
	// mailbox limit counts over, plus a wide margin.
	mailboxCreationEventRetention = 30 * 24 * time.Hour
	// sendQueueRetention bounds how long terminal queue rows persist. These rows hold
	// the full MIME body in mime_base64, so keeping them forever would retain the
	// text and attachments of every delivered message.
	sendQueueRetention = 30 * 24 * time.Hour
)

// sessionCleanupWorker prunes rows that no code path removes.
//
// Every other growing table in this package already has a cleanup pass
// (login_rate_limit.go, status_webhook.go, telegram_notifications.go); these three
// were simply missed.
func (a *App) sessionCleanupWorker(ctx context.Context) {
	a.log.Info("session cleanup worker started")
	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()
	for {
		a.cleanupExpiredRows(ctx)
		select {
		case <-ctx.Done():
			a.log.Info("session cleanup worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func (a *App) cleanupExpiredRows(ctx context.Context) {
	now := a.now().UTC()
	sessionCutoff := now.Add(-sessionRetentionAfterExpiry).Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?`, sessionCutoff); err != nil {
		a.log.Warn("failed to prune expired sessions", "error", err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM login_challenges WHERE expires_at<?`, now.Format(time.RFC3339Nano)); err != nil {
		a.log.Warn("failed to prune expired login challenges", "error", err)
	}
	eventCutoff := now.Add(-mailboxCreationEventRetention).Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM mailbox_creation_events WHERE created_at<?`, eventCutoff); err != nil {
		a.log.Warn("failed to prune mailbox creation events", "error", err)
	}
	// Only terminal rows: queued and sending items are still the worker's business.
	queueCutoff := now.Add(-sendQueueRetention).Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM send_queue WHERE updated_at<? AND status IN ('delivered','failed','canceled')`, queueCutoff); err != nil {
		a.log.Warn("failed to prune terminal send queue rows", "error", err)
	}
}
