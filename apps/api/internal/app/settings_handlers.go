package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SystemSettings struct {
	PublicHostname          string   `json:"publicHostname"`
	PublicBaseURL           string   `json:"publicBaseUrl"`
	SMTPHost                string   `json:"smtpHost"`
	SMTPPort                string   `json:"smtpPort"`
	SMTPUsername            string   `json:"smtpUsername"`
	SMTPPasswordSet         bool     `json:"smtpPasswordSet"`
	SMTPRequireTLS          bool     `json:"smtpRequireTls"`
	MaildirRoot             string   `json:"maildirRoot"`
	MaildirScanSeconds      int      `json:"maildirScanSeconds"`
	SessionTTLHours         int      `json:"sessionTtlHours"`
	AllowInsecureHTTP       bool     `json:"allowInsecureHttp"`
	OpenRegistration        bool     `json:"openRegistration"`
	TwoFactorEnabled        bool     `json:"twoFactorEnabled"`
	TurnstileEnabled        bool     `json:"turnstileEnabled"`
	TurnstileSiteKey        string   `json:"turnstileSiteKey"`
	TurnstileSecretSet      bool     `json:"turnstileSecretSet"`
	CatchAllEnabled         bool     `json:"catchAllEnabled"`
	MailAutoRefresh         bool     `json:"mailAutoRefresh"`
	MailRefreshSeconds      int      `json:"mailRefreshSeconds"`
	UserMailboxApplyEnabled bool     `json:"userMailboxApplyEnabled"`
	UserMailboxDomainIDs    []string `json:"userMailboxDomainIds"`
	ReservedMailboxPrefixes string   `json:"reservedMailboxPrefixes"`
}

type systemSettingsUpdate struct {
	PublicHostname          string   `json:"publicHostname"`
	PublicBaseURL           string   `json:"publicBaseUrl"`
	SMTPHost                string   `json:"smtpHost"`
	SMTPPort                string   `json:"smtpPort"`
	SMTPUsername            string   `json:"smtpUsername"`
	SMTPPassword            string   `json:"smtpPassword"`
	SMTPRequireTLS          bool     `json:"smtpRequireTls"`
	MaildirRoot             string   `json:"maildirRoot"`
	MaildirScanSeconds      int      `json:"maildirScanSeconds"`
	SessionTTLHours         int      `json:"sessionTtlHours"`
	AllowInsecureHTTP       bool     `json:"allowInsecureHttp"`
	OpenRegistration        bool     `json:"openRegistration"`
	TwoFactorEnabled        bool     `json:"twoFactorEnabled"`
	TurnstileEnabled        bool     `json:"turnstileEnabled"`
	TurnstileSiteKey        string   `json:"turnstileSiteKey"`
	TurnstileSecretKey      string   `json:"turnstileSecretKey"`
	CatchAllEnabled         bool     `json:"catchAllEnabled"`
	MailAutoRefresh         bool     `json:"mailAutoRefresh"`
	MailRefreshSeconds      int      `json:"mailRefreshSeconds"`
	UserMailboxApplyEnabled bool     `json:"userMailboxApplyEnabled"`
	UserMailboxDomainIDs    []string `json:"userMailboxDomainIds"`
	ReservedMailboxPrefixes string   `json:"reservedMailboxPrefixes"`
}

type PublicSettings struct {
	OpenRegistration bool           `json:"openRegistration"`
	TurnstileEnabled bool           `json:"turnstileEnabled"`
	TurnstileSiteKey string         `json:"turnstileSiteKey"`
	MailAutoRefresh  bool           `json:"mailAutoRefresh"`
	MailRefreshMs    int            `json:"mailRefreshMs"`
	MailboxDomains   []PublicDomain `json:"mailboxDomains,omitempty"`
}

type PublicDomain struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type smtpTestRequest struct {
	To string `json:"to"`
}

func (a *App) handleGetSystemSettings(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, a.systemSettingsSnapshot())
}

