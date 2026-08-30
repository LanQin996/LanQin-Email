package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultTelegramAPIBaseURL        = "https://api.telegram.org"
	telegramNotificationMaxAttempts  = 10
	telegramNotificationTextMaxRunes = 3500
	telegramNotificationTestCooldown = 10 * time.Second
)

var (
	telegramBotTokenPattern = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,100}$`)
	telegramChatIDPattern   = regexp.MustCompile(`^(?:-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
)

type TelegramSettings struct {
	Available       bool       `json:"available"`
	Configured      bool       `json:"configured"`
	TokenSet        bool       `json:"tokenSet"`
	BotUsername     string     `json:"botUsername"`
	ChatID          string     `json:"chatId"`
	Enabled         bool       `json:"enabled"`
	LastDeliveredAt *time.Time `json:"lastDeliveredAt,omitempty"`
	LastError       string     `json:"lastError,omitempty"`
}

type telegramSettingsRecord struct {
	UserID             string
	BotTokenCiphertext string
	BotUsername        string
	ChatID             string
	Enabled            bool
	LastTestAt         time.Time
	LastDeliveredAt    time.Time
	LastError          string
}

type telegramNotificationPayload struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

type telegramAPIResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      struct {
		Username string `json:"username"`
		From     struct {
			Username string `json:"username"`
		} `json:"from"`
	} `json:"result"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type telegramDeliveryError struct {
	message    string
	retryAfter time.Duration
	permanent  bool
}

func (e *telegramDeliveryError) Error() string { return e.message }

func (a *App) handleGetTelegramSettings(w http.ResponseWriter, r *http.Request) {
	item, err := a.telegramSettingsSnapshot(r.Context(), currentUser(r).ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load Telegram settings")
		return
	}
	respondJSON(w, http.StatusOK, item)
}

func (a *App) handleSaveTelegramSettings(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(a.cfg.NotificationSecretKey) == "" {
		respondError(w, http.StatusServiceUnavailable, "Telegram notifications are not available")
		return
	}
	user := currentUser(r)
	var req struct {
		BotToken string `json:"botToken"`
		ChatID   string `json:"chatId"`
		Enabled  bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	chatID := strings.TrimSpace(req.ChatID)
	if !telegramChatIDPattern.MatchString(chatID) {
		badRequest(w, errors.New("invalid Telegram chat ID"))
		return
	}
	existing, err := a.telegramSettingsRecord(r.Context(), user.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusInternalServerError, "failed to load Telegram settings")
		return
	}
	token := strings.TrimSpace(req.BotToken)
	ciphertext := existing.BotTokenCiphertext
	if token == "" {
		if ciphertext == "" {
			badRequest(w, errors.New("Telegram bot token is required"))
			return
		}
		token, err = a.decryptNotificationSecret(ciphertext)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to read Telegram credentials")
			return
		}
	} else {
		if !telegramBotTokenPattern.MatchString(token) {
			badRequest(w, errors.New("invalid Telegram bot token"))
			return
		}
		ciphertext, err = a.encryptNotificationSecret(token)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to protect Telegram credentials")
			return
		}
	}
	botUsername := existing.BotUsername
	if req.Enabled || strings.TrimSpace(req.BotToken) != "" || existing.BotTokenCiphertext == "" {
		botUsername, err = a.lookupTelegramBot(r.Context(), token)
		if err != nil {
			respondError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save Telegram settings")
		return
	}
	defer tx.Rollback()
	query := upsertSQL(a.cfg.DBDriver, `INSERT INTO telegram_notification_settings(user_id,bot_token_ciphertext,bot_username,chat_id,enabled,last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, `(user_id)`,
		`bot_token_ciphertext=excluded.bot_token_ciphertext,bot_username=excluded.bot_username,chat_id=excluded.chat_id,enabled=excluded.enabled,last_error='',updated_at=excluded.updated_at`,
		`bot_token_ciphertext=VALUES(bot_token_ciphertext),bot_username=VALUES(bot_username),chat_id=VALUES(chat_id),enabled=VALUES(enabled),last_error='',updated_at=VALUES(updated_at)`)
	if _, err := tx.ExecContext(r.Context(), query, user.ID, ciphertext, botUsername, chatID, boolInt(req.Enabled), "", now, now); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save Telegram settings")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NULL`, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset pending Telegram notifications")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save Telegram settings")
		return
	}
	item, err := a.telegramSettingsSnapshot(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load Telegram settings")
		return
	}
	respondJSON(w, http.StatusOK, item)
}

