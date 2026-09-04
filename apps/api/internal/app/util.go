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
	"mime"
	"net"
	"net/http"
	netmail "net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
)

// isPublicUnicastIP reports whether ip is a globally routable unicast address.
//
// Every outbound-connection guard shares this predicate on purpose: when the
// rules lived in one copy per feature they drifted apart, and the weaker copy
// silently became the SSRF hole.
func isPublicUnicastIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsGlobalUnicast() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
}

// safeAttachmentContentType reduces a stored MIME type to a well-formed media
// type. The value originates from the sender's headers, so it is untrusted;
// parameters are dropped because responses are always sent as a download.
func safeAttachmentContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

// attachmentDisposition builds a Content-Disposition value that survives
// non-ASCII filenames. The quoted form is kept as an ASCII fallback for old
// clients and the RFC 5987 form carries the real name.
//
// Do not use mime.QEncoding here: RFC 2047 encoded-words are an email header
// construct and browsers render them literally in HTTP responses.
func attachmentDisposition(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = "attachment"
	}
	if runes := []rune(filename); len(runes) > 120 {
		filename = string(runes[:120])
	}
	var fallback strings.Builder
	fallback.Grow(len(filename))
	for _, r := range filename {
		switch {
		case r < 0x20 || r == 0x7f:
			// Drop control characters, including CR and LF.
		case r > 0x7e || r == '"' || r == '\\':
			fallback.WriteByte('_')
		default:
			fallback.WriteRune(r)
		}
	}
	ascii := strings.TrimSpace(fallback.String())
	if ascii == "" {
		ascii = "attachment"
	}
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + rfc5987Escape(filename)
}

// rfc5987Escape percent-encodes everything outside the unreserved set, which is
// always a valid subset of RFC 5987 attr-char.
func rfc5987Escape(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// writeAttachmentHeaders applies the download headers shared by every
// attachment endpoint. nosniff is defence in depth: Content-Disposition already
// prevents rendering, but the media type is attacker-controlled.
func writeAttachmentHeaders(w http.ResponseWriter, contentType, filename string, size int64) {
	w.Header().Set("Content-Type", safeAttachmentContentType(contentType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", attachmentDisposition(filename))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
}

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

// domainRe mirrors localPartRe for the right-hand side of an address. Without it
// normalizeDomain would let embedded CR/LF through, and every recipient string
// eventually reaches BuildMIME, which writes headers verbatim.
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9\-]*[a-z0-9])?)*$`)

// validRecipientAddress reports whether an already-normalized address is safe to
// place in a mail header.
//
// This deliberately follows normalizeRuleForwardAddress: parse, re-normalize, then
// require the parse to agree with the normalized form. Any address that survives a
// round-trip through net/mail cannot contain a bare CR or LF.
func validRecipientAddress(email string) bool {
	if email == "" || len(email) > 320 {
		return false
	}
	if strings.ContainsAny(email, "\r\n") {
		return false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return false
	}
	if !domainRe.MatchString(domain) {
		return false
	}
	parsed, err := netmail.ParseAddress(email)
	return err == nil && strings.EqualFold(parsed.Address, email)
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

// dedupeEmails normalizes, validates and de-duplicates recipient addresses.
//
// Invalid entries are dropped rather than reported: callers treat an empty result
// as "no recipients" and reject the send, so a request consisting only of malformed
// addresses still fails loudly.
func dedupeEmails(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		email := normalizeEmail(item)
		if !validRecipientAddress(email) || seen[email] {
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

const maxJSONRequestBodyBytes int64 = 64 << 20

func decodeJSON(r *http.Request, dst any) error {
	return decodeJSONWithLimit(r, dst, maxJSONRequestBodyBytes)
}

func decodeJSONWithLimit(r *http.Request, dst any, limit int64) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
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
