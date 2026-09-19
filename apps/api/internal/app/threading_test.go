package app

import (
	"context"
	"database/sql"
	"testing"
)

func TestMigrateMessageThreadsWithUnregisteredMail(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	// Reproduce the pre-threading schema, including valid unregistered mail.
	if _, err := db.ExecContext(ctx, `CREATE TABLE messages (
		id TEXT PRIMARY KEY, mailbox_id TEXT, message_id TEXT NOT NULL, received_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, messageID string
		mailboxID     any
	}{
		{"unregistered", "<Orphan@example.test>", nil},
		{"missing-message-id", "", nil},
		{"registered", "<Registered@example.test>", "mailbox-1"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (id,mailbox_id,message_id,received_at) VALUES (?,?,?,?)`,
			item.id, item.mailboxID, item.messageID, "2026-09-19T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{db: db}
	for run := 0; run < 2; run++ {
		if err := a.migrateMessageThreads(ctx); err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
		for id, want := range map[string]string{
			"unregistered":       "orphan@example.test",
			"missing-message-id": "missing-message-id",
			"registered":         "registered@example.test",
		} {
			var got string
			if err := db.QueryRowContext(ctx, `SELECT thread_id FROM messages WHERE id=?`, id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s: thread_id=%q, want %q", id, got, want)
			}
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id IS NULL`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("unregistered mail ownership changed: got %d NULL mailboxes", count)
		}
	}
}

func TestNormalizeMessageID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" <ABC@example.test> ", "abc@example.test"},
		{"bad", ""},
		{"<bad\n@example.test>", ""},
	} {
		if got := normalizeMessageID(tc.in); got != tc.want {
			t.Errorf("normalizeMessageID(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMessageIDList(t *testing.T) {
	got := messageIDList("<Root@example.test>  <Parent@example.test> <root@example.test>")
	if len(got) != 2 || got[0] != "root@example.test" || got[1] != "parent@example.test" {
		t.Fatalf("unexpected ids: %#v", got)
	}
}
