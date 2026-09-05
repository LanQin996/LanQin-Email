package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	netmail "net/mail"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxRuleForwardRecipients       = 5
	maxForwardAddresses            = 10
	forwardVerificationTTL         = 10 * time.Minute
	forwardVerificationCooldown    = time.Minute
	forwardVerificationMaxAttempts = 5
	forwardAddressSettingPrefix    = "mail_forward_address:"
)

type ForwardAddress struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Verified   bool       `json:"verified"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type forwardAddressState struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	CodeHash   string `json:"codeHash,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	LastSentAt string `json:"lastSentAt,omitempty"`
	Attempts   int    `json:"attempts,omitempty"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

func normalizeRuleForwardAddress(value string) string {
	address, err := netmail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	email := normalizeEmail(address.Address)
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	if parsed, err := netmail.ParseAddress(email); err != nil || !strings.EqualFold(parsed.Address, email) {
		return ""
	}
	return email
}

func ruleForwardRecipients(actions []MailRuleAction) []string {
	recipients := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Type == "forward" {
			recipients = append(recipients, normalizeRuleForwardAddress(action.Value))
		}
	}
	return dedupeEmails(recipients)
}

func ruleForwardSource(ruleID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ruleID)))
	return fmt.Sprintf("%s:%x", sendSourceRuleForward, sum[:8])
}

func forwardAddressID(email string) string {
	sum := sha256.Sum256([]byte(normalizeRuleForwardAddress(email)))
	return hex.EncodeToString(sum[:16])
}

func forwardAddressSettingKey(userID, id string) string {
	return forwardAddressSettingPrefix + strings.TrimSpace(userID) + ":" + strings.TrimSpace(id)
}

func (a *App) handleListForwardAddresses(w http.ResponseWriter, r *http.Request) {
	items, err := a.forwardAddressesForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load forward addresses")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleRequestForwardAddressVerification(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	email := normalizeRuleForwardAddress(req.Email)
	if email == "" {
		badRequest(w, errors.New("invalid forward address"))
		return
	}
	items, err := a.forwardAddressStatesForUser(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load forward addresses")
		return
	}
	id := forwardAddressID(email)
	var previous *forwardAddressState
	for index := range items {
		if items[index].ID == id {
			copy := items[index]
			previous = &copy
			break
		}
	}
	if previous == nil && len(items) >= maxForwardAddresses {
		badRequest(w, fmt.Errorf("at most %d forward addresses are allowed", maxForwardAddresses))
		return
	}
	if previous != nil && previous.VerifiedAt != "" {
		respondJSON(w, http.StatusOK, forwardAddressFromState(*previous))
		return
	}

	now := a.now().UTC()
	if previous != nil && previous.LastSentAt != "" && parseTime(previous.LastSentAt).After(now.Add(-forwardVerificationCooldown)) {
		respondError(w, http.StatusTooManyRequests, "verification code was sent recently")
		return
	}
	owned, err := a.mailboxByAddress(r.Context(), email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusInternalServerError, "failed to check mailbox ownership")
		return
	}
	if owned != nil && owned.UserID == user.ID {
		state := forwardAddressState{ID: id, Email: email, VerifiedAt: now.Format(time.RFC3339Nano), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}
		if previous != nil {
			state.CreatedAt = previous.CreatedAt
		}
		if err := a.saveForwardAddressState(r.Context(), user.ID, state); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save forward address")
			return
		}
		respondJSON(w, http.StatusOK, forwardAddressFromState(state))
		return
	}
	if strings.TrimSpace(a.cfg.SMTPHost) == "" {
		badRequest(w, errors.New("SMTP host is not configured"))
		return
	}
	mb, err := a.mailboxForUser(r.Context(), user.ID)
	if err != nil {
		badRequest(w, errors.New("an active mailbox is required"))
		return
	}
	code, err := randomForwardVerificationCode()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate verification code")
		return
	}
	codeHash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to secure verification code")
		return
	}
	createdAt := now.Format(time.RFC3339Nano)
	if previous != nil && previous.CreatedAt != "" {
		createdAt = previous.CreatedAt
	}
	state := forwardAddressState{
		ID: id, Email: email, CodeHash: string(codeHash), ExpiresAt: now.Add(forwardVerificationTTL).Format(time.RFC3339Nano),
		LastSentAt: now.Format(time.RFC3339Nano), CreatedAt: createdAt, UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := a.saveForwardAddressState(r.Context(), user.ID, state); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save forward address")
		return
	}
	if err := a.enqueueForwardVerification(r.Context(), user, mb, email, code); err != nil {
		if previous != nil {
			_ = a.saveForwardAddressState(r.Context(), user.ID, *previous)
		} else {
			_ = a.deleteForwardAddressState(r.Context(), user.ID, id)
		}
		respondError(w, http.StatusBadGateway, "failed to queue verification email")
		return
	}
	respondJSON(w, http.StatusAccepted, forwardAddressFromState(state))
}

func (a *App) handleVerifyForwardAddress(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	code := strings.TrimSpace(req.Code)
	for attempt := 0; attempt < 8; attempt++ {
		state, raw, err := a.forwardAddressStateByIDWithRaw(r.Context(), user.ID, id)
		if err != nil {
			respondError(w, http.StatusNotFound, "forward address not found")
			return
		}
		if state.VerifiedAt != "" {
			respondJSON(w, http.StatusOK, forwardAddressFromState(state))
			return
		}
		now := a.now().UTC()
		if state.ExpiresAt == "" || !parseTime(state.ExpiresAt).After(now) {
			badRequest(w, errors.New("verification code expired"))
			return
		}
		if state.Attempts >= forwardVerificationMaxAttempts {
			respondError(w, http.StatusTooManyRequests, "too many verification attempts")
			return
		}
		valid := bcrypt.CompareHashAndPassword([]byte(state.CodeHash), []byte(code)) == nil
		if valid {
			state.CodeHash = ""
			state.ExpiresAt = ""
			state.LastSentAt = ""
			state.Attempts = 0
			state.VerifiedAt = now.Format(time.RFC3339Nano)
		} else {
			state.Attempts++
		}
		state.UpdatedAt = now.Format(time.RFC3339Nano)
		updated, err := a.compareAndSwapForwardAddressState(r.Context(), user.ID, raw, state)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to verify forward address")
			return
		}
		if !updated {
			continue
		}
		if !valid {
			respondError(w, http.StatusUnauthorized, "invalid verification code")
			return
		}
		respondJSON(w, http.StatusOK, forwardAddressFromState(state))
		return
	}
	respondError(w, http.StatusConflict, "verification state changed; retry")
}

func (a *App) handleDeleteForwardAddress(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, err := a.forwardAddressStateByID(r.Context(), user.ID, id); err != nil {
		respondError(w, http.StatusNotFound, "forward address not found")
		return
	}
	if err := a.deleteForwardAddressState(r.Context(), user.ID, id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete forward address")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) enqueueForwardVerification(ctx context.Context, user *User, mb *Mailbox, email, code string) error {
	if err := a.recordSMTPRate(ctx, user, mb, 1); err != nil {
		return err
	}
	now := a.now().UTC()
	domain := strings.TrimSpace(a.cfg.PublicHostname)
	if parts := strings.SplitN(mb.Address, "@", 2); len(parts) == 2 && parts[1] != "" {
		domain = parts[1]
	}
	messageID := fmt.Sprintf("<%s@%s>", newID("forward-verify"), domain)
	mimeBytes, err := BuildMIME(MIMEMessage{
		From: mb.Address, FromName: "LanQin Email", To: []string{email}, Subject: "LanQin Email 自动转发验证",
		Text:      "你正在验证此邮箱作为自动转发目标。\n\n验证码：" + code + "\n\n验证码 10 分钟内有效。若非本人操作，请忽略此邮件。",
		HTML:      "<p>你正在验证此邮箱作为自动转发目标。</p><p>验证码：<strong>" + code + "</strong></p><p>验证码 10 分钟内有效。若非本人操作，请忽略此邮件。</p>",
		MessageID: messageID, Date: now,
	})
	if err != nil {
		return err
	}
	mimeBytes = append([]byte("Auto-Submitted: auto-generated\r\n"), mimeBytes...)
	a.recordSendAudit(ctx, sendAuditAccepted, sendQueueStatusQueued, sendAuditInput{
		UserID: user.ID, MailboxID: mb.ID, Source: sendSourceForwardVerification, MailFrom: mb.Address, HeaderFrom: mb.Address, Recipients: []string{email},
	})
	_, err = a.enqueueSend(ctx, sendQueueInput{
		UserID: user.ID, MailboxID: mb.ID, MessageID: messageID, Source: sendSourceForwardVerification,
		MailFrom: mb.Address, HeaderFrom: mb.Address, Recipients: []string{email}, MIMEBytes: mimeBytes, Now: now,
	})
	return err
}

func randomForwardVerificationCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (a *App) forwardAddressStatesForUser(ctx context.Context, userID string) ([]forwardAddressState, error) {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	rows, err := a.db.QueryContext(ctx, `SELECT value FROM system_settings WHERE `+keyColumn+` LIKE ?`, forwardAddressSettingPrefix+strings.TrimSpace(userID)+":%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []forwardAddressState{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item forwardAddressState
		if json.Unmarshal([]byte(raw), &item) == nil && item.ID != "" && item.Email != "" {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
	return items, rows.Err()
}

func (a *App) forwardAddressesForUser(ctx context.Context, userID string) ([]ForwardAddress, error) {
	states, err := a.forwardAddressStatesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	items := make([]ForwardAddress, 0, len(states))
	for _, state := range states {
		items = append(items, forwardAddressFromState(state))
	}
	return items, nil
}

func (a *App) forwardAddressStateByID(ctx context.Context, userID, id string) (forwardAddressState, error) {
	state, _, err := a.forwardAddressStateByIDWithRaw(ctx, userID, id)
	return state, err
}

func (a *App) forwardAddressStateByIDWithRaw(ctx context.Context, userID, id string) (forwardAddressState, string, error) {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	var raw string
	if err := a.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE `+keyColumn+`=?`, forwardAddressSettingKey(userID, id)).Scan(&raw); err != nil {
		return forwardAddressState{}, "", err
	}
	var state forwardAddressState
	if err := json.Unmarshal([]byte(raw), &state); err != nil || state.ID != id {
		return forwardAddressState{}, "", errors.New("invalid forward address state")
	}
	return state, raw, nil
}

