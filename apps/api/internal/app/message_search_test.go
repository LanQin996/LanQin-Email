package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMessageSearchFTSBackfillAndTriggers(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	if !a.messageSearchFTS {
		t.Fatal("message FTS was not enabled by the bundled SQLite driver")
	}

	for _, stmt := range []string{
		`DROP TRIGGER messages_fts_ai`,
		`DROP TRIGGER messages_fts_ad`,
		`DROP TRIGGER messages_fts_au`,
		`DROP TABLE messages_fts`,
	} {
		if _, err := a.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	a.messageSearchFTS = false
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	folderID, err := a.ensureFolder(context.Background(), mailbox.ID, "Search")
	if err != nil {
		t.Fatal(err)
	}
	messageID := insertSearchTestMessage(t, a, mailbox, folderID, "historical-backfill-needle")

	if err := a.ensureMessageSearchFTS(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFTSMessageMatch(t, a, messageID, "backfill-needle", true)

	if _, err := a.db.Exec(`UPDATE messages SET snippet=?,body_text=? WHERE id=?`, "updated-trigger-needle", "updated-trigger-needle", messageID); err != nil {
		t.Fatal(err)
	}
	assertFTSMessageMatch(t, a, messageID, "backfill-needle", false)
	assertFTSMessageMatch(t, a, messageID, "trigger-needle", true)

	if _, err := a.db.Exec(`DELETE FROM messages WHERE id=?`, messageID); err != nil {
		t.Fatal(err)
	}
	assertFTSMessageMatch(t, a, messageID, "trigger-needle", false)

	var legacyIndexCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_search'`).Scan(&legacyIndexCount); err != nil {
		t.Fatal(err)
	}
	if legacyIndexCount != 0 {
		t.Fatal("legacy message search index was not removed")
	}
}

func TestMessageSearchFTSAcrossAPIs(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	client := &testClient{t: t, server: server}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	folderID, err := a.ensureFolder(context.Background(), mailbox.ID, "Search")
	if err != nil {
		t.Fatal(err)
	}
	ftsID := insertSearchTestMessage(t, a, mailbox, folderID, `prefix Alpha-42 "quoted" suffix`)
	shortID := insertSearchTestMessage(t, a, mailbox, folderID, "这是蓝琴邮箱正文")

	assertSearchResult(t, client, "/api/mail/messages?folder=Search&q="+url.QueryEscape("alpha-42"), ftsID)
	assertSearchResult(t, client, "/api/mail/messages?folder=Search&q="+url.QueryEscape(`Alpha-42 "quoted"`), ftsID)
	assertSearchResult(t, client, "/api/mail/messages?folder=Search&q="+url.QueryEscape("蓝琴"), shortID)
	assertSearchResult(t, client, "/api/mail/messages?folder=Search&q="+url.QueryEscape("琴邮箱"), shortID)

	token := createTestAPIToken(t, client, "message-search")
	openClient := &testClient{t: t, server: server, bearer: token}
	assertSearchResult(t, openClient, "/api/open/mailboxes/"+url.PathEscape(mailbox.ID)+"/messages?folder=Search&q="+url.QueryEscape("Alpha-42"), ftsID)
	assertSearchResult(t, client, "/api/admin/messages?mailboxId="+url.QueryEscape(mailbox.ID)+"&q="+url.QueryEscape("Alpha-42"), ftsID)

	var byMailbox mailMessagePage
	if code := client.do("GET", "/api/admin/messages?mailboxId="+url.QueryEscape(mailbox.ID)+"&q="+url.QueryEscape(mailbox.Address), nil, &byMailbox); code != http.StatusOK {
		t.Fatalf("admin mailbox search code=%d", code)
	}
	if !mailPageContains(byMailbox.Items, ftsID) || !mailPageContains(byMailbox.Items, shortID) {
		t.Fatalf("admin mailbox address search omitted messages: %+v", byMailbox.Items)
	}
}

func TestMessageSearchQueryPlanUsesFTS(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	rows, err := a.db.Query(`EXPLAIN QUERY PLAN SELECT m.id FROM messages m
		WHERE m.mailbox_id=? AND m.rowid IN (
			SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?
		)`, "mb_test", messageFTSLiteralQuery("performance", webmailSearchColumns))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plans, "\n"), "messages_fts VIRTUAL TABLE") {
		t.Fatalf("query plan does not use messages_fts: %v", plans)
	}
}

func TestMessageSearchFTSLiteralQueryEscapesSyntax(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	for _, query := range []string{`"""`, `alpha OR beta`, `{subject} : alpha`, `NEAR(alpha beta)`, `"unterminated`} {
		var count int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, messageFTSLiteralQuery(query, webmailSearchColumns)).Scan(&count); err != nil {
			t.Fatalf("literal FTS query %q failed: %v", query, err)
		}
	}
}

func insertSearchTestMessage(t *testing.T, a *App, mailbox *Mailbox, folderID, body string) string {
	t.Helper()
	now := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	id, err := a.insertMessage(context.Background(), storedMessage{
		MailboxID:  mailbox.ID,
		FolderID:   folderID,
		MessageUID: newID("uid"),
		MessageID:  "<" + newID("search") + "@example.test>",
		Subject:    "search test",
		From:       "sender@example.test",
		To:         []string{mailbox.Address},
		SentAt:     now,
		ReceivedAt: now,
		Snippet:    body,
		BodyText:   body,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertFTSMessageMatch(t *testing.T, a *App, messageID, query string, want bool) {
	t.Helper()
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid=(SELECT rowid FROM messages WHERE id=?) AND messages_fts MATCH ?`, messageID, messageFTSLiteralQuery(query, webmailSearchColumns)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if got := count > 0; got != want {
		t.Fatalf("FTS match for message=%s query=%q is %v, want %v", messageID, query, got, want)
	}
}

func assertSearchResult(t *testing.T, client *testClient, path, messageID string) {
	t.Helper()
	var page mailMessagePage
	if code := client.do("GET", path, nil, &page); code != http.StatusOK {
		t.Fatalf("GET %s code=%d", path, code)
	}
	if !mailPageContains(page.Items, messageID) {
		t.Fatalf("GET %s did not return message %s: %+v", path, messageID, page.Items)
	}
}

func mailPageContains(items []MailMessage, messageID string) bool {
	for _, item := range items {
		if item.ID == messageID {
			return true
		}
	}
	return false
}