func (a *App) handlePublicSettings(w http.ResponseWriter, r *http.Request) {
	enabled := a.cfg.TurnstileEnabled && strings.TrimSpace(a.cfg.TurnstileSiteKey) != "" && strings.TrimSpace(a.cfg.TurnstileSecretKey) != ""
	refreshSeconds := a.cfg.MailRefreshSeconds
	if refreshSeconds <= 0 {
		refreshSeconds = 30
	}
	settings := PublicSettings{OpenRegistration: a.cfg.OpenRegistration, TurnstileEnabled: enabled, TurnstileSiteKey: a.cfg.TurnstileSiteKey, MailAutoRefresh: a.cfg.MailAutoRefresh, MailRefreshMs: refreshSeconds * 1000}

	// Include available domains for mailbox creation during registration
	if a.cfg.OpenRegistration {
		rows, err := a.db.QueryContext(r.Context(), `SELECT id, name FROM domains WHERE status='active' ORDER BY name`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d PublicDomain
				if err := rows.Scan(&d.ID, &d.Name); err != nil {
					continue
				}
				settings.MailboxDomains = append(settings.MailboxDomains, d)
			}
		}
	}

	respondJSON(w, http.StatusOK, settings)
}

func (a *App) handleUpdateSystemSettings(w http.ResponseWriter, r *http.Request) {
	var req systemSettingsUpdate
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	next := a.cfg
	next.PublicHostname = normalizeHostname(req.PublicHostname)
	if next.PublicHostname == "" {
		badRequest(w, errors.New("publicHostname is required"))
		return
	}
	next.PublicBaseURL = strings.TrimSpace(req.PublicBaseURL)
	next.SMTPHost = strings.TrimSpace(req.SMTPHost)
	next.SMTPPort = strings.TrimSpace(req.SMTPPort)
	if next.SMTPPort == "" {
		next.SMTPPort = "25"
	}
	if _, err := strconv.Atoi(next.SMTPPort); err != nil {
		badRequest(w, errors.New("smtpPort must be a number"))
		return
	}
	next.SMTPUsername = strings.TrimSpace(req.SMTPUsername)
	if strings.TrimSpace(req.SMTPPassword) != "" {
		next.SMTPPassword = req.SMTPPassword
	}
	next.SMTPRequireTLS = req.SMTPRequireTLS
	next.MaildirRoot = strings.TrimSpace(req.MaildirRoot)
	if req.MaildirScanSeconds <= 0 {
		req.MaildirScanSeconds = 30
	}
	next.MaildirScanSeconds = req.MaildirScanSeconds
	if req.SessionTTLHours <= 0 {
		req.SessionTTLHours = 24 * 7
	}
	next.SessionTTLHours = req.SessionTTLHours
	next.AllowInsecureHTTP = req.AllowInsecureHTTP
	next.OpenRegistration = req.OpenRegistration
	next.TwoFactorEnabled = req.TwoFactorEnabled
	next.TurnstileEnabled = req.TurnstileEnabled
	next.TurnstileSiteKey = strings.TrimSpace(req.TurnstileSiteKey)
	if strings.TrimSpace(req.TurnstileSecretKey) != "" {
		next.TurnstileSecretKey = strings.TrimSpace(req.TurnstileSecretKey)
	}
	if next.TurnstileEnabled && (next.TurnstileSiteKey == "" || next.TurnstileSecretKey == "") {
		badRequest(w, errors.New("turnstile keys are required when enabled"))
		return
	}
	next.CatchAllEnabled = req.CatchAllEnabled
	next.MailAutoRefresh = req.MailAutoRefresh
	if req.MailRefreshSeconds <= 0 {
		req.MailRefreshSeconds = 30
	}
	next.MailRefreshSeconds = req.MailRefreshSeconds
	next.UserMailboxApplyEnabled = req.UserMailboxApplyEnabled
	next.UserMailboxDomainIDs = strings.Join(cleanIDList(req.UserMailboxDomainIDs), ",")
	next.ReservedMailboxPrefixes = strings.Join(parseReservedPrefixes(req.ReservedMailboxPrefixes), ",")

	if err := a.saveSystemSettings(r.Context(), next); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	a.cfg = next
	respondJSON(w, http.StatusOK, a.systemSettingsSnapshot())
}