func (a *App) saveForwardAddressState(ctx context.Context, userID string, state forwardAddressState) error {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	now := a.now().UTC().Format(time.RFC3339Nano)
	query := upsertSQL(a.cfg.DBDriver, `INSERT INTO system_settings(`+keyColumn+`,value,updated_at) VALUES(?,?,?)`, `(`+keyColumn+`)`,
		`value=excluded.value,updated_at=excluded.updated_at`, `value=VALUES(value),updated_at=VALUES(updated_at)`)
	_, err := a.db.ExecContext(ctx, query, forwardAddressSettingKey(userID, state.ID), jsonEncode(state), now)
	return err
}

func (a *App) compareAndSwapForwardAddressState(ctx context.Context, userID, previous string, state forwardAddressState) (bool, error) {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	result, err := a.db.ExecContext(ctx, `UPDATE system_settings SET value=?,updated_at=? WHERE `+keyColumn+`=? AND value=?`,
		jsonEncode(state), a.now().UTC().Format(time.RFC3339Nano), forwardAddressSettingKey(userID, state.ID), previous)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (a *App) deleteForwardAddressState(ctx context.Context, userID, id string) error {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	_, err := a.db.ExecContext(ctx, `DELETE FROM system_settings WHERE `+keyColumn+`=?`, forwardAddressSettingKey(userID, id))
	return err
}

func forwardAddressFromState(state forwardAddressState) ForwardAddress {
	item := ForwardAddress{ID: state.ID, Email: state.Email, Verified: state.VerifiedAt != "", CreatedAt: parseTime(state.CreatedAt)}
	if state.ExpiresAt != "" {
		expires := parseTime(state.ExpiresAt)
		item.ExpiresAt = &expires
	}
	if state.VerifiedAt != "" {
		verified := parseTime(state.VerifiedAt)
		item.VerifiedAt = &verified
	}
	return item
}

func (a *App) ruleForwardAddressesVerified(ctx context.Context, userID string, recipients []string) (bool, error) {
	for _, recipient := range dedupeEmails(recipients) {
		state, err := a.forwardAddressStateByID(ctx, userID, forwardAddressID(recipient))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		if state.VerifiedAt == "" || !strings.EqualFold(state.Email, recipient) {
			return false, nil
		}
	}
	return true, nil
}

func (a *App) enqueueRuleForward(ctx context.Context, mailboxID, messageID, ruleID string, recipients []string) error {
	mb, err := a.mailboxByID(ctx, mailboxID)
	if err != nil || mb.Status != "active" {
		return err
	}
	user, err := a.userByID(ctx, mb.UserID)
	if err != nil {
		return err
	}
	if user.Disabled || !userHasPermission(user, PermissionMailRules) {
		return nil
	}
	if verified, err := a.ruleForwardAddressesVerified(ctx, user.ID, recipients); err != nil || !verified {
		return err
	}

	targets := make([]string, 0, len(recipients))
	for _, recipient := range dedupeEmails(recipients) {
		if !strings.EqualFold(recipient, mb.Address) {
			targets = append(targets, recipient)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > maxRuleForwardRecipients {
		return fmt.Errorf("too many forward recipients: max %d", maxRuleForwardRecipients)
	}

	stored, err := a.storedMessageByID(ctx, messageID)
	if err != nil {
		return err
	}
	queueMessageID := strings.TrimSpace(stored.MessageID)
	if queueMessageID == "" {
		queueMessageID = "local:" + messageID
	}
	source := ruleForwardSource(ruleID)
	var existing int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM send_queue WHERE mailbox_id=? AND source=? AND message_id=?`, mailboxID, source, queueMessageID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}

	mimeBytes, suppressed, err := a.ruleForwardMIME(ctx, messageID, stored, mb.Address)
	if err != nil || suppressed {
		return err
	}
	if err := a.recordSMTPRate(ctx, user, mb, len(targets)); err != nil {
		return err
	}
	now := a.now().UTC()
	a.recordSendAudit(ctx, sendAuditAccepted, sendQueueStatusQueued, sendAuditInput{
		UserID: user.ID, MailboxID: mb.ID, Source: source, MailFrom: mb.Address, HeaderFrom: stored.From, Recipients: targets,
	})
	_, err = a.enqueueSend(ctx, sendQueueInput{
		UserID: user.ID, MailboxID: mb.ID, MessageID: queueMessageID, Source: source,
		MailFrom: mb.Address, HeaderFrom: stored.From, Recipients: targets, MIMEBytes: mimeBytes, Now: now,
	})
	return err
}

func (a *App) ruleForwardMIME(ctx context.Context, messageID string, msg storedMessage, forwardedBy string) ([]byte, bool, error) {
	var raw []byte
	if strings.TrimSpace(msg.RawPath) != "" {
		if safe, err := a.pathIsUnderMaildirRoot(msg.RawPath); err != nil {
			return nil, false, err
		} else if safe {
			raw, err = os.ReadFile(msg.RawPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, false, err
			}
		}
	}
	if len(raw) == 0 {
		attachments, err := a.attachmentInputsForMessage(ctx, messageID)
		if err != nil {
			return nil, false, err
		}
		raw, err = BuildMIME(MIMEMessage{
			From: msg.From, FromName: msg.FromName, To: msg.To, CC: msg.CC, BCC: msg.BCC,
			Subject: msg.Subject, Text: msg.BodyText, HTML: msg.BodyHTML, MessageID: msg.MessageID,
			Date: messageDate(msg), Attachments: attachments,
		})
		if err != nil {
			return nil, false, err
		}
	}
	return prepareRuleForwardMIME(raw, forwardedBy)
}

func prepareRuleForwardMIME(raw []byte, forwardedBy string) ([]byte, bool, error) {
	message, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, false, err
	}
	if value := strings.ToLower(strings.TrimSpace(message.Header.Get("Auto-Submitted"))); value != "" && value != "no" {
		return nil, true, nil
	}
	if strings.TrimSpace(message.Header.Get("X-LanQin-Auto-Forwarded")) != "" {
		return nil, true, nil
	}
	prefix := fmt.Sprintf("Auto-Submitted: auto-generated\r\nX-LanQin-Auto-Forwarded: %s\r\nX-Auto-Response-Suppress: All\r\n", normalizeEmail(forwardedBy))
	forwarded := make([]byte, 0, len(prefix)+len(raw))
	forwarded = append(forwarded, prefix...)
	forwarded = append(forwarded, raw...)
	return forwarded, false, nil
}
