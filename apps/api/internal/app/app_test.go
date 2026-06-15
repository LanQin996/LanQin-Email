package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Addr:              ":0",
		DBPath:            filepath.Join(dir, "lanqin.db"),
		DataDir:           filepath.Join(dir, "data"),
		CookieName:        "lanqin_test",
		SessionTTLHours:   24,
		AdminEmail:        "admin@lanqin.local",
		AdminPassword:     "ChangeMe123!",
		PublicHostname:    "mail.example.test",
		PublicBaseURL:     "http://localhost:5173",
		AllowInsecureHTTP: true,
	}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func startFakeSMTP(t *testing.T) (string, string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleFakeSMTPConn(conn, received)
		}
	}()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return host, port, received
}

func handleFakeSMTPConn(conn net.Conn, received chan<- string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "220 lanqin.test ESMTP\r\n")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO") || strings.HasPrefix(cmd, "HELO"):
			_, _ = io.WriteString(conn, "250-lanqin.test\r\n250 OK\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			_, _ = io.WriteString(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
			var data strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(line, "\r\n") == "." {
					break
				}
				data.WriteString(line)
			}
			select {
			case received <- data.String():
			default:
			}
			_, _ = io.WriteString(conn, "250 OK\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			_, _ = io.WriteString(conn, "221 Bye\r\n")
			return
		default:
			_, _ = io.WriteString(conn, "250 OK\r\n")
		}
	}
}

type testClient struct {
	t      *testing.T
	server *httptest.Server
	cookie *http.Cookie
}

