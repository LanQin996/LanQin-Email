package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type loginTestResponse struct {
	status     int
	retryAfter string
	body       map[string]any
}

// newTestAppBehindProxy mirrors the bundled Compose deployment, where Nginx fronts
// the API and appends the real peer to X-Forwarded-For. Without a trusted proxy
// count the forwarding header is ignored, so these tests could not vary client IP.
func newTestAppBehindProxy(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return newTestAppWithConfig(t, Config{
		Addr:              ":0",
		DBPath:            filepath.Join(dir, "lanqin.db"),
		DataDir:           filepath.Join(dir, "data"),
		CookieName:        "lanqin_test",
		SessionTTLHours:   24,
		AdminEmail:        "admin@lanqin.local",
		AdminPassword:     "ChangeMe123!",
		PublicHostname:    "mail.example.test",
		PublicBaseURL:     "http://localhost:5173",
		AllowInsecureHTTP: true,
		TrustedProxyCount: 1,
	})
}

func doLoginTestRequest(t *testing.T, serverURL, clientIP string, payload map[string]string) loginTestResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", clientIP)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return loginTestResponse{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: decoded}
}

func TestLoginRateLimitPasswordAccountAndSuccessReset(t *testing.T) {
	a := newTestAppBehindProxy(t)
	now := time.Now().UTC().Truncate(time.Second)
	a.now = func() time.Time { return now }
	server := httptest.NewServer(a.Router())
	defer server.Close()

	known := doLoginTestRequest(t, server.URL, "198.51.100.10", map[string]string{
		"email": "admin@lanqin.local", "password": "wrong-password",
	})
	unknown := doLoginTestRequest(t, server.URL, "198.51.100.11", map[string]string{
		"email": "missing@lanqin.local", "password": "wrong-password",
	})
	if known.status != http.StatusUnauthorized || unknown.status != http.StatusUnauthorized || known.body["error"] != unknown.body["error"] {
		t.Fatalf("known=%+v unknown=%+v", known, unknown)
	}

	for attempt := 2; attempt <= 4; attempt++ {
		response := doLoginTestRequest(t, server.URL, "198.51.100.10", map[string]string{
			"email": "ADMIN@lanqin.local", "password": "wrong-password",
		})
		if response.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d body=%v", attempt, response.status, response.body)
		}
	}
	response := doLoginTestRequest(t, server.URL, "198.51.100.10", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	})
	if response.status != http.StatusOK {
		t.Fatalf("successful login status=%d body=%v", response.status, response.body)
	}

	accountHash := loginRateLimitHash(loginRateLimitAccountScope, "admin@lanqin.local")
	var accountRows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM login_rate_limits WHERE scope=? AND subject_hash=?`, loginRateLimitAccountScope, accountHash).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 {
		t.Fatalf("account failures were not cleared: %d", accountRows)
	}
	ipHash := loginRateLimitHash(loginRateLimitIPScope, "198.51.100.10")
	var ipFailures int
	if err := a.db.QueryRow(`SELECT failure_count FROM login_rate_limits WHERE scope=? AND subject_hash=?`, loginRateLimitIPScope, ipHash).Scan(&ipFailures); err != nil {
		t.Fatal(err)
	}
	if ipFailures != 4 {
		t.Fatalf("IP failures=%d, want 4", ipFailures)
	}

	for attempt := 1; attempt <= loginRateLimitAccountFailures; attempt++ {
		response = doLoginTestRequest(t, server.URL, "198.51.100.12", map[string]string{
			"email": "admin@lanqin.local", "password": "wrong-again",
		})
		want := http.StatusUnauthorized
		if attempt == loginRateLimitAccountFailures {
			want = http.StatusTooManyRequests
		}
		if response.status != want {
			t.Fatalf("attempt %d status=%d, want %d body=%v", attempt, response.status, want, response.body)
		}
	}
	if response.retryAfter != "900" {
		t.Fatalf("Retry-After=%q, want 900", response.retryAfter)
	}
}

func TestLoginRateLimitIPAddressAcrossAccounts(t *testing.T) {
	a := newTestAppBehindProxy(t)
	now := time.Now().UTC().Truncate(time.Second)
	a.now = func() time.Time { return now }
	server := httptest.NewServer(a.Router())
	defer server.Close()

	var response loginTestResponse
	for attempt := 1; attempt <= loginRateLimitIPFailures; attempt++ {
		response = doLoginTestRequest(t, server.URL, "203.0.113.20", map[string]string{
			"email":    "missing" + string(rune('a'+attempt)) + "@lanqin.local",
			"password": "wrong-password",
		})
		want := http.StatusUnauthorized
		if attempt == loginRateLimitIPFailures {
			want = http.StatusTooManyRequests
		}
		if response.status != want {
			t.Fatalf("attempt %d status=%d, want %d body=%v", attempt, response.status, want, response.body)
		}
	}
	if response.retryAfter != "900" {
		t.Fatalf("Retry-After=%q, want 900", response.retryAfter)
	}

	response = doLoginTestRequest(t, server.URL, "203.0.113.20", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	})
	if response.status != http.StatusTooManyRequests {
		t.Fatalf("blocked IP bypassed with valid account: status=%d body=%v", response.status, response.body)
	}
	now = now.Add(loginRateLimitLockDuration + time.Second)
	response = doLoginTestRequest(t, server.URL, "203.0.113.20", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	})
	if response.status != http.StatusOK {
		t.Fatalf("login after lock expiry status=%d body=%v", response.status, response.body)
	}
}

func TestLoginRateLimitTOTPAndSuccessfulCleanup(t *testing.T) {
	a := newTestAppBehindProxy(t)
	a.cfg.TwoFactorEnabled = true
	now := time.Now().UTC().Truncate(time.Second)
	a.now = func() time.Time { return now }
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE users SET two_factor_secret=?, two_factor_enabled=1 WHERE email=?`, secret, "admin@lanqin.local"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Router())
	defer server.Close()

	challenge := doLoginTestRequest(t, server.URL, "192.0.2.30", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	})
	challengeToken, _ := challenge.body["challengeToken"].(string)
	if challenge.status != http.StatusOK || challengeToken == "" {
		t.Fatalf("challenge=%+v", challenge)
	}
	for attempt := 1; attempt <= 4; attempt++ {
		response := doLoginTestRequest(t, server.URL, "192.0.2.30", map[string]string{
			"challengeToken": challengeToken, "twoFactorCode": "not-a-code",
		})
		if response.status != http.StatusUnauthorized {
			t.Fatalf("TOTP attempt %d status=%d body=%v", attempt, response.status, response.body)
		}
	}
	code, err := generateTOTP(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	response := doLoginTestRequest(t, server.URL, "192.0.2.30", map[string]string{
		"challengeToken": challengeToken, "twoFactorCode": code,
	})
	if response.status != http.StatusOK {
		t.Fatalf("valid TOTP status=%d body=%v", response.status, response.body)
	}
	var accountRows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM login_rate_limits WHERE scope=? AND subject_hash=?`,
		loginRateLimitAccountScope, loginRateLimitHash(loginRateLimitAccountScope, "admin@lanqin.local")).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 {
		t.Fatalf("TOTP success did not clear account failures: %d", accountRows)
	}

	challenge = doLoginTestRequest(t, server.URL, "192.0.2.31", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	})
	challengeToken, _ = challenge.body["challengeToken"].(string)
	if challengeToken == "" {
		t.Fatalf("second challenge=%+v", challenge)
	}
	for attempt := 1; attempt <= loginRateLimitAccountFailures; attempt++ {
		response = doLoginTestRequest(t, server.URL, "192.0.2.31", map[string]string{
			"challengeToken": challengeToken, "twoFactorCode": "not-a-code",
		})
		want := http.StatusUnauthorized
		if attempt == loginRateLimitAccountFailures {
			want = http.StatusTooManyRequests
		}
		if response.status != want {
			t.Fatalf("TOTP limit attempt %d status=%d, want %d body=%v", attempt, response.status, want, response.body)
		}
	}
	if response.retryAfter != "900" {
		t.Fatalf("TOTP Retry-After=%q, want 900", response.retryAfter)
	}
}

func TestLoginAuditDoesNotLogCredentialsOrTokens(t *testing.T) {
	var logs bytes.Buffer
	dir := t.TempDir()
	a, err := New(Config{
		Addr:              ":0",
		DBPath:            filepath.Join(dir, "lanqin.db"),
		DataDir:           filepath.Join(dir, "data"),
		CookieName:        "lanqin_test",
		SessionTTLHours:   24,
		AdminEmail:        "admin@lanqin.local",
		AdminPassword:     "ChangeMe123!",
		PublicHostname:    "mail.example.test",
		PublicBaseURL:     "http://localhost:5173",
		AllowInsecureHTTP: true,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	server := httptest.NewServer(a.Router())
	defer server.Close()
	password := "NeverLogThisPassword-42"
	_ = doLoginTestRequest(t, server.URL, "198.51.100.40", map[string]string{
		"email": "private-user@example.test", "password": password,
	})

	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE users SET two_factor_secret=?, two_factor_enabled=1 WHERE email=?`, secret, "admin@lanqin.local"); err != nil {
		t.Fatal(err)
	}
	a.cfg.TwoFactorEnabled = true
	challengeToken, err := a.createLoginChallenge(context.Background(), mustAdminUserID(t, a))
	if err != nil {
		t.Fatal(err)
	}
	totpCode := "987654"
	_ = doLoginTestRequest(t, server.URL, "198.51.100.41", map[string]string{
		"challengeToken": challengeToken, "twoFactorCode": totpCode,
	})

	output := logs.String()
	for _, secretValue := range []string{password, totpCode, challengeToken, "private-user@example.test"} {
		if strings.Contains(output, secretValue) {
			t.Fatalf("audit log contains sensitive value %q: %s", secretValue, output)
		}
	}
	if !strings.Contains(output, "authentication audit") || !strings.Contains(output, "account_ref") || !strings.Contains(output, "client_ref") {
		t.Fatalf("audit log is missing safe references: %s", output)
	}
}

func mustAdminUserID(t *testing.T, a *App) string {
	t.Helper()
	var userID string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, "admin@lanqin.local").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return userID
}
