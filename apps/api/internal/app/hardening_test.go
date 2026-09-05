package app

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// TestSyncEventsReachOnlyTheAffectedAudience covers the report's L7: every maildir sync
// woke every open stream, so one user receiving mail refreshed every other logged-in
// browser and leaked the timing of that delivery.
func TestSyncEventsReachOnlyTheAffectedAudience(t *testing.T) {
	a := newTestApp(t)
	a.cfg.MaildirRoot = newTestDir(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	alice := createTestMailbox(t, admin, domainID, "sse-alice", "SSE Alice", "Password123!", nil)
	bob := createTestMailbox(t, admin, domainID, "sse-bob", "SSE Bob", "Password123!", nil)
	aliceUser, _, err := a.userByEmail(t.Context(), alice.Address)
	if err != nil {
		t.Fatal(err)
	}
	bobUser, _, err := a.userByEmail(t.Context(), bob.Address)
	if err != nil {
		t.Fatal(err)
	}

	aliceEvents, stopAlice := a.subscribeSyncEvents(aliceUser.ID)
	defer stopAlice()
	bobEvents, stopBob := a.subscribeSyncEvents(bobUser.ID)
	defer stopBob()

	dir := filepath.Join(a.cfg.MaildirRoot, "lanqin.local", "sse-alice", "Maildir", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIncrementalMaildirMessage(t, dir, "for-alice", "<sse-audience@example.test>")

	counts, err := a.syncMaildirOnceTracked(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Imported != 1 {
		t.Fatalf("counts=%+v, want one imported message", counts)
	}
	select {
	case <-aliceEvents:
	default:
		t.Error("the mailbox owner was not notified of their own delivery")
	}
	select {
	case <-bobEvents:
		t.Error("an unrelated user was notified of somebody else's delivery")
	default:
	}
}

// TestSyncEventAudienceFollowsLiveSharesOnly pins the audience predicate: a mailbox
// shared with someone has to reach them too, or targeting the fan-out would quietly
// cost shared-mailbox viewers their live refresh. A revoked share must not.
func TestSyncEventAudienceFollowsLiveSharesOnly(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	owned := createTestMailbox(t, admin, domainID, "share-owner", "Share Owner", "Password123!", nil)
	viewer := createTestMailbox(t, admin, domainID, "share-viewer", "Share Viewer", "Password123!", nil)
	revoked := createTestMailbox(t, admin, domainID, "share-revoked", "Share Revoked", "Password123!", nil)

	now := a.now().UTC().Format(time.RFC3339Nano)
	insertShare := func(sharedWith string, revokedAt any) {
		t.Helper()
		if _, err := a.db.ExecContext(t.Context(), `INSERT INTO mailbox_shares(id,mailbox_id,owner_user_id,shared_with_user_id,scope,revoked_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?)`, newID("shr"), owned.ID, owned.UserID, sharedWith, "all", revokedAt, now, now); err != nil {
			t.Fatal(err)
		}
	}
	insertShare(viewer.UserID, nil)
	insertShare(revoked.UserID, now)

	audience, err := a.usersForMailboxes(t.Context(), []string{owned.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := audience[owned.UserID]; !ok {
		t.Error("mailbox owner missing from the audience")
	}
	if _, ok := audience[viewer.UserID]; !ok {
		t.Error("live share recipient missing from the audience")
	}
	if _, ok := audience[revoked.UserID]; ok {
		t.Error("revoked share recipient is still in the audience")
	}
	if len(audience) != 2 {
		t.Errorf("audience=%v, want exactly the owner and the live share recipient", audience)
	}
}

// TestAttachmentCountCapRejectsOversizedSend covers the remainder of the report's M4:
// the size limit applies per attachment, so without a count bound one request could
// carry thousands of tiny parts, each costing a MIME part, a row and a file.
func TestAttachmentCountCapRejectsOversizedSend(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var boxes struct {
		Items []Mailbox `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/mailboxes", nil, &boxes); code != http.StatusOK || len(boxes.Items) == 0 {
		t.Fatalf("mailboxes code=%d items=%d", code, len(boxes.Items))
	}

	attachments := make([]map[string]string, maxAttachmentsPerMessage+1)
	for i := range attachments {
		attachments[i] = map[string]string{
			"filename":      "part.txt",
			"contentType":   "text/plain",
			"contentBase64": base64.StdEncoding.EncodeToString([]byte("x")),
		}
	}
	var body map[string]any
	if code := admin.do("POST", "/api/mail/send", map[string]any{
		"mailboxId": boxes.Items[0].ID, "to": []string{"someone@example.test"},
		"subject": "many parts", "text": "body", "html": "", "attachments": attachments,
	}, &body); code != http.StatusBadRequest {
		t.Fatalf("oversized attachment count code=%d, want 400 body=%v", code, body)
	}
	// One part below the cap still goes through.
	if code := admin.do("POST", "/api/mail/send", map[string]any{
		"mailboxId": boxes.Items[0].ID, "to": []string{"someone@example.test"},
		"subject": "at the cap", "text": "body", "html": "", "attachments": attachments[:maxAttachmentsPerMessage],
	}, &body); code != http.StatusCreated {
		t.Fatalf("send at the cap code=%d body=%v", code, body)
	}
}

// TestAdminPasswordResetDisablesAPITokens covers the rest of the report's H4: the reset
// revoked every session, but API tokens authenticate independently of sessions, so the
// very action taken to lock an attacker out left them with full API access.
func TestAdminPasswordResetDisablesAPITokens(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	owned := createTestMailbox(t, admin, domainID, "token-owner", "Token Owner", "Password123!", nil)

	owner := &testClient{t: t, server: ts}
	if code := owner.do("POST", "/api/auth/login", map[string]string{"email": owned.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("owner login code=%d", code)
	}
	token := createTestAPIToken(t, owner, "containment")
	bearer := &testClient{t: t, server: ts, bearer: token}
	var list struct {
		Items []SendQueueEntry `json:"items"`
	}
	if code := bearer.do("GET", "/api/open/v1/send", nil, &list); code != http.StatusOK {
		t.Fatalf("token should work before the reset: code=%d", code)
	}

	if code := admin.do("POST", "/api/admin/users/"+owned.UserID+"/password", map[string]string{"password": "NewPassword123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin reset code=%d", code)
	}
	if code := bearer.do("GET", "/api/open/v1/send", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("token still authenticates after a containment reset: code=%d", code)
	}
	// The owner's sessions are gone too, so the reset is a complete cut-off.
	if code := owner.do("GET", "/api/me", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("session survived the reset: code=%d", code)
	}
}

// TestOpenAPIPasswordResetAlsoContainsTheAccount pins the third reset path. The report's
// H4 named the admin UI reset and the self-service change; the Open API mailbox password
// reset resets somebody else's credentials too and was leaving every session and token
// untouched.
func TestOpenAPIPasswordResetAlsoContainsTheAccount(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	owned := createTestMailbox(t, admin, domainID, "openapi-reset", "OpenAPI Reset", "Password123!", nil)
	adminToken := createTestAPIToken(t, admin, "operator")

	owner := &testClient{t: t, server: ts}
	if code := owner.do("POST", "/api/auth/login", map[string]string{"email": owned.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("owner login code=%d", code)
	}
	ownerToken := createTestAPIToken(t, owner, "victim")

	operator := &testClient{t: t, server: ts, bearer: adminToken}
	if code := operator.do("POST", "/api/open/v1/mailboxes/"+owned.ID+"/password", map[string]string{"password": "NewPassword123!"}, nil); code != http.StatusOK {
		t.Fatalf("open api reset code=%d", code)
	}
	if code := owner.do("GET", "/api/me", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("session survived the reset: code=%d", code)
	}
	victim := &testClient{t: t, server: ts, bearer: ownerToken}
	if code := victim.do("GET", "/api/open/v1/send", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("token still authenticates after the reset: code=%d", code)
	}
	// The operator's own token is untouched: only the target account is contained.
	if code := operator.do("GET", "/api/open/v1/mailboxes", nil, nil); code != http.StatusOK {
		t.Errorf("the operator's token was disabled too: code=%d", code)
	}
}

// TestTOTPSecretIsEncryptedAtRestWhenKeyConfigured covers the last of the report's L1
// recommendations: the seed used to sit in the users table in plaintext, unlike every
// other secret of its class.
func TestTOTPSecretIsEncryptedAtRestWhenKeyConfigured(t *testing.T) {
	a := newTestApp(t)
	a.cfg.TwoFactorEnabled = true
	a.cfg.NotificationSecretKey = "totp-seed-root-key"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}

	var setup struct {
		Secret string `json:"secret"`
	}
	if code := client.do("POST", "/api/me/2fa/setup", nil, &setup); code != http.StatusOK || setup.Secret == "" {
		t.Fatalf("2fa setup code=%d secret=%q", code, setup.Secret)
	}

	var stored string
	if err := a.db.QueryRow(`SELECT two_factor_secret FROM users WHERE email=?`, "admin@lanqin.local").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, setup.Secret) {
		t.Error("the seed was stored in a form that still contains the plaintext")
	}
	if !strings.HasPrefix(stored, totpSecretEncryptionPrefix) {
		t.Errorf("stored seed is not marked as ciphertext: %q", stored)
	}

	// The round trip must still authenticate, otherwise encryption has locked the
	// account out rather than protected it.
	code, err := generateTOTP(setup.Secret, a.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var enabled struct {
		User User `json:"user"`
	}
	if status := client.do("POST", "/api/me/2fa/enable", map[string]string{"code": code}, &enabled); status != http.StatusOK || !enabled.User.TwoFactorEnabled {
		t.Fatalf("enable with an encrypted seed status=%d user=%+v", status, enabled.User)
	}
}

// TestTOTPPlaintextSecretsKeepWorking guards the compatibility half of the change:
// seeds written before encryption existed carry no marker and must still verify.
func TestTOTPPlaintextSecretsKeepWorking(t *testing.T) {
	a := newTestApp(t)
	a.cfg.NotificationSecretKey = "totp-seed-root-key"
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, "admin@lanqin.local").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	// Written the way the pre-encryption code did: no marker, no ciphertext.
	if _, err := a.db.Exec(`UPDATE users SET two_factor_enabled=1, two_factor_secret=? WHERE id=?`, secret, userID); err != nil {
		t.Fatal(err)
	}

	loaded, stored, err := a.loadUserAuthByID(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.TwoFactorEnabled || stored != secret {
		t.Fatalf("plaintext seed did not survive the read path: %q", stored)
	}
	code, err := generateTOTP(stored, a.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !a.consumeTOTP(t.Context(), userID, stored, code) {
		t.Error("a pre-encryption seed no longer verifies")
	}
}

// TestEncryptedTOTPSecretWithoutKeyFailsLoudly checks the failure mode is named rather
// than silent. Treating ciphertext as a seed would reject every correct code and look
// like the user's authenticator was broken.
func TestEncryptedTOTPSecretWithoutKeyFailsLoudly(t *testing.T) {
	a := newTestApp(t)
	a.cfg.NotificationSecretKey = "totp-seed-root-key"
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := a.encryptTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}

	a.cfg.NotificationSecretKey = ""
	if _, err := a.decryptTOTPSecret(stored); err == nil {
		t.Fatal("decrypting without the key silently succeeded")
	}
}

// TestAdminCanResetAnotherUsersTwoFactor covers the recovery path that makes seed
// encryption survivable: with no administrative reset, a lost key or lost authenticator
// locks the account permanently.
func TestAdminCanResetAnotherUsersTwoFactor(t *testing.T) {
	a := newTestApp(t)
	a.cfg.TwoFactorEnabled = true
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	locked := createTestMailbox(t, admin, domainID, "lost-authenticator", "Locked Out", "Password123!", nil)

	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := a.encryptTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	// The session and token are established before the second factor goes on the row:
	// once it is enabled, logging in only yields a 2FA challenge. They stand in for
	// whatever the user still holds at the moment they lose their authenticator.
	owner := &testClient{t: t, server: ts}
	if code := owner.do("POST", "/api/auth/login", map[string]string{"email": locked.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("owner login code=%d", code)
	}
	bearer := &testClient{t: t, server: ts, bearer: createTestAPIToken(t, owner, "locked-out")}
	if _, err := a.db.Exec(`UPDATE users SET two_factor_enabled=1, two_factor_secret=? WHERE id=?`, encrypted, locked.UserID); err != nil {
		t.Fatal(err)
	}
	// With 2FA on and the authenticator gone, the password alone no longer gets in.
	stuck := &testClient{t: t, server: ts}
	var challenge struct {
		TwoFactorRequired bool `json:"twoFactorRequired"`
	}
	if code := stuck.do("POST", "/api/auth/login", map[string]string{"email": locked.Address, "password": "Password123!"}, &challenge); code != http.StatusOK || !challenge.TwoFactorRequired {
		t.Fatalf("expected a 2fa challenge before the reset: code=%d challenge=%+v", code, challenge)
	}

	if code := admin.do("POST", "/api/admin/users/"+locked.UserID+"/two-factor/reset", nil, nil); code != http.StatusOK {
		t.Fatalf("admin 2fa reset code=%d", code)
	}

	var enabled int
	var stored string
	if err := a.db.QueryRow(`SELECT two_factor_enabled,two_factor_secret FROM users WHERE id=?`, locked.UserID).Scan(&enabled, &stored); err != nil {
		t.Fatal(err)
	}
	if intBool(enabled) || stored != "" {
		t.Errorf("second factor survived the reset: enabled=%d secret=%q", enabled, stored)
	}
	// Same containment semantics as a password reset: the account is in trouble, so
	// its sessions and tokens go with it.
	if code := owner.do("GET", "/api/me", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("session survived the 2fa reset: code=%d", code)
	}
	if code := bearer.do("GET", "/api/open/v1/send", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("api token survived the 2fa reset: code=%d", code)
	}
	// The account is usable again with the password alone.
	fresh := &testClient{t: t, server: ts}
	if code := fresh.do("POST", "/api/auth/login", map[string]string{"email": locked.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Errorf("login after the reset code=%d", code)
	}
	if code := fresh.do("GET", "/api/admin/users/"+locked.UserID+"/two-factor/reset", nil, nil); code == http.StatusOK {
		t.Error("a non-admin reached the reset endpoint")
	}
}

// TestMaildirRewriteKeepsFileWhenNameCollides covers a real bug the draft test caught
// only intermittently: maildirFilename is derived from the wall clock, which advances in
// ~0.5 ms steps on Windows, so saving a message and immediately rewriting it produced the
// same filename. os.Rename then overwrote in place and the unconditional cleanup of the
// old path deleted the file just written, leaving raw_path pointing at nothing.
func TestMaildirRewriteKeepsFileWhenNameCollides(t *testing.T) {
	a := newTestApp(t)
	a.cfg.MaildirRoot = newTestDir(t)
	ctx := t.Context()
	_, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)
	folderID, err := a.ensureFolder(ctx, mb.ID, "Drafts")
	if err != nil {
		t.Fatal(err)
	}

	now := a.now().UTC()
	stored := storedMessage{
		MailboxID: mb.ID, FolderID: folderID, MessageUID: newID("uid"),
		MessageID: "<collision@example.test>", Subject: "first", From: mb.Address,
		To: []string{"someone@example.test"}, SentAt: now, ReceivedAt: now,
		BodyText: "first body", IsRead: true,
	}
	id, err := a.insertMessage(ctx, stored, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.writeStoredMessageToMaildir(ctx, id, stored, nil); err != nil {
		t.Fatal(err)
	}
	firstPath := maildirRawPathForTest(t, a, id)

	// Rewrite immediately, which is what saving and then editing a draft does.
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET subject=?,body_text=? WHERE id=?`, "second", "second body", id); err != nil {
		t.Fatal(err)
	}
	if err := a.rewriteMessageMaildir(ctx, id); err != nil {
		t.Fatal(err)
	}
	secondPath := maildirRawPathForTest(t, a, id)

	raw, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("rewritten message is not on disk: %v", err)
	}
	if !strings.Contains(string(raw), "Subject: second") {
		t.Errorf("rewritten file still holds the old subject:\n%s", raw)
	}
	// When the name did change, the old file must be gone — the collision guard must
	// not turn into a leak.
	if !sameMaildirPath(firstPath, secondPath) {
		if _, err := os.Stat(firstPath); err == nil {
			t.Errorf("old maildir file was left behind: %s", firstPath)
		}
	}
}

func TestDeploymentCSPAllowsTurnstile(t *testing.T) {
	t.Parallel()
	header := regexp.MustCompile("add_header\\s+Content-Security-Policy\\s+\"([^\"]+)\"\\s+always;")
	for _, name := range []string{"nginx/default.conf", "all-in-one/nginx.conf"} {
		t.Run(name, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", name))
			if err != nil {
				t.Fatal(err)
			}
			match := header.FindStringSubmatch(string(content))
			if len(match) != 2 {
				t.Fatal("missing enforced CSP header")
			}
			directives := map[string][]string{}
			for _, directive := range strings.Split(match[1], ";") {
				parts := strings.Fields(directive)
				if len(parts) > 0 {
					directives[parts[0]] = parts[1:]
				}
			}
			for _, key := range []string{"script-src", "frame-src"} {
				values := directives[key]
				if !slices.Contains(values, "https://challenges.cloudflare.com") {
					t.Errorf("%s blocks Turnstile: %v", key, values)
				}
				for _, unsafe := range []string{"*", "https:", "'unsafe-inline'", "'unsafe-eval'"} {
					if slices.Contains(values, unsafe) {
						t.Errorf("%s must not be broadened to %s", key, unsafe)
					}
				}
			}
			for key, want := range map[string]string{"default-src": "'self'", "object-src": "'none'", "frame-ancestors": "'none'"} {
				if !slices.Equal(directives[key], []string{want}) {
					t.Errorf("%s=%v, want %s", key, directives[key], want)
				}
			}
		})
	}
}
