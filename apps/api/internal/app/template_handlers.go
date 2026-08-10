package app

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const smtpTestTemplateKey = "smtp_test"

type MailTemplate struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Subject   string    `json:"subject"`
	BodyText  string    `json:"bodyText"`
	BodyHTML  string    `json:"bodyHtml"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type mailTemplateUpdate struct {
	Subject  string `json:"subject"`
	BodyText string `json:"bodyText"`
	BodyHTML string `json:"bodyHtml"`
}

type templateRenderData struct {
	To             string
	From           string
	Subject        string
	PublicHostname string
	PublicBaseURL  string
	Time           time.Time
}

func defaultMailTemplates() []MailTemplate {
	now := time.Unix(0, 0).UTC()
	return []MailTemplate{
		{
			Key:       "welcome",
			Name:      "欢迎邮件",
			Subject:   "欢迎使用 LanQin Email",
			BodyText:  "你的自建邮箱 Webmail 已经初始化完成。\n\n请尽快修改默认管理员密码，并配置 MX/SPF/DKIM/DMARC。",
			BodyHTML:  "<p>你的自建邮箱 Webmail 已经初始化完成。</p><p>请尽快修改默认管理员密码，并配置 MX/SPF/DKIM/DMARC。</p>",
			UpdatedAt: now,
		},
		{
			Key:       smtpTestTemplateKey,
			Name:      "SMTP 测试",
			Subject:   "LanQin Email SMTP 测试",
			BodyText:  "这是一封 SMTP 测试邮件。\n\n发件人：{{from}}\n收件人：{{to}}\n时间：{{time}}\n主机：{{publicHostname}}",
			BodyHTML:  "<p>这是一封 SMTP 测试邮件。</p><p>发件人：{{from}}<br>收件人：{{to}}<br>时间：{{time}}<br>主机：{{publicHostname}}</p>",
			UpdatedAt: now,
		},
	}
}

func (a *App) ensureDefaultMailTemplates(ctx context.Context) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	for _, tpl := range defaultMailTemplates() {
		query := insertIgnoreSQL(a.cfg.DBDriver, `INSERT INTO mail_templates(`+keyColumn+`,name,subject,body_text,body_html,updated_at)
			VALUES(?,?,?,?,?,?)`, `(`+keyColumn+`)`)
		if _, err := a.db.ExecContext(ctx, query,
			tpl.Key, tpl.Name, tpl.Subject, tpl.BodyText, tpl.BodyHTML, now); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) handleListMailTemplates(w http.ResponseWriter, r *http.Request) {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	rows, err := a.db.QueryContext(r.Context(), `SELECT `+keyColumn+`,name,subject,body_text,body_html,updated_at FROM mail_templates ORDER BY name`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	defer rows.Close()
	items := []MailTemplate{}
	for rows.Next() {
		var item MailTemplate
		var updated string
		if err := rows.Scan(&item.Key, &item.Name, &item.Subject, &item.BodyText, &item.BodyHTML, &updated); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan templates")
			return
		}
		item.UpdatedAt = parseTime(updated)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleUpdateMailTemplate(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	var req mailTemplateUpdate
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		badRequest(w, errors.New("subject is required"))
		return
	}
	bodyText := strings.TrimSpace(req.BodyText)
	bodyHTML := strings.TrimSpace(req.BodyHTML)
	if bodyText == "" && bodyHTML == "" {
		badRequest(w, errors.New("template body is required"))
		return
	}
	if bodyText == "" {
		bodyText = stripTags(bodyHTML)
	}
	if bodyHTML == "" {
		bodyHTML = "<p>" + htmlEscape(bodyText) + "</p>"
	}
	bodyHTML = a.policy.Sanitize(bodyHTML)
	now := a.now().UTC().Format(time.RFC3339Nano)
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	res, err := a.db.ExecContext(r.Context(), `UPDATE mail_templates SET subject=?,body_text=?,body_html=?,updated_at=? WHERE `+keyColumn+`=?`,
		subject, bodyText, bodyHTML, now, key)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update template")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "template not found")
		return
	}
	tpl, err := a.mailTemplate(r.Context(), key)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load template")
		return
	}
	respondJSON(w, http.StatusOK, tpl)
}

func (a *App) handleResetMailTemplate(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	var defaults = defaultMailTemplates()
	for _, tpl := range defaults {
		if tpl.Key != key {
			continue
		}
		now := a.now().UTC().Format(time.RFC3339Nano)
		keyColumn := keyColumnSQL(a.cfg.DBDriver)
		res, err := a.db.ExecContext(r.Context(), `UPDATE mail_templates SET name=?,subject=?,body_text=?,body_html=?,updated_at=? WHERE `+keyColumn+`=?`,
			tpl.Name, tpl.Subject, tpl.BodyText, tpl.BodyHTML, now, key)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to reset template")
			return
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			respondError(w, http.StatusNotFound, "template not found")
			return
		}
		updated, err := a.mailTemplate(r.Context(), key)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load template")
			return
		}
		respondJSON(w, http.StatusOK, updated)
		return
	}
	respondError(w, http.StatusNotFound, "template not found")
}

func (a *App) mailTemplate(ctx context.Context, key string) (MailTemplate, error) {
	keyColumn := keyColumnSQL(a.cfg.DBDriver)
	row := a.db.QueryRowContext(ctx, `SELECT `+keyColumn+`,name,subject,body_text,body_html,updated_at FROM mail_templates WHERE `+keyColumn+`=?`, key)
	var tpl MailTemplate
	var updated string
	if err := row.Scan(&tpl.Key, &tpl.Name, &tpl.Subject, &tpl.BodyText, &tpl.BodyHTML, &updated); err != nil {
		return MailTemplate{}, err
	}
	tpl.UpdatedAt = parseTime(updated)
	return tpl, nil
}

func renderMailTemplate(tpl MailTemplate, data templateRenderData) MIMEMessage {
	values := map[string]string{
		"to":             data.To,
		"from":           data.From,
		"subject":        data.Subject,
		"publicHostname": data.PublicHostname,
		"publicBaseUrl":  data.PublicBaseURL,
		"time":           data.Time.Format("2006-01-02 15:04:05 MST"),
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	apply := func(input string) string {
		out := input
		for _, key := range keys {
			out = strings.ReplaceAll(out, "{{"+key+"}}", values[key])
		}
		return out
	}
	return MIMEMessage{
		Subject: apply(tpl.Subject),
		Text:    apply(tpl.BodyText),
		HTML:    apply(tpl.BodyHTML),
	}
}
