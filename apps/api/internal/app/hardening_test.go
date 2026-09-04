package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTOTPCannotBeReplayed covers the report's L1: a code stayed valid for the whole
// acceptance window, so the same six digits could be presented more than once.
func TestTOTPCannotBeReplayed(t *testing.T) {
	a := newTestApp(t)
	clock := time.Now().UTC().Truncate(time.Second)
	a.now = func() time.Time { return clock }

	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, "admin@lanqin.local").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	code, err := generateTOTP(secret, clock)
	if err != nil {
		t.Fatal(err)
	}

	if !a.consumeTOTP(t.Context(), userID, secret, code) {
		t.Fatal("first use of a valid code was rejected")
	}
	if a.consumeTOTP(t.Context(), userID, secret, code) {
		t.Error("the same code was accepted twice")
	}
	// A later step is still accepted, so the guard does not lock the account out.
	clock = clock.Add(31 * time.Second)
	next, err := generateTOTP(secret, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !a.consumeTOTP(t.Context(), userID, secret, next) {
		t.Error("a code from a later step was rejected")
	}
}

func TestValidatePasswordLengthBounds(t *testing.T) {
	if err := validatePasswordLength("short"); err == nil {
		t.Error("a 5-character password was accepted")
	}
	if err := validatePasswordLength("Password123!"); err != nil {
		t.Errorf("a reasonable password was rejected: %v", err)
	}
	// bcrypt returns ErrPasswordTooLong past 72 bytes; that used to surface as a 500.
	if err := validatePasswordLength(strings.Repeat("a", maxPasswordBytes+1)); err == nil {
		t.Error("a password longer than bcrypt's limit was accepted")
	}
	if err := validatePasswordLength(strings.Repeat("a", maxPasswordBytes)); err != nil {
		t.Errorf("a password exactly at the limit was rejected: %v", err)
	}
}

// TestRequireCleanLocalPartRejectsRewrites covers L5: normalizeLocalPart used to strip
// disallowed characters silently, creating an address the user never asked for.
func TestRequireCleanLocalPartRejectsRewrites(t *testing.T) {
	for _, item := range []string{"a/b", "a b", "a@b", "user!", ".leading", "trailing.", strings.Repeat("x", 65), "", "   "} {
		if got, err := requireCleanLocalPart(item); err == nil {
			t.Errorf("requireCleanLocalPart(%q) = %q, want an error", item, got)
		}
	}
	for _, item := range []string{"alice", "Alice", "a.b_c+d-e", "user123"} {
		got, err := requireCleanLocalPart(item)
		if err != nil {
			t.Errorf("requireCleanLocalPart(%q) failed: %v", item, err)
			continue
		}
		if got != strings.ToLower(strings.TrimSpace(item)) {
			t.Errorf("requireCleanLocalPart(%q) = %q, want the lowercased input", item, got)
		}
	}
}

// TestRecipientCapRejectsOversizedSend covers M6: one message could carry unlimited
// recipients while consuming a single unit of the send quota.
func TestRecipientCapRejectsOversizedSend(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	var boxes struct {
		Items []Mailbox `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/mailboxes", nil, &boxes); code != http.StatusOK || len(boxes.Items) == 0 {
		t.Fatalf("mailboxes code=%d items=%d", code, len(boxes.Items))
	}

	recipients := make([]string, maxRecipientsPerMessage+1)
	for i := range recipients {
		recipients[i] = "user" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com"
	}
	var body map[string]any
	code := admin.do("POST", "/api/mail/send", map[string]any{
		"mailboxId": boxes.Items[0].ID, "to": recipients, "cc": []string{}, "bcc": []string{},
		"subject": "probe", "text": "body", "html": "", "attachments": []any{},
	}, &body)
	if code != http.StatusBadRequest {
		t.Fatalf("oversized recipient list code=%d, want 400 body=%v", code, body)
	}
}

// TestSessionCleanupRemovesExpiredRows covers M8: sessions, login challenges, mailbox
// creation events and terminal queue rows had no cleanup at all.
func TestSessionCleanupRemovesExpiredRows(t *testing.T) {
	a := newTestApp(t)
	now := a.now().UTC()
	var userID string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, "admin@lanqin.local").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	long := now.Add(-sessionRetentionAfterExpiry - time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,?,?)`,
		"ses_old", userID, "hash_old", long, long); err != nil {
		t.Fatal(err)
	}
	// A session that expired only moments ago is kept for support purposes.
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,?,?)`,
		"ses_recent", userID, "hash_recent", recent, recent); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-mailboxCreationEventRetention - time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO mailbox_creation_events(id,user_id,mailbox_id,created_at) VALUES(?,?,?,?)`,
		"mce_old", userID, "mbx_gone", stale); err != nil {
		t.Fatal(err)
	}

	a.cleanupExpiredRows(t.Context())

	var kept int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=?`, "ses_old").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Error("long-expired session was not pruned")
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=?`, "ses_recent").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Error("recently expired session was pruned too eagerly")
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM mailbox_creation_events WHERE id=?`, "mce_old").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Error("stale mailbox creation event was not pruned")
	}
}

// TestInactiveDomainRejectedByCreationFunnel covers L6: only some entry points checked
// the domain was active, so a client-supplied domainId could target a disabled domain.
func TestInactiveDomainRejectedByCreationFunnel(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	domain := createTestDomain(t, admin, "disabled.test")
	var updated Domain
	if code := admin.do("POST", "/api/admin/domains/"+domain.ID, map[string]string{"status": "disabled"}, &updated); code != http.StatusOK {
		t.Fatalf("disable domain code=%d", code)
	}

	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "user@example.net", "displayName": "User", "role": "user",
		"password": "Password123!", "disabled": false,
	}, &created); code != http.StatusCreated {
		t.Fatalf("create user code=%d", code)
	}
	if _, err := a.createMailboxWithPasswordHash(t.Context(), created.ID, domain.ID, "someone", "", "hash", 1024, "active", nil); err == nil {
		t.Fatal("creating a mailbox on a disabled domain succeeded")
	}
}
