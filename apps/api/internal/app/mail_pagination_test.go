package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"testing"
	"time"
)

type mailMessagePage struct {
	Items      []MailMessage `json:"items"`
	NextCursor string        `json:"nextCursor"`
	TotalCount int           `json:"totalCount"`
}

func TestMailMessageListUsesStableCursorPagination(t *testing.T) {
	t.Run("folder list ignores inserts before the cursor", func(t *testing.T) {
		a, client, mailbox, folderID, _, ids, receivedAt := newMailPaginationTest(t)

		first := getMailMessagePage(t, client, "/api/mail/messages?folder=Pagination")
		assertMailMessageIDs(t, first.Items, ids[:mailMessagesPageSize])
		if first.TotalCount != len(ids) {
			t.Fatalf("totalCount=%d, want %d", first.TotalCount, len(ids))
		}
		assertStableMessageCursor(t, first.NextCursor)

		limited := getMailMessagePage(t, client, "/api/mail/messages?folder=Pagination&limit=20")
		assertMailMessageIDs(t, limited.Items, ids[:20])
		if limited.TotalCount != len(ids) {
			t.Fatalf("limited totalCount=%d, want %d", limited.TotalCount, len(ids))
		}
		limitedSecond := getMailMessagePage(t, client, "/api/mail/messages?folder=Pagination&limit=20&cursor="+url.QueryEscape(limited.NextCursor))
		assertMailMessageIDs(t, limitedSecond.Items, ids[20:])
		if limitedSecond.NextCursor != "" || limitedSecond.TotalCount != len(ids) {
			t.Fatalf("limited second nextCursor=%q totalCount=%d", limitedSecond.NextCursor, limitedSecond.TotalCount)
		}

		legacy := getMailMessagePage(t, client, "/api/mail/messages?folder=Pagination&cursor=30")
		assertMailMessageIDs(t, legacy.Items, ids[mailMessagesPageSize:])

		newerID := insertPaginationTestMessage(t, a, mailbox, folderID, receivedAt.Add(time.Hour), false)
		second := getMailMessagePage(t, client, "/api/mail/messages?folder=Pagination&cursor="+url.QueryEscape(first.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[mailMessagesPageSize:])
		if second.TotalCount != len(ids)+1 {
			t.Fatalf("second totalCount=%d, want %d", second.TotalCount, len(ids)+1)
		}
		for _, item := range second.Items {
			if item.ID == newerID {
				t.Fatalf("new message %s appeared after an older page cursor", newerID)
			}
		}

		if code := client.do("GET", "/api/mail/messages?folder=Pagination&cursor=not-a-cursor", nil, &map[string]any{}); code != http.StatusBadRequest {
			t.Fatalf("invalid cursor code=%d, want %d", code, http.StatusBadRequest)
		}
		if code := client.do("GET", "/api/mail/messages?folder=Pagination&limit=10", nil, &map[string]any{}); code != http.StatusBadRequest {
			t.Fatalf("invalid limit code=%d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("starred list ignores deletes before the cursor", func(t *testing.T) {
		a, client, _, _, _, ids, _ := newMailPaginationTest(t)

		first := getMailMessagePage(t, client, "/api/mail/starred")
		assertMailMessageIDs(t, first.Items, ids[:mailMessagesPageSize])
		if _, err := a.db.Exec(`DELETE FROM messages WHERE id=?`, first.Items[0].ID); err != nil {
			t.Fatal(err)
		}

		second := getMailMessagePage(t, client, "/api/mail/starred?cursor="+url.QueryEscape(first.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[mailMessagesPageSize:])
	})

	t.Run("label list keeps equal timestamps in deterministic order", func(t *testing.T) {
		_, client, mailbox, _, labelID, ids, _ := newMailPaginationTest(t)
		path := "/api/mail/messages?mailboxId=" + url.QueryEscape(mailbox.ID) + "&labelId=" + url.QueryEscape(labelID)

		first := getMailMessagePage(t, client, path)
		assertMailMessageIDs(t, first.Items, ids[:mailMessagesPageSize])
		second := getMailMessagePage(t, client, path+"&cursor="+url.QueryEscape(first.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[mailMessagesPageSize:])
	})
}

func newMailPaginationTest(t *testing.T) (*App, *testClient, *Mailbox, string, string, []string, time.Time) {
	t.Helper()
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	t.Cleanup(server.Close)
	client := &testClient{t: t, server: server}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	folderID, err := a.ensureFolder(context.Background(), mailbox.ID, "Pagination")
	if err != nil {
		t.Fatal(err)
	}
	label, err := a.ensureLabel(context.Background(), mailbox.ID, "Pagination", "#2563eb")
	if err != nil {
		t.Fatal(err)
	}
	receivedAt := time.Date(2026, time.January, 2, 3, 4, 5, 6000000, time.UTC)
	ids := make([]string, 0, 35)
	for i := 0; i < 35; i++ {
		id := insertPaginationTestMessage(t, a, mailbox, folderID, receivedAt, true)
		if _, err := a.db.Exec(`INSERT INTO message_labels(message_id,label_id,created_at) VALUES(?,?,?)`, id, label.ID, receivedAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return a, client, mailbox, folderID, label.ID, ids, receivedAt
}

func insertPaginationTestMessage(t *testing.T, a *App, mailbox *Mailbox, folderID string, receivedAt time.Time, starred bool) string {
	t.Helper()
	id, err := a.insertMessage(context.Background(), storedMessage{
		MailboxID:  mailbox.ID,
		FolderID:   folderID,
		MessageUID: newID("uid"),
		MessageID:  "<" + newID("pagination") + "@example.test>",
		Subject:    "pagination test",
		From:       "sender@example.test",
		To:         []string{mailbox.Address},
		SentAt:     receivedAt,
		ReceivedAt: receivedAt,
		Snippet:    "pagination test",
		BodyText:   "pagination test",
		IsStarred:  starred,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func getMailMessagePage(t *testing.T, client *testClient, path string) mailMessagePage {
	t.Helper()
	var page mailMessagePage
	if code := client.do("GET", path, nil, &page); code != http.StatusOK {
		t.Fatalf("GET %s code=%d", path, code)
	}
	return page
}

func assertMailMessageIDs(t *testing.T, items []MailMessage, want []string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("item count=%d, want %d", len(items), len(want))
	}
	got := make([]string, len(items))
	for i := range items {
		got[i] = items[i].ID
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item[%d]=%s, want %s\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

func assertStableMessageCursor(t *testing.T, cursor string) {
	t.Helper()
	if cursor == "" {
		t.Fatal("next cursor is empty")
	}
	if _, err := strconv.Atoi(cursor); err == nil {
		t.Fatalf("next cursor %q is still an offset", cursor)
	}
	if receivedAt, id, offset, err := parseMessageListCursor(cursor); err != nil || receivedAt == "" || id == "" || offset != 0 {
		t.Fatalf("invalid stable cursor %q: receivedAt=%q id=%q offset=%d err=%v", cursor, receivedAt, id, offset, err)
	}
}