func (c *testClient) do(method, path string, body any, out any) int {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.server.URL+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if strings.Contains(cookie.Name, "lanqin") && cookie.Value != "" {
			c.cookie = cookie
		}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("decode %s %s: %v", method, path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func createTestDomain(t *testing.T, admin *testClient, name string) Domain {
	t.Helper()
	var domain Domain
	if code := admin.do("POST", "/api/admin/domains", map[string]string{"name": name}, &domain); code != http.StatusCreated {
		t.Fatalf("create domain %s code=%d domain=%+v", name, code, domain)
	}
	return domain
}

func createTestMailbox(t *testing.T, admin *testClient, domainID, localPart, displayName, password string, extra map[string]any) Mailbox {
	t.Helper()
	payload := map[string]any{"domainId": domainID, "localPart": localPart, "displayName": displayName, "password": password}
	for key, value := range extra {
		payload[key] = value
	}
	var mailbox Mailbox
	if code := admin.do("POST", "/api/admin/mailboxes", payload, &mailbox); code != http.StatusCreated {
		t.Fatalf("create mailbox %s code=%d mailbox=%+v", localPart, code, mailbox)
	}
	return mailbox
}

func systemSettingsPayload(settings SystemSettings) map[string]any {
	return map[string]any{
		"publicHostname":          settings.PublicHostname,
		"publicBaseUrl":           settings.PublicBaseURL,
		"smtpHost":                settings.SMTPHost,
		"smtpPort":                settings.SMTPPort,
		"smtpUsername":            settings.SMTPUsername,
		"smtpPassword":            "",
		"smtpRequireTls":          settings.SMTPRequireTLS,
		"maildirRoot":             settings.MaildirRoot,
		"maildirScanSeconds":      settings.MaildirScanSeconds,
		"sessionTtlHours":         settings.SessionTTLHours,
		"allowInsecureHttp":       settings.AllowInsecureHTTP,
		"openRegistration":        settings.OpenRegistration,
		"twoFactorEnabled":        settings.TwoFactorEnabled,
		"turnstileEnabled":        settings.TurnstileEnabled,
		"turnstileSiteKey":        settings.TurnstileSiteKey,
		"turnstileSecretKey":      "",
		"catchAllEnabled":         settings.CatchAllEnabled,
		"mailAutoRefresh":         settings.MailAutoRefresh,
		"mailRefreshSeconds":      settings.MailRefreshSeconds,
		"userMailboxApplyEnabled": settings.UserMailboxApplyEnabled,
		"userMailboxDomainIds":    settings.UserMailboxDomainIDs,
		"reservedMailboxPrefixes": settings.ReservedMailboxPrefixes,
	}
}

func TestAuthAdminAndLocalDeliveryFlow(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	domainID := createTestDomain(t, admin, "lanqin.local").ID

	mb1 := createTestMailbox(t, admin, domainID, "alice", "Alice", "Password123!", nil)
	mb2 := createTestMailbox(t, admin, domainID, "bob", "Bob", "Password123!", nil)

	var alias Alias
	if code := admin.do("POST", "/api/admin/aliases", map[string]any{"domainId": domainID, "source": "sales", "destination": mb1.Address}, &alias); code != http.StatusCreated {
		t.Fatalf("alias code=%d alias=%+v", code, alias)
	}

	alice := &testClient{t: t, server: ts}
	if code := alice.do("POST", "/api/auth/login", map[string]string{"email": mb1.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("alice login=%d", code)
	}
	payload := map[string]any{
		"to":          []string{mb2.Address},
		"subject":     "hello bob",
		"html":        "<p>Hello <strong>Bob</strong></p><script>alert(1)</script>",
		"attachments": []map[string]string{{"filename": "note.txt", "contentType": "text/plain", "contentBase64": base64.StdEncoding.EncodeToString([]byte("hi"))}},
	}
	var sent MailMessage
	if code := alice.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated || !sent.HasAttachments {
		t.Fatalf("send code=%d msg=%+v", code, sent)
	}

	bob := &testClient{t: t, server: ts}
	if code := bob.do("POST", "/api/auth/login", map[string]string{"email": mb2.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("bob login=%d", code)
	}
	var list struct {
		Items      []MailMessage `json:"items"`
		NextCursor string        `json:"nextCursor"`
	}
	if code := bob.do("GET", "/api/mail/messages?folder=Inbox", nil, &list); code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("bob inbox code=%d items=%d", code, len(list.Items))
	}
	if strings.Contains(list.Items[0].Snippet, "script") {
		t.Fatalf("message was not sanitized: %q", list.Items[0].Snippet)
	}

	var detail MailMessage
	if code := bob.do("GET", "/api/mail/messages/"+list.Items[0].ID, nil, &detail); code != http.StatusOK || len(detail.Attachments) != 1 || !detail.IsRead {
		t.Fatalf("detail code=%d detail=%+v", code, detail)
	}
	if strings.Contains(detail.BodyHTML, "script") {
		t.Fatalf("html was not sanitized: %s", detail.BodyHTML)
	}

	var ok map[string]any
	if code := bob.do("POST", "/api/mail/messages/"+detail.ID+"/star", map[string]bool{"starred": true}, &ok); code != http.StatusOK {
		t.Fatalf("star code=%d", code)
	}
	if code := bob.do("POST", "/api/mail/messages/"+detail.ID+"/move", map[string]string{"folder": "Archive"}, &ok); code != http.StatusOK {
		t.Fatalf("move code=%d", code)
	}
	var labelUpdate struct {
		Labels []MailLabel `json:"labels"`
	}
	if code := bob.do("POST", "/api/mail/messages/"+detail.ID+"/labels", map[string]string{"name": "重要"}, &labelUpdate); code != http.StatusOK || len(labelUpdate.Labels) != 1 {
		t.Fatalf("add label code=%d labels=%+v", code, labelUpdate.Labels)
	}
	var labels struct {
		Items []MailLabel `json:"items"`
	}
	if code := bob.do("GET", "/api/mail/labels?mailboxId="+mb2.ID, nil, &labels); code != http.StatusOK || len(labels.Items) != 1 || labels.Items[0].MessageCount != 1 {
		t.Fatalf("labels code=%d items=%+v", code, labels.Items)
	}
	var labeled struct {
		Items []MailMessage `json:"items"`
	}
	if code := bob.do("GET", "/api/mail/messages?mailboxId="+mb2.ID+"&labelId="+labels.Items[0].ID, nil, &labeled); code != http.StatusOK || len(labeled.Items) != 1 || labeled.Items[0].ID != detail.ID {
		t.Fatalf("labeled messages code=%d items=%+v", code, labeled.Items)
	}
	if code := bob.do("DELETE", "/api/mail/messages/"+detail.ID+"/labels/"+labels.Items[0].ID, nil, &labelUpdate); code != http.StatusOK || len(labelUpdate.Labels) != 0 {
		t.Fatalf("remove label code=%d labels=%+v", code, labelUpdate.Labels)
	}
	var starred struct {
		Items []MailMessage `json:"items"`
	}
	if code := bob.do("GET", "/api/mail/starred", nil, &starred); code != http.StatusOK || len(starred.Items) != 1 || starred.Items[0].ID != detail.ID || starred.Items[0].Folder != "Archive" {
		t.Fatalf("starred view code=%d items=%+v", code, starred.Items)
	}
	if code := bob.do("DELETE", "/api/mail/messages/"+detail.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete code=%d", code)
	}
}

func TestOpenRegistrationCreatesLoginUserOnly(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var out map[string]any
	if code := client.do("POST", "/api/auth/register", map[string]string{"email": "newuser@example.com", "displayName": "New User", "password": "Password123!"}, &out); code != http.StatusForbidden {
		t.Fatalf("closed registration code=%d body=%v", code, out)
	}

	a.cfg.OpenRegistration = true
	var registered struct {
		User User `json:"user"`
	}
	if code := client.do("POST", "/api/auth/register", map[string]string{"email": "newuser@example.com", "displayName": "New User", "password": "Password123!"}, &registered); code != http.StatusCreated || registered.User.Email != "newuser@example.com" || registered.User.Role != "user" {
		t.Fatalf("register code=%d user=%+v", code, registered.User)
	}
	var me struct {
		User User `json:"user"`
	}
	if code := client.do("GET", "/api/me", nil, &me); code != http.StatusOK || me.User.Email != "newuser@example.com" {
		t.Fatalf("me code=%d user=%+v", code, me.User)
	}
	var mine struct {
		Items []Mailbox `json:"items"`
	}
	if code := client.do("GET", "/api/mail/mailboxes", nil, &mine); code != http.StatusOK || len(mine.Items) != 0 {
		t.Fatalf("registered user should not get implicit mailbox: code=%d items=%+v", code, mine.Items)
	}

	another := &testClient{t: t, server: ts}
	if code := another.do("POST", "/api/auth/login", map[string]string{"email": "newuser@example.com", "password": "Password123!"}, &out); code != http.StatusOK {
		t.Fatalf("login registered user code=%d body=%v", code, out)
	}
}

func TestUserMailboxApplicationUsesAllowedDomainsAndReservedPrefixes(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}
	allowedDomain := createTestDomain(t, admin, "a.com")
	blockedDomain := createTestDomain(t, admin, "b.com")

	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{"email": "person@example.net", "displayName": "Person", "role": "user", "password": "Password123!", "disabled": false}, &created); code != http.StatusCreated {
		t.Fatalf("create user code=%d user=%+v", code, created)
	}

	userClient := &testClient{t: t, server: ts}
	if code := userClient.do("POST", "/api/auth/login", map[string]string{"email": "person@example.net", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login code=%d", code)
	}
	var options MailboxApplyOptions
	if code := userClient.do("GET", "/api/me/mailbox-apply-options", nil, &options); code != http.StatusOK || options.Enabled || len(options.Domains) != 0 {
		t.Fatalf("disabled options code=%d options=%+v", code, options)
	}

	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := systemSettingsPayload(settings)
	update["userMailboxApplyEnabled"] = true
	update["userMailboxDomainIds"] = []string{allowedDomain.ID}
	update["reservedMailboxPrefixes"] = "admin\nroot"
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK || !settings.UserMailboxApplyEnabled || len(settings.UserMailboxDomainIDs) != 1 {
		t.Fatalf("enable apply code=%d settings=%+v", code, settings)
	}

	if code := userClient.do("GET", "/api/me/mailbox-apply-options", nil, &options); code != http.StatusOK || !options.Enabled || len(options.Domains) != 1 || options.Domains[0].ID != allowedDomain.ID {
		t.Fatalf("enabled options code=%d options=%+v", code, options)
	}
	var errBody map[string]any
	if code := userClient.do("POST", "/api/me/mailboxes/apply", map[string]string{"domainId": allowedDomain.ID, "localPart": "admin"}, &errBody); code != http.StatusForbidden {
		t.Fatalf("reserved prefix code=%d body=%v", code, errBody)
	}
	if code := userClient.do("POST", "/api/me/mailboxes/apply", map[string]string{"domainId": blockedDomain.ID, "localPart": "alice"}, &errBody); code != http.StatusForbidden {
		t.Fatalf("blocked domain code=%d body=%v", code, errBody)
	}
	var mailbox Mailbox
	if code := userClient.do("POST", "/api/me/mailboxes/apply", map[string]string{"domainId": allowedDomain.ID, "localPart": "alice", "displayName": "Alice"}, &mailbox); code != http.StatusCreated || mailbox.Address != "alice@a.com" || mailbox.UserID != created.ID {
		t.Fatalf("apply mailbox code=%d mailbox=%+v", code, mailbox)
	}
	var mine struct {
		Items []Mailbox `json:"items"`
	}
	if code := userClient.do("GET", "/api/mail/mailboxes", nil, &mine); code != http.StatusOK || len(mine.Items) != 1 || mine.Items[0].Address != "alice@a.com" {
		t.Fatalf("mine code=%d items=%+v", code, mine.Items)
	}
	if code := userClient.do("POST", "/api/me/mailboxes/apply", map[string]string{"domainId": allowedDomain.ID, "localPart": "alice"}, &errBody); code != http.StatusConflict {
		t.Fatalf("duplicate apply code=%d body=%v", code, errBody)
	}
}

func TestUserCanSelectMultipleMailboxes(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	domainID := createTestDomain(t, admin, "lanqin.local").ID
	createTestMailbox(t, admin, domainID, "admin", "Admin", "ChangeMe123!", map[string]any{"ownerEmail": "admin@lanqin.local", "role": "admin"})

	primary := createTestMailbox(t, admin, domainID, "multi", "Multi", "Password123!", nil)
	secondary := createTestMailbox(t, admin, domainID, "multi-work", "Multi Work", "Password456!", map[string]any{"ownerEmail": primary.Address})
	if primary.UserID != secondary.UserID {
		t.Fatalf("mailboxes were not bound to one user: primary=%s secondary=%s", primary.UserID, secondary.UserID)
	}

	userClient := &testClient{t: t, server: ts}
	if code := userClient.do("POST", "/api/auth/login", map[string]string{"email": primary.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login=%d", code)
	}
	var mine struct {
		Items []Mailbox `json:"items"`
	}
	if code := userClient.do("GET", "/api/mail/mailboxes", nil, &mine); code != http.StatusOK || len(mine.Items) != 2 {
		t.Fatalf("my mailboxes code=%d items=%d", code, len(mine.Items))
	}
	if code := userClient.do("GET", "/api/mail/folders?mailboxId="+secondary.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("folders for selected mailbox code=%d", code)
	}

	var sent MailMessage
	payload := map[string]any{
		"mailboxId": secondary.ID,
		"to":        []string{"admin@lanqin.local"},
		"subject":   "selected mailbox sender",
		"text":      "hello from selected mailbox",
	}
	if code := userClient.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated || sent.From != secondary.Address {
		t.Fatalf("send with selected mailbox code=%d from=%q want=%q", code, sent.From, secondary.Address)
	}
	var adminInbox struct {
		Items []MailMessage `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/messages?folder=Inbox&q=selected%20mailbox%20sender", nil, &adminInbox); code != http.StatusOK || len(adminInbox.Items) != 1 || adminInbox.Items[0].From != secondary.Address {
		t.Fatalf("admin inbox code=%d items=%d first=%+v", code, len(adminInbox.Items), adminInbox.Items)
	}
}

func TestCatchAllStoresUnregisteredMailForAdminOnly(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	domainID := createTestDomain(t, admin, "lanqin.local").ID
	createTestMailbox(t, admin, domainID, "admin", "Admin", "ChangeMe123!", map[string]any{"ownerEmail": "admin@lanqin.local", "role": "admin"})

	payload := map[string]any{
		"to":      []string{"ghost@lanqin.local"},
		"subject": "should be rejected by default",
		"text":    "default disabled",
	}
	var sent MailMessage
	if code := admin.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated {
		t.Fatalf("send disabled catch-all code=%d", code)
	}
	var list struct {
		Items []MailMessage `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/messages?mailboxId=unregistered&q=should%20be%20rejected", nil, &list); code != http.StatusOK || len(list.Items) != 0 {
		t.Fatalf("disabled catch-all should not store unregistered mail: code=%d items=%+v", code, list.Items)
	}

	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := map[string]any{
		"publicHostname":     settings.PublicHostname,
		"publicBaseUrl":      settings.PublicBaseURL,
		"smtpHost":           settings.SMTPHost,
		"smtpPort":           settings.SMTPPort,
		"smtpUsername":       settings.SMTPUsername,
		"smtpPassword":       "",
		"smtpRequireTls":     settings.SMTPRequireTLS,
		"maildirRoot":        settings.MaildirRoot,
		"maildirScanSeconds": settings.MaildirScanSeconds,
		"sessionTtlHours":    settings.SessionTTLHours,
		"allowInsecureHttp":  settings.AllowInsecureHTTP,
		"openRegistration":   settings.OpenRegistration,
		"twoFactorEnabled":   settings.TwoFactorEnabled,
		"turnstileEnabled":   settings.TurnstileEnabled,
		"turnstileSiteKey":   settings.TurnstileSiteKey,
		"turnstileSecretKey": "",
		"catchAllEnabled":    true,
		"mailAutoRefresh":    settings.MailAutoRefresh,
		"mailRefreshSeconds": settings.MailRefreshSeconds,
	}
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK || !settings.CatchAllEnabled {
		t.Fatalf("enable catch-all code=%d settings=%+v", code, settings)
	}

	payload = map[string]any{
		"to":      []string{"ghost@lanqin.local"},
		"subject": "stored for admin only",
		"text":    "unregistered mailbox content",
	}
	if code := admin.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated {
		t.Fatalf("send enabled catch-all code=%d", code)
	}
	if code := admin.do("GET", "/api/admin/messages?mailboxId=unregistered&q=stored%20for%20admin", nil, &list); code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("enabled catch-all admin list code=%d items=%+v", code, list.Items)
	}
	if got := list.Items[0].RecipientAddr; got != "ghost@lanqin.local" {
		t.Fatalf("recipientAddress=%q", got)
	}
}

func TestAdminSMTPTestEndpoint(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startFakeSMTP(t)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	var out map[string]any
	var templates struct {
		Items []MailTemplate `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/mail-templates", nil, &templates); code != http.StatusOK || len(templates.Items) == 0 {
		t.Fatalf("templates code=%d items=%d", code, len(templates.Items))
	}
	var updated MailTemplate
	if code := admin.do("POST", "/api/admin/mail-templates/smtp_test", map[string]string{
		"subject":  "自定义 SMTP 测试",
		"bodyText": "hello {{to}} from {{from}}",
		"bodyHtml": "<p>hello {{to}} from {{from}}</p>",
	}, &updated); code != http.StatusOK || updated.Subject != "自定义 SMTP 测试" {
		t.Fatalf("update template code=%d template=%+v", code, updated)
	}
	if code := admin.do("POST", "/api/admin/settings/test-smtp", map[string]string{"to": "test@example.com"}, &out); code != http.StatusOK {
		t.Fatalf("smtp test code=%d body=%v", code, out)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "From: admin@lanqin.local") || !strings.Contains(body, "To: test@example.com") || !strings.Contains(body, "=?utf-8?q?=E8=87=AA=E5=AE=9A=E4=B9=89_SMTP_=E6=B5=8B=E8=AF=95?=") {
			t.Fatalf("unexpected smtp body: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("smtp test message not received")
	}
}

func TestProfileAndPasswordUpdate(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	var profile struct {
		User User `json:"user"`
	}
	if code := client.do("POST", "/api/me/profile", map[string]string{"displayName": "蓝钦管理员"}, &profile); code != http.StatusOK || profile.User.DisplayName != "蓝钦管理员" {
		t.Fatalf("profile code=%d user=%+v", code, profile.User)
	}

	var ok map[string]any
	if code := client.do("POST", "/api/me/password", map[string]string{"currentPassword": "wrong", "newPassword": "NewPassword123!"}, &ok); code != http.StatusUnauthorized {
		t.Fatalf("wrong password change code=%d", code)
	}
	if code := client.do("POST", "/api/me/password", map[string]string{"currentPassword": "ChangeMe123!", "newPassword": "NewPassword123!"}, &ok); code != http.StatusOK {
		t.Fatalf("password change code=%d body=%v", code, ok)
	}

	fresh := &testClient{t: t, server: ts}
	if code := fresh.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("old password login code=%d", code)
	}
	if code := fresh.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "NewPassword123!"}, &login); code != http.StatusOK {
		t.Fatalf("new password login code=%d", code)
	}
}

func TestUserTwoFactorSetupAndLogin(t *testing.T) {
	a := newTestApp(t)
	a.cfg.TwoFactorEnabled = true
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	var setup struct {
		Secret     string `json:"secret"`
		OtpauthURL string `json:"otpauthUrl"`
	}
	if code := client.do("POST", "/api/me/2fa/setup", map[string]string{}, &setup); code != http.StatusOK || setup.Secret == "" || !strings.HasPrefix(setup.OtpauthURL, "otpauth://totp/") {
		t.Fatalf("setup code=%d setup=%+v", code, setup)
	}

	var out map[string]any
	if code := client.do("POST", "/api/me/2fa/enable", map[string]string{"code": "000000"}, &out); code != http.StatusUnauthorized {
		t.Fatalf("wrong enable code=%d body=%v", code, out)
	}
	code, err := generateTOTP(setup.Secret, a.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var enabled struct {
		User User `json:"user"`
	}
	if status := client.do("POST", "/api/me/2fa/enable", map[string]string{"code": code}, &enabled); status != http.StatusOK || !enabled.User.TwoFactorEnabled {
		t.Fatalf("enable status=%d user=%+v", status, enabled.User)
	}

	fresh := &testClient{t: t, server: ts}
	var challenge struct {
		TwoFactorRequired bool   `json:"twoFactorRequired"`
		ChallengeToken    string `json:"challengeToken"`
	}
	if status := fresh.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &challenge); status != http.StatusOK || !challenge.TwoFactorRequired || challenge.ChallengeToken == "" || fresh.cookie != nil {
		t.Fatalf("challenge status=%d challenge=%+v cookie=%v", status, challenge, fresh.cookie)
	}
	if status := fresh.do("POST", "/api/auth/login", map[string]string{"challengeToken": challenge.ChallengeToken, "twoFactorCode": "000000"}, &out); status != http.StatusUnauthorized {
		t.Fatalf("wrong challenge status=%d body=%v", status, out)
	}
	code, err = generateTOTP(setup.Secret, a.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if status := fresh.do("POST", "/api/auth/login", map[string]string{"challengeToken": challenge.ChallengeToken, "twoFactorCode": code}, &login); status != http.StatusOK || fresh.cookie == nil {
		t.Fatalf("2fa login status=%d body=%v cookie=%v", status, login, fresh.cookie)
	}
	if status := fresh.do("POST", "/api/me/2fa/disable", map[string]string{"code": code}, &enabled); status != http.StatusOK || enabled.User.TwoFactorEnabled {
		t.Fatalf("disable status=%d user=%+v", status, enabled.User)
	}
}

func TestDNSRecords(t *testing.T) {
	a := newTestApp(t)
	domainID, err := a.createDomainTx(context.Background(), nil, "lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.domainByID(context.Background(), domainID)
	if err != nil {
		t.Fatal(err)
	}
	records := a.dnsRecordsFor(d)
	if len(records) != 4 {
		t.Fatalf("records=%d", len(records))
	}
	if records[0].Type != "MX" || !strings.Contains(records[2].Value, "v=DKIM1") {
		t.Fatalf("unexpected records: %+v", records)
	}
}

func TestMaildirSyncImportsRFC822(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	root := t.TempDir()
	a.cfg.MaildirRoot = root
	domainID, err := a.createDomainTx(ctx, nil, "lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	adminUser, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.createMailbox(ctx, adminUser.ID, domainID, "admin", "Admin", "ChangeMe123!", 1024, "active"); err != nil {
		t.Fatal(err)
	}

	mailboxes, err := a.maildirMailboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var admin maildirMailbox
	for _, mb := range mailboxes {
		if mb.Address == "admin@lanqin.local" {
			admin = mb
			break
		}
	}
	if admin.ID == "" {
		t.Fatal("admin mailbox not found")
	}

	dir := filepath.Join(root, admin.Domain, admin.LocalPart, "Maildir", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: admin@lanqin.local",
		"Subject: Maildir import test",
		"Message-Id: <maildir-import@example.test>",
		"Date: Sat, 13 Jun 2026 13:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"hello from maildir",
	}, "\r\n")
	if err := os.WriteFile(filepath.Join(dir, "1749819600.M1P1.test"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("imported=%d, want 1", count)
	}
	count, err = a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("second import=%d, want duplicate skip", count)
	}

	var subject, body string
	err = a.db.QueryRow(`SELECT subject, body_text FROM messages WHERE mailbox_id=? AND message_id='<maildir-import@example.test>'`, admin.ID).Scan(&subject, &body)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Maildir import test" || !strings.Contains(body, "hello from maildir") {
		t.Fatalf("unexpected imported message subject=%q body=%q", subject, body)
	}
}

func mustDefaultDomainID(t *testing.T, a *App) string {
	t.Helper()
	var id string
	if err := a.db.QueryRow(`SELECT id FROM domains LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
