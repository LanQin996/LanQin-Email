package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPerformanceIndexesCoverHotMailboxQueries(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)

	tests := []struct {
		name      string
		indexName string
		query     string
		args      []any
	}{
		{
			name:      "message attachments",
			indexName: "idx_attachments_message_filename",
			query:     `SELECT filename FROM attachments WHERE message_id=? ORDER BY filename`,
			args:      []any{"msg_test"},
		},
		{
			name:      "folder unread counts",
			indexName: "idx_messages_folder_read",
			query:     `SELECT id FROM messages WHERE folder_id=? AND is_read=?`,
			args:      []any{"fld_test", 0},
		},
		{
			name:      "folder message list",
			indexName: "idx_messages_mailbox_folder_received_id",
			query:     `SELECT id FROM messages WHERE mailbox_id=? AND folder_id=? AND (received_at,id)<(?,?) ORDER BY received_at DESC,id DESC LIMIT 31`,
			args:      []any{"mb_test", "fld_test", "2026-01-01T00:00:00Z", "mail_test"},
		},
		{
			name:      "starred message list",
			indexName: "idx_messages_mailbox_starred_received_id",
			query:     `SELECT id FROM messages WHERE mailbox_id=? AND is_starred=1 AND (received_at,id)<(?,?) ORDER BY received_at DESC,id DESC LIMIT 31`,
			args:      []any{"mb_test", "2026-01-01T00:00:00Z", "mail_test"},
		},
		{
			name:      "maildir message id lookup",
			indexName: "idx_messages_mailbox_message_id",
			query:     `SELECT id FROM messages WHERE mailbox_id=? AND message_id=? AND message_id<>''`,
			args:      []any{"mb_test", "<message@example.test>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := a.db.Query(`EXPLAIN QUERY PLAN `+tt.query, tt.args...)
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
			if !strings.Contains(strings.Join(plans, "\n"), tt.indexName) {
				t.Fatalf("query plan does not use %s: %v", tt.indexName, plans)
			}
		})
	}
}

func TestAPITokenLastUsedWriteIsThrottled(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	token := createTestAPIToken(t, admin, "last-used-throttle")
	var tokenID string
	if err := a.db.QueryRow(`SELECT id FROM api_tokens WHERE token_hash=?`, hashToken(token)).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}

	current := time.Now().UTC().Truncate(time.Millisecond)
	a.now = func() time.Time { return current }
	openAdmin := &testClient{t: t, server: ts, bearer: token}
	if code := openAdmin.do("GET", "/api/open/domains", nil, &map[string]any{}); code != http.StatusOK {
		t.Fatalf("first bearer request code=%d", code)
	}
	first := apiTokenLastUsedAt(t, a, tokenID)

	current = current.Add(30 * time.Second)
	if code := openAdmin.do("GET", "/api/open/domains", nil, &map[string]any{}); code != http.StatusOK {
		t.Fatalf("second bearer request code=%d", code)
	}
	if got := apiTokenLastUsedAt(t, a, tokenID); got != first {
		t.Fatalf("last_used_at changed inside throttle window: first=%q got=%q", first, got)
	}

	current = current.Add(31 * time.Second)
	if code := openAdmin.do("GET", "/api/open/domains", nil, &map[string]any{}); code != http.StatusOK {
		t.Fatalf("third bearer request code=%d", code)
	}
	if got, want := apiTokenLastUsedAt(t, a, tokenID), current.Format(time.RFC3339Nano); got != want {
		t.Fatalf("last_used_at was not refreshed after throttle window: got=%q want=%q", got, want)
	}
}

func apiTokenLastUsedAt(t *testing.T, a *App, tokenID string) string {
	t.Helper()
	var value string
	if err := a.db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, tokenID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
