package app

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func TestBackupOAuthLoggerRedactsCallbackButPreservesAuthenticatedHandlerQuery(t *testing.T) {
	// Deliberately NOT parallel: chi's documented DefaultLogger hook is global.
	// Go's sequential test scheduling and restoring it before this test returns
	// keep the existing parallel tests from observing this temporary formatter.
	var output bytes.Buffer
	previous := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: log.New(&output, "", 0), NoColor: true})
	defer func() { middleware.DefaultLogger = previous }()
	const raw = "/api/admin/backups/drive/oauth/callback?code=synthetic-auth-code&state=synthetic-single-use-state&error=synthetic-provider-error"
	var calls int
	handler := backupOAuthSafeLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("code") != "synthetic-auth-code" || r.URL.Query().Get("state") != "synthetic-single-use-state" || r.URL.Query().Get("error") != "synthetic-provider-error" || r.RequestURI != raw {
			t.Error("log sanitization destroyed OAuth callback parameters")
		}
		if r.Context().Value(contextKey("synthetic-log-test")) != "retained" || middleware.GetLogEntry(r) == nil {
			t.Error("middleware context/log entry lost")
		}
		w.WriteHeader(http.StatusSeeOther)
	}))
	req := httptest.NewRequest("GET", raw, nil)
	req = req.WithContext(context.WithValue(req.Context(), contextKey("synthetic-log-test"), "retained"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if calls != 1 || response.Code != http.StatusSeeOther {
		t.Fatal("callback not invoked")
	}
	line := output.String()
	if !strings.Contains(line, "/api/admin/backups/drive/oauth/callback") || !strings.Contains(line, "303") {
		t.Fatal("safe access record missing")
	}
	for _, secret := range []string{"synthetic-auth-code", "synthetic-single-use-state", "synthetic-provider-error", "?code=", "state="} {
		if strings.Contains(line, secret) {
			t.Fatal("OAuth query secret entered application access log")
		}
	}
	output.Reset()
	ordinary := backupOAuthSafeLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	ordinary.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/admin/backups/jobs?cursor=synthetic-cursor", nil))
	if !strings.Contains(output.String(), "?cursor=synthetic-cursor") {
		t.Fatal("unrelated access logging changed")
	}
}

func TestBackupOAuthLoggerRedactsRejectedUnauthenticatedCallback(t *testing.T) {
	a := newTestApp(t)
	// This test is also sequential; see the chi hook note above.
	var output bytes.Buffer
	previous := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: log.New(&output, "", 0), NoColor: true})
	defer func() { middleware.DefaultLogger = previous }()
	r := httptest.NewRequest("GET", "/api/admin/backups/drive/oauth/callback?code=unauth-synthetic-code&state=unauth-synthetic-state", nil)
	w := httptest.NewRecorder()
	a.Router().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("callback bypassed authentication", w.Code)
	}
	if strings.Contains(output.String(), "unauth-synthetic") || strings.Contains(w.Body.String(), "unauth-synthetic") {
		t.Fatal("rejected callback leaked code/state")
	}
}
