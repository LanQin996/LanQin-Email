package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	loginRateLimitAccountScope = "account"
	loginRateLimitIPScope      = "ip"

	loginRateLimitAccountFailures = 5
	loginRateLimitIPFailures      = 20
	loginRateLimitWindow          = 15 * time.Minute
	loginRateLimitLockDuration    = 15 * time.Minute
	loginRateLimitRetention       = 24 * time.Hour
)

type loginRateLimitSubject struct {
	scope string
	hash  string
	limit int
}

func loginRateLimitSubjects(account, clientIP string) []loginRateLimitSubject {
	subjects := make([]loginRateLimitSubject, 0, 2)
	if account = normalizeEmail(account); account != "" {
		subjects = append(subjects, loginRateLimitSubject{
			scope: loginRateLimitAccountScope,
			hash:  loginRateLimitHash(loginRateLimitAccountScope, account),
			limit: loginRateLimitAccountFailures,
		})
	}
	clientIP = normalizeLoginClientIP(clientIP)
	subjects = append(subjects, loginRateLimitSubject{
		scope: loginRateLimitIPScope,
		hash:  loginRateLimitHash(loginRateLimitIPScope, clientIP),
		limit: loginRateLimitIPFailures,
	})
	return subjects
}

func normalizeLoginClientIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	remoteAddr = strings.Trim(remoteAddr, "[]")
	if ip := net.ParseIP(remoteAddr); ip != nil {
		return ip.String()
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return strings.ToLower(remoteAddr)
}

func loginRateLimitHash(scope, value string) string {
	sum := sha256.Sum256([]byte("lanqin-login-rate-limit\x00" + scope + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func loginRateLimitRef(scope, value string) string {
	hash := loginRateLimitHash(scope, value)
	return hash[:16]
}

func (a *App) checkLoginRateLimit(ctx context.Context, account, clientIP string) (time.Duration, error) {
	now := a.now().UTC()
	var retryAfter time.Duration
	for _, subject := range loginRateLimitSubjects(account, clientIP) {
		var blockedUntilValue string
		err := a.db.QueryRowContext(ctx, `SELECT blocked_until FROM login_rate_limits WHERE scope=? AND subject_hash=?`, subject.scope, subject.hash).Scan(&blockedUntilValue)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		blockedUntil := parseTime(blockedUntilValue)
		if remaining := blockedUntil.Sub(now); remaining > retryAfter {
			retryAfter = remaining
		}
	}
	return retryAfter, nil
}

func (a *App) recordLoginFailure(ctx context.Context, account, clientIP string) (time.Duration, error) {
	now := a.now().UTC()
	cutoff := now.Add(-loginRateLimitWindow).Format(time.RFC3339Nano)
	blockedUntil := now.Add(loginRateLimitLockDuration).Format(time.RFC3339Nano)
	nowValue := now.Format(time.RFC3339Nano)

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, subject := range loginRateLimitSubjects(account, clientIP) {
		if err := a.upsertLoginFailure(ctx, tx, subject, nowValue, cutoff, blockedUntil); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_rate_limits WHERE updated_at<?`, now.Add(-loginRateLimitRetention).Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return a.checkLoginRateLimit(ctx, account, clientIP)
}

func (a *App) upsertLoginFailure(ctx context.Context, tx *sql.Tx, subject loginRateLimitSubject, nowValue, cutoff, blockedUntil string) error {
	if a.cfg.DBDriver == databaseDriverMySQL {
		_, err := tx.ExecContext(ctx, `INSERT INTO login_rate_limits(scope,subject_hash,failure_count,window_started_at,blocked_until,updated_at)
			VALUES(?,?,1,?,'',?)
			ON DUPLICATE KEY UPDATE
			blocked_until=IF(window_started_at<?, '', IF(failure_count+1>=?, ?, blocked_until)),
			failure_count=IF(window_started_at<?, 1, failure_count+1),
			window_started_at=IF(window_started_at<?, VALUES(window_started_at), window_started_at),
			updated_at=VALUES(updated_at)`,
			subject.scope, subject.hash, nowValue, nowValue,
			cutoff, subject.limit, blockedUntil, cutoff, cutoff)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO login_rate_limits(scope,subject_hash,failure_count,window_started_at,blocked_until,updated_at)
		VALUES(?,?,1,?,'',?)
		ON CONFLICT(scope,subject_hash) DO UPDATE SET
		blocked_until=CASE
			WHEN login_rate_limits.window_started_at<? THEN ''
			WHEN login_rate_limits.failure_count+1>=? THEN ?
			ELSE login_rate_limits.blocked_until
		END,
		failure_count=CASE WHEN login_rate_limits.window_started_at<? THEN 1 ELSE login_rate_limits.failure_count+1 END,
		window_started_at=CASE WHEN login_rate_limits.window_started_at<? THEN excluded.window_started_at ELSE login_rate_limits.window_started_at END,
		updated_at=excluded.updated_at`,
		subject.scope, subject.hash, nowValue, nowValue,
		cutoff, subject.limit, blockedUntil, cutoff, cutoff)
	return err
}

func (a *App) clearLoginAccountFailures(ctx context.Context, account string) error {
	account = normalizeEmail(account)
	if account == "" {
		return nil
	}
	_, err := a.db.ExecContext(ctx, `DELETE FROM login_rate_limits WHERE scope=? AND subject_hash=?`,
		loginRateLimitAccountScope, loginRateLimitHash(loginRateLimitAccountScope, account))
	return err
}

func (a *App) auditPasswordStageSuccess(account, clientIP string, twoFactorRequired bool) {
	outcome := "success"
	if twoFactorRequired {
		outcome = "challenge_issued"
	}
	a.auditLoginAttempt("password", outcome, account, clientIP)
}

func respondLoginRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	respondError(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后重试")
}

func (a *App) auditLoginAttempt(stage, outcome, account, clientIP string) {
	account = normalizeEmail(account)
	clientIP = normalizeLoginClientIP(clientIP)
	attrs := []any{
		"stage", stage,
		"outcome", outcome,
		"client_ref", loginRateLimitRef(loginRateLimitIPScope, clientIP),
	}
	if account != "" {
		attrs = append(attrs, "account_ref", loginRateLimitRef(loginRateLimitAccountScope, account))
	}
	if outcome == "success" || outcome == "challenge_issued" {
		a.log.Info("authentication audit", attrs...)
		return
	}
	a.log.Warn("authentication audit", attrs...)
}