func (a *App) handleTestSMTP(w http.ResponseWriter, r *http.Request) {
	var req smtpTestRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	cfg := a.cfg
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		badRequest(w, errors.New("SMTP 主机未设置"))
		return
	}
	if strings.TrimSpace(cfg.SMTPPort) == "" {
		cfg.SMTPPort = "25"
	}
	if _, err := strconv.Atoi(cfg.SMTPPort); err != nil {
		badRequest(w, errors.New("SMTP 端口无效"))
		return
	}
	to := normalizeEmail(req.To)
	if to == "" || !strings.Contains(to, "@") {
		badRequest(w, errors.New("收件邮箱无效"))
		return
	}
	from := cfg.AdminEmail
	if user := currentUser(r); user != nil && strings.Contains(user.Email, "@") {
		from = user.Email
	}
	if strings.TrimSpace(from) == "" || !strings.Contains(from, "@") {
		badRequest(w, errors.New("发件邮箱无效"))
		return
	}
	domain := cfg.PublicHostname
	if parts := strings.SplitN(from, "@", 2); len(parts) == 2 && parts[1] != "" {
		domain = parts[1]
	}
	if domain == "" {
		domain = "lanqin.local"
	}
	now := a.now().UTC()
	subject := "LanQin Email SMTP 测试"
	bodyText := "这是一封 SMTP 测试邮件。"
	bodyHTML := "<p>这是一封 SMTP 测试邮件。</p>"
	if tpl, err := a.mailTemplate(r.Context(), smtpTestTemplateKey); err == nil {
		rendered := renderMailTemplate(tpl, templateRenderData{
			To:             to,
			From:           from,
			PublicHostname: cfg.PublicHostname,
			PublicBaseURL:  cfg.PublicBaseURL,
			Time:           now,
		})
		subject, bodyText, bodyHTML = rendered.Subject, rendered.Text, rendered.HTML
	}
	mimeBytes, err := BuildMIME(MIMEMessage{
		From:      from,
		To:        []string{to},
		Subject:   subject,
		Text:      bodyText,
		HTML:      bodyHTML,
		MessageID: "<" + newID("msg") + "@" + domain + ">",
		Date:      now,
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	if err := sendSMTPWithConfig(cfg, from, []string{to}, mimeBytes); err != nil {
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) systemSettingsSnapshot() SystemSettings {
	return SystemSettings{
		PublicHostname:          a.cfg.PublicHostname,
		PublicBaseURL:           a.cfg.PublicBaseURL,
		SMTPHost:                a.cfg.SMTPHost,
		SMTPPort:                a.cfg.SMTPPort,
		SMTPUsername:            a.cfg.SMTPUsername,
		SMTPPasswordSet:         strings.TrimSpace(a.cfg.SMTPPassword) != "",
		SMTPRequireTLS:          a.cfg.SMTPRequireTLS,
		MaildirRoot:             a.cfg.MaildirRoot,
		MaildirScanSeconds:      a.cfg.MaildirScanSeconds,
		SessionTTLHours:         a.cfg.SessionTTLHours,
		AllowInsecureHTTP:       a.cfg.AllowInsecureHTTP,
		OpenRegistration:        a.cfg.OpenRegistration,
		TwoFactorEnabled:        a.cfg.TwoFactorEnabled,
		TurnstileEnabled:        a.cfg.TurnstileEnabled,
		TurnstileSiteKey:        a.cfg.TurnstileSiteKey,
		TurnstileSecretSet:      strings.TrimSpace(a.cfg.TurnstileSecretKey) != "",
		CatchAllEnabled:         a.cfg.CatchAllEnabled,
		MailAutoRefresh:         a.cfg.MailAutoRefresh,
		MailRefreshSeconds:      a.cfg.MailRefreshSeconds,
		UserMailboxApplyEnabled: a.cfg.UserMailboxApplyEnabled,
		UserMailboxDomainIDs:    cleanIDList(strings.Split(a.cfg.UserMailboxDomainIDs, ",")),
		ReservedMailboxPrefixes: strings.Join(parseReservedPrefixes(a.cfg.ReservedMailboxPrefixes), "\n"),
	}
}

func (a *App) loadPersistedSystemSettings(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT key,value FROM system_settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		switch key {
		case "publicHostname":
			a.cfg.PublicHostname = value
		case "publicBaseUrl":
			a.cfg.PublicBaseURL = value
		case "smtpHost":
			a.cfg.SMTPHost = value
		case "smtpPort":
			a.cfg.SMTPPort = value
		case "smtpUsername":
			a.cfg.SMTPUsername = value
		case "smtpPassword":
			a.cfg.SMTPPassword = value
		case "smtpRequireTls":
			a.cfg.SMTPRequireTLS = value == "true"
		case "maildirRoot":
			a.cfg.MaildirRoot = value
		case "maildirScanSeconds":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				a.cfg.MaildirScanSeconds = n
			}
		case "sessionTtlHours":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				a.cfg.SessionTTLHours = n
			}
		case "allowInsecureHttp":
			a.cfg.AllowInsecureHTTP = value == "true"
		case "openRegistration":
			a.cfg.OpenRegistration = value == "true"
		case "twoFactorEnabled":
			a.cfg.TwoFactorEnabled = value == "true"
		case "turnstileEnabled":
			a.cfg.TurnstileEnabled = value == "true"
		case "turnstileSiteKey":
			a.cfg.TurnstileSiteKey = value
		case "turnstileSecretKey":
			a.cfg.TurnstileSecretKey = value
		case "catchAllEnabled":
			a.cfg.CatchAllEnabled = value == "true"
		case "mailAutoRefresh":
			a.cfg.MailAutoRefresh = value == "true"
		case "mailRefreshSeconds":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				a.cfg.MailRefreshSeconds = n
			}
		case "userMailboxApplyEnabled":
			a.cfg.UserMailboxApplyEnabled = value == "true"
		case "userMailboxDomainIds":
			a.cfg.UserMailboxDomainIDs = value
		case "reservedMailboxPrefixes":
			a.cfg.ReservedMailboxPrefixes = value
		}
	}
	return rows.Err()
}

