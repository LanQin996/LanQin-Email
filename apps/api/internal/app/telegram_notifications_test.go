package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTelegramBotToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijk"

func newTelegramTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	a := newTestAppWithConfig(t, Config{
		Addr:                  ":0",
		DBPath:                filepath.Join(dir, "lanqin.db"),
		DataDir:               filepath.Join(dir, "data"),
		CookieName:            "lanqin_test",
		SessionTTLHours:       24,
		AdminEmail:            "admin@lanqin.local",
		AdminPassword:         "ChangeMe123!",
		PublicHostname:        "mail.example.test",
		PublicBaseURL:         "https://mail.example.test",
		AllowInsecureHTTP:     true,
		NotificationSecretKey: "notification-test-secret",
	})
	stopTestWorkers(a)
	return a
}

func loginTelegramTestAdmin(t *testing.T, a *App) (*httptest.Server, *testClient) {
	t.Helper()
	server := httptest.NewServer(a.Router())
	t.Cleanup(server.Close)
	client := &testClient{t: t, server: server}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	return server, client
}

func TestTelegramSettingsEncryptsTokenAndSupportsTestDelivery(t *testing.T) {
	a := newTelegramTestApp(t)
	var mu sync.Mutex
	requests := []map[string]any{}
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"lanqin_test_bot"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"from":{"username":"lanqin_test_bot"}}}`)
	}))
	defer telegram.Close()
	a.telegramAPIBaseURL = telegram.URL
	_, client := loginTelegramTestAdmin(t, a)

	var initial TelegramSettings
	if code := client.do("GET", "/api/me/telegram", nil, &initial); code != http.StatusOK || !initial.Available || initial.Configured {
		t.Fatalf("initial code=%d settings=%+v", code, initial)
	}
	var saved TelegramSettings
	if code := client.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "-1001234567890", "enabled": true}, &saved); code != http.StatusOK {
		t.Fatalf("save code=%d settings=%+v", code, saved)
	}
	if !saved.Configured || !saved.TokenSet || saved.BotUsername != "lanqin_test_bot" || saved.ChatID != "-1001234567890" {
		t.Fatalf("saved settings=%+v", saved)
	}
	var ciphertext string
	if err := a.db.QueryRow(`SELECT bot_token_ciphertext FROM telegram_notification_settings WHERE user_id=(SELECT id FROM users WHERE email=?)`, a.cfg.AdminEmail).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "" || ciphertext == testTelegramBotToken || strings.Contains(ciphertext, testTelegramBotToken) {
		t.Fatalf("bot token was not encrypted: %q", ciphertext)
	}
	plain, err := a.decryptNotificationSecret(ciphertext)
	if err != nil || plain != testTelegramBotToken {
		t.Fatalf("decrypt token=%q err=%v", plain, err)
	}

	if code := client.do("POST", "/api/me/telegram", map[string]any{"botToken": "", "chatId": "@lanqin_channel", "enabled": true}, &saved); code != http.StatusOK || saved.ChatID != "@lanqin_channel" {
		t.Fatalf("update code=%d settings=%+v", code, saved)
	}
	if code := client.do("POST", "/api/me/telegram/test", nil, nil); code != http.StatusOK {
		t.Fatalf("test delivery code=%d", code)
	}
	var afterTest TelegramSettings
	if code := client.do("GET", "/api/me/telegram", nil, &afterTest); code != http.StatusOK || afterTest.LastDeliveredAt == nil {
		t.Fatalf("settings after test code=%d settings=%+v", code, afterTest)
	}
	if code := client.do("POST", "/api/me/telegram/test", nil, nil); code != http.StatusTooManyRequests {
		t.Fatalf("second test delivery code=%d, want 429", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("Telegram request count=%d, want 3", len(requests))
	}
	if requests[2]["chat_id"] != "@lanqin_channel" || !strings.Contains(requests[2]["text"].(string), "测试成功") {
		t.Fatalf("test request=%v", requests[2])
	}
}

func TestTelegramRuleOutboxIsIdempotentAndRetriesRateLimit(t *testing.T) {
	a := newTelegramTestApp(t)
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	requestCount := 0
	var deliveredBody map[string]any
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"lanqin_test_bot"}}`)
			return
		}
		requestCount++
		_ = json.NewDecoder(r.Body).Decode(&deliveredBody)
		if requestCount == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":2}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer telegram.Close()
	a.telegramAPIBaseURL = telegram.URL
	_, client := loginTelegramTestAdmin(t, a)
	if code := client.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "123456789", "enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("save Telegram code=%d", code)
	}
	var rule MailRule
	if code := client.do("POST", "/api/me/rules", map[string]any{
		"name": "Telegram all mail", "mailboxId": "", "matchMode": "all",
		"conditions": []map[string]string{{"field": "all", "operator": "equals", "value": "true"}},
		"actions":    []map[string]string{{"type": "telegram"}}, "enabled": true,
	}, &rule); code != http.StatusCreated {
		t.Fatalf("create rule code=%d rule=%+v", code, rule)
	}
	if code := client.do("POST", "/api/me/rules", map[string]any{
		"name": "Telegram non-matching", "mailboxId": "", "matchMode": "all",
		"conditions": []map[string]string{{"field": "subject", "operator": "equals", "value": "does not match"}},
		"actions":    []map[string]string{{"type": "telegram"}}, "enabled": true,
	}, nil); code != http.StatusCreated {
		t.Fatalf("create non-matching rule code=%d", code)
	}
	user, mailbox := defaultAdminUserAndMailbox(t, a)
	inboxID, err := a.ensureFolder(context.Background(), mailbox.ID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := a.insertMessage(context.Background(), storedMessage{
		MailboxID: mailbox.ID, FolderID: inboxID, MessageUID: newID("uid"), MessageID: "<telegram-test@example.test>",
		Subject: "Build <passed>", From: "sender@example.test", FromName: "Sender & Team", To: []string{mailbox.Address},
		SentAt: now, ReceivedAt: now, Snippet: "Sensitive preview text", BodyText: "Sensitive preview text",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.applyInboundControls(context.Background(), messageID, mailbox.ID, "sender@example.test", "Build <passed>")
	a.applyInboundControls(context.Background(), messageID, mailbox.ID, "sender@example.test", "Build <passed>")
	if err := a.applyRuleActions(context.Background(), mailbox.ID, messageID, newID("rule"), []MailRuleAction{{Type: "telegram"}}, false); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE user_id=?`, user.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v, want 1", queued, err)
	}

	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var nextAttempt string
	if err := a.db.QueryRow(`SELECT attempt_count,next_attempt_at FROM telegram_notification_outbox`).Scan(&attempts, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !parseTime(nextAttempt).Equal(now.Add(2*time.Second)) {
		t.Fatalf("attempts=%d next=%s", attempts, nextAttempt)
	}
	now = now.Add(2 * time.Second)
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deliveredAt string
	if err := a.db.QueryRow(`SELECT delivered_at FROM telegram_notification_outbox`).Scan(&deliveredAt); err != nil || deliveredAt == "" {
		t.Fatalf("deliveredAt=%q err=%v", deliveredAt, err)
	}
	text, _ := deliveredBody["text"].(string)
	if !strings.Contains(text, mailbox.Address) || !strings.Contains(text, "Sender & Team") || !strings.Contains(text, "Build <passed>") || !strings.Contains(text, "Sensitive preview text") {
		t.Fatalf("Telegram text=%q", text)
	}
	markup, ok := deliveredBody["reply_markup"].(map[string]any)
	if !ok || markup["inline_keyboard"] == nil {
		t.Fatalf("reply markup=%v", deliveredBody["reply_markup"])
	}
}

func TestTelegramSettingsUnavailableWithoutEncryptionKey(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	_, client := loginTelegramTestAdmin(t, a)
	var settings TelegramSettings
	if code := client.do("GET", "/api/me/telegram", nil, &settings); code != http.StatusOK || settings.Available {
		t.Fatalf("get code=%d settings=%+v", code, settings)
	}
	if code := client.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "123", "enabled": true}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("save code=%d, want 503", code)
	}
}

func TestTelegramSettingsAreUserScopedAndCleanupOnlyPendingOutbox(t *testing.T) {
	a := newTelegramTestApp(t)
	upstreamAvailable := true
	upstreamRequests := 0
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		w.Header().Set("Content-Type", "application/json")
		if !upstreamAvailable {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"ok":false,"description":"temporarily unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"lanqin_test_bot"}}`)
	}))
	defer telegram.Close()
	a.telegramAPIBaseURL = telegram.URL

	server, admin := loginTelegramTestAdmin(t, a)
	anonymous := &testClient{t: t, server: server}
	if code := anonymous.do("GET", "/api/me/telegram", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous get code=%d, want 401", code)
	}

	otherMailbox := createTestMailbox(t, admin, mustDefaultDomainID(t, a), "telegram-other", "Telegram Other", "Password123!", nil)
	other := &testClient{t: t, server: server}
	if code := other.do("POST", "/api/auth/login", map[string]string{"email": otherMailbox.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("other login code=%d", code)
	}
	if code := admin.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "111", "enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("save admin Telegram code=%d", code)
	}
	var otherInitial TelegramSettings
	if code := other.do("GET", "/api/me/telegram", nil, &otherInitial); code != http.StatusOK || otherInitial.Configured {
		t.Fatalf("other initial code=%d settings=%+v", code, otherInitial)
	}
	if code := other.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "222", "enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("save other Telegram code=%d", code)
	}
	var adminSettings TelegramSettings
	if code := admin.do("GET", "/api/me/telegram", nil, &adminSettings); code != http.StatusOK || adminSettings.ChatID != "111" {
		t.Fatalf("admin settings leaked or changed code=%d settings=%+v", code, adminSettings)
	}

	adminUser, _ := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC().Format(time.RFC3339Nano)
	insertOutbox := func(id, eventKey string, delivered bool) {
		t.Helper()
		var deliveredAt any
		if delivered {
			deliveredAt = now
		}
		_, err := a.db.Exec(`INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,next_attempt_at,created_at,updated_at,delivered_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, eventKey, adminUser.ID, "mailbox", "message", "rule", `{}`, now, now, now, deliveredAt)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertOutbox("tgn_delivered", "telegram:delivered", true)
	insertOutbox("tgn_pending_disable", "telegram:pending-disable", false)
	requestsBeforeDisable := upstreamRequests
	upstreamAvailable = false
	if code := admin.do("POST", "/api/me/telegram", map[string]any{"botToken": "", "chatId": "111", "enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("disable Telegram code=%d", code)
	}
	if upstreamRequests != requestsBeforeDisable {
		t.Fatalf("disable contacted Telegram: requests before=%d after=%d", requestsBeforeDisable, upstreamRequests)
	}
	var pending, delivered int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NULL`, adminUser.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending after disable=%d err=%v", pending, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NOT NULL`, adminUser.ID).Scan(&delivered); err != nil || delivered != 1 {
		t.Fatalf("delivered history after disable=%d err=%v", delivered, err)
	}

	upstreamAvailable = true
	if code := admin.do("POST", "/api/me/telegram", map[string]any{"botToken": "", "chatId": "111", "enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("re-enable Telegram code=%d", code)
	}
	insertOutbox("tgn_pending_delete", "telegram:pending-delete", false)
	if code := admin.do("DELETE", "/api/me/telegram", nil, nil); code != http.StatusOK {
		t.Fatalf("delete Telegram code=%d", code)
	}
	var settingsCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_settings WHERE user_id=?`, adminUser.ID).Scan(&settingsCount); err != nil || settingsCount != 0 {
		t.Fatalf("settings after delete=%d err=%v", settingsCount, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NULL`, adminUser.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending after delete=%d err=%v", pending, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NOT NULL`, adminUser.ID).Scan(&delivered); err != nil || delivered != 1 {
		t.Fatalf("delivered history after delete=%d err=%v", delivered, err)
	}
	var otherSettings TelegramSettings
	if code := other.do("GET", "/api/me/telegram", nil, &otherSettings); code != http.StatusOK || !otherSettings.Configured || otherSettings.ChatID != "222" {
		t.Fatalf("other settings after admin delete code=%d settings=%+v", code, otherSettings)
	}
}

