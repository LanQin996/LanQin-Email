package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

type loginChallenge struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
}

func newTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func totpProvisioningURI(issuer, account, secret string) string {
	issuer = strings.TrimSpace(issuer)
	account = strings.TrimSpace(account)
	secret = strings.TrimSpace(secret)
	label := url.PathEscape(issuer + ":" + account)
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s&digits=6&period=30", label, url.QueryEscape(secret), url.QueryEscape(issuer))
}

func generateTOTP(secret string, now time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	counter := now.Unix() / 30
	return generateTOTPForCounter(key, counter), nil
}

func verifyTOTP(secret, code string, now time.Time) bool {
	_, ok := matchTOTPCounter(secret, code, now)
	return ok
}

// matchTOTPCounter returns the 30-second step a code belongs to.
//
// The step is what makes replay detectable: a code stays valid for the whole
// acceptance window, so without remembering the last accepted step the same six
// digits can be presented repeatedly for about 90 seconds.
func matchTOTPCounter(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	counter := now.Unix() / 30
	for delta := int64(-1); delta <= 1; delta++ {
		if generateTOTPForCounter(key, counter+delta) == code {
			return counter + delta, true
		}
	}
	return 0, false
}

// consumeTOTP verifies a code and burns the step it belongs to.
//
// The conditional UPDATE is the whole point: it both records the step and rejects
// any code from a step already used, including two concurrent requests carrying the
// same code, since only one can move the column forward.
func (a *App) consumeTOTP(ctx context.Context, userID, secret, code string) bool {
	counter, ok := matchTOTPCounter(secret, code, a.now().UTC())
	if !ok {
		return false
	}
	result, err := a.db.ExecContext(ctx,
		`UPDATE users SET two_factor_last_counter=? WHERE id=? AND two_factor_last_counter<?`,
		counter, userID, counter)
	if err != nil {
		a.log.Warn("failed to record TOTP counter", "error", err)
		return false
	}
	affected, err := result.RowsAffected()
	return err == nil && affected == 1
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	secret = strings.ToUpper(strings.TrimSpace(secret))
	if secret == "" {
		return nil, errors.New("empty secret")
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
}

func generateTOTPForCounter(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset : offset+4])
	value &= 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}

func (a *App) createLoginChallenge(ctx context.Context, userID string) (string, error) {
	token := randomToken()
	now := a.now().UTC()
	expires := now.Add(5 * time.Minute)
	_, err := a.db.ExecContext(ctx, `INSERT INTO login_challenges(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,?,?)`,
		newID("lch"), userID, hashToken(token), expires.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return token, nil
}

func (a *App) loginChallengeByToken(ctx context.Context, token string) (*loginChallenge, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,expires_at FROM login_challenges WHERE token_hash=?`, hashToken(token))
	var challenge loginChallenge
	var expires string
	if err := row.Scan(&challenge.ID, &challenge.UserID, &expires); err != nil {
		return nil, err
	}
	challenge.ExpiresAt = parseTime(expires)
	if !challenge.ExpiresAt.IsZero() && !challenge.ExpiresAt.After(a.now().UTC()) {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM login_challenges WHERE id=?`, challenge.ID)
		return nil, errors.New("challenge expired")
	}
	return &challenge, nil
}

func (a *App) deleteLoginChallenge(ctx context.Context, id string) {
	_, _ = a.db.ExecContext(ctx, `DELETE FROM login_challenges WHERE id=?`, id)
}

// totpSecretEncryptionPrefix marks a stored seed as ciphertext.
//
// An explicit marker is used rather than sniffing the encoding: a base32 seed can be
// valid base64, so guessing would eventually misread a plaintext seed as ciphertext.
const totpSecretEncryptionPrefix = "enc:v1:"

