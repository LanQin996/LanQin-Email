package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"
)

func TestAdminMessageListUsesStableCursorPagination(t *testing.T) {
	t.Run("all mailboxes ignore inserts before the cursor", func(t *testing.T) {
		a, client, mailbox := newAdminPaginationTest(t)
		if _, err := a.db.Exec(`DELETE FROM messages`); err != nil {
			t.Fatal(err)
		}
		folderID, err := a.ensureFolder(context.Background(), mailbox.ID, "AdminPagination")
		if err != nil {
			t.Fatal(err)
		}
		receivedAt := time.Date(2026, time.March, 4, 5, 6, 7, 8000000, time.UTC)
		ids := insertAdminPaginationMessages(t, a, mailbox.ID, folderID, receivedAt, 55, "admin-global-pagination")

		first := getAdminMessagePage(t, client, "/api/admin/messages?mailboxId=all")
		assertMailMessageIDs(t, first.Items, ids[:adminMessagesPageSize])
		assertStableMessageCursor(t, first.NextCursor)

		legacy := getAdminMessagePage(t, client, "/api/admin/messages?mailboxId=all&cursor=50")
		assertMailMessageIDs(t, legacy.Items, ids[adminMessagesPageSize:])

		newer := insertAdminPaginationMessages(t, a, mailbox.ID, folderID, receivedAt.Add(time.Hour), 1, "admin-global-pagination")
		second := getAdminMessagePage(t, client, "/api/admin/messages?mailboxId=all&cursor="+url.QueryEscape(first.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[adminMessagesPageSize:])
		if mailPageContains(second.Items, newer[0]) {
			t.Fatalf("new message %s appeared after an older page cursor", newer[0])
		}

		if code := client.do("GET", "/api/admin/messages?cursor=not-a-cursor", nil, &map[string]any{}); code != http.StatusBadRequest {
			t.Fatalf("invalid cursor code=%d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("mailbox folder search ignores deletes before the cursor", func(t *testing.T) {
		a, client, mailbox := newAdminPaginationTest(t)
		folderName := "AdminPagination"
		folderID, err := a.ensureFolder(context.Background(), mailbox.ID, folderName)
		if err != nil {
			t.Fatal(err)
		}
		receivedAt := time.Date(2026, time.March, 4, 5, 6, 7, 8000000, time.UTC)
		marker := "admin-filtered-pagination"
		ids := insertAdminPaginationMessages(t, a, mailbox.ID, folderID, receivedAt, 55, marker)
		path := "/api/admin/messages?mailboxId=" + url.QueryEscape(mailbox.ID) + "&folder=" + url.QueryEscape(folderName) + "&q=" + url.QueryEscape(marker)

		first := getAdminMessagePage(t, client, path)
		assertMailMessageIDs(t, first.Items, ids[:adminMessagesPageSize])
		if _, err := a.db.Exec(`DELETE FROM messages WHERE id=?`, first.Items[0].ID); err != nil {
			t.Fatal(err)
		}

		second := getAdminMessagePage(t, client, path+"&cursor="+url.QueryEscape(first.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[adminMessagesPageSize:])
	})

	t.Run("unregistered filters share stable ordering", func(t *testing.T) {
		a, client, _ := newAdminPaginationTest(t)
		receivedAt := time.Date(2026, time.March, 4, 5, 6, 7, 8000000, time.UTC)
		ids := insertAdminPaginationMessages(t, a, "", "", receivedAt, 55, "admin-unregistered-pagination")

		byMailbox := getAdminMessagePage(t, client, "/api/admin/messages?mailboxId=unregistered")
		assertMailMessageIDs(t, byMailbox.Items, ids[:adminMessagesPageSize])
		byFolder := getAdminMessagePage(t, client, "/api/admin/messages?folder=Unregistered")
		assertMailMessageIDs(t, byFolder.Items, ids[:adminMessagesPageSize])

		second := getAdminMessagePage(t, client, "/api/admin/messages?mailboxId=unregistered&cursor="+url.QueryEscape(byMailbox.NextCursor))
		assertMailMessageIDs(t, second.Items, ids[adminMessagesPageSize:])
	})
}

func newAdminPaginationTest(t *testing.T) (*App, *testClient, *Mailbox) {
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
	return a, client, mailbox
}

func insertAdminPaginationMessages(t *testing.T, a *App, mailboxID, folderID string, receivedAt time.Time, count int, marker string) []string {
	t.Helper()
	var mailboxValue, folderValue any
	recipient := "unregistered@example.test"
	if mailboxID != "" {
		mailboxValue = mailboxID
		recipient = "admin@lanqin.local"
	}
	if folderID != "" {
		folderValue = folderID
	}
	ids := make([]string, 0, count)
	stamp := receivedAt.Format(time.RFC3339Nano)
	for i := 0; i < count; i++ {
		id := newID("mail")
		if _, err := a.db.Exec(`INSERT INTO messages(id,mailbox_id,folder_id,recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, mailboxValue, folderValue, recipient, newID("uid"), "<"+newID("admin-pagination")+"@example.test>", marker, "sender@example.test", "Sender", "[]", "[]", "[]", stamp, stamp, marker, marker, "", 0, 0, 0, len(marker), stamp, stamp); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

func getAdminMessagePage(t *testing.T, client *testClient, path string) mailMessagePage {
	t.Helper()
	var page mailMessagePage
	if code := client.do("GET", path, nil, &page); code != http.StatusOK {
		t.Fatalf("GET %s code=%d", path, code)
	}
	return page
}
