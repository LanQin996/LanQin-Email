package app

import (
	"net/http"
	"testing"
	"time"
)

func TestAdminDeliveryQueueListAndMutationsRedactAndGuardState(t *testing.T) {
	a := newTelegramTestApp(t)
	server, admin := loginTelegramTestAdmin(t, a)
	defer server.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var adminID string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, a.cfg.AdminEmail).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,attempt_count,next_attempt_at,last_error,created_at,updated_at,lease_owner,lease_token,lease_until)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "queue-admin-test", "event-admin-test", adminID, "mb", "msg", "rule", `{"text":"secret body https://private.example.test"}`, 10, now, "token 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijk", now, now, "", "", nil); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Items []AdminDeliveryQueueItem `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/delivery-queue?queueType=telegram&status=failed", nil, &listed); code != http.StatusOK || len(listed.Items) != 1 {
		t.Fatalf("list code=%d items=%+v", code, listed.Items)
	}
	if listed.Items[0].LastError == "" || listed.Items[0].LastError == "token 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijk" || listed.Items[0].LastError == "secret body https://private.example.test" {
		t.Fatalf("sensitive queue error was not redacted: %q", listed.Items[0].LastError)
	}
	if code := admin.do("POST", "/api/admin/delivery-queue/telegram/queue-admin-test/retry", nil, nil); code != http.StatusOK {
		t.Fatalf("retry code=%d", code)
	}
	var attempts int
	if err := a.db.QueryRow(`SELECT attempt_count FROM telegram_notification_outbox WHERE id=?`, "queue-admin-test").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("retry attempts=%d err=%v", attempts, err)
	}
	if code := admin.do("POST", "/api/admin/delivery-queue/telegram/queue-admin-test/retry", nil, nil); code != http.StatusConflict {
		t.Fatalf("retry pending code=%d", code)
	}
	if code := admin.do("DELETE", "/api/admin/delivery-queue/telegram/queue-admin-test", nil, nil); code != http.StatusOK {
		t.Fatalf("cancel code=%d", code)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE id=?`, "queue-admin-test").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("cancel did not remove item count=%d err=%v", attempts, err)
	}
}

func TestAdminDeliveryQueueRequiresAdmin(t *testing.T) {
	a := newTelegramTestApp(t)
	ts, admin := loginTelegramTestAdmin(t, a)
	defer ts.Close()
	_ = admin
	unauth := (&testClient{t: t, server: ts}).do("GET", "/api/admin/delivery-queue", nil, nil)
	if unauth != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", unauth)
	}
}