// encryptTOTPSecret protects a seed at rest when a root key is configured.
//
// TOTP is core authentication and predates both root keys, so a missing key must not
// break it: the seed is then stored as it always was. Deployments that do set a key get
// encryption for seeds written from then on, and existing plaintext rows keep working
// because decryptTOTPSecret falls back on the absence of the marker.
//
// Losing a configured key makes those seeds unreadable. That is recoverable rather than
// terminal only because an administrator can clear a user's second factor
// (handleAdminResetTwoFactor); do not remove that entry point while this exists.
func (a *App) encryptTOTPSecret(secret string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", nil
	}
	key, ok := a.totpSecretEncryptionKey()
	if !ok {
		return secret, nil
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
	out := append(nonce, gcm.Seal(nil, nonce, []byte(secret), nil)...)
	return totpSecretEncryptionPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// decryptTOTPSecret reverses encryptTOTPSecret. Values without the marker are returned
// unchanged: that is both a pre-encryption row and a deployment with no key set.
func (a *App) decryptTOTPSecret(stored string) (string, error) {
	if !strings.HasPrefix(stored, totpSecretEncryptionPrefix) {
		return stored, nil
	}
	key, ok := a.totpSecretEncryptionKey()
	if !ok {
		// Refusing here is the point: treating ciphertext as a seed would silently
		// reject every correct code instead of naming the missing key.
		return "", errors.New("two-factor secret is encrypted but no encryption key is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, totpSecretEncryptionPrefix))
	if err != nil {
		return "", errors.New("invalid encrypted two-factor secret")
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
		return "", errors.New("invalid encrypted two-factor secret")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("invalid encrypted two-factor secret")
	}
	return string(plain), nil
}

// totpSecretEncryptionKey reuses whichever root key the deployment already sets, in
// preference order, rather than adding a third one to lose.
// totpSecretEncryptionKey derives the seed-encryption key from exactly one root key.
//
// LANQIN_NOTIFICATION_SECRET_KEY is the only accepted source, and there is no fallback
// to the other root key on purpose. A key that can change identity is worse than no key
// at all here: a deployment holding only the IMAP key that later added a notification
// key would start deriving a different key, and every seed written under the old one
// would stop decrypting — locking those users out with no signal beforehand.
//
// The value is domain-separated so this key cannot coincide with the one protecting
// Telegram tokens even though both derive from the same secret.
func (a *App) totpSecretEncryptionKey() ([]byte, bool) {
	trimmed := strings.TrimSpace(a.cfg.NotificationSecretKey)
	if trimmed == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte("lanqin-totp-seed\x00" + trimmed))
	return sum[:], true
}

func (a *App) loadUserAuthByID(ctx context.Context, id string) (*User, string, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,email,display_name,role,disabled,two_factor_enabled,two_factor_secret,created_at FROM users WHERE id=?`, id)
	var u User
	var disabled, twoFactorEnabled int
	var secret, created string
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled, &twoFactorEnabled, &secret, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", errNotFound
		}
		return nil, "", err
	}
	u.Disabled = intBool(disabled)
	u.TwoFactorEnabled = intBool(twoFactorEnabled)
	u.CreatedAt = parseTime(created)
	if err := a.attachUserAuthorization(ctx, &u); err != nil {
		return nil, "", err
	}
	plainSecret, err := a.decryptTOTPSecret(secret)
	if err != nil {
		return nil, "", err
	}
	return &u, plainSecret, nil
}

func (a *App) handleTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.TwoFactorEnabled {
		respondError(w, http.StatusBadRequest, "双因素认证已关闭")
		return
	}
	user := currentUser(r)
	if user == nil {
		respondError(w, http.StatusUnauthorized, "需要登录后才能操作")
		return
	}
	current, _, err := a.loadUserAuthByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if current.TwoFactorEnabled {
		respondError(w, http.StatusBadRequest, "two-factor authentication is already enabled")
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate secret")
		return
	}
	stored, err := a.encryptTOTPSecret(secret)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to protect secret")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(r.Context(), `UPDATE users SET two_factor_secret=?, two_factor_enabled=0, updated_at=? WHERE id=?`, stored, now, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save secret")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"secret":     secret,
		"otpauthUrl": totpProvisioningURI("LanQin Email", current.Email, secret),
	})
}

func (a *App) handleTwoFactorEnable(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.TwoFactorEnabled {
		respondError(w, http.StatusBadRequest, "双因素认证已关闭")
		return
	}
	user := currentUser(r)
	if user == nil {
		respondError(w, http.StatusUnauthorized, "需要登录后才能操作")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	current, secret, err := a.loadUserAuthByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if current.TwoFactorEnabled {
		respondJSON(w, http.StatusOK, map[string]any{"user": current})
		return
	}
	if strings.TrimSpace(secret) == "" {
		badRequest(w, errors.New("two-factor secret not set"))
		return
	}
	if !a.consumeTOTP(r.Context(), user.ID, secret, req.Code) {
		respondError(w, http.StatusUnauthorized, "invalid verification code")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE users SET two_factor_enabled=1, updated_at=? WHERE id=?`, a.now().UTC().Format(time.RFC3339Nano), user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to enable two-factor authentication")
		return
	}
	updated, _, err := a.loadUserAuthByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"user": updated})
}

func (a *App) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		respondError(w, http.StatusUnauthorized, "需要登录后才能操作")
		return
	}
	var req struct {
		Code            string `json:"code"`
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	current, secret, err := a.loadUserAuthByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if !current.TwoFactorEnabled && strings.TrimSpace(secret) == "" {
		respondJSON(w, http.StatusOK, map[string]any{"user": current})
		return
	}
	if err := a.verifyCurrentPassword(r.Context(), user.ID, req.CurrentPassword); err != nil {
		respondError(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	if strings.TrimSpace(secret) != "" && current.TwoFactorEnabled && !a.consumeTOTP(r.Context(), user.ID, secret, req.Code) {
		respondError(w, http.StatusUnauthorized, "invalid verification code")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE users SET two_factor_secret='', two_factor_enabled=0, updated_at=? WHERE id=?`, a.now().UTC().Format(time.RFC3339Nano), user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to disable two-factor authentication")
		return
	}
	updated, _, err := a.loadUserAuthByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"user": updated})
}

// verifyCurrentPassword re-authenticates the signed-in user.
//
// Used by operations that weaken account security, where holding a session should not
// be sufficient on its own.
func (a *App) verifyCurrentPassword(ctx context.Context, userID, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("current password is required")
	}
	var hash string
	if err := a.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&hash); err != nil {
		return err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// handleAdminResetTwoFactor clears another user's second factor.
//
// Without this there is no recovery path at all: only the account holder can disable
// their own 2FA, so a lost authenticator means a permanently locked account that
// requires direct database access to fix.
//
// It carries the same containment semantics as an administrator password reset — the
// action is taken because the account is in trouble, so every session and API token of
// the target goes with it. It deliberately requires PermissionUsersResetPassword rather
// than the broader update permission: stripping somebody's second factor is the same
// class of act as resetting their password.
func (a *App) handleAdminResetTwoFactor(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `UPDATE users SET two_factor_secret='', two_factor_enabled=0, updated_at=? WHERE id=?`, now, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset two-factor authentication")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := a.containUserAfterAdminResetTx(r.Context(), tx, id, now); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke sessions")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset two-factor authentication")
		return
	}
	a.log.Info("administrator reset two-factor authentication", "target", id, "actor", actorID(r))
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func actorID(r *http.Request) string {
	if user := currentUser(r); user != nil {
		return user.ID
	}
	return ""
}
