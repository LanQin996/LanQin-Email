package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPasswordChangeRevokesOtherSessions covers the report's H4: changing a
// password used to leave every other session usable, which defeats the one recovery
// action a user knows to take after a suspected compromise.
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()

	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "victim@example.net", "displayName": "Victim", "role": "user",
		"password": "Password123!", "disabled": false,
	}, &created); code != http.StatusCreated {
		t.Fatalf("create user code=%d", code)
	}

	// Two independent logins stand in for "the user" and "the attacker".
	victim := &testClient{t: t, server: ts}
	stolen := &testClient{t: t, server: ts}
	for _, c := range []*testClient{victim, stolen} {
		if code := c.do("POST", "/api/auth/login", map[string]string{"email": "victim@example.net", "password": "Password123!"}, &login); code != http.StatusOK {
			t.Fatalf("victim login code=%d", code)
		}
	}
	var me map[string]any
	if code := stolen.do("GET", "/api/me", nil, &me); code != http.StatusOK {
		t.Fatalf("stolen session should start out valid, code=%d", code)
	}

	if code := victim.do("POST", "/api/me/password", map[string]string{
		"currentPassword": "Password123!", "newPassword": "BrandNew123!",
	}, &me); code != http.StatusOK {
		t.Fatalf("change password code=%d", code)
	}

	if code := stolen.do("GET", "/api/me", nil, &me); code != http.StatusUnauthorized {
		t.Errorf("other session after password change code=%d, want 401", code)
	}
	// The session that performed the change stays signed in.
	if code := victim.do("GET", "/api/me", nil, &me); code != http.StatusOK {
		t.Errorf("own session after password change code=%d, want 200", code)
	}
}

// TestAdminPasswordResetRevokesAllSessions is the administrator-driven counterpart:
// there is no session to preserve, so every one of the target's sessions must end.
func TestAdminPasswordResetRevokesAllSessions(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()

	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "target@example.net", "displayName": "Target", "role": "user",
		"password": "Password123!", "disabled": false,
	}, &created); code != http.StatusCreated {
		t.Fatalf("create user code=%d", code)
	}
	target := &testClient{t: t, server: ts}
	if code := target.do("POST", "/api/auth/login", map[string]string{"email": "target@example.net", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("target login code=%d", code)
	}

	var ok map[string]any
	if code := admin.do("POST", "/api/admin/users/"+created.ID+"/password", map[string]string{"password": "AdminSet123!"}, &ok); code != http.StatusOK {
		t.Fatalf("reset password code=%d", code)
	}
	var me map[string]any
	if code := target.do("GET", "/api/me", nil, &me); code != http.StatusUnauthorized {
		t.Errorf("target session after admin reset code=%d, want 401", code)
	}

	var remaining int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, created.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("sessions remaining=%d, want 0", remaining)
	}
}

// TestClientIPIgnoresSpoofedHeadersWithoutTrustedProxies covers the report's H2:
// middleware.RealIP trusted True-Client-IP, X-Real-IP and X-Forwarded-For from any
// caller, so the rate-limit and audit key was client-controlled. With no trusted
// proxies configured, headers must be ignored entirely.
func TestClientIPIgnoresSpoofedHeadersWithoutTrustedProxies(t *testing.T) {
	a := newTestApp(t)
	if a.cfg.TrustedProxyCount != 0 {
		t.Fatalf("TrustedProxyCount = %d, want 0 by default", a.cfg.TrustedProxyCount)
	}

	var seen string
	handler := a.clientIPMiddleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	for _, header := range []string{"True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		req.Header.Set(header, "9.9.9.9")
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if seen != "203.0.113.9" {
			t.Errorf("%s spoofing changed the client IP to %q, want 203.0.113.9", header, seen)
		}
	}
}

// TestClientIPUsesTrustedProxyHop checks the configured-proxy path: with one trusted
// hop the value nginx appends wins, and anything the client put to its left is
// ignored.
func TestClientIPUsesTrustedProxyHop(t *testing.T) {
	a := newTestApp(t)
	a.cfg.TrustedProxyCount = 1

	var seen string
	handler := a.clientIPMiddleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	// What nginx produces from `$proxy_add_x_forwarded_for` when the client sent a
	// forged entry of its own: the real peer is appended last.
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.9")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.9" {
		t.Errorf("client IP = %q, want 203.0.113.9 (the hop nginx appended)", seen)
	}
}

// TestRouterIgnoresSpoofedIPForRateLimit proves the wiring, not just the helper:
// the previous test exercises clientIPMiddleware directly, so it would still pass if
// the router went back to middleware.RealIP. Here the requests go through the real
// router, and a spoofed per-request IP must not hand out fresh rate-limit buckets.
//
// Distinct account names keep the 5-failure account dimension from tripping first,
// leaving the 20-failure IP dimension as the only thing that can reject.
func TestRouterIgnoresSpoofedIPForRateLimit(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()

	limited := false
	for i := 0; i < loginRateLimitIPFailures+1; i++ {
		client := &testClient{t: t, server: ts}
		var body map[string]any
		code := client.doWithHeaders("POST", "/api/auth/login", map[string]string{
			"email":    fmt.Sprintf("nobody%d@example.net", i),
			"password": "WrongPassword123!",
		}, map[string]string{
			// A fresh forged address on every attempt: with middleware.RealIP each of
			// these became its own bucket and the limit never applied.
			"True-Client-IP":  fmt.Sprintf("9.9.9.%d", i+1),
			"X-Real-IP":       fmt.Sprintf("9.9.8.%d", i+1),
			"X-Forwarded-For": fmt.Sprintf("9.9.7.%d", i+1),
		}, &body)
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code=%d, want 401 or 429", i, code)
		}
	}
	if !limited {
		t.Error("rate limiting never triggered: per-request IP headers are still trusted")
	}
}
