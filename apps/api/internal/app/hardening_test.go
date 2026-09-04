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

// TestFailedRegistrationLeavesNoUserOrConsumedInvite covers the report's L6: the
// mailbox used to be created after the transaction had already been committed, so a
// rejection there returned 201 with a live session, an account without a mailbox and
// an invite use spent on it.
func TestFailedRegistrationLeavesNoUserOrConsumedInvite(t *testing.T) {
	a := newTestApp(t)
	a.cfg.InviteRegistrationEnabled = true
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var invite RegistrationInvite
	if code := admin.do("POST", "/api/admin/registration-invites", map[string]any{"code": "rollback-1", "maxUses": 1}, &invite); code != http.StatusCreated {
		t.Fatalf("create invite code=%d", code)
	}

	domainID := mustDefaultDomainID(t, a)
	if _, err := a.db.ExecContext(t.Context(), `UPDATE domains SET status='disabled' WHERE id=?`, domainID); err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if code := (&testClient{t: t, server: ts}).do("POST", "/api/auth/register", map[string]any{
		"email": "rollback@example.test", "displayName": "Rollback", "password": "Password123!",
		"inviteCode": invite.Code, "domainId": domainID, "localPart": "rollback",
	}, &body); code != http.StatusBadRequest {
		t.Fatalf("registration onto a disabled domain code=%d body=%v", code, body)
	}

	var users int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, "rollback@example.test").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("failed registration left %d user rows", users)
	}
	var list struct {
		Items []RegistrationInvite `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/registration-invites", nil, &list); code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("list invites code=%d items=%+v", code, list.Items)
	}
	if list.Items[0].UsedCount != 0 || list.Items[0].RemainingUses != 1 {
		t.Errorf("failed registration consumed the invite: %+v", list.Items[0])
	}
}

// TestSMTPDailyQuotaChargesPerRecipient covers the report's M6: the daily allowance was
// charged once per message regardless of how many addresses it carried, so a single
// send could cover up to maxRecipientsPerMessage recipients for the price of one.
func TestSMTPDailyQuotaChargesPerRecipient(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	// A daily allowance of 5 with the per-minute cap left unlimited isolates the
	// volume budget, which is the guard this test is about.
	updateRegularPermissionGroupWithLimits(t, admin, regularUserDefaultPermissions(), PermissionLimits{MaxAttachmentMB: 10, SMTPDailyLimit: 5})

	domainID := mustDefaultDomainID(t, a)
	sender := createTestMailbox(t, admin, domainID, "quota-sender", "Quota Sender", "Password123!", nil)
	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("sender login code=%d", code)
	}
	send := func(subject string, to []string) int {
		var body map[string]any
		return user.do("POST", "/api/mail/send", map[string]any{
			"mailboxId": sender.ID, "to": to, "subject": subject, "text": "body", "html": "<p>body</p>",
		}, &body)
	}

	three := []string{"a@example.test", "b@example.test", "c@example.test"}
	if code := send("first", three); code != http.StatusCreated {
		t.Fatalf("three-recipient send code=%d", code)
	}
	// Three of five units are spent, so another three no longer fit.
	if code := send("second", three); code != http.StatusTooManyRequests {
		t.Errorf("second three-recipient send code=%d, want 429", code)
	}
	// The remaining two units are still usable, then the allowance is exhausted.
	if code := send("third", []string{"d@example.test", "e@example.test"}); code != http.StatusCreated {
		t.Errorf("two-recipient send code=%d, want 201", code)
	}
	if code := send("fourth", []string{"f@example.test"}); code != http.StatusTooManyRequests {
		t.Errorf("send past the allowance code=%d, want 429", code)
	}
	var charged int
	if err := a.db.QueryRow(`SELECT COALESCE(SUM(recipients),0) FROM smtp_send_events`).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != 5 {
		t.Errorf("charged %d units, want 5", charged)
	}
}

// TestSMTPRateRecipientsMigrationBackfillsExistingTable guards the upgrade path: the
// inline DDL only covers fresh databases, so an existing deployment depends on the
// PRAGMA-probing migration to gain the column.
func TestSMTPRateRecipientsMigrationBackfillsExistingTable(t *testing.T) {
	a := newTestApp(t)
	ctx := t.Context()
	if _, err := a.db.ExecContext(ctx, `ALTER TABLE smtp_send_events DROP COLUMN recipients`); err != nil {
		t.Fatal(err)
	}
	columns, err := sqliteTableColumns(ctx, a.db, "smtp_send_events")
	if err != nil {
		t.Fatal(err)
	}
	if columns["recipients"] {
		t.Fatal("column was not dropped, the test cannot prove anything")
	}
	if err := a.migrateSMTPRateRecipients(ctx); err != nil {
		t.Fatal(err)
	}
	columns, err = sqliteTableColumns(ctx, a.db, "smtp_send_events")
	if err != nil {
		t.Fatal(err)
	}
	if !columns["recipients"] {
		t.Fatal("migration did not add smtp_send_events.recipients")
	}
	// Rows written before the upgrade must read as one charged unit, which is what
	// per-message accounting always meant.
	var userID, mailboxID string
	if err := a.db.QueryRowContext(ctx, `SELECT u.id,m.id FROM users u JOIN mailboxes m ON m.user_id=u.id LIMIT 1`).Scan(&userID, &mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO smtp_send_events(id,user_id,mailbox_id,created_at) VALUES(?,?,?,?)`,
		newID("smtp"), userID, mailboxID, a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var charged int
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(recipients),0) FROM smtp_send_events`).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != 1 {
		t.Errorf("legacy row charged %d units, want 1", charged)
	}
	// Idempotent: startup runs every migration on every boot.
	if err := a.migrateSMTPRateRecipients(ctx); err != nil {
		t.Fatalf("re-running the migration failed: %v", err)
	}
}
