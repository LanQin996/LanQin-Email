package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	linuxDoProvider            = "linuxdo"
	linuxDoAuthorizeURL        = "https://connect.linux.do/oauth2/authorize"
	linuxDoTokenURL            = "https://connect.linux.do/oauth2/token"
	linuxDoUserInfoURL         = "https://connect.linux.do/api/user"
	linuxDoOAuthStateTTL       = 10 * time.Minute
	linuxDoRegistrationTTL     = 15 * time.Minute
	linuxDoMaxResponseBodySize = 64 << 10
)

var errLinuxDoIneligible = errors.New("linux.do account is not eligible")

type linuxDoOAuthClient interface {
	AuthorizationURL(clientID, redirectURI, state string) string
	ExchangeProfile(context.Context, string, string, string, string) (linuxDoProfile, error)
}

type linuxDoHTTPClient struct {
	client *http.Client
}

type linuxDoProfile struct {
	Subject     string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"name"`
}

type linuxDoUserInfo struct {
	ID       json.RawMessage `json:"id"`
	Username string          `json:"username"`
	Name     string          `json:"name"`
	Active   *bool           `json:"active"`
	Silenced *bool           `json:"silenced"`
}

type linuxDoLoginState struct {
	Purpose   string
	UserID    string
	ExpiresAt time.Time
}

type linuxDoRegistrationChallenge struct {
	Subject     string
	Username    string
	DisplayName string
	ExpiresAt   time.Time
}

type linuxDoIdentity struct {
	Linked   bool   `json:"linked"`
	Username string `json:"username,omitempty"`
}

func newLinuxDoHTTPClient() linuxDoOAuthClient {
	return &linuxDoHTTPClient{client: &http.Client{Timeout: 15 * time.Second}}
}

func (c *linuxDoHTTPClient) AuthorizationURL(clientID, redirectURI, state string) string {
	values := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"user"},
		"state":         {state},
	}
	return linuxDoAuthorizeURL + "?" + values.Encode()
}

func (c *linuxDoHTTPClient) ExchangeProfile(ctx context.Context, code, redirectURI, clientID, clientSecret string) (linuxDoProfile, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linuxDoTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return linuxDoProfile{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.doJSON(req, &token); err != nil {
		return linuxDoProfile{}, fmt.Errorf("token exchange: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return linuxDoProfile{}, errors.New("token response missing access_token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, linuxDoUserInfoURL, nil)
	if err != nil {
		return linuxDoProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/json")
	var payload linuxDoUserInfo
	if err := c.doJSON(req, &payload); err != nil {
		return linuxDoProfile{}, fmt.Errorf("user info: %w", err)
	}
	return linuxDoProfileFromUserInfo(payload)
}

func linuxDoProfileFromUserInfo(payload linuxDoUserInfo) (linuxDoProfile, error) {
	subject, err := linuxDoSubject(payload.ID)
	if err != nil {
		return linuxDoProfile{}, err
	}
	username := strings.TrimSpace(payload.Username)
	if len(subject) > 255 || username == "" || len([]rune(username)) > 255 || payload.Active == nil || payload.Silenced == nil {
		return linuxDoProfile{}, errors.New("user info missing required fields")
	}
	if !*payload.Active || *payload.Silenced {
		return linuxDoProfile{}, errLinuxDoIneligible
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = username
	}
	if len([]rune(name)) > 255 {
		return linuxDoProfile{}, errors.New("user info contains oversized fields")
	}
	return linuxDoProfile{Subject: subject, Username: username, DisplayName: name}, nil
}

func (c *linuxDoHTTPClient) doJSON(req *http.Request, dst any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, linuxDoMaxResponseBodySize+1))
	if err != nil {
		return err
	}
	if len(body) > linuxDoMaxResponseBodySize {
		return errors.New("response body too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

func linuxDoSubject(raw json.RawMessage) (string, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return "", errors.New("user info missing id")
	}
	if strings.HasPrefix(value, "\"") {
		var subject string
		if err := json.Unmarshal(raw, &subject); err != nil || strings.TrimSpace(subject) == "" {
			return "", errors.New("invalid user id")
		}
		return strings.TrimSpace(subject), nil
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", errors.New("invalid user id")
		}
	}
	return value, nil
}

func (a *App) linuxDoCallbackURL() string {
	return strings.TrimRight(strings.TrimSpace(a.cfg.PublicBaseURL), "/") + "/api/auth/linuxdo/callback"
}

func (a *App) linuxDoEnabled() bool {
	return validateLinuxDoSettings(a.cfg) == nil && a.cfg.LinuxDoSSOEnabled
}

func validateLinuxDoSettings(cfg Config) error {
	if !cfg.LinuxDoSSOEnabled {
		return nil
	}
	if strings.TrimSpace(cfg.LinuxDoClientID) == "" || strings.TrimSpace(cfg.LinuxDoClientSecret) == "" {
		return errors.New("Linux.do Client ID 和 Client Secret 未完整配置")
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.PublicBaseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("Linux.do SSO 需要有效的公网基础地址")
	}
	if !cfg.AllowInsecureHTTP && parsed.Scheme != "https" {
		return errors.New("Linux.do SSO 在安全模式下必须使用 HTTPS 公网地址")
	}
	return nil
}

func (a *App) handleLinuxDoLoginStart(w http.ResponseWriter, r *http.Request) {
	if !a.linuxDoEnabled() {
		respondError(w, http.StatusNotFound, "Linux.do SSO 未启用")
		return
	}
	state, err := a.createLinuxDoState(r.Context(), "login", "")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法启动 Linux.do 登录")
		return
	}
	a.setLinuxDoCookie(w, "state", state, "/api/auth/linuxdo/callback", linuxDoOAuthStateTTL)
	http.Redirect(w, r, a.linuxDoOAuth.AuthorizationURL(a.cfg.LinuxDoClientID, a.linuxDoCallbackURL(), state), http.StatusFound)
}

func (a *App) handleLinuxDoLinkStart(w http.ResponseWriter, r *http.Request) {
	if !a.linuxDoEnabled() {
		respondError(w, http.StatusBadRequest, "Linux.do SSO 未启用")
		return
	}
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		TwoFactorCode   string `json:"twoFactorCode"`
	}
	if err := decodeJSONWithLimit(r, &req, 16<<10); err != nil {
		badRequest(w, err)
		return
	}
	user := currentUser(r)
	if err := a.verifyLinuxDoReauthentication(r.Context(), user.ID, req.CurrentPassword, req.TwoFactorCode); err != nil {
		respondError(w, http.StatusUnauthorized, "密码或双因素验证码错误")
		return
	}
	state, err := a.createLinuxDoState(r.Context(), "link", user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法启动 Linux.do 绑定")
		return
	}
	a.setLinuxDoCookie(w, "state", state, "/api/auth/linuxdo/callback", linuxDoOAuthStateTTL)
	respondJSON(w, http.StatusOK, map[string]string{"url": a.linuxDoOAuth.AuthorizationURL(a.cfg.LinuxDoClientID, a.linuxDoCallbackURL(), state)})
}

func (a *App) handleLinuxDoCallback(w http.ResponseWriter, r *http.Request) {
	stateValue := strings.TrimSpace(r.URL.Query().Get("state"))
	cookie, err := r.Cookie(a.linuxDoCookieName("state"))
	if err != nil || stateValue == "" || subtle.ConstantTimeCompare([]byte(stateValue), []byte(cookie.Value)) != 1 {
		a.redirectLinuxDoResult(w, r, "/login", "state")
		return
	}
	state, err := a.consumeLinuxDoState(r.Context(), stateValue)
	a.clearLinuxDoCookie(w, "state", "/api/auth/linuxdo/callback")
	if err != nil {
		a.redirectLinuxDoResult(w, r, "/login", "state")
		return
	}
	target := "/login"
	if state.Purpose == "link" {
		target = "/profile"
	}
	if !a.linuxDoEnabled() {
		a.redirectLinuxDoResult(w, r, target, "configuration")
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("error")) != "" {
		a.redirectLinuxDoResult(w, r, target, "cancelled")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		a.redirectLinuxDoResult(w, r, target, "code")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	profile, err := a.linuxDoOAuth.ExchangeProfile(ctx, code, a.linuxDoCallbackURL(), a.cfg.LinuxDoClientID, a.cfg.LinuxDoClientSecret)
	if err != nil {
		if errors.Is(err, errLinuxDoIneligible) {
			a.redirectLinuxDoResult(w, r, target, "ineligible")
		} else {
			a.redirectLinuxDoResult(w, r, target, "upstream")
		}
		return
	}
	if state.Purpose == "link" {
		a.finishLinuxDoLink(w, r, state, profile)
		return
	}
	a.finishLinuxDoLogin(w, r, profile)
}

func (a *App) finishLinuxDoLink(w http.ResponseWriter, r *http.Request, state linuxDoLoginState, profile linuxDoProfile) {
	user, err := a.authenticateRequest(r)
	if err != nil || user.ID != state.UserID {
		a.redirectLinuxDoResult(w, r, "/profile", "session")
		return
	}
	if err := a.bindLinuxDoIdentity(r.Context(), user.ID, profile); err != nil {
		if errors.Is(err, errLinuxDoIdentityConflict) {
			a.redirectLinuxDoResult(w, r, "/profile", "conflict")
		} else {
			a.redirectLinuxDoResult(w, r, "/profile", "save")
		}
		return
	}
	a.redirectLinuxDoResult(w, r, "/profile", "linked")
}

func (a *App) finishLinuxDoLogin(w http.ResponseWriter, r *http.Request, profile linuxDoProfile) {
	userID, err := a.userIDForLinuxDoIdentity(r.Context(), profile.Subject)
	if err == nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE oauth_identities SET username=?,updated_at=? WHERE provider=? AND subject=?`, profile.Username, a.now().UTC().Format(time.RFC3339Nano), linuxDoProvider, profile.Subject)
		user, secret, loadErr := a.loadUserAuthByID(r.Context(), userID)
		if loadErr != nil || user.Disabled {
			a.redirectLinuxDoResult(w, r, "/login", "disabled")
			return
		}
		if user.TwoFactorEnabled && strings.TrimSpace(secret) != "" {
			challenge, challengeErr := a.createLoginChallenge(r.Context(), user.ID)
			if challengeErr != nil {
				a.redirectLinuxDoResult(w, r, "/login", "session")
				return
			}
			a.setLinuxDoCookie(w, "2fa", challenge, "/api/auth/linuxdo", 5*time.Minute)
			a.redirectLinuxDoResult(w, r, "/login", "2fa")
			return
		}
		if err := a.issueSession(w, r, user.ID); err != nil {
			a.redirectLinuxDoResult(w, r, "/login", "session")
			return
		}
		a.redirectLinuxDoResult(w, r, "/", "success")
		return
	}
	if !errors.Is(err, errNotFound) {
		a.redirectLinuxDoResult(w, r, "/login", "lookup")
		return
	}
	if !a.cfg.LinuxDoRegistrationEnabled {
		a.redirectLinuxDoResult(w, r, "/login", "unbound")
		return
	}
	token, err := a.createLinuxDoRegistrationChallenge(r.Context(), profile)
	if err != nil {
		a.redirectLinuxDoResult(w, r, "/login", "registration")
		return
	}
	a.setLinuxDoCookie(w, "registration", token, "/api/auth/linuxdo", linuxDoRegistrationTTL)
	a.redirectLinuxDoResult(w, r, "/register", "complete")
}