func (a *App) handleDeleteTelegramSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete Telegram settings")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM telegram_notification_outbox WHERE user_id=? AND delivered_at IS NULL`, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete Telegram settings")
		return
	}
	result, err := tx.ExecContext(r.Context(), `DELETE FROM telegram_notification_settings WHERE user_id=?`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete Telegram settings")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "Telegram settings not found")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete Telegram settings")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleTestTelegramSettings(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(a.cfg.NotificationSecretKey) == "" {
		respondError(w, http.StatusServiceUnavailable, "Telegram notifications are not available")
		return
	}
	user := currentUser(r)
	settings, err := a.telegramSettingsRecord(r.Context(), user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "Telegram settings not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load Telegram settings")
		return
	}
	now := a.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	cooldownThreshold := now.Add(-telegramNotificationTestCooldown).Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(), `UPDATE telegram_notification_settings SET last_test_at=?,updated_at=?
		WHERE user_id=? AND (last_test_at IS NULL OR last_test_at<=?)`, nowText, nowText, user.ID, cooldownThreshold)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to test Telegram settings")
		return
	}
	if affected, err := result.RowsAffected(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to test Telegram settings")
		return
	} else if affected == 0 {
		respondError(w, http.StatusTooManyRequests, "please wait before testing Telegram again")
		return
	}
	if err := a.deliverTelegramMessage(r.Context(), settings, telegramNotificationPayload{Text: "LanQin Email Telegram 通知测试成功。"}); err != nil {
		a.recordTelegramSettingsError(r.Context(), user.ID, err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE telegram_notification_settings SET last_delivered_at=?,last_error='',updated_at=? WHERE user_id=?`, nowText, nowText, user.ID)
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) telegramSettingsSnapshot(ctx context.Context, userID string) (TelegramSettings, error) {
	out := TelegramSettings{Available: strings.TrimSpace(a.cfg.NotificationSecretKey) != ""}
	record, err := a.telegramSettingsRecord(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return TelegramSettings{}, err
	}
	out.Configured = record.BotTokenCiphertext != "" && record.ChatID != ""
	out.TokenSet = record.BotTokenCiphertext != ""
	out.BotUsername = record.BotUsername
	out.ChatID = record.ChatID
	out.Enabled = record.Enabled
	if !record.LastDeliveredAt.IsZero() {
		value := record.LastDeliveredAt
		out.LastDeliveredAt = &value
	}
	out.LastError = record.LastError
	return out, nil
}

func (a *App) telegramSettingsRecord(ctx context.Context, userID string) (telegramSettingsRecord, error) {
	var item telegramSettingsRecord
	var enabled int
	var lastTest, lastDelivered sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT user_id,bot_token_ciphertext,bot_username,chat_id,enabled,last_test_at,last_delivered_at,last_error
		FROM telegram_notification_settings WHERE user_id=?`, userID).
		Scan(&item.UserID, &item.BotTokenCiphertext, &item.BotUsername, &item.ChatID, &enabled, &lastTest, &lastDelivered, &item.LastError)
	item.Enabled = intBool(enabled)
	if lastTest.Valid {
		item.LastTestAt = parseTime(lastTest.String)
	}
	if lastDelivered.Valid {
		item.LastDeliveredAt = parseTime(lastDelivered.String)
	}
	return item, err
}

func (a *App) telegramConfiguredForUser(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(a.cfg.NotificationSecretKey) == "" {
		return false, nil
	}
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM telegram_notification_settings WHERE user_id=? AND enabled=1 AND bot_token_ciphertext<>'' AND chat_id<>''`, userID).Scan(&count)
	return count > 0, err
}

func (a *App) enqueueTelegramNotification(ctx context.Context, userID, mailboxID, messageID, ruleID string) error {
	configured, err := a.telegramConfiguredForUser(ctx, userID)
	if err != nil {
		return err
	}
	if !configured {
		return nil
	}
	var mailboxAddress, fromAddress, fromName, subject, snippet string
	err = a.db.QueryRowContext(ctx, `SELECT mb.address,m.from_addr,COALESCE(m.from_name,''),m.subject,m.snippet
		FROM messages m JOIN mailboxes mb ON mb.id=m.mailbox_id WHERE m.id=? AND m.mailbox_id=? AND mb.user_id=?`, messageID, mailboxID, userID).
		Scan(&mailboxAddress, &fromAddress, &fromName, &subject, &snippet)
	if err != nil {
		return err
	}
	payload := telegramNotificationPayload{
		Text: telegramNotificationText(mailboxAddress, strings.TrimSpace(fromName+" "+fromAddress), subject, snippet),
		URL:  a.telegramMessageURL(mailboxID, messageID),
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	eventKey := "telegram:" + ruleID + ":" + messageID
	query := insertIgnoreSQL(a.cfg.DBDriver, `INSERT INTO telegram_notification_outbox(id,event_key,user_id,mailbox_id,message_id,rule_id,payload_json,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, `(event_key)`)
	_, err = a.db.ExecContext(ctx, query, newID("tgn"), eventKey, userID, mailboxID, messageID, ruleID, jsonEncode(payload), now, now, now)
	return err
}

