package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Simulate a mail service accepting SMTP and writing the recipient Maildir.
// This deliberately exercises the importer, not real SMTP or IMAP transport.
func importSyntheticSMTPMessage(t *testing.T, a *App, recipient Mailbox, sentID string) {
	t.Helper()
	ctx := context.Background()
	msg, err := a.storedMessageByID(ctx, sentID)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := a.attachmentInputsForMessage(ctx, sentID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildMIME(MIMEMessage{From: msg.From, To: msg.To, CC: msg.CC, Subject: msg.Subject, Text: msg.BodyText, HTML: msg.BodyHTML, MessageID: msg.MessageID, Date: msg.SentAt, Attachments: attachments})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic.eml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	folder, err := a.ensureFolder(ctx, recipient.ID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.syncMaildirFile(ctx, maildirMailbox{ID: recipient.ID, Address: recipient.Address}, maildirFolder{ID: folder, Name: "Inbox"}, path); err != nil {
		t.Fatal(err)
	}
}

func collectionFixture(t *testing.T) (*App, *testClient, Mailbox, Mailbox, Mailbox) {
	t.Helper()
	a, admin, _ := newAdminPaginationTest(t)
	boxes := []Mailbox{}
	for _, name := range []string{"a.test", "b.test", "c.test"} {
		var d Domain
		if code := admin.do("POST", "/api/admin/domains", map[string]any{"name": name}, &d); code != 201 {
			t.Fatalf("domain %s: %d", name, code)
		}
		boxes = append(boxes, createTestMailbox(t, admin, d.ID, "archive", "Synthetic collection", "Password123!", nil))
	}
	return a, admin, boxes[0], boxes[1], boxes[2]
}

func setCollection(t *testing.T, c *testClient, domain, target string) {
	t.Helper()
	if code := c.do("POST", "/api/admin/domains/"+domain+"/collection", domainCollection{Enabled: target != "", TargetMailboxID: target}, nil); code != 200 {
		t.Fatalf("set collection: %d", code)
	}
}

func TestDomainCollectionRouting(t *testing.T) {
	a, admin, aa, bb, cc := collectionFixture(t)
	setCollection(t, admin, aa.DomainID, bb.ID)
	setCollection(t, admin, bb.DomainID, cc.ID)
	targets, unknown, err := a.localRecipientTargets(t.Context(), []string{"ARCHIVE+tag@A.TEST", "missing@a.test"})
	if err != nil || len(unknown) != 0 || len(targets) != 2 || !targets[aa.ID] || !targets[bb.ID] || targets[cc.ID] {
		t.Fatalf("cascade/plus: %v %v %v", targets, unknown, err)
	}
	targets, _, err = a.localRecipientTargets(t.Context(), []string{bb.Address})
	if err != nil || !targets[bb.ID] || !targets[cc.ID] {
		t.Fatalf("direct target domain must still collect: %v %v", targets, err)
	}
	setCollection(t, admin, bb.DomainID, aa.ID)
	targets, _, err = a.localRecipientTargets(t.Context(), []string{aa.Address, bb.Address, "archive+tag@a.test"})
	if err != nil || len(targets) != 2 {
		t.Fatalf("cycle/dedupe: %v %v", targets, err)
	}
	setCollection(t, admin, bb.DomainID, bb.ID)
	targets, _, err = a.localRecipientTargets(t.Context(), []string{aa.Address, bb.Address, "missing@a.test", "missing@b.test"})
	if err != nil || len(targets) != 2 {
		t.Fatalf("shared destination: %v %v", targets, err)
	}
	a.cfg.CatchAllEnabled = true
	targets, unknown, err = a.localRecipientTargets(t.Context(), []string{"unknown@a.test", "unknown@c.test"})
	if err != nil || len(targets) != 1 || len(unknown) != 1 || unknown[0] != "unknown@c.test" {
		t.Fatalf("fallback precedence: %v %v %v", targets, unknown, err)
	}
	var alias Alias
	if code := admin.do("POST", "/api/admin/aliases", map[string]any{"domainId": aa.DomainID, "source": "sales", "destination": cc.Address}, &alias); code != 201 {
		t.Fatalf("alias: %d", code)
	}
	targets, _, err = a.localRecipientTargets(t.Context(), []string{"sales@a.test"})
	if err != nil || len(targets) != 2 || !targets[bb.ID] || !targets[cc.ID] {
		t.Fatalf("alias and original domain collection: %v %v", targets, err)
	}
}

func TestDomainCollectionFinalFailureAndOpenAPI(t *testing.T) {
	a, admin, aa, bb, _ := collectionFixture(t)
	reader := &testClient{t: t, server: admin.server, bearer: createTestAPITokenWithScopes(t, admin, "collection-reader", []string{"domains:read"})}
	writer := &testClient{t: t, server: admin.server, bearer: createTestAPITokenWithScopes(t, admin, "collection-writer", []string{"domains:read", "domains:write", "mailboxes:write"})}
	path := "/api/open/v1/domains/" + aa.DomainID + "/collection"
	if code := reader.do("POST", path, domainCollection{Enabled: true, TargetMailboxID: bb.ID}, nil); code != 403 {
		t.Fatalf("reader write: %d", code)
	}
	if code := writer.do("POST", path, domainCollection{Enabled: true, TargetMailboxID: bb.ID}, nil); code != 200 {
		t.Fatalf("writer: %d", code)
	}
	if code := reader.do("GET", path, nil, nil); code != 200 {
		t.Fatalf("reader: %d", code)
	}
	if code := writer.do("DELETE", "/api/open/v1/mailboxes/"+bb.ID, nil, nil); code != 409 {
		t.Fatalf("Open API target delete: %d", code)
	}
	if _, err := a.db.Exec("UPDATE mailboxes SET quota_mb=1 WHERE id=?", bb.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := newID("source")
	msg := storedMessage{MessageID: "<synthetic-failure@test.invalid>", From: aa.Address, To: []string{aa.Address}, Subject: "final failure", BodyText: strings.Repeat("x", 1024*1024+1), SentAt: now, ReceivedAt: now}
	if err := a.deliverLocalRecipients(t.Context(), source, []string{aa.Address}, msg, nil); err != nil {
		t.Fatal(err)
	}
	var job string
	if err := a.db.QueryRow("SELECT id FROM local_delivery_jobs WHERE source_id=? AND target_mailbox_id=?", source, bb.ID).Scan(&job); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 8; i++ {
		if _, err := a.db.Exec("UPDATE local_delivery_jobs SET next_attempt_at='' WHERE id=?", job); err != nil {
			t.Fatal(err)
		}
		a.processLocalDelivery(t.Context(), job)
	}
	var status, payload string
	var attempts, count int
	if err := a.db.QueryRow("SELECT status,payload,attempts FROM local_delivery_jobs WHERE id=?", job).Scan(&status, &payload, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || payload != "" || attempts != 8 {
		t.Fatalf("terminal: %s payload=%d attempts=%d", status, len(payload), attempts)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND subject='final failure'", aa.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("original=%d err=%v", count, err)
	}
	a.processLocalDelivery(t.Context(), job)
	if code := reader.do("GET", "/api/open/v1/local-delivery-jobs", nil, nil); code != 200 {
		t.Fatal(code)
	}
}

func assertDomainCollectionContract(t *testing.T, a *App) {
	t.Helper()
	ctx := context.Background()
	var mailbox, domain, address string
	if err := a.db.QueryRowContext(ctx, "SELECT id,domain_id,address FROM mailboxes WHERE status='active' LIMIT 1").Scan(&mailbox, &domain, &address); err != nil {
		t.Fatal(err)
	}
	now := a.now().UTC()
	if _, err := a.db.ExecContext(ctx, "INSERT INTO domain_collections(domain_id,target_mailbox_id,updated_at) VALUES(?,?,?)", domain, mailbox, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	defer a.db.ExecContext(ctx, "DELETE FROM domain_collections WHERE domain_id=?", domain)
	if err := a.collectionTargetInUse(ctx, "mailbox", mailbox); err != errCollectionTargetInUse {
		t.Fatalf("guard: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, "DELETE FROM mailboxes WHERE id=?", mailbox); err == nil {
		t.Fatal("collection foreign key did not prevent deletion")
	}
	msg := storedMessage{MessageID: "<collection-contract@synthetic.test>", From: address, To: []string{address}, Subject: "collection contract", BodyText: "synthetic", SentAt: now, ReceivedAt: now}
	source := newID("source")
	for i := 0; i < 2; i++ {
		if err := a.deliverLocalRecipients(ctx, source, []string{address}, msg, nil); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var status string
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM local_delivery_jobs WHERE source_id=?", source).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotent jobs=%d err=%v", count, err)
	}
	if err := a.db.QueryRowContext(ctx, "SELECT status FROM local_delivery_jobs WHERE source_id=?", source).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("delivery=%s err=%v", status, err)
	}
}

func TestDomainCollectionSQLiteContract(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	assertDomainCollectionContract(t, a)
}

func TestDomainCollectionRecoversPersistedDelivery(t *testing.T) {
	a, admin, aa, bb, _ := collectionFixture(t)
	setCollection(t, admin, aa.DomainID, bb.ID)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfg.MaildirRoot = blocked
	now := time.Now().UTC()
	msg := storedMessage{MessageID: "<recover@synthetic.test>", From: aa.Address, To: []string{"unknown@a.test"}, Subject: "synthetic recovery", BodyText: "body", SentAt: now, ReceivedAt: now}
	source := newID("source")
	if err := a.deliverLocalRecipients(t.Context(), source, msg.To, msg, nil); err != nil {
		t.Fatal(err)
	}
	var job, messageID, status string
	if err := a.db.QueryRow("SELECT id,message_id,status FROM local_delivery_jobs WHERE source_id=?", source).Scan(&job, &messageID, &status); err != nil {
		t.Fatal(err)
	}
	if messageID == "" || status != "queued" {
		t.Fatalf("persisted partial delivery: %s %s", messageID, status)
	}
	// Model a process dying with a lease after insertion and before filesystem delivery.
	if _, err := a.db.Exec("UPDATE local_delivery_jobs SET status='sending',lease_until='',lease_token='expired' WHERE id=?", job); err != nil {
		t.Fatal(err)
	}
	a.cfg.MaildirRoot = t.TempDir()
	a.processLocalDelivery(t.Context(), job)
	a.processLocalDelivery(t.Context(), job)
	var finalID, rawPath string
	var count int
	if err := a.db.QueryRow("SELECT status,message_id FROM local_delivery_jobs WHERE id=?", job).Scan(&status, &finalID); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || finalID != messageID {
		t.Fatalf("recovery changed identity: %s %s", status, finalID)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND subject=?", bb.ID, msg.Subject).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery duplicated message: %d %v", count, err)
	}
	if err := a.db.QueryRow("SELECT raw_path FROM messages WHERE id=?", messageID).Scan(&rawPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Dir(rawPath))
	if err != nil || len(files) != 1 {
		t.Fatalf("Maildir files=%d error=%v", len(files), err)
	}
}

func TestDomainCollectionSubmissionRouting(t *testing.T) {
	a, admin, aa, bb, _ := collectionFixture(t)
	setCollection(t, admin, aa.DomainID, bb.ID)
	user, sender := defaultAdminUserAndMailbox(t, a)
	raw := "From: " + sender.Address + "\r\nTo: " + aa.Address + "\r\nMessage-ID: <submission-collection@synthetic.test>\r\nSubject: synthetic submission\r\n\r\nSynthetic body"
	for i := 0; i < 2; i++ {
		if err := a.submitSMTPMessage(t.Context(), user, sender, sender.Address, []string{aa.Address}, strings.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{aa.ID, bb.ID} {
		var n int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND subject='synthetic submission'", target).Scan(&n); err != nil || n != 1 {
			t.Fatalf("submission recipient copies=%d err=%v", n, err)
		}
	}
}

func TestDomainCollectionIndependentRetry(t *testing.T) {
	a, admin, aa, bb, _ := collectionFixture(t)
	setCollection(t, admin, aa.DomainID, bb.ID)
	if _, err := a.db.Exec("UPDATE mailboxes SET quota_mb=1 WHERE id=?", bb.ID); err != nil {
		t.Fatal(err)
	}
	folder, err := a.ensureFolder(t.Context(), bb.ID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	filler, err := a.insertMessage(t.Context(), storedMessage{MailboxID: bb.ID, FolderID: folder, MessageUID: newID("uid"), MessageID: "<quota@synthetic.test>", BodyText: strings.Repeat("x", 1024*1024), SentAt: time.Now(), ReceivedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sent MailMessage
	payload := map[string]any{"to": []string{aa.Address, "archive+tag@a.test"}, "bcc": []string{"private@external.test"}, "subject": "synthetic collection retry", "text": "synthetic text", "attachments": []map[string]string{{"filename": "sample.txt", "contentType": "text/plain", "contentBase64": base64.StdEncoding.EncodeToString([]byte("synthetic attachment"))}}}
	if code := admin.do("POST", "/api/mail/send", payload, &sent); code != 201 {
		t.Fatalf("send: %d", code)
	}
	count := func(mailbox string) int {
		var n int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND subject=?", mailbox, "synthetic collection retry").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(aa.ID) != 1 || count(bb.ID) != 0 {
		t.Fatal("original must succeed independently")
	}
	var id, status, last string
	var attempts int
	if err := a.db.QueryRow("SELECT id,status,attempts,last_error FROM local_delivery_jobs WHERE source_id=? AND target_mailbox_id=?", sent.ID, bb.ID).Scan(&id, &status, &attempts, &last); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 1 || last != "mailbox_quota_exceeded" {
		t.Fatalf("retry state %s %d %s", status, attempts, last)
	}
	a.deleteMessage(t.Context(), filler)
	if _, err := a.db.Exec("UPDATE local_delivery_jobs SET next_attempt_at='' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	a.processLocalDelivery(t.Context(), id)
	a.processLocalDelivery(t.Context(), id)
	if count(bb.ID) != 1 {
		t.Fatal("retry must deliver exactly once")
	}
	var bcc, raw string
	if err := a.db.QueryRow("SELECT bcc_addrs FROM messages WHERE mailbox_id=? AND subject=?", bb.ID, "synthetic collection retry").Scan(&bcc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bcc, "private") {
		t.Fatal("Bcc leaked into recipient copy")
	}
	if err := a.db.QueryRow("SELECT status,payload FROM local_delivery_jobs WHERE id=?", id).Scan(&status, &raw); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || raw != "" {
		t.Fatal("delivered payload must be cleared")
	}
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM attachments at JOIN messages m ON m.id=at.message_id WHERE m.mailbox_id=? AND m.subject=?", bb.ID, "synthetic collection retry").Scan(&n); err != nil || n != 1 {
		t.Fatalf("attachment: %d %v", n, err)
	}
}

func TestDomainCollectionGuardsAndAudit(t *testing.T) {
	a, admin, aa, bb, cc := collectionFixture(t)
	path := "/api/admin/domains/" + aa.DomainID + "/collection"
	if code := admin.do("POST", path, domainCollection{Enabled: true, TargetMailboxID: "outside@example.org"}, nil); code != 400 {
		t.Fatalf("external target: %d", code)
	}
	setCollection(t, admin, aa.DomainID, bb.ID)
	for _, tc := range []struct {
		path   string
		body   any
		method string
	}{
		{"/api/admin/mailboxes/" + bb.ID, nil, "DELETE"},
		{"/api/admin/users/" + bb.UserID, nil, "DELETE"},
		{"/api/admin/domains/" + bb.DomainID, map[string]any{"status": "disabled"}, "POST"},
		{"/api/admin/mailboxes/" + bb.ID, map[string]any{"displayName": "Synthetic", "userId": cc.UserID, "status": "active", "quotaMb": 10}, "POST"},
		{"/api/admin/users/" + bb.UserID, map[string]any{"displayName": "Synthetic", "role": "user", "disabled": true}, "POST"},
		{"/api/admin/aliases", map[string]any{"domainId": bb.DomainID, "source": bb.Address, "destination": cc.Address}, "POST"},
	} {
		if code := admin.do(tc.method, tc.path, tc.body, nil); code != 409 {
			t.Errorf("guard %s: %d", tc.path, code)
		}
	}
	regular := &testClient{t: t, server: admin.server}
	if code := regular.do("POST", "/api/auth/login", map[string]string{"email": aa.Address, "password": "Password123!"}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := regular.do("GET", path, nil, nil); code != 403 {
		t.Fatalf("non-admin read: %d", code)
	}
	if code := regular.do("POST", path, domainCollection{}, nil); code != 403 {
		t.Fatalf("non-admin write: %d", code)
	}
	setCollection(t, admin, aa.DomainID, cc.ID)
	setCollection(t, admin, aa.DomainID, "")
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM domain_collection_audit WHERE domain_id=?", aa.DomainID).Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit %d %v", n, err)
	}
	if code := admin.do("POST", "/api/admin/domains/"+bb.DomainID, map[string]any{"status": "disabled"}, nil); code != 200 {
		t.Fatalf("released target: %d", code)
	}
	if code := admin.do("POST", path, domainCollection{Enabled: true, TargetMailboxID: bb.ID}, nil); code != 400 {
		t.Fatalf("disabled domain target: %d", code)
	}
}

func TestDomainCollectionPostfixSQLiteMaps(t *testing.T) {
	a, admin, aa, bb, cc := collectionFixture(t)
	setCollection(t, admin, aa.DomainID, bb.ID)
	setCollection(t, admin, bb.DomainID, cc.ID)
	queryMap := func(name, address string) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "postfix", name))
		if err != nil {
			t.Fatal(err)
		}
		query := strings.TrimSpace(strings.SplitN(string(data), "query = ", 2)[1])
		query = strings.ReplaceAll(query, "%s", address)
		rows, err := a.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := []string{}
		for rows.Next() {
			var dest string
			if err := rows.Scan(&dest); err != nil {
				t.Fatal(err)
			}
			result = append(result, dest)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, tc := range []struct{ file, address, want string }{
		{"sqlite-collection-bcc.cf", aa.Address, bb.Address},
		{"sqlite-collection-bcc.cf", "archive+tag@a.test", bb.Address},
		{"sqlite-aliases.cf", "ghost@a.test", bb.Address},
		{"sqlite-aliases.cf", bb.Address, bb.Address},
	} {
		if got := queryMap(tc.file, tc.address); fmt.Sprint(got) != fmt.Sprint([]string{tc.want}) {
			t.Errorf("%s %s: %v", tc.file, tc.address, got)
		}
	}
	setCollection(t, admin, bb.DomainID, bb.ID)
	if got := queryMap("sqlite-collection-bcc.cf", "archive+tag@b.test"); len(got) != 0 {
		t.Fatal("self BCC", got)
	}
	if got := queryMap("sqlite-aliases.cf", "archive+tag@a.test"); len(got) != 1 || got[0] != aa.Address {
		t.Fatal("known plus address must normalize to its mailbox", got)
	}
}