func (a *App) handleLinuxDoPendingRegistration(w http.ResponseWriter, r *http.Request) {
	if !a.linuxDoEnabled() || !a.cfg.LinuxDoRegistrationEnabled {
		respondError(w, http.StatusForbidden, "Linux.do 注册未启用")
		return
	}
	challenge, _, err := a.linuxDoRegistrationFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "Linux.do 注册信息已过期，请重新授权")
		return
	}
	domains, err := a.activePublicDomains(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载邮箱域名")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"username":    challenge.Username,
		"displayName": challenge.DisplayName,
		"domains":     domains,
	})
}

func (a *App) handleLinuxDoRegister(w http.ResponseWriter, r *http.Request) {
	if !a.linuxDoEnabled() || !a.cfg.LinuxDoRegistrationEnabled {
		respondError(w, http.StatusForbidden, "Linux.do 注册未启用")
		return
	}
	var req struct {
		DomainID       string `json:"domainId"`
		LocalPart      string `json:"localPart"`
		DisplayName    string `json:"displayName"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := decodeJSONWithLimit(r, &req, 32<<10); err != nil {
		badRequest(w, err)
		return
	}
	challenge, token, err := a.linuxDoRegistrationFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "Linux.do 注册信息已过期，请重新授权")
		return
	}
	if err := a.verifyTurnstile(r.Context(), req.TurnstileToken, r.RemoteAddr); err != nil {
		respondError(w, http.StatusUnauthorized, "人机验证失败，请重试")
		return
	}
	localPart := normalizeLocalPart(req.LocalPart)
	if localPart == "" || strings.TrimSpace(req.DomainID) == "" {
		badRequest(w, errors.New("请选择邮箱域名并填写邮箱前缀"))
		return
	}
	for _, reserved := range parseReservedPrefixes(a.cfg.ReservedMailboxPrefixes) {
		if localPart == reserved {
			respondError(w, http.StatusForbidden, "该前缀已被保留，请使用其他前缀")
			return
		}
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("密码至少需要 8 个字符"))
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = challenge.DisplayName
	}
	if len([]rune(displayName)) > 80 {
		badRequest(w, errors.New("显示名称不能超过 80 个字符"))
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
		return
	}
	defer tx.Rollback()
	var domain string
	if err := tx.QueryRowContext(r.Context(), `SELECT name FROM domains WHERE id=? AND status='active'`, strings.TrimSpace(req.DomainID)).Scan(&domain); err != nil {
		respondError(w, http.StatusBadRequest, "邮箱域名不可用")
		return
	}
	email := localPart + "@" + domain
	now := a.now().UTC().Format(time.RFC3339Nano)
	userID := newID("usr")
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, userID, email, displayName, "user", string(passwordHash), 0, now, now); err != nil {
		a.respondLinuxDoRegistrationDBError(w, err)
		return
	}
	// The account was inserted moments ago inside this transaction, so its
	// mailbox counter is 0 and no quota could reject the very first mailbox.
	// Loading a *User here would also have to read permission groups outside
	// this transaction, where the new row is not yet visible. The counter is
	// still incremented by the call below.
	if _, err := a.createMailboxWithPasswordHashTx(r.Context(), tx, userID, req.DomainID, localPart, displayName, string(passwordHash), 1024, "active", nil); err != nil {
		a.respondLinuxDoRegistrationDBError(w, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO oauth_identities(provider,subject,user_id,username,created_at,updated_at) VALUES(?,?,?,?,?,?)`, linuxDoProvider, challenge.Subject, userID, challenge.Username, now, now); err != nil {
		a.respondLinuxDoRegistrationDBError(w, err)
		return
	}
	result, err := tx.ExecContext(r.Context(), `DELETE FROM oauth_registration_challenges WHERE token_hash=?`, hashToken(token))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
		return
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		respondError(w, http.StatusConflict, "Linux.do 注册信息已被使用，请重新授权")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
		return
	}
	a.clearLinuxDoCookie(w, "registration", "/api/auth/linuxdo")
	if err := a.issueSession(w, r, userID); err != nil {
		respondError(w, http.StatusInternalServerError, "账号已创建，但登录失败，请重新登录")
		return
	}
	user, err := a.userByID(r.Context(), userID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "账号已创建，但资料加载失败")
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (a *App) respondLinuxDoRegistrationDBError(w http.ResponseWriter, err error) {
	if isUniqueViolation(err) {
		respondError(w, http.StatusConflict, "邮箱地址或 Linux.do 账号已被使用")
		return
	}
	respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
}