func (a *App) saveSystemSettings(ctx context.Context, cfg Config) error {
	values := map[string]string{
		"publicHostname":          cfg.PublicHostname,
		"publicBaseUrl":           cfg.PublicBaseURL,
		"smtpHost":                cfg.SMTPHost,
		"smtpPort":                cfg.SMTPPort,
		"smtpUsername":            cfg.SMTPUsername,
		"smtpPassword":            cfg.SMTPPassword,
		"smtpRequireTls":          strconv.FormatBool(cfg.SMTPRequireTLS),
		"maildirRoot":             cfg.MaildirRoot,
		"maildirScanSeconds":      strconv.Itoa(cfg.MaildirScanSeconds),
		"sessionTtlHours":         strconv.Itoa(cfg.SessionTTLHours),
		"allowInsecureHttp":       strconv.FormatBool(cfg.AllowInsecureHTTP),
		"openRegistration":        strconv.FormatBool(cfg.OpenRegistration),
		"twoFactorEnabled":        strconv.FormatBool(cfg.TwoFactorEnabled),
		"turnstileEnabled":        strconv.FormatBool(cfg.TurnstileEnabled),
		"turnstileSiteKey":        cfg.TurnstileSiteKey,
		"turnstileSecretKey":      cfg.TurnstileSecretKey,
		"catchAllEnabled":         strconv.FormatBool(cfg.CatchAllEnabled),
		"mailAutoRefresh":         strconv.FormatBool(cfg.MailAutoRefresh),
		"mailRefreshSeconds":      strconv.Itoa(cfg.MailRefreshSeconds),
		"userMailboxApplyEnabled": strconv.FormatBool(cfg.UserMailboxApplyEnabled),
		"userMailboxDomainIds":    strings.Join(cleanIDList(strings.Split(cfg.UserMailboxDomainIDs, ",")), ","),
		"reservedMailboxPrefixes": strings.Join(parseReservedPrefixes(cfg.ReservedMailboxPrefixes), ","),
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func cleanIDList(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func parseReservedPrefixes(value string) []string {
	value = strings.NewReplacer("\r", "\n", ",", "\n", ";", "\n", "，", "\n", "；", "\n").Replace(value)
	seen := map[string]bool{}
	out := []string{}
	for _, item := range strings.Split(value, "\n") {
		item = normalizeLocalPart(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func normalizeHostname(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	if i := strings.Index(value, "/"); i >= 0 {
		value = value[:i]
	}
	return value
}
