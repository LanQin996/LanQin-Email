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