func (a *App) handleLinuxDoTwoFactor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSONWithLimit(r, &req, 8<<10); err != nil {
		badRequest(w, err)
		return
	}
	cookie, err := r.Cookie(a.linuxDoCookieName("2fa"))
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		respondError(w, http.StatusUnauthorized, "验证已过期，请重新登录")
		return
	}
	challenge, err := a.loginChallengeByToken(r.Context(), cookie.Value)
	if err != nil {
		a.clearLinuxDoCookie(w, "2fa", "/api/auth/linuxdo")
		respondError(w, http.StatusUnauthorized, "验证已过期，请重新登录")
		return
	}
	user, secret, err := a.loadUserAuthByID(r.Context(), challenge.UserID)
	if err != nil || user.Disabled || !user.TwoFactorEnabled || !verifyTOTP(secret, req.Code, a.now().UTC()) {
		respondError(w, http.StatusUnauthorized, "验证码错误")
		return
	}
	a.deleteLoginChallenge(r.Context(), challenge.ID)
	a.clearLinuxDoCookie(w, "2fa", "/api/auth/linuxdo")
	if err := a.issueSession(w, r, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (a *App) handleLinuxDoIdentity(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var username string
	err := a.db.QueryRowContext(r.Context(), `SELECT username FROM oauth_identities WHERE provider=? AND user_id=?`, linuxDoProvider, user.ID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		respondJSON(w, http.StatusOK, linuxDoIdentity{})
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载 Linux.do 绑定状态")
		return
	}
	respondJSON(w, http.StatusOK, linuxDoIdentity{Linked: true, Username: username})
}

func (a *App) handleLinuxDoUnlink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		TwoFactorCode   string `json:"twoFactorCode"`
	}
	if err := decodeJSONWithLimit(r, &req, 16<<10); err != nil {
		badRequest(w, err)
		return
	}
	user := currentUser(r)
	if err := a.verifyLinuxDoReauthentication(r.Context(), user.ID, req.CurrentPassword, req.TwoFactorCode); err != nil {
		respondError(w, http.StatusUnauthorized, "密码或双因素验证码错误")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM oauth_identities WHERE provider=? AND user_id=?`, linuxDoProvider, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "解除绑定失败")
		return
	}
	affected, _ := result.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "unlinked": affected > 0})
}

var errLinuxDoIdentityConflict = errors.New("linux.do identity conflict")

func (a *App) bindLinuxDoIdentity(ctx context.Context, userID string, profile linuxDoProfile) error {
	var existingUserID string
	err := a.db.QueryRowContext(ctx, `SELECT user_id FROM oauth_identities WHERE provider=? AND subject=?`, linuxDoProvider, profile.Subject).Scan(&existingUserID)
	if err == nil && existingUserID != userID {
		return errLinuxDoIdentityConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var existingSubject string
	err = a.db.QueryRowContext(ctx, `SELECT subject FROM oauth_identities WHERE provider=? AND user_id=?`, linuxDoProvider, userID).Scan(&existingSubject)
	if err == nil && existingSubject != profile.Subject {
		return errLinuxDoIdentityConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if existingUserID == userID {
		_, err = a.db.ExecContext(ctx, `UPDATE oauth_identities SET username=?,updated_at=? WHERE provider=? AND subject=?`, profile.Username, now, linuxDoProvider, profile.Subject)
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO oauth_identities(provider,subject,user_id,username,created_at,updated_at) VALUES(?,?,?,?,?,?)`, linuxDoProvider, profile.Subject, userID, profile.Username, now, now)
	if isUniqueViolation(err) {
		return errLinuxDoIdentityConflict
	}
	return err
}