func TestTelegramWorkerHandlesTransientAndPermanentFailuresWithoutLeakingContent(t *testing.T) {
	a := newTelegramTestApp(t)
	now := time.Date(2026, time.August, 29, 14, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	responses := 0
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses++
		w.Header().Set("Content-Type", "application/json")
		switch responses {
		case 1:
			_, _ = io.WriteString(w, `{"ok":false,"description":"temporary rejection"}`)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"ok":false,"description":"upstream failure"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"ok":false,"description":"invalid credentials"}`)
		}
	}))
	defer telegram.Close()
	a.telegramAPIBaseURL = telegram.URL

	user, mailbox := defaultAdminUserAndMailbox(t, a)
	ciphertext, err := a.encryptNotificationSecret(testTelegramBotToken)
	if err != nil {
		t.Fatal(err)
	}
	nowText := now.Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO telegram_notification_settings(user_id,bot_token_ciphertext,bot_username,chat_id,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)`, user.ID, ciphertext, "lanqin_test_bot", "123456789", 1, nowText, nowText); err != nil {
		t.Fatal(err)
	}
	payload := telegramNotificationPayload{Text: "private subject and private preview", URL: "https://mail.example.test/?mailboxId=mbx&messageId=mail"}
	if _, err := a.db.Exec(`INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "tgn_failures", "telegram:failures", user.ID, mailbox.ID, "mail", "rule", jsonEncode(payload), nowText, nowText, nowText); err != nil {
		t.Fatal(err)
	}

	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var nextAttempt, lastError string
	if err := a.db.QueryRow(`SELECT attempt_count,next_attempt_at,last_error FROM telegram_notification_outbox WHERE id=?`, "tgn_failures").Scan(&attempts, &nextAttempt, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !parseTime(nextAttempt).Equal(now.Add(sendRetryDelay(1))) || !strings.Contains(lastError, "temporary rejection") {
		t.Fatalf("first failure attempts=%d next=%s error=%q", attempts, nextAttempt, lastError)
	}

	if _, err := a.db.Exec(`UPDATE telegram_notification_outbox SET next_attempt_at=? WHERE id=?`, nowText, "tgn_failures"); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT attempt_count,last_error FROM telegram_notification_outbox WHERE id=?`, "tgn_failures").Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || !strings.Contains(lastError, "upstream failure") {
		t.Fatalf("second failure attempts=%d error=%q", attempts, lastError)
	}

	if _, err := a.db.Exec(`UPDATE telegram_notification_outbox SET next_attempt_at=? WHERE id=?`, nowText, "tgn_failures"); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT attempt_count,last_error FROM telegram_notification_outbox WHERE id=?`, "tgn_failures").Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != telegramNotificationMaxAttempts || !strings.Contains(lastError, "invalid credentials") {
		t.Fatalf("permanent failure attempts=%d error=%q", attempts, lastError)
	}
	if strings.Contains(lastError, payload.Text) || strings.Contains(lastError, testTelegramBotToken) {
		t.Fatalf("Telegram error leaked message content or token: %q", lastError)
	}
}

func TestTelegramRequestTimeoutAndNotificationTextLimit(t *testing.T) {
	a := newTelegramTestApp(t)
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer telegram.Close()
	a.telegramAPIBaseURL = telegram.URL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.callTelegramAPI(ctx, testTelegramBotToken, "sendMessage", map[string]string{"text": "private body"}); err == nil || err.Error() != "Telegram request failed" || strings.Contains(err.Error(), testTelegramBotToken) || strings.Contains(err.Error(), "private body") {
		t.Fatalf("timeout error=%v", err)
	}

	text := telegramNotificationText("admin@example.test", "Sender <sender@example.test>", "Subject <&>", strings.Repeat("摘要 <&> ", 1000))
	if len([]rune(text)) > telegramNotificationTextMaxRunes || !strings.HasSuffix(text, "...") {
		t.Fatalf("notification text length=%d suffix=%q", len([]rune(text)), text[len(text)-3:])
	}
	if !strings.Contains(text, "Subject <&>") || !strings.Contains(text, "摘要 <&>") {
		t.Fatalf("notification text escaped or lost plain text content: %q", text[:200])
	}
}
