package app

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// Give the logger a separate sanitized request, then restore the real query
// only for the authenticated handler. Codes/state never enter app access logs.
// A reverse proxy's logging policy must be configured separately by operators.
func backupOAuthSafeLogger(next http.Handler) http.Handler {
	ordinary := middleware.Logger(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/backups/drive/oauth/callback" {
			ordinary.ServeHTTP(w, r)
			return
		}
		safe := r.Clone(r.Context())
		u := *r.URL
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		safe.URL = &u
		safe.RequestURI = u.RequestURI()
		middleware.Logger(http.HandlerFunc(func(w http.ResponseWriter, logged *http.Request) {
			next.ServeHTTP(w, r.WithContext(logged.Context()))
		})).ServeHTTP(w, safe)
	})
}