func telegramNotificationText(mailbox, from, subject, snippet string) string {
	clean := func(value, fallback string) string {
		value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
		if value == "" {
			return fallback
		}
		return value
	}
	text := "新邮件\n收件邮箱: " + clean(mailbox, "未知") + "\n发件人: " + clean(from, "未知") + "\n主题: " + clean(subject, "(无主题)") + "\n摘要: " + clean(snippet, "(无摘要)")
	runes := []rune(text)
	if len(runes) > telegramNotificationTextMaxRunes {
		text = string(runes[:telegramNotificationTextMaxRunes-3]) + "..."
	}
	return text
}

func (a *App) telegramMessageURL(mailboxID, messageID string) string {
	base, err := url.Parse(strings.TrimSpace(a.cfg.PublicBaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return ""
	}
	base.Path = "/"
	query := base.Query()
	query.Set("mailboxId", mailboxID)
	query.Set("messageId", messageID)
	base.RawQuery = query.Encode()
	base.Fragment = ""
	return base.String()
}

func (a *App) telegramNotificationWorker(ctx context.Context) {
	if strings.TrimSpace(a.cfg.NotificationSecretKey) == "" {
		return
	}
	a.log.Info("Telegram notification worker started")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.processDueTelegramNotifications(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.log.Warn("Telegram notification worker failed", "error", err)
		}
		select {
		case <-ctx.Done():
			a.log.Info("Telegram notification worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func (a *App) processDueTelegramNotifications(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.NotificationSecretKey) == "" {
		return nil
	}
	_, _ = a.db.ExecContext(ctx, `DELETE FROM telegram_notification_outbox WHERE updated_at<? AND (delivered_at IS NOT NULL OR attempt_count>=?)`, a.now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339Nano), telegramNotificationMaxAttempts)
	nowText := a.now().UTC().Format(time.RFC3339Nano)
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM telegram_notification_outbox
		WHERE delivered_at IS NULL AND attempt_count<? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY next_attempt_at,created_at LIMIT 20`, telegramNotificationMaxAttempts, nowText, nowText)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		value, err := a.claimTelegramNotification(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var payload telegramNotificationPayload
		if err := json.Unmarshal([]byte(value.payload), &payload); err != nil {
			a.failTelegramNotification(ctx, value.id, value.userID, value.attempt, value.leaseToken, &telegramDeliveryError{message: "invalid Telegram notification payload", permanent: true})
			continue
		}
		settings, err := a.telegramSettingsRecord(ctx, value.userID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !settings.Enabled) {
			_, _ = a.db.ExecContext(ctx, `DELETE FROM telegram_notification_outbox WHERE id=? AND lease_token=?`, value.id, value.leaseToken)
			continue
		}
		if err != nil {
			return err
		}
		if err := a.deliverTelegramMessage(ctx, settings, payload); err != nil {
			a.failTelegramNotification(ctx, value.id, value.userID, value.attempt, value.leaseToken, err)
			continue
		}
		now := a.now().UTC().Format(time.RFC3339Nano)
		res, _ := a.db.ExecContext(ctx, `UPDATE telegram_notification_outbox SET last_error='',updated_at=?,delivered_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND delivered_at IS NULL AND lease_token=?`, now, now, value.id, value.leaseToken)
		if changed, _ := res.RowsAffected(); changed > 0 {
			_, _ = a.db.ExecContext(ctx, `UPDATE telegram_notification_settings SET last_delivered_at=?,last_error='',updated_at=? WHERE user_id=?`, now, now, value.userID)
		}
	}
	return nil
}

type telegramNotificationOutboxItem struct {
	id, userID, payload, leaseToken string
	attempt                         int
}

func (a *App) claimTelegramNotification(ctx context.Context, id string) (telegramNotificationOutboxItem, error) {
	now := a.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	token := newID("lease")
	res, err := a.db.ExecContext(ctx, `UPDATE telegram_notification_outbox SET attempt_count=attempt_count+1,updated_at=?,lease_owner=?,lease_token=?,lease_until=? WHERE id=? AND delivered_at IS NULL AND attempt_count<? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?)`, nowText, a.workerID, token, now.Add(outboxLeaseDuration).Format(time.RFC3339Nano), id, telegramNotificationMaxAttempts, nowText, nowText)
	if err != nil {
		return telegramNotificationOutboxItem{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return telegramNotificationOutboxItem{}, sql.ErrNoRows
	}
	var item telegramNotificationOutboxItem
	err = a.db.QueryRowContext(ctx, `SELECT id,user_id,payload_json,attempt_count,lease_token FROM telegram_notification_outbox WHERE id=? AND lease_token=?`, id, token).Scan(&item.id, &item.userID, &item.payload, &item.attempt, &item.leaseToken)
	return item, err
}

func (a *App) failTelegramNotification(ctx context.Context, id, userID string, attempt int, leaseToken string, sendErr error) {
	now := a.now().UTC()
	nextAttempt := attempt
	delay := sendRetryDelay(attempt)
	var deliveryErr *telegramDeliveryError
	if errors.As(sendErr, &deliveryErr) {
		if deliveryErr.retryAfter > 0 {
			delay = deliveryErr.retryAfter
		}
		if deliveryErr.permanent {
			nextAttempt = telegramNotificationMaxAttempts
		}
	}
	message := truncateWebhookError(sendErr.Error())
	res, _ := a.db.ExecContext(ctx, `UPDATE telegram_notification_outbox SET attempt_count=?,next_attempt_at=?,last_error=?,updated_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND delivered_at IS NULL AND lease_token=?`, nextAttempt, now.Add(delay).Format(time.RFC3339Nano), message, now.Format(time.RFC3339Nano), id, leaseToken)
	if changed, _ := res.RowsAffected(); changed > 0 {
		_, _ = a.db.ExecContext(ctx, `UPDATE telegram_notification_settings SET last_error=?,updated_at=? WHERE user_id=?`, message, now.Format(time.RFC3339Nano), userID)
	}
}

func (a *App) recordTelegramSettingsError(ctx context.Context, userID string, err error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, _ = a.db.ExecContext(ctx, `UPDATE telegram_notification_settings SET last_error=?,updated_at=? WHERE user_id=?`, truncateWebhookError(err.Error()), now, userID)
}

func (a *App) lookupTelegramBot(ctx context.Context, token string) (string, error) {
	response, err := a.callTelegramAPI(ctx, token, "getMe", map[string]any{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Result.Username), nil
}

func (a *App) deliverTelegramMessage(ctx context.Context, settings telegramSettingsRecord, payload telegramNotificationPayload) error {
	token, err := a.decryptNotificationSecret(settings.BotTokenCiphertext)
	if err != nil {
		return &telegramDeliveryError{message: "failed to decrypt Telegram credentials", permanent: true}
	}
	body := map[string]any{
		"chat_id":                  settings.ChatID,
		"text":                     payload.Text,
		"disable_web_page_preview": true,
	}
	if payload.URL != "" {
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "打开邮件", "url": payload.URL}}}}
	}
	_, err = a.callTelegramAPI(ctx, token, "sendMessage", body)
	return err
}

func (a *App) callTelegramAPI(ctx context.Context, token, method string, payload any) (telegramAPIResponse, error) {
	if !telegramBotTokenPattern.MatchString(token) {
		return telegramAPIResponse{}, &telegramDeliveryError{message: "invalid Telegram credentials", permanent: true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return telegramAPIResponse{}, &telegramDeliveryError{message: "failed to encode Telegram request", permanent: true}
	}
	base := strings.TrimRight(a.telegramAPIBaseURL, "/")
	if base == "" {
		base = defaultTelegramAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return telegramAPIResponse{}, &telegramDeliveryError{message: "failed to create Telegram request", permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LanQin-Email-Telegram/1.0")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return telegramAPIResponse{}, &telegramDeliveryError{message: "Telegram request failed"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return telegramAPIResponse{}, &telegramDeliveryError{message: "failed to read Telegram response"}
	}
	var result telegramAPIResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return telegramAPIResponse{}, &telegramDeliveryError{message: fmt.Sprintf("Telegram returned HTTP %d with an invalid response", resp.StatusCode)}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && result.OK {
		return result, nil
	}
	message := "Telegram rejected the request"
	if description := strings.TrimSpace(result.Description); description != "" {
		message += ": " + truncateWebhookError(description)
	}
	deliveryErr := &telegramDeliveryError{message: message}
	if resp.StatusCode == http.StatusTooManyRequests || result.Parameters.RetryAfter > 0 {
		seconds := result.Parameters.RetryAfter
		if seconds <= 0 {
			seconds = 30
		}
		deliveryErr.retryAfter = time.Duration(seconds) * time.Second
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		deliveryErr.permanent = true
	}
	return telegramAPIResponse{}, deliveryErr
}

func (a *App) encryptNotificationSecret(value string) (string, error) {
	key, err := a.notificationEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(nonce, gcm.Seal(nil, nonce, []byte(value), nil)...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *App) decryptNotificationSecret(value string) (string, error) {
	key, err := a.notificationEncryptionKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", errors.New("invalid encrypted notification secret")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted notification secret")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("invalid encrypted notification secret")
	}
	return string(plain), nil
}

func (a *App) notificationEncryptionKey() ([]byte, error) {
	secret := strings.TrimSpace(a.cfg.NotificationSecretKey)
	if secret == "" {
		return nil, errors.New("LANQIN_NOTIFICATION_SECRET_KEY is required")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}