func (a *App) userIDForLinuxDoIdentity(ctx context.Context, subject string) (string, error) {
	var userID string
	err := a.db.QueryRowContext(ctx, `SELECT user_id FROM oauth_identities WHERE provider=? AND subject=?`, linuxDoProvider, subject).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return userID, err
}

func (a *App) verifyLinuxDoReauthentication(ctx context.Context, userID, password, code string) error {
	var passwordHash, secret string
	var enabled int
	if err := a.db.QueryRowContext(ctx, `SELECT password_hash,two_factor_secret,two_factor_enabled FROM users WHERE id=? AND disabled=0`, userID).Scan(&passwordHash, &secret, &enabled); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		return errors.New("invalid password")
	}
	if intBool(enabled) && !verifyTOTP(secret, code, a.now().UTC()) {
		return errors.New("invalid two-factor code")
	}
	return nil
}

func (a *App) createLinuxDoState(ctx context.Context, purpose, userID string) (string, error) {
	if purpose != "login" && purpose != "link" {
		return "", errors.New("invalid oauth purpose")
	}
	now := a.now().UTC()
	_, _ = a.db.ExecContext(ctx, `DELETE FROM oauth_login_states WHERE expires_at<=?`, now.Format(time.RFC3339Nano))
	token := randomToken()
	_, err := a.db.ExecContext(ctx, `INSERT INTO oauth_login_states(token_hash,purpose,user_id,expires_at,created_at) VALUES(?,?,?,?,?)`, hashToken(token), purpose, nullableString(userID), now.Add(linuxDoOAuthStateTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return token, err
}

func (a *App) consumeLinuxDoState(ctx context.Context, token string) (linuxDoLoginState, error) {
	var state linuxDoLoginState
	var userID sql.NullString
	var expires string
	hash := hashToken(token)
	if err := a.db.QueryRowContext(ctx, `SELECT purpose,user_id,expires_at FROM oauth_login_states WHERE token_hash=?`, hash).Scan(&state.Purpose, &userID, &expires); err != nil {
		return state, err
	}
	result, err := a.db.ExecContext(ctx, `DELETE FROM oauth_login_states WHERE token_hash=?`, hash)
	if err != nil {
		return state, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return state, errors.New("oauth state already consumed")
	}
	state.UserID = userID.String
	state.ExpiresAt = parseTime(expires)
	if !state.ExpiresAt.After(a.now().UTC()) {
		return state, errors.New("oauth state expired")
	}
	return state, nil
}

func (a *App) createLinuxDoRegistrationChallenge(ctx context.Context, profile linuxDoProfile) (string, error) {
	now := a.now().UTC()
	_, _ = a.db.ExecContext(ctx, `DELETE FROM oauth_registration_challenges WHERE expires_at<=?`, now.Format(time.RFC3339Nano))
	token := randomToken()
	_, err := a.db.ExecContext(ctx, `INSERT INTO oauth_registration_challenges(token_hash,provider,subject,username,display_name,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, hashToken(token), linuxDoProvider, profile.Subject, profile.Username, profile.DisplayName, now.Add(linuxDoRegistrationTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return token, err
}

func (a *App) linuxDoRegistrationFromRequest(r *http.Request) (linuxDoRegistrationChallenge, string, error) {
	var challenge linuxDoRegistrationChallenge
	cookie, err := r.Cookie(a.linuxDoCookieName("registration"))
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return challenge, "", errors.New("registration cookie missing")
	}
	var provider, expires string
	err = a.db.QueryRowContext(r.Context(), `SELECT provider,subject,username,display_name,expires_at FROM oauth_registration_challenges WHERE token_hash=?`, hashToken(cookie.Value)).Scan(&provider, &challenge.Subject, &challenge.Username, &challenge.DisplayName, &expires)
	if err != nil || provider != linuxDoProvider {
		return challenge, "", errors.New("registration challenge not found")
	}
	challenge.ExpiresAt = parseTime(expires)
	if !challenge.ExpiresAt.After(a.now().UTC()) {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM oauth_registration_challenges WHERE token_hash=?`, hashToken(cookie.Value))
		return challenge, "", errors.New("registration challenge expired")
	}
	return challenge, cookie.Value, nil
}

func (a *App) activePublicDomains(ctx context.Context) ([]PublicDomain, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id,name FROM domains WHERE status='active' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	domains := []PublicDomain{}
	for rows.Next() {
		var domain PublicDomain
		if err := rows.Scan(&domain.ID, &domain.Name); err != nil {
			return nil, err
		}
		domains = append(domains, domain)
	}
	return domains, rows.Err()
}

func (a *App) linuxDoCookieName(kind string) string {
	return a.cfg.CookieName + "_linuxdo_" + kind
}

func (a *App) setLinuxDoCookie(w http.ResponseWriter, kind, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: a.linuxDoCookieName(kind), Value: value, Path: path, Expires: a.now().UTC().Add(ttl), MaxAge: int(ttl.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: !a.cfg.AllowInsecureHTTP})
}

func (a *App) clearLinuxDoCookie(w http.ResponseWriter, kind, path string) {
	http.SetCookie(w, &http.Cookie{Name: a.linuxDoCookieName(kind), Value: "", Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: !a.cfg.AllowInsecureHTTP})
}

func (a *App) redirectLinuxDoResult(w http.ResponseWriter, r *http.Request, path, result string) {
	base, err := url.Parse(strings.TrimSpace(a.cfg.PublicBaseURL))
	if err != nil || base.Host == "" {
		respondError(w, http.StatusInternalServerError, "invalid public base URL")
		return
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	query := base.Query()
	query.Set("linuxdo", result)
	base.RawQuery = query.Encode()
	base.Fragment = ""
	http.Redirect(w, r, base.String(), http.StatusFound)
}
