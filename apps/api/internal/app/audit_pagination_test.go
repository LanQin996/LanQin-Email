package app

import (
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"
)

type sendAuditPage struct {
	Items      []SendAuditEvent `json:"items"`
	NextCursor string           `json:"nextCursor"`
}

func TestAdminSendAuditUsesStableCursorPagination(t *testing.T) {
	t.Run("global list ignores inserts before the cursor", func(t *testing.T) {
		a, client, mailbox := newAdminPaginationTest(t)
		if _, err := a.db.Exec(`DELETE FROM send_audit_events`); err != nil {
			t.Fatal(err)
		}
		createdAt := time.Date(2026, time.April, 5, 6, 7, 8, 9000000, time.UTC)
		ids := insertAuditPaginationEvents(t, a, mailbox.ID, sendAuditQueued, createdAt, 55)

		first := getSendAuditPage(t, client, "/api/admin/send-audit")
		assertSendAuditIDs(t, first.Items, ids[:adminSendAuditPageSize])
		assertStableSendAuditCursor(t, first.NextCursor)

		legacy := getSendAuditPage(t, client, "/api/admin/send-audit?cursor=50")
		assertSendAuditIDs(t, legacy.Items, ids[adminSendAuditPageSize:])

		newer := insertAuditPaginationEvents(t, a, mailbox.ID, sendAuditQueued, createdAt.Add(time.Hour), 1)
		second := getSendAuditPage(t, client, "/api/admin/send-audit?cursor="+url.QueryEscape(first.NextCursor))
		assertSendAuditIDs(t, second.Items, ids[adminSendAuditPageSize:])
		for _, item := range second.Items {
			if item.ID == newer[0] {
				t.Fatalf("new audit event %s appeared after an older page cursor", newer[0])
			}
		}

		if code := client.do("GET", "/api/admin/send-audit?cursor=not-a-cursor", nil, &map[string]any{}); code != http.StatusBadRequest {
			t.Fatalf("invalid cursor code=%d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("filters ignore deletes before the cursor", func(t *testing.T) {
		a, client, mailbox := newAdminPaginationTest(t)
		if _, err := a.db.Exec(`DELETE FROM send_audit_events`); err != nil {
			t.Fatal(err)
		}
		createdAt := time.Date(2026, time.April, 5, 6, 7, 8, 9000000, time.UTC)
		ids := insertAuditPaginationEvents(t, a, mailbox.ID, sendAuditFailed, createdAt, 55)
		insertAuditPaginationEvents(t, a, mailbox.ID, sendAuditDelivered, createdAt, 2)
		path := "/api/admin/send-audit?mailboxId=" + url.QueryEscape(mailbox.ID) + "&event=" + url.QueryEscape(sendAuditFailed) + "&from=2026-04-05&to=2026-04-05"

		first := getSendAuditPage(t, client, path)
		assertSendAuditIDs(t, first.Items, ids[:adminSendAuditPageSize])
		if _, err := a.db.Exec(`DELETE FROM send_audit_events WHERE id=?`, first.Items[0].ID); err != nil {
			t.Fatal(err)
		}

		second := getSendAuditPage(t, client, path+"&cursor="+url.QueryEscape(first.NextCursor))
		assertSendAuditIDs(t, second.Items, ids[adminSendAuditPageSize:])
	})
}

func insertAuditPaginationEvents(t *testing.T, a *App, mailboxID, event string, createdAt time.Time, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	stamp := createdAt.Format(time.RFC3339Nano)
	for i := 0; i < count; i++ {
		id := newID("audit")
		if _, err := a.db.Exec(`INSERT INTO send_audit_events(id,queue_id,user_id,mailbox_id,sent_message_id,source,event,status,mail_from,header_from,recipients_json,error,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "queue-pagination", "user-pagination", mailboxID, "", sendSourceWebmail, event, auditStatusForEvent(event), "sender@example.test", "sender@example.test", `["recipient@example.test"]`, "", stamp); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

func auditStatusForEvent(event string) string {
	switch event {
	case sendAuditDelivered:
		return sendQueueStatusDelivered
	case sendAuditFailed:
		return sendQueueStatusFailed
	default:
		return sendQueueStatusQueued
	}
}

func getSendAuditPage(t *testing.T, client *testClient, path string) sendAuditPage {
	t.Helper()
	var page sendAuditPage
	if code := client.do("GET", path, nil, &page); code != http.StatusOK {
		t.Fatalf("GET %s code=%d", path, code)
	}
	return page
}

func assertSendAuditIDs(t *testing.T, items []SendAuditEvent, want []string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("audit item count=%d, want %d", len(items), len(want))
	}
	for i := range want {
		if items[i].ID != want[i] {
			t.Fatalf("audit item[%d]=%s, want %s", i, items[i].ID, want[i])
		}
	}
}

func assertStableSendAuditCursor(t *testing.T, cursor string) {
	t.Helper()
	createdAt, id, offset, err := parseSendQueueCursor(cursor)
	if err != nil || createdAt == "" || id == "" || offset != 0 {
		t.Fatalf("invalid stable audit cursor %q: createdAt=%q id=%q offset=%d err=%v", cursor, createdAt, id, offset, err)
	}
}
