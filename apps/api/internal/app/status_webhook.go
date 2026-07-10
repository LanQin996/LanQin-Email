package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const statusWebhookMaxAttempts = 10

type statusWebhookEnvelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CreatedAt string `json:"createdAt"`
	Data      any    `json:"data"`
}

func (a *App) enqueueStatusWebhook(ctx context.Context, db dbExecutor, eventKey, eventType, mailboxID string, data any) error {
	if strings.TrimSpace(a.cfg.StatusWebhookURL) == "" {
		return nil
	}
	now := a.now().UTC()
	id := newID("whk")
	payload := jsonEncode(statusWebhookEnvelope{ID: id, Type: eventType, CreatedAt: now.Format(time.RFC3339Nano), Data: data})
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO status_webhook_outbox(id,event_key,event_type,mailbox_id,payload_json,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, id, eventKey, eventType, mailboxID, payload, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}

func (a *App) statusWebhookWorker(ctx context.Context) {
	if strings.TrimSpace(a.cfg.StatusWebhookURL) == "" {
		return
	}
	a.log.Info("status webhook worker started")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.processDueStatusWebhooks(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.log.Warn("status webhook worker failed", "error", err)
		}
		select {
		case <-ctx.Done():
			a.log.Info("status webhook worker stopped")
			return
		case <-ticker.C:
		}
	}
}

func (a *App) processDueStatusWebhooks(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.StatusWebhookURL) == "" {
		return nil
	}
	_, _ = a.db.ExecContext(ctx, `DELETE FROM status_webhook_outbox
		WHERE updated_at<? AND (delivered_at IS NOT NULL OR attempt_count>=?)`, a.now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339Nano), statusWebhookMaxAttempts)
	rows, err := a.db.QueryContext(ctx, `SELECT id,payload_json,attempt_count FROM status_webhook_outbox
		WHERE delivered_at IS NULL AND attempt_count<? AND next_attempt_at<=? ORDER BY next_attempt_at,created_at LIMIT 20`, statusWebhookMaxAttempts, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	type item struct {
		id, payload string
		attempt     int
	}
	items := []item{}
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.payload, &value.attempt); err != nil {
			rows.Close()
			return err
		}
		items = append(items, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range items {
		if err := a.deliverStatusWebhook(ctx, value.id, []byte(value.payload)); err != nil {
			now := a.now().UTC()
			next := now.Add(sendRetryDelay(value.attempt + 1))
			_, _ = a.db.ExecContext(ctx, `UPDATE status_webhook_outbox SET attempt_count=attempt_count+1,next_attempt_at=?,last_error=?,updated_at=? WHERE id=? AND delivered_at IS NULL`, next.Format(time.RFC3339Nano), truncateWebhookError(err.Error()), now.Format(time.RFC3339Nano), value.id)
			continue
		}
		now := a.now().UTC().Format(time.RFC3339Nano)
		_, _ = a.db.ExecContext(ctx, `UPDATE status_webhook_outbox SET attempt_count=attempt_count+1,last_error='',updated_at=?,delivered_at=? WHERE id=? AND delivered_at IS NULL`, now, now, value.id)
	}
	return nil
}

func (a *App) deliverStatusWebhook(ctx context.Context, eventID string, payload []byte) error {
	target, err := a.validatedStatusWebhookURL(ctx)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(a.now().UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(a.cfg.StatusWebhookSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LanQin-Email-Webhook/1.0")
	req.Header.Set("X-LanQin-Webhook-Id", eventID)
	req.Header.Set("X-LanQin-Timestamp", timestamp)
	req.Header.Set("X-LanQin-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: a.statusWebhookDialContext, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status webhook returned %d", resp.StatusCode)
	}
	return nil
}

func (a *App) validatedStatusWebhookURL(ctx context.Context) (*url.URL, error) {
	if strings.TrimSpace(a.cfg.StatusWebhookSecret) == "" {
		return nil, errors.New("LANQIN_STATUS_WEBHOOK_SECRET is required")
	}
	target, err := url.Parse(strings.TrimSpace(a.cfg.StatusWebhookURL))
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("invalid status webhook URL")
	}
	if target.Scheme != "https" && !(a.cfg.StatusWebhookAllowPrivateHosts && target.Scheme == "http") {
		return nil, errors.New("status webhook URL must use HTTPS")
	}
	if !a.cfg.StatusWebhookAllowPrivateHosts {
		if err := validatePublicWebhookHost(ctx, target.Hostname()); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func (a *App) statusWebhookDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if a.cfg.StatusWebhookAllowPrivateHosts {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !isPublicStatusWebhookIP(ip) {
			return nil, errors.New("private or local status webhook hosts are not allowed")
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("status webhook host resolved without usable addresses")
	}
	return nil, lastErr
}

func validatePublicWebhookHost(ctx context.Context, host string) error {
	if strings.EqualFold(host, "localhost") {
		return errors.New("localhost status webhook hosts are not allowed")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("failed to resolve status webhook host: %w", err)
	}
	for _, ip := range ips {
		if !isPublicStatusWebhookIP(ip) {
			return errors.New("private or local status webhook hosts are not allowed")
		}
	}
	return nil
}

func isPublicStatusWebhookIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func truncateWebhookError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}
