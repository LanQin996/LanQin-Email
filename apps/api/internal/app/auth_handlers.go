package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email          string `json:"email"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstileToken"`
		ChallengeToken string `json:"challengeToken"`
		TwoFactorCode  string `json:"twoFactorCode"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(req.ChallengeToken) != "" {
		challenge, err := a.loginChallengeByToken(r.Context(), req.ChallengeToken)
		if err != nil {
			respondError(w, http.StatusUnauthorized, "验证已过期，请重新登录")
			return
		}
		user, secret, err := a.loadUserAuthByID(r.Context(), challenge.UserID)
		if err != nil || user.Disabled || !user.TwoFactorEnabled || strings.TrimSpace(secret) == "" {
			a.deleteLoginChallenge(r.Context(), challenge.ID)
			respondError(w, http.StatusUnauthorized, "验证已过期，请重新登录")
			return
		}
		if !verifyTOTP(secret, req.TwoFactorCode, a.now().UTC()) {
			respondError(w, http.StatusUnauthorized, "验证码错误")
			return
		}
		a.deleteLoginChallenge(r.Context(), challenge.ID)
		if err := a.issueSession(w, r, user.ID); err != nil {
			respondError(w, http.StatusInternalServerError, "登录失败，请稍后重试")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"user": user})
		return
	}
	if err := a.verifyTurnstile(r.Context(), req.TurnstileToken, r.RemoteAddr); err != nil {
		respondError(w, http.StatusUnauthorized, "人机验证失败，请重试")
		return
	}
	email := normalizeEmail(req.Email)
	user, passwordHash, err := a.userByEmail(r.Context(), email)
	if err != nil || user.Disabled {
		respondError(w, http.StatusUnauthorized, "邮箱或密码错误")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		respondError(w, http.StatusUnauthorized, "邮箱或密码错误")
		return
	}
	if a.cfg.TwoFactorEnabled && user.TwoFactorEnabled {
		challengeToken, err := a.createLoginChallenge(r.Context(), user.ID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "验证码生成失败，请稍后重试")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"twoFactorRequired": true, "challengeToken": challengeToken})
		return
	}
	if err := a.issueSession(w, r, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.OpenRegistration {
		respondError(w, http.StatusForbidden, "当前未开放注册")
		return
	}
	var req struct {
		Email          string `json:"email"`
		DisplayName    string `json:"displayName"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstileToken"`
		DomainID       string `json:"domainId"`
		LocalPart      string `json:"localPart"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := a.verifyTurnstile(r.Context(), req.TurnstileToken, r.RemoteAddr); err != nil {
		respondError(w, http.StatusUnauthorized, "人机验证失败，请重试")
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		badRequest(w, errors.New("邮箱地址无效"))
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("密码至少需要 8 个字符"))
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}
	if len([]rune(displayName)) > 80 {
		badRequest(w, errors.New("显示名称不能超过 80 个字符"))
		return
	}
	if _, _, err := a.userByEmail(r.Context(), email); err == nil {
		respondError(w, http.StatusConflict, "该邮箱已被注册")
		return
	} else if !errors.Is(err, errNotFound) {
		respondError(w, http.StatusInternalServerError, "failed to check user")
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	userID := newID("usr")
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, email, displayName, "user", string(passwordHash), 0, now, now); err != nil {
		if isUniqueViolation(err) {
			respondError(w, http.StatusConflict, "该邮箱已被注册")
			return
		}
		respondError(w, http.StatusInternalServerError, "注册失败，请稍后重试")
		return
	}
	user, err := a.userByID(r.Context(), userID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if err := a.issueSession(w, r, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}

	// Create a mailbox for the registered user
	var mailboxDomainID string
	var mailboxLocalPart string
	if strings.TrimSpace(req.DomainID) != "" && strings.TrimSpace(req.LocalPart) != "" {
		// User selected a specific domain and local part
		mailboxDomainID = strings.TrimSpace(req.DomainID)
		mailboxLocalPart = normalizeLocalPart(req.LocalPart)
	} else {
		// Auto-detect: use the first active domain and email local part
		if err := a.db.QueryRowContext(r.Context(), `SELECT id FROM domains WHERE status='active' ORDER BY created_at ASC LIMIT 1`).Scan(&mailboxDomainID); err != nil {
			mailboxDomainID = ""
		}
		if mailboxDomainID != "" {
			mailboxLocalPart = strings.SplitN(email, "@", 2)[0]
		}
	}
	if mailboxDomainID != "" && mailboxLocalPart != "" {
		// Check reserved prefixes
		reserved := map[string]bool{}
		for _, item := range parseReservedPrefixes(a.cfg.ReservedMailboxPrefixes) {
			reserved[item] = true
		}
		if reserved[mailboxLocalPart] {
			respondError(w, http.StatusForbidden, "该前缀已被保留，请使用其他前缀")
			return
		}
		if _, mbErr := a.createMailboxWithPasswordHash(r.Context(), user.ID, mailboxDomainID, mailboxLocalPart, displayName, string(passwordHash), 1024, "active"); mbErr != nil {
			a.log.Warn("failed to create mailbox for registered user", "error", mbErr, "email", email)
		}
	}

	respondJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(a.cfg.CookieName); err == nil {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash=?`, hashToken(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: a.cfg.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{"user": currentUser(r)})
}

func (a *App) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		DisplayName string `json:"displayName"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		badRequest(w, errors.New("请输入显示名称"))
		return
	}
	if len([]rune(displayName)) > 80 {
		badRequest(w, errors.New("显示名称不能超过 80 个字符"))
		return
	}
	_, err := a.db.ExecContext(r.Context(), `UPDATE users SET display_name=?, updated_at=? WHERE id=?`,
		displayName, a.now().UTC().Format(time.RFC3339Nano), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update profile")
		return
	}
	updated, err := a.userByID(r.Context(), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load profile")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"user": updated})
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.NewPassword) < 8 {
		badRequest(w, errors.New("新密码至少需要 8 个字符"))
		return
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE id=?`, user.ID)
	var currentHash string
	if err := row.Scan(&currentHash); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(req.CurrentPassword)); err != nil {
		respondError(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, string(newHash), now, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update password")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE mailboxes SET password_hash=?, updated_at=? WHERE user_id=?`, string(newHash), now, user.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox password")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save password")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}
