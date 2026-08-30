package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTelegramOutboxLeaseAllowsOnlyOneConcurrentClaim(t *testing.T) {
	a := newTelegramTestApp(t)
	user, mailbox := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"tgn_lease", "lease:event", user.ID, mailbox.ID, "message", "rule", `{}`, now, now, now); err != nil {
		t.Fatal(err)
	}

	workers := []*App{{db: a.db, now: a.now, workerID: "worker-a"}, {db: a.db, now: a.now, workerID: "worker-b"}}
	start := make(chan struct{})
	results := make(chan error, len(workers))
	var wg sync.WaitGroup
	for _, worker := range workers {
		wg.Add(1)
		go func(worker *App) {
			defer wg.Done()
			<-start
			_, err := worker.claimTelegramNotification(context.Background(), "tgn_lease")
			results <- err
		}(worker)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("claim failed unexpectedly: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent claims succeeded %d times, want 1", successes)
	}
	var attempts int
	if err := a.db.QueryRow(`SELECT attempt_count FROM telegram_notification_outbox WHERE id='tgn_lease'`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempt_count=%d err=%v, want 1", attempts, err)
	}
}

func TestOutboxExpiredLeaseCanBeReclaimedAndRejectsStaleUpdate(t *testing.T) {
	a := newTelegramTestApp(t)
	user, mailbox := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"tgn_reclaim", "reclaim:event", user.ID, mailbox.ID, "message", "rule", `{}`, nowText, nowText, nowText); err != nil {
		t.Fatal(err)
	}
	first, err := a.claimTelegramNotification(context.Background(), "tgn_reclaim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE telegram_notification_outbox SET lease_until=? WHERE id=?`, now.Add(-time.Second).Format(time.RFC3339Nano), first.id); err != nil {
		t.Fatal(err)
	}
	secondWorker := &App{db: a.db, now: a.now, workerID: "worker-second"}
	second, err := secondWorker.claimTelegramNotification(context.Background(), first.id)
	if err != nil {
		t.Fatalf("reclaim expired lease: %v", err)
	}
	if first.leaseToken == second.leaseToken {
		t.Fatal("reclaimed lease reused its token")
	}
	a.failTelegramNotification(context.Background(), first.id, user.ID, first.attempt, first.leaseToken, errors.New("stale failure"))
	var token, lastError string
	var attempts int
	if err := a.db.QueryRow(`SELECT lease_token,last_error,attempt_count FROM telegram_notification_outbox WHERE id=?`, first.id).Scan(&token, &lastError, &attempts); err != nil {
		t.Fatal(err)
	}
	if token != second.leaseToken || lastError != "" || attempts != 2 {
		t.Fatalf("stale update changed current lease: token=%q error=%q attempts=%d", token, lastError, attempts)
	}
}

func TestStatusWebhookAndSendQueueLeaseOwnership(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	user, mailbox := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO status_webhook_outbox(id,event_key,event_type,mailbox_id,payload_json,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"whk_lease", "lease:webhook", "send.failed", mailbox.ID, `{}`, nowText, nowText, nowText); err != nil {
		t.Fatal(err)
	}
	webhook, err := a.claimStatusWebhook(context.Background(), "whk_lease")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE status_webhook_outbox SET lease_until=? WHERE id=?`, now.Add(-time.Second).Format(time.RFC3339Nano), webhook.id); err != nil {
		t.Fatal(err)
	}
	other := &App{db: a.db, now: a.now, workerID: "worker-other"}
	reclaimedWebhook, err := other.claimStatusWebhook(context.Background(), webhook.id)
	if err != nil {
		t.Fatalf("reclaim webhook: %v", err)
	}
	res, err := a.db.Exec(`UPDATE status_webhook_outbox SET delivered_at=? WHERE id=? AND lease_token=?`, nowText, webhook.id, webhook.leaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := res.RowsAffected(); changed != 0 {
		t.Fatal("stale webhook lease updated delivery state")
	}

	if _, err := a.db.Exec(`INSERT INTO send_queue(id,user_id,mailbox_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"snd_lease", user.ID, mailbox.ID, sendSourceWebmail, mailbox.Address, mailbox.Address, `[]`, "", sendQueueStatusQueued, nowText, nowText, nowText); err != nil {
		t.Fatal(err)
	}
	firstSend, err := a.claimSendQueueItem(context.Background(), "snd_lease")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,lease_until=? WHERE id=?`, sendQueueStatusQueued, now.Add(-time.Second).Format(time.RFC3339Nano), firstSend.ID); err != nil {
		t.Fatal(err)
	}
	secondSend, err := other.claimSendQueueItem(context.Background(), firstSend.ID)
	if err != nil {
		t.Fatalf("reclaim send queue: %v", err)
	}
	a.markSendQueueFailed(context.Background(), firstSend, errors.New("stale failure"))
	var status, token string
	if err := a.db.QueryRow(`SELECT status,lease_token FROM send_queue WHERE id=?`, firstSend.ID).Scan(&status, &token); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusSending || token != secondSend.LeaseToken || token == reclaimedWebhook.leaseToken {
		t.Fatalf("stale send update changed current lease: status=%q token=%q", status, token)
	}
}
