package app

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
)

type HTMLPolicy struct{ policy *bluemonday.Policy }

func NewHTMLPolicy() *HTMLPolicy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("html", "head", "body", "center", "font")
	p.AllowAttrs("style").Globally()
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).Globally()
	p.AllowAttrs("align", "valign").Matching(bluemonday.Paragraph).Globally()
	p.AllowAttrs("width", "height").Matching(bluemonday.NumberOrPercent).Globally()
	p.AllowAttrs("bgcolor", "color").Matching(regexp.MustCompile(`(?i)^#[0-9a-f]{3,8}$|^[a-z][a-z0-9 -]{0,31}$`)).Globally()
	p.AllowAttrs("border", "cellpadding", "cellspacing").Matching(bluemonday.Number).OnElements("table")
	p.AllowStyles(
		"background", "background-color", "background-image", "border", "border-collapse", "border-color",
		"border-radius", "border-spacing", "border-style", "border-width", "box-shadow", "color", "display",
		"font", "font-family", "font-size", "font-style", "font-weight", "height", "letter-spacing",
		"line-height", "margin", "margin-bottom", "margin-left", "margin-right", "margin-top", "max-width",
		"min-width", "opacity", "padding", "padding-bottom", "padding-left", "padding-right", "padding-top",
		"text-align", "text-decoration", "text-transform", "vertical-align", "white-space", "width",
	).MatchingHandler(safeEmailCSSValue).Globally()
	return &HTMLPolicy{policy: p}
}

func (p *HTMLPolicy) Sanitize(s string) string {
	if p == nil || p.policy == nil {
		return s
	}
	styles, withoutStyles := extractSafeEmailStyles(s)
	clean := p.policy.Sanitize(withoutStyles)
	if len(styles) == 0 {
		return clean
	}
	return strings.Join(styles, "") + clean
}

var emailStyleTagRe = regexp.MustCompile(`(?is)<style\b([^>]*)>(.*?)</style>`)
var htmlNonContentTagRe = regexp.MustCompile(`(?is)<(style|script|head|title|noscript)\b[^>]*>.*?</\s*(style|script|head|title|noscript)\s*>`)

func extractSafeEmailStyles(value string) ([]string, string) {
	styles := []string{}
	withoutStyles := emailStyleTagRe.ReplaceAllStringFunc(value, func(tag string) string {
		match := emailStyleTagRe.FindStringSubmatch(tag)
		if len(match) != 3 {
			return ""
		}
		attrs, css := match[1], strings.TrimSpace(match[2])
		if !safeEmailStyleAttrs(attrs) || !safeEmailCSSBlock(css) {
			return ""
		}
		styles = append(styles, `<style type="text/css">`+css+`</style>`)
		return ""
	})
	return styles, withoutStyles
}

func safeEmailStyleAttrs(attrs string) bool {
	attrs = strings.ToLower(strings.TrimSpace(attrs))
	if attrs == "" {
		return true
	}
	return regexp.MustCompile(`^\s*type\s*=\s*["']?text/css["']?\s*$`).MatchString(attrs)
}

func safeEmailCSSBlock(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 50000 {
		return false
	}
	unsafe := []string{"expression", "javascript:", "vbscript:", "data:", "behavior", "-moz-binding", "@import", "</", "url("}
	for _, token := range unsafe {
		if strings.Contains(value, token) {
			return false
		}
	}
	return true
}

func safeEmailCSSValue(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 512 {
		return false
	}
	unsafe := []string{"expression", "javascript:", "vbscript:", "data:", "behavior", "-moz-binding", "@import", "</", "url("}
	for _, token := range unsafe {
		if strings.Contains(value, token) {
			return false
		}
	}
	return true
}

func newID(prefix string) string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

func randomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	return s
}

var localPartRe = regexp.MustCompile(`[^a-z0-9._%+\-]`)

func normalizeLocalPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = localPartRe.ReplaceAllString(s, "")
	s = strings.Trim(s, ".")
	return s
}

func normalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.Contains(s, "@") {
		return s
	}
	parts := strings.SplitN(s, "@", 2)
	return normalizeLocalPart(parts[0]) + "@" + normalizeDomain(parts[1])
}

func dedupeEmails(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		email := normalizeEmail(item)
		if email == "" || !strings.Contains(email, "@") || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	return out
}

func jsonEncode(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonDecodeSlice(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]any{"error": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func intBool(v int) bool { return v != 0 }

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func nullableTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

func snippetFrom(text, html string) string {
	s := text
	if strings.TrimSpace(s) == "" {
		s = stripTags(html)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 160 {
		r := []rune(s)
		s = string(r[:160]) + "…"
	}
	return s
}

func stripTags(s string) string {
	s = htmlNonContentTagRe.ReplaceAllString(s, " ")
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				if unicode.IsSpace(r) {
					b.WriteRune(' ')
				} else {
					b.WriteRune(r)
				}
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func badRequest(w http.ResponseWriter, err error) {
	msg := "bad request"
	if err != nil {
		msg = err.Error()
	}
	respondError(w, http.StatusBadRequest, msg)
}

func requireString(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

var errNotFound = errors.New("not found")
