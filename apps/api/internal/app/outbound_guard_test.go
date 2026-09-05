package app

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsPublicUnicastIPRejectsInternalRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",        // loopback
		"::1",              // IPv6 loopback
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"10.0.0.1",         // RFC1918
		"172.16.0.1",       // RFC1918
		"192.168.1.1",      // RFC1918
		"fd00::1",          // RFC4193 unique local
		"169.254.169.254",  // cloud metadata (link-local)
		"fe80::1",          // IPv6 link-local
		"224.0.0.1",        // IPv4 multicast
		"ff02::1",          // IPv6 multicast
		"0.0.0.0",          // unspecified
		"::",               // IPv6 unspecified
	}
	for _, item := range blocked {
		ip := net.ParseIP(item)
		if ip == nil {
			t.Fatalf("test data %q is not a valid IP", item)
		}
		if isPublicUnicastIP(ip) {
			t.Errorf("isPublicUnicastIP(%s) = true, want false", item)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"}
	for _, item := range allowed {
		if !isPublicUnicastIP(net.ParseIP(item)) {
			t.Errorf("isPublicUnicastIP(%s) = false, want true", item)
		}
	}

	if isPublicUnicastIP(nil) {
		t.Error("isPublicUnicastIP(nil) = true, want false")
	}
}

// TestExternalIMAPDialerRejectsRebinding covers the DNS rebinding fix: the guard
// must run on the address actually being dialed, not on a separately resolved
// hostname. Control is what closes that window, so it is exercised directly.
func TestExternalIMAPDialerRejectsRebinding(t *testing.T) {
	a := &App{cfg: Config{}}
	dialer := a.externalIMAPDialer()
	if dialer.Control == nil {
		t.Fatal("dialer must install a Control hook when private hosts are disallowed")
	}

	rebound := []string{"127.0.0.1:993", "169.254.169.254:80", "10.1.2.3:143", "[::1]:993"}
	for _, address := range rebound {
		if err := dialer.Control("tcp", address, nil); err == nil {
			t.Errorf("Control accepted %s, want rejection", address)
		}
	}
	if err := dialer.Control("tcp", "1.1.1.1:993", nil); err != nil {
		t.Errorf("Control rejected public address: %v", err)
	}
	if err := dialer.Control("tcp", "not-an-address", nil); err == nil {
		t.Error("Control accepted an unparsable address, want rejection")
	}
}

func TestExternalIMAPDialerHonoursPrivateHostOptIn(t *testing.T) {
	a := &App{cfg: Config{ExternalIMAPAllowPrivateHosts: true}}
	if a.externalIMAPDialer().Control != nil {
		t.Error("Control must be absent when private hosts are explicitly allowed")
	}
}

func TestSafeAttachmentContentTypeFallsBackOnJunk(t *testing.T) {
	cases := map[string]string{
		"":                          "application/octet-stream",
		"   ":                       "application/octet-stream",
		"not a media type at all":   "application/octet-stream",
		"text/html":                 "text/html",
		"TEXT/HTML":                 "text/html",
		"text/plain; charset=utf-8": "text/plain",
		"application/pdf":           "application/pdf",
	}
	for input, want := range cases {
		if got := safeAttachmentContentType(input); got != want {
			t.Errorf("safeAttachmentContentType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAttachmentDispositionEncodesNonASCII(t *testing.T) {
	got := attachmentDisposition("季度报告.pdf")
	if !strings.HasPrefix(got, "attachment; ") {
		t.Fatalf("disposition must start with attachment: %q", got)
	}
	// The quoted fallback has to stay ASCII for clients that ignore filename*.
	for i := 0; i < len(got); i++ {
		if got[i] > 0x7e {
			t.Fatalf("disposition contains a raw non-ASCII byte: %q", got)
		}
	}
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("disposition is missing the RFC 5987 form: %q", got)
	}
	// %E5%AD%A3 is the UTF-8 encoding of 季.
	if !strings.Contains(got, "%E5%AD%A3") {
		t.Errorf("disposition did not percent-encode the name: %q", got)
	}
	if strings.Contains(got, "=?utf-8?") {
		t.Errorf("disposition used RFC 2047 encoded-words, which browsers show literally: %q", got)
	}
}

func TestAttachmentDispositionStripsHeaderBreakers(t *testing.T) {
	got := attachmentDisposition("a\r\nX-Injected: 1\"b\\c.txt")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("disposition leaked a line break: %q", got)
	}
	quoted := got[len(`attachment; filename="`):]
	quoted = quoted[:strings.IndexByte(quoted, '"')]
	if strings.ContainsAny(quoted, `"\`) {
		t.Errorf("quoted fallback still contains quoting characters: %q", quoted)
	}
	if attachmentDisposition("") == "" {
		t.Error("empty filename must still yield a disposition")
	}
}

// TestValidRecipientAddressRejectsHeaderInjection covers the report's H1: a
// recipient carrying CR/LF used to reach BuildMIME verbatim and split into extra
// headers, letting an authenticated user forge Reply-To on locally delivered mail.
func TestValidRecipientAddressRejectsHeaderInjection(t *testing.T) {
	rejected := []string{
		"victim@example.com\r\nReply-To: attacker@evil.example",
		"victim@example.com\nBcc: attacker@evil.example",
		"victim@exa mple.com",
		"victim@",
		"@example.com",
		"victim@-example.com",
		"victim@example-.com",
		"victim@exa_mple.com",
		"",
	}
	for _, item := range rejected {
		if validRecipientAddress(normalizeEmail(item)) {
			t.Errorf("validRecipientAddress(%q) = true, want false", item)
		}
	}
	accepted := []string{"victim@example.com", "a.b+c@sub.example.co.uk", "x@e.io"}
	for _, item := range accepted {
		if !validRecipientAddress(normalizeEmail(item)) {
			t.Errorf("validRecipientAddress(%q) = false, want true", item)
		}
	}

	// Called directly, the predicate must also refuse a bare CR or LF: normalizeEmail
	// happens to trim those at the edges, but the guard cannot rely on its caller.
	for _, item := range []string{"victim@example.com\r", "victim@example.com\n", "a\rb@example.com"} {
		if validRecipientAddress(item) {
			t.Errorf("validRecipientAddress(%q) = true, want false", item)
		}
	}
}
func TestDedupeEmailsDropsInjectedRecipients(t *testing.T) {
	got := dedupeEmails([]string{
		"ok@example.com",
		"victim@example.com\r\nReply-To: attacker@evil.example",
		"OK@EXAMPLE.COM",
	})
	if len(got) != 1 || got[0] != "ok@example.com" {
		t.Fatalf("dedupeEmails = %+v, want [ok@example.com]", got)
	}
}

// TestBuildMIMEStripsHeaderBreaks is defence in depth: even if a caller bypasses
// dedupeEmails, no header value may introduce a new line.
func TestBuildMIMEStripsHeaderBreaks(t *testing.T) {
	raw, err := BuildMIME(MIMEMessage{
		From:      "sender@example.com",
		To:        []string{"victim@example.com\r\nReply-To: attacker@evil.example"},
		Subject:   "probe",
		Text:      "body",
		MessageID: "<probe@example.com>",
		Date:      time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	headers, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	var toLine string
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "reply-to:") {
			t.Fatalf("injected header survived BuildMIME:\n%s", headers)
		}
		if strings.HasPrefix(line, "To: ") {
			toLine = line
		}
	}
	// The injected text must survive only as inert content of the To line, never as
	// its own header. Exact spacing is not pinned; staying on one line is the point.
	if !strings.Contains(toLine, "victim@example.com") || !strings.Contains(toLine, "Reply-To: attacker@evil.example") {
		t.Errorf("To header lost the folded value: %q", toLine)
	}
}
