package app

import (
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
