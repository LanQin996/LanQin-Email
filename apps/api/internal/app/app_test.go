package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	smtpclient "github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
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
	return newTestAppWithConfig(t, cfg)
}

func newTestAppWithConfig(t *testing.T, cfg Config) *App {
	t.Helper()
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func stopTestWorkers(a *App) {
	if a != nil && a.workerCancel != nil {
		a.workerCancel()
		a.workerWG.Wait()
	}
}

func defaultAdminUserAndMailbox(t *testing.T, a *App) (*User, *Mailbox) {
	t.Helper()
	ctx := context.Background()
	user, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := a.mailboxByAddress(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	return user, mb
}

func writeTestCertificateFiles(t *testing.T, hostname string) (string, string) {
	t.Helper()
	if strings.TrimSpace(hostname) == "" {
		hostname = "localhost"
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname, "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "cert.pem")
	keyPath := filepath.Join(filepath.Dir(certPath), "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
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

func startCapturingSMTP(t *testing.T, capacity int) (string, string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, capacity)
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
	bearer string
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
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
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

func createTestAPIToken(t *testing.T, client *testClient, name string) string {
	t.Helper()
	var resp struct {
		Token string   `json:"token"`
		Item  APIToken `json:"item"`
	}
	if code := client.do("POST", "/api/me/api-tokens", map[string]string{"name": name}, &resp); code != http.StatusCreated {
		t.Fatalf("create api token code=%d resp=%+v", code, resp)
	}
	if resp.Token == "" || resp.Item.ID == "" || resp.Item.Name != name {
		t.Fatalf("api token response=%+v", resp)
	}
	return resp.Token
}

func updateRegularPermissionGroup(t *testing.T, admin *testClient, permissions []string) PermissionGroup {
	t.Helper()
	var group PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups/"+PermissionGroupRegular, map[string]any{
		"name":        "Regular Users",
		"description": "Default permissions for regular users",
		"permissions": permissions,
	}, &group); code != http.StatusOK {
		t.Fatalf("update regular permission group code=%d group=%+v", code, group)
	}
	return group
}

func updateRegularPermissionGroupWithLimits(t *testing.T, admin *testClient, permissions []string, limits PermissionLimits) PermissionGroup {
	t.Helper()
	var group PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups/"+PermissionGroupRegular, map[string]any{
		"name":        "Regular Users",
		"description": "Default permissions for regular users",
		"permissions": permissions,
		"limits":      limits,
	}, &group); code != http.StatusOK {
		t.Fatalf("update regular permission group limits code=%d group=%+v", code, group)
	}
	return group
}

func systemSettingsPayload(settings SystemSettings) map[string]any {
	return map[string]any{
		"publicHostname":                  settings.PublicHostname,
		"publicBaseUrl":                   settings.PublicBaseURL,
		"smtpHost":                        settings.SMTPHost,
		"smtpPort":                        settings.SMTPPort,
		"smtpUsername":                    settings.SMTPUsername,
		"smtpPassword":                    "",
		"smtpRequireTls":                  settings.SMTPRequireTLS,
		"maildirRoot":                     settings.MaildirRoot,
		"maildirScanSeconds":              settings.MaildirScanSeconds,
		"sessionTtlHours":                 settings.SessionTTLHours,
		"allowInsecureHttp":               settings.AllowInsecureHTTP,
		"openRegistration":                settings.OpenRegistration,
		"twoFactorEnabled":                settings.TwoFactorEnabled,
		"turnstileEnabled":                settings.TurnstileEnabled,
		"turnstileSiteKey":                settings.TurnstileSiteKey,
		"turnstileSecretKey":              "",
		"catchAllEnabled":                 settings.CatchAllEnabled,
		"mailAutoRefresh":                 settings.MailAutoRefresh,
		"mailRefreshSeconds":              settings.MailRefreshSeconds,
		"userMailboxApplyEnabled":         settings.UserMailboxApplyEnabled,
		"userMailboxDomainIds":            settings.UserMailboxDomainIDs,
		"reservedMailboxPrefixes":         settings.ReservedMailboxPrefixes,
		"externalImapEnabled":             settings.ExternalIMAPEnabled,
		"externalImapSecretKey":           "",
		"externalImapSyncSeconds":         settings.ExternalIMAPSyncSeconds,
		"externalImapAllowPrivateHosts":   settings.ExternalIMAPAllowPrivateHosts,
		"externalImapGmailClientId":       settings.ExternalIMAPGmailClientID,
		"externalImapGmailClientSecret":   "",
		"externalImapOutlookClientId":     settings.ExternalIMAPOutlookClientID,
		"externalImapOutlookClientSecret": "",
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

	var domainList = struct {
		Items []Domain `json:"items"`
	}{}
	if code := admin.do("GET", "/api/admin/domains", nil, &domainList); code != http.StatusOK || len(domainList.Items) == 0 {
		t.Fatalf("list domains code=%d items=%+v", code, domainList.Items)
	}
	domainID := domainList.Items[0].ID

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

func TestExternalIMAPAccountEncryptsPasswordAndDoesNotReturnSecret(t *testing.T) {
	dir := t.TempDir()
	a := newTestAppWithConfig(t, Config{
		Addr:                          ":0",
		DBPath:                        filepath.Join(dir, "lanqin.db"),
		DataDir:                       filepath.Join(dir, "data"),
		CookieName:                    "lanqin_test",
		SessionTTLHours:               24,
		AdminEmail:                    "admin@lanqin.local",
		AdminPassword:                 "ChangeMe123!",
		PublicHostname:                "mail.example.test",
		PublicBaseURL:                 "http://localhost:5173",
		AllowInsecureHTTP:             true,
		ExternalIMAPEnabled:           true,
		ExternalIMAPSecretKey:         "test-secret",
		ExternalIMAPAllowPrivateHosts: true,
	})
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mb := defaultAdminUserAndMailbox(t, a)
	var created ExternalIMAPAccount
	payload := map[string]any{"mailboxId": mb.ID, "name": "Gmail", "host": "imap.gmail.com", "port": 993, "tlsMode": "tls", "username": "user@gmail.com", "password": "app-password", "storageMode": "remote", "syncReadState": true, "enabled": true}
	if code := admin.do("POST", "/api/me/external-imap-accounts", payload, &created); code != http.StatusCreated {
		t.Fatalf("create external imap code=%d account=%+v", code, created)
	}
	raw, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "app-password") {
		t.Fatalf("external account response leaked password: %s", string(raw))
	}
	var ciphertext string
	if err := a.db.QueryRow(`SELECT password_ciphertext FROM external_imap_accounts WHERE id=?`, created.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "" || ciphertext == "app-password" {
		t.Fatalf("password was not encrypted: %q", ciphertext)
	}
	plain, err := a.decryptExternalIMAPPassword(ciphertext)
	if err != nil || plain != "app-password" {
		t.Fatalf("decrypt password=%q err=%v", plain, err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if code := admin.do("GET", "/api/me/external-imap-accounts?mailboxId="+mb.ID, nil, &list); code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("list external imap code=%d items=%+v", code, list.Items)
	}
	if _, ok := list.Items[0]["password"]; ok {
		t.Fatalf("list response exposed password field: %+v", list.Items[0])
	}
}

func TestExternalIMAPDisabledByDefaultAndAdminSettings(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mb := defaultAdminUserAndMailbox(t, a)
	payload := map[string]any{"mailboxId": mb.ID, "name": "Disabled", "host": "imap.example.com", "port": 993, "tlsMode": "tls", "username": "user@example.com", "password": "secret", "storageMode": "remote"}
	var body map[string]any
	if code := admin.do("POST", "/api/me/external-imap-accounts", payload, &body); code != http.StatusForbidden {
		t.Fatalf("external imap should be disabled by default code=%d body=%v", code, body)
	}
	var public PublicSettings
	if code := admin.do("GET", "/api/public/settings", nil, &public); code != http.StatusOK || public.ExternalIMAPEnabled {
		t.Fatalf("public settings should expose disabled external imap code=%d settings=%+v", code, public)
	}
	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := systemSettingsPayload(settings)
	update["externalImapEnabled"] = true
	if code := admin.do("POST", "/api/admin/settings", update, &body); code != http.StatusBadRequest {
		t.Fatalf("enable without secret should fail code=%d body=%v", code, body)
	}
	update["externalImapSecretKey"] = "test-secret"
	update["externalImapSyncSeconds"] = 120
	update["externalImapAllowPrivateHosts"] = true
	update["externalImapGmailClientId"] = "gmail-client"
	update["externalImapGmailClientSecret"] = "gmail-secret"
	update["externalImapOutlookClientId"] = "outlook-client"
	update["externalImapOutlookClientSecret"] = "outlook-secret"
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK || !settings.ExternalIMAPEnabled || !settings.ExternalIMAPSecretSet || settings.ExternalIMAPSyncSeconds != 120 || !settings.ExternalIMAPAllowPrivateHosts || !settings.ExternalIMAPGmailClientSecretSet || !settings.ExternalIMAPOutlookClientSecretSet {
		t.Fatalf("enable external imap code=%d settings=%+v", code, settings)
	}
	if settings.ExternalIMAPGmailClientID != "gmail-client" || settings.ExternalIMAPOutlookClientID != "outlook-client" {
		t.Fatalf("oauth client ids not saved: %+v", settings)
	}
	if a.cfg.ExternalIMAPSecretKey != "test-secret" || a.cfg.ExternalIMAPGmailClientSecret != "gmail-secret" || a.cfg.ExternalIMAPOutlookClientSecret != "outlook-secret" {
		t.Fatalf("secret settings not persisted in config")
	}
	if code := admin.do("GET", "/api/public/settings", nil, &public); code != http.StatusOK || !public.ExternalIMAPEnabled {
		t.Fatalf("public settings should expose enabled external imap code=%d settings=%+v", code, public)
	}
}

func TestExternalIMAPRejectsPrivateHostsByDefault(t *testing.T) {
	a := newTestApp(t)
	a.cfg.ExternalIMAPEnabled = true
	a.cfg.ExternalIMAPSecretKey = "test-secret"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mb := defaultAdminUserAndMailbox(t, a)
	var out map[string]any
	payload := map[string]any{"mailboxId": mb.ID, "name": "Local", "host": "127.0.0.1", "port": 143, "tlsMode": "plain", "username": "local", "password": "secret", "storageMode": "remote"}
	if code := admin.do("POST", "/api/me/external-imap-accounts", payload, &out); code != http.StatusBadRequest {
		t.Fatalf("private host should be rejected code=%d body=%v", code, out)
	}
}

func TestExternalIMAPOAuthStateDoesNotDefaultToLocalMailbox(t *testing.T) {
	dir := t.TempDir()
	a := newTestAppWithConfig(t, Config{
		Addr:                            ":0",
		DBPath:                          filepath.Join(dir, "lanqin.db"),
		DataDir:                         filepath.Join(dir, "data"),
		CookieName:                      "lanqin_test",
		SessionTTLHours:                 24,
		AdminEmail:                      "admin@lanqin.local",
		AdminPassword:                   "ChangeMe123!",
		PublicHostname:                  "mail.example.test",
		PublicBaseURL:                   "http://localhost:5173",
		AllowInsecureHTTP:               true,
		ExternalIMAPEnabled:             true,
		ExternalIMAPSecretKey:           "test-secret",
		ExternalIMAPOutlookClientID:     "client-id",
		ExternalIMAPOutlookClientSecret: "client-secret",
	})
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mb := defaultAdminUserAndMailbox(t, a)

	var start struct {
		URL string `json:"url"`
	}
	if code := admin.do("POST", "/api/me/external-imap-oauth/outlook/start", map[string]any{"mailboxId": mb.ID, "storageMode": "local", "syncReadState": true, "enabled": true}, &start); code != http.StatusOK {
		t.Fatalf("start oauth code=%d url=%q", code, start.URL)
	}
	stateValue := mustOAuthStateFromURL(t, start.URL)
	state, err := a.decryptExternalIMAPOAuthState(stateValue)
	if err != nil {
		t.Fatal(err)
	}
	if state.Email != "" {
		t.Fatalf("oauth state defaulted to local mailbox email: %q", state.Email)
	}

	if code := admin.do("POST", "/api/me/external-imap-oauth/outlook/start", map[string]any{"mailboxId": mb.ID, "email": "User@Example.COM", "storageMode": "remote", "syncReadState": true, "enabled": true}, &start); code != http.StatusOK {
		t.Fatalf("start oauth with email code=%d url=%q", code, start.URL)
	}
	stateValue = mustOAuthStateFromURL(t, start.URL)
	state, err = a.decryptExternalIMAPOAuthState(stateValue)
	if err != nil {
		t.Fatal(err)
	}
	if state.Email != "user@example.com" {
		t.Fatalf("oauth state did not preserve requested external email, got %q", state.Email)
	}
}

func TestExternalIMAPOAuthEmailFromIDToken(t *testing.T) {
	token := (&oauth2.Token{AccessToken: "access"}).WithExtra(map[string]any{
		"id_token": testIDToken(map[string]any{"preferred_username": "User@Example.COM"}),
	})
	email, err := externalIMAPOAuthEmail(externalIMAPOAuthOutlook, token)
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@example.com" {
		t.Fatalf("unexpected outlook oauth email %q", email)
	}

	token = (&oauth2.Token{AccessToken: "access"}).WithExtra(map[string]any{
		"id_token": testIDToken(map[string]any{"email": "Person@Gmail.COM"}),
	})
	email, err = externalIMAPOAuthEmail(externalIMAPOAuthGmail, token)
	if err != nil {
		t.Fatal(err)
	}
	if email != "person@gmail.com" {
		t.Fatalf("unexpected gmail oauth email %q", email)
	}
}

func TestExternalIMAPXOAUTH2ClientFormat(t *testing.T) {
	client := newExternalIMAPXOAUTH2Client("user@example.com", "access-token")
	mech, initialResponse, err := client.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mech != "XOAUTH2" {
		t.Fatalf("mechanism=%q, want XOAUTH2", mech)
	}
	want := "user=user@example.com\x01auth=Bearer access-token\x01\x01"
	if string(initialResponse) != want {
		t.Fatalf("initial response=%q, want %q", string(initialResponse), want)
	}
}

func mustOAuthStateFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("oauth url missing state: %s", rawURL)
	}
	return state
}

func testIDToken(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestExternalIMAPAccountOwnershipIsolation(t *testing.T) {
	dir := t.TempDir()
	a := newTestAppWithConfig(t, Config{
		Addr:                          ":0",
		DBPath:                        filepath.Join(dir, "lanqin.db"),
		DataDir:                       filepath.Join(dir, "data"),
		CookieName:                    "lanqin_test",
		SessionTTLHours:               24,
		AdminEmail:                    "admin@lanqin.local",
		AdminPassword:                 "ChangeMe123!",
		PublicHostname:                "mail.example.test",
		PublicBaseURL:                 "http://localhost:5173",
		AllowInsecureHTTP:             true,
		ExternalIMAPEnabled:           true,
		ExternalIMAPSecretKey:         "test-secret",
		ExternalIMAPAllowPrivateHosts: true,
	})
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login admin code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	owner := createTestMailbox(t, admin, domainID, "ximap-owner", "Owner", "Password123!", nil)
	other := createTestMailbox(t, admin, domainID, "ximap-other", "Other", "Password123!", nil)
	ownerClient := &testClient{t: t, server: ts}
	if code := ownerClient.do("POST", "/api/auth/login", map[string]string{"email": owner.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("login owner code=%d", code)
	}
	otherClient := &testClient{t: t, server: ts}
	if code := otherClient.do("POST", "/api/auth/login", map[string]string{"email": other.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("login other code=%d", code)
	}
	var created ExternalIMAPAccount
	payload := map[string]any{"mailboxId": owner.ID, "name": "Owner external", "host": "imap.example.com", "port": 993, "tlsMode": "tls", "username": "owner@example.com", "password": "secret", "storageMode": "remote"}
	if code := ownerClient.do("POST", "/api/me/external-imap-accounts", payload, &created); code != http.StatusCreated {
		t.Fatalf("create code=%d account=%+v", code, created)
	}
	var denied map[string]any
	if code := otherClient.do("POST", "/api/me/external-imap-accounts/"+created.ID, map[string]any{"mailboxId": other.ID, "name": "steal", "host": "imap.example.com", "port": 993, "tlsMode": "tls", "username": "other@example.com", "storageMode": "remote"}, &denied); code != http.StatusNotFound {
		t.Fatalf("cross-user update should be hidden code=%d body=%v", code, denied)
	}
	if code := otherClient.do("DELETE", "/api/me/external-imap-accounts/"+created.ID, nil, &denied); code != http.StatusNotFound {
		t.Fatalf("cross-user delete should be hidden code=%d body=%v", code, denied)
	}
}

func TestParseMailAuthenticationResults(t *testing.T) {
	header := textproto.MIMEHeader{}
	header.Add("Authentication-Results", "mx.example.test; spf=pass smtp.mailfrom=sender.example; dkim=fail (bad signature) header.d=sender.example; dmarc=none")
	header.Add("Received-SPF", "softfail (mx.example.test: transitioning domain) client-ip=192.0.2.10; envelope-from=sender@example.test")

	auth := parseMailAuthentication(header)
	if auth.SPF != "pass" || auth.DKIM != "fail" || auth.DMARC != "none" {
		t.Fatalf("unexpected auth summary: %+v", auth)
	}
	if !strings.Contains(auth.AuthenticationResults, "spf=pass") || !strings.Contains(auth.ReceivedSPF, "softfail") {
		t.Fatalf("raw auth headers not preserved: %+v", auth)
	}

	unknown := parseMailAuthentication(textproto.MIMEHeader{})
	if unknown.SPF != "unknown" || unknown.DKIM != "unknown" || unknown.DMARC != "unknown" {
		t.Fatalf("missing headers should be unknown, got %+v", unknown)
	}
}

func TestSnippetFromHTMLIgnoresStyleContent(t *testing.T) {
	html := `<html><head><style>body { margin: 0; padding: 24px; background: #f4f4f5; }</style><title>Hidden title</title></head><body><p>蓝钦AI 余额充值成功</p></body></html>`
	got := snippetFrom("", html)
	if strings.Contains(got, "body {") || strings.Contains(got, "margin:") || strings.Contains(got, "Hidden title") {
		t.Fatalf("snippet kept non-content html text: %q", got)
	}
	if !strings.Contains(got, "蓝钦AI 余额充值成功") {
		t.Fatalf("snippet missing body text: %q", got)
	}
}

func TestRebuildHTMLOnlyMessageSnippetsDropsStyleContent(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	_, mb := defaultAdminUserAndMailbox(t, a)
	folderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	bodyHTML := `<html><head><style>body { margin: 0; padding: 24px; background: #f4f4f5; }</style></head><body><p>hello readable body</p></body></html>`
	if _, err := a.db.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,folder_id,recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "msg_css_snippet", mb.ID, folderID, "", newID("uid"), "<css-snippet@example.test>", "css", mb.Address, "", "[]", "[]", "[]", now, now, "body { margin: 0; padding: 24px; }", "", bodyHTML, 1, 0, 0, int64(len(bodyHTML)), now, now); err != nil {
		t.Fatal(err)
	}
	if err := a.rebuildHTMLOnlyMessageSnippets(ctx); err != nil {
		t.Fatal(err)
	}
	var snippet string
	if err := a.db.QueryRowContext(ctx, `SELECT snippet FROM messages WHERE id='msg_css_snippet'`).Scan(&snippet); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snippet, "body {") || strings.Contains(snippet, "margin:") {
		t.Fatalf("snippet was not rebuilt: %q", snippet)
	}
	if snippet != "hello readable body" {
		t.Fatalf("snippet=%q, want body text", snippet)
	}
}

func TestMailRulesConditionGroupsAndActions(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	sender := createTestMailbox(t, admin, domainID, "rule-sender", "Rule Sender", "Password123!", nil)
	recipient := createTestMailbox(t, admin, domainID, "rule-recipient", "Rule Recipient", "Password123!", nil)

	rcpt := &testClient{t: t, server: ts}
	if code := rcpt.do("POST", "/api/auth/login", map[string]string{"email": recipient.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("recipient login=%d", code)
	}
	var rule MailRule
	rulePayload := map[string]any{
		"mailboxId":  recipient.ID,
		"name":       "priority archive",
		"matchMode":  "all",
		"conditions": []map[string]any{{"matchMode": "any", "conditions": []map[string]string{{"field": "from", "operator": "contains", "value": sender.Address}, {"field": "subject", "operator": "contains", "value": "urgent"}}}, {"field": "cc", "operator": "contains", "value": "lead@example.test"}, {"field": "attachment", "operator": "contains", "value": "plan.pdf"}, {"field": "size", "operator": "gte", "value": "10"}, {"field": "date", "operator": "after", "value": "2020-01-01"}},
		"actions":    []map[string]string{{"type": "label", "value": "Priority"}, {"type": "move", "value": "Archive"}, {"type": "mark-read"}, {"type": "star"}},
	}
	if code := rcpt.do("POST", "/api/me/rules", rulePayload, &rule); code != http.StatusCreated {
		t.Fatalf("create rule code=%d rule=%+v", code, rule)
	}

	senderClient := &testClient{t: t, server: ts}
	if code := senderClient.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("sender login=%d", code)
	}
	var sent MailMessage
	if code := senderClient.do("POST", "/api/mail/send", map[string]any{
		"to":          []string{recipient.Address},
		"cc":          []string{"lead@example.test"},
		"subject":     "quarterly update",
		"text":        "body",
		"attachments": []map[string]string{{"filename": "plan.pdf", "contentType": "application/pdf", "contentBase64": base64.StdEncoding.EncodeToString([]byte("rule attachment payload"))}},
	}, &sent); code != http.StatusCreated {
		t.Fatalf("send code=%d sent=%+v", code, sent)
	}

	var archived struct {
		Items []MailMessage `json:"items"`
	}
	if code := rcpt.do("GET", "/api/mail/messages?mailboxId="+recipient.ID+"&folder=Archive", nil, &archived); code != http.StatusOK || len(archived.Items) != 1 {
		t.Fatalf("archive list code=%d items=%+v", code, archived.Items)
	}
	msg := archived.Items[0]
	if !msg.IsRead || !msg.IsStarred {
		t.Fatalf("rule flags read=%v starred=%v", msg.IsRead, msg.IsStarred)
	}
	if len(msg.Labels) != 1 || msg.Labels[0].Name != "Priority" {
		t.Fatalf("labels=%+v, want Priority", msg.Labels)
	}
}

func TestMailRulesMailboxIsolation(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	owner := createTestMailbox(t, admin, domainID, "rule-owner", "Rule Owner", "Password123!", nil)
	other := createTestMailbox(t, admin, domainID, "rule-other", "Rule Other", "Password123!", nil)
	sender := createTestMailbox(t, admin, domainID, "rule-outsider", "Rule Outsider", "Password123!", nil)

	ownerClient := &testClient{t: t, server: ts}
	if code := ownerClient.do("POST", "/api/auth/login", map[string]string{"email": owner.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("owner login=%d", code)
	}
	var denied map[string]any
	if code := ownerClient.do("POST", "/api/me/rules", map[string]any{"mailboxId": other.ID, "fromContains": sender.Address, "action": "archive"}, &denied); code != http.StatusNotFound {
		t.Fatalf("cross-mailbox rule create code=%d body=%v", code, denied)
	}
	var rule MailRule
	if code := ownerClient.do("POST", "/api/me/rules", map[string]any{"mailboxId": owner.ID, "fromContains": sender.Address, "action": "archive"}, &rule); code != http.StatusCreated {
		t.Fatalf("create owner rule code=%d rule=%+v", code, rule)
	}

	senderClient := &testClient{t: t, server: ts}
	if code := senderClient.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("sender login=%d", code)
	}
	var sent MailMessage
	if code := senderClient.do("POST", "/api/mail/send", map[string]any{"to": []string{other.Address}, "subject": "isolation", "text": "body"}, &sent); code != http.StatusCreated {
		t.Fatalf("send to other code=%d sent=%+v", code, sent)
	}

	otherClient := &testClient{t: t, server: ts}
	if code := otherClient.do("POST", "/api/auth/login", map[string]string{"email": other.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("other login=%d", code)
	}
	var inbox struct {
		Items []MailMessage `json:"items"`
	}
	if code := otherClient.do("GET", "/api/mail/messages?mailboxId="+other.ID+"&folder=Inbox", nil, &inbox); code != http.StatusOK || len(inbox.Items) != 1 {
		t.Fatalf("other inbox code=%d items=%+v", code, inbox.Items)
	}
	var archived struct {
		Items []MailMessage `json:"items"`
	}
	if code := otherClient.do("GET", "/api/mail/messages?mailboxId="+other.ID+"&folder=Archive", nil, &archived); code != http.StatusOK || len(archived.Items) != 0 {
		t.Fatalf("other archive code=%d items=%+v", code, archived.Items)
	}
}

func TestBlockedSenderMovesInboundToSpamAndIsolatesUsers(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var domains struct {
		Items []Domain `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/domains", nil, &domains); code != http.StatusOK || len(domains.Items) == 0 {
		t.Fatalf("domains code=%d items=%+v", code, domains.Items)
	}
	sender := createTestMailbox(t, admin, domains.Items[0].ID, "blocked-sender", "Blocked Sender", "Password123!", nil)
	recipient := createTestMailbox(t, admin, domains.Items[0].ID, "blocked-recipient", "Blocked Recipient", "Password123!", nil)
	other := createTestMailbox(t, admin, domains.Items[0].ID, "blocked-other", "Blocked Other", "Password123!", nil)

	recipientClient := &testClient{t: t, server: ts}
	if code := recipientClient.do("POST", "/api/auth/login", map[string]string{"email": recipient.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("recipient login code=%d", code)
	}
	var blocked BlockedSender
	if code := recipientClient.do("POST", "/api/me/blocked-senders", map[string]any{"mailboxId": recipient.ID, "email": sender.Address, "reason": "test"}, &blocked); code != http.StatusCreated {
		t.Fatalf("blocked sender code=%d body=%+v", code, blocked)
	}

	senderClient := &testClient{t: t, server: ts}
	if code := senderClient.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("sender login code=%d", code)
	}
	var sent MailMessage
	if code := senderClient.do("POST", "/api/mail/send", map[string]any{"to": []string{recipient.Address}, "subject": "blocked sender test", "text": "body"}, &sent); code != http.StatusCreated {
		t.Fatalf("send code=%d sent=%+v", code, sent)
	}

	var inbox struct {
		Items []MailMessage `json:"items"`
	}
	if code := recipientClient.do("GET", "/api/mail/messages?mailboxId="+recipient.ID+"&folder=Inbox&q=blocked%20sender", nil, &inbox); code != http.StatusOK || len(inbox.Items) != 0 {
		t.Fatalf("recipient inbox code=%d items=%+v", code, inbox.Items)
	}
	var spam struct {
		Items []MailMessage `json:"items"`
	}
	if code := recipientClient.do("GET", "/api/mail/messages?mailboxId="+recipient.ID+"&folder=Spam&q=blocked%20sender", nil, &spam); code != http.StatusOK || len(spam.Items) != 1 {
		t.Fatalf("recipient spam code=%d items=%+v", code, spam.Items)
	}

	otherClient := &testClient{t: t, server: ts}
	if code := otherClient.do("POST", "/api/auth/login", map[string]string{"email": other.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("other login code=%d", code)
	}
	var denied map[string]any
	if code := otherClient.do("GET", "/api/mail/messages/"+spam.Items[0].ID+"?markRead=0", nil, &denied); code != http.StatusNotFound {
		t.Fatalf("other user should not read spam message code=%d body=%v", code, denied)
	}
}

func TestScheduleSendQueuesFutureMessage(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var domains struct {
		Items []Domain `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/domains", nil, &domains); code != http.StatusOK || len(domains.Items) == 0 {
		t.Fatalf("domains code=%d items=%+v", code, domains.Items)
	}
	sender := createTestMailbox(t, admin, domains.Items[0].ID, "later", "Later", "Password123!", nil)
	recipient := createTestMailbox(t, admin, domains.Items[0].ID, "later-bob", "Later Bob", "Password123!", nil)

	alice := &testClient{t: t, server: ts}
	if code := alice.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("sender login code=%d", code)
	}
	var scheduled ScheduledSend
	payload := map[string]any{
		"mailboxId": sender.ID,
		"to":        []string{recipient.Address},
		"subject":   "send later",
		"text":      "not yet",
		"html":      "<p>not yet</p>",
		"sendAt":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if code := alice.do("POST", "/api/mail/schedule-send", payload, &scheduled); code != http.StatusCreated || scheduled.Status != "pending" {
		t.Fatalf("schedule code=%d scheduled=%+v", code, scheduled)
	}
	if scheduled.Subject != "send later" || len(scheduled.To) != 1 || scheduled.To[0] != recipient.Address || scheduled.Snippet != "not yet" {
		t.Fatalf("scheduled preview not populated: %+v", scheduled)
	}
	var scheduledList struct {
		Items []ScheduledSend `json:"items"`
	}
	if code := alice.do("GET", "/api/mail/scheduled-sends?mailboxId="+sender.ID, nil, &scheduledList); code != http.StatusOK || len(scheduledList.Items) != 1 || scheduledList.Items[0].ID != scheduled.ID {
		t.Fatalf("scheduled list code=%d items=%+v", code, scheduledList.Items)
	}
	if scheduledList.Items[0].Subject != "send later" || scheduledList.Items[0].Snippet != "not yet" {
		t.Fatalf("scheduled list preview not populated: %+v", scheduledList.Items[0])
	}

	bob := &testClient{t: t, server: ts}
	if code := bob.do("POST", "/api/auth/login", map[string]string{"email": recipient.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("recipient login code=%d", code)
	}
	var inbox struct {
		Items []MailMessage `json:"items"`
	}
	if code := bob.do("GET", "/api/mail/messages?folder=Inbox", nil, &inbox); code != http.StatusOK || len(inbox.Items) != 0 {
		t.Fatalf("future scheduled mail should not be delivered immediately: code=%d items=%+v", code, inbox.Items)
	}
	if code := alice.do("DELETE", "/api/mail/schedule-send/"+scheduled.ID, nil, &map[string]any{}); code != http.StatusOK {
		t.Fatalf("cancel scheduled send code=%d", code)
	}
	if code := alice.do("GET", "/api/mail/scheduled-sends?mailboxId="+sender.ID, nil, &scheduledList); code != http.StatusOK || len(scheduledList.Items) != 0 {
		t.Fatalf("scheduled list after cancel code=%d items=%+v", code, scheduledList.Items)
	}
}

func TestPermissionGroupMailLimits(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}
	updateRegularPermissionGroupWithLimits(t, admin, regularUserDefaultPermissions(), PermissionLimits{MaxAttachmentMB: 1, SMTPDailyLimit: 10, SMTPMinuteLimit: 1, IMAPMinuteLimit: 1, POP3MinuteLimit: 1})

	domainID := mustDefaultDomainID(t, a)
	sender := createTestMailbox(t, admin, domainID, "limited-sender", "Limited Sender", "Password123!", nil)
	recipient := createTestMailbox(t, admin, domainID, "limited-recipient", "Limited Recipient", "Password123!", nil)

	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login code=%d", code)
	}
	var me struct {
		User User `json:"user"`
	}
	if code := user.do("GET", "/api/me", nil, &me); code != http.StatusOK {
		t.Fatalf("me code=%d user=%+v", code, me.User)
	}
	if me.User.Limits.MaxAttachmentMB != 1 || me.User.Limits.SMTPMinuteLimit != 1 || me.User.Limits.IMAPMinuteLimit != 1 || me.User.Limits.POP3MinuteLimit != 1 {
		t.Fatalf("user limits not attached: %+v", me.User.Limits)
	}

	tooLargeAttachment := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 1024*1024+1))
	var errBody map[string]any
	if code := user.do("POST", "/api/mail/send", map[string]any{
		"mailboxId": sender.ID,
		"to":        []string{recipient.Address},
		"subject":   "too large",
		"text":      "body",
		"html":      "<p>body</p>",
		"attachments": []map[string]string{{
			"filename":      "large.bin",
			"contentType":   "application/octet-stream",
			"contentBase64": tooLargeAttachment,
		}},
	}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("oversized attachment should be rejected code=%d body=%v", code, errBody)
	}

	var sent MailMessage
	payload := map[string]any{
		"mailboxId": sender.ID,
		"to":        []string{recipient.Address},
		"subject":   "first limited send",
		"text":      "body",
		"html":      "<p>body</p>",
	}
	if code := user.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated {
		t.Fatalf("first send code=%d msg=%+v", code, sent)
	}
	payload["subject"] = "second limited send"
	if code := user.do("POST", "/api/mail/send", payload, &errBody); code != http.StatusTooManyRequests {
		t.Fatalf("smtp minute limit should reject second send code=%d body=%v", code, errBody)
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
	if code := client.do("GET", "/api/mail/mailboxes", nil, &mine); code != http.StatusOK || len(mine.Items) != 1 {
		t.Fatalf("registered user should get auto-created mailbox: code=%d items=%+v", code, mine.Items)
	}

	another := &testClient{t: t, server: ts}
	if code := another.do("POST", "/api/auth/login", map[string]string{"email": "newuser@example.com", "password": "Password123!"}, &out); code != http.StatusOK {
		t.Fatalf("login registered user code=%d body=%v", code, out)
	}
}

func TestLegacyBootstrapMailboxMigrationRemovesImplicitAdminMailbox(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr:              ":0",
		DBPath:            filepath.Join(dir, "lanqin.db"),
		DataDir:           filepath.Join(dir, "data"),
		CookieName:        "lanqin_test",
		SessionTTLHours:   24,
		AdminEmail:        "lanqinnet@gmail.com",
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

	ctx := context.Background()

	// seed() now creates user + domain gmail.com + mailbox lanqinnet@gmail.com
	// with display_name = admin email (not "LanQin Admin").
	// Modify the mailbox to look like the old legacy pattern so the migration can find it.
	if _, err := a.db.ExecContext(ctx, `UPDATE mailboxes SET display_name='LanQin Admin' WHERE address=?`, cfg.AdminEmail); err != nil {
		t.Fatal(err)
	}

	// Get the domain ID for the verification step
	var domainID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM domains WHERE name=?`, "gmail.com").Scan(&domainID); err != nil {
		t.Fatal(err)
	}

	if err := a.migrateLegacyBootstrapMailbox(ctx); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email=? AND role='admin'`, cfg.AdminEmail).Scan(&count); err != nil || count != 1 {
		t.Fatalf("admin user count=%d err=%v", count, err)
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE address=?`, cfg.AdminEmail).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy mailbox count=%d err=%v", count, err)
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains WHERE id=?`, domainID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy domain count=%d err=%v", count, err)
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

	// seed() already created domain lanqin.local and mailbox admin@lanqin.local
	var domainList = struct {
		Items []Domain `json:"items"`
	}{}
	if code := admin.do("GET", "/api/admin/domains", nil, &domainList); code != http.StatusOK || len(domainList.Items) == 0 {
		t.Fatalf("list domains code=%d items=%+v", code, domainList.Items)
	}
	domainID := domainList.Items[0].ID

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

func TestCustomMailFoldersCreateAndMove(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	var bad map[string]any
	if code := admin.do("POST", "/api/mail/folders", map[string]string{"name": "Inbox"}, &bad); code != http.StatusBadRequest {
		t.Fatalf("system folder create should be rejected code=%d body=%v", code, bad)
	}
	if code := admin.do("POST", "/api/mail/folders", map[string]string{"name": "../bad"}, &bad); code != http.StatusBadRequest {
		t.Fatalf("invalid folder create should be rejected code=%d body=%v", code, bad)
	}

	var custom MailFolder
	if code := admin.do("POST", "/api/mail/folders", map[string]string{"name": "客户归档"}, &custom); code != http.StatusCreated || custom.Name != "客户归档" || custom.Role != "客户归档" {
		t.Fatalf("custom folder create code=%d folder=%+v", code, custom)
	}
	var folders struct {
		Items []MailFolder `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/folders", nil, &folders); code != http.StatusOK || !folderListContains(folders.Items, "客户归档") {
		t.Fatalf("folder list code=%d items=%+v", code, folders.Items)
	}

	var sent MailMessage
	if code := admin.do("POST", "/api/mail/send", map[string]any{"to": []string{"person@example.test"}, "subject": "custom folder", "text": "body"}, &sent); code != http.StatusCreated {
		t.Fatalf("send code=%d msg=%+v", code, sent)
	}
	var ok map[string]any
	if code := admin.do("POST", "/api/mail/messages/"+sent.ID+"/move", map[string]string{"folder": "客户归档"}, &ok); code != http.StatusOK {
		t.Fatalf("move to custom folder code=%d body=%v", code, ok)
	}
	var list struct {
		Items []MailMessage `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/messages?folder="+url.QueryEscape("客户归档"), nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != sent.ID {
		t.Fatalf("custom folder messages code=%d items=%+v", code, list.Items)
	}
	if code := admin.do("POST", "/api/mail/messages/"+sent.ID+"/move", map[string]string{"folder": "bad/name"}, &bad); code != http.StatusBadRequest {
		t.Fatalf("invalid move folder should be rejected code=%d body=%v", code, bad)
	}
}

func TestCustomMailFoldersDeleteMovesMessagesToInbox(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	var custom MailFolder
	if code := admin.do("POST", "/api/mail/folders", map[string]string{"name": "临时项目"}, &custom); code != http.StatusCreated {
		t.Fatalf("create custom folder code=%d folder=%+v", code, custom)
	}
	var sent MailMessage
	if code := admin.do("POST", "/api/mail/send", map[string]any{"to": []string{"person@example.test"}, "subject": "delete folder keeps message", "text": "body"}, &sent); code != http.StatusCreated {
		t.Fatalf("send code=%d msg=%+v", code, sent)
	}
	var ok map[string]any
	if code := admin.do("POST", "/api/mail/messages/"+sent.ID+"/move", map[string]string{"folder": "临时项目"}, &ok); code != http.StatusOK {
		t.Fatalf("move to custom folder code=%d body=%v", code, ok)
	}
	if code := admin.do("DELETE", "/api/mail/folders/"+custom.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete custom folder code=%d body=%v", code, ok)
	}
	var folders struct {
		Items []MailFolder `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/folders", nil, &folders); code != http.StatusOK || folderListContains(folders.Items, "临时项目") {
		t.Fatalf("folder should be deleted code=%d items=%+v", code, folders.Items)
	}
	var inbox struct {
		Items []MailMessage `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/messages?folder=Inbox&q="+url.QueryEscape("delete folder keeps message"), nil, &inbox); code != http.StatusOK || len(inbox.Items) == 0 || inbox.Items[0].ID != sent.ID {
		t.Fatalf("message should be moved to inbox code=%d items=%+v", code, inbox.Items)
	}
	var bad map[string]any
	var inboxID string
	for _, item := range folders.Items {
		if item.Name == "Inbox" {
			inboxID = item.ID
			break
		}
	}
	if inboxID == "" {
		t.Fatalf("inbox id not found")
	}
	if code := admin.do("DELETE", "/api/mail/folders/"+inboxID, nil, &bad); code != http.StatusBadRequest {
		t.Fatalf("delete system folder should be rejected code=%d body=%v", code, bad)
	}
}

func TestCustomMailFoldersReorder(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	createFolder := func(name string) MailFolder {
		t.Helper()
		var folder MailFolder
		if code := admin.do("POST", "/api/mail/folders", map[string]string{"name": name}, &folder); code != http.StatusCreated {
			t.Fatalf("create folder %s code=%d folder=%+v", name, code, folder)
		}
		return folder
	}
	customer := createFolder("客户")
	bills := createFolder("账单")
	project := createFolder("项目")

	var ok map[string]any
	if code := admin.do("POST", "/api/mail/folders/reorder", map[string]any{"folderIds": []string{project.ID, customer.ID, bills.ID}}, &ok); code != http.StatusOK {
		t.Fatalf("reorder code=%d body=%v", code, ok)
	}
	if code := admin.do("POST", "/api/mail/folders/reorder", map[string]any{"folders": []map[string]any{
		{"id": customer.ID, "sortOrder": 500},
		{"id": project.ID, "sortOrder": 2500},
		{"id": bills.ID, "sortOrder": 3500},
	}}, &ok); code != http.StatusOK {
		t.Fatalf("reorder with explicit sort order code=%d body=%v", code, ok)
	}
	var folders struct {
		Items []MailFolder `json:"items"`
	}
	if code := admin.do("GET", "/api/mail/folders", nil, &folders); code != http.StatusOK {
		t.Fatalf("list folders code=%d items=%+v", code, folders.Items)
	}
	got := customFolderNames(folders.Items)
	want := []string{"客户", "项目", "账单"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("custom folder order=%v want=%v", got, want)
	}
	if folders.Items[0].ID != customer.ID {
		t.Fatalf("customer folder should be before inbox after explicit sort order, first=%+v", folders.Items[0])
	}
	var inboxID string
	for _, item := range folders.Items {
		if item.Name == "Inbox" {
			inboxID = item.ID
			break
		}
	}
	if inboxID == "" {
		t.Fatalf("inbox not found in folders: %+v", folders.Items)
	}
	var bad map[string]any
	if code := admin.do("POST", "/api/mail/folders/reorder", map[string]any{"folderIds": []string{project.ID, inboxID, customer.ID, bills.ID}}, &bad); code != http.StatusBadRequest {
		t.Fatalf("reorder with system folder should be rejected code=%d body=%v", code, bad)
	}

	domain := createTestDomain(t, admin, "folders.test")
	other := createTestMailbox(t, admin, domain.ID, "other", "Other", "Password123!", nil)
	otherFolderID, err := a.ensureFolder(context.Background(), other.ID, "其他")
	if err != nil {
		t.Fatal(err)
	}
	if code := admin.do("POST", "/api/mail/folders/reorder", map[string]any{"folderIds": []string{project.ID, customer.ID, otherFolderID}}, &bad); code != http.StatusBadRequest {
		t.Fatalf("reorder with other mailbox folder should be rejected code=%d body=%v", code, bad)
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
	// seed() already created domain lanqin.local and mailbox admin@lanqin.local
	var domainList = struct {
		Items []Domain `json:"items"`
	}{}
	if code := admin.do("GET", "/api/admin/domains", nil, &domainList); code != http.StatusOK || len(domainList.Items) == 0 {
		t.Fatalf("list domains code=%d items=%+v", code, domainList.Items)
	}
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
	update := systemSettingsPayload(settings)
	update["catchAllEnabled"] = true
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

func TestHTMLPolicyPreservesEmailLayoutStyles(t *testing.T) {
	policy := NewHTMLPolicy()
	out := policy.Sanitize(`<html><head><style type="text/css">.card{max-width:600px;margin:0 auto;background:linear-gradient(135deg,#667eea,#764ba2);box-shadow:0 8px 24px rgba(0,0,0,.12);color:#fff}.content{text-align:center;padding:24px}</style></head><body>
		<div class="card" style="max-width:600px;margin:0 auto;background:linear-gradient(135deg,#667eea,#764ba2);box-shadow:0 8px 24px rgba(0,0,0,.12);color:#fff" onclick="alert(1)">
		<table width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse"><tr><td align="center" style="padding:24px;text-align:center;background-color:#f8fafc">
		<a href="javascript:alert(1)">bad</a><img src="x" onerror="alert(1)"><script>alert(1)</script>hello
		</td></tr></table>
	</div></body></html>`)
	for _, want := range []string{"<style type=\"text/css\">", ".card{", "class=\"card\"", "max-width: 600px", "margin: 0 auto", "background: linear-gradient", "box-shadow:", "cellpadding=\"0\"", "cellspacing=\"0\"", "align=\"center\"", "text-align: center"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sanitized html missing %q: %s", want, out)
		}
	}
	blockedOut := policy.Sanitize(`<style>.bad{background:url(https://tracker.example/x);color:red}</style><p>ok</p>`)
	if strings.Contains(blockedOut, "<style") {
		t.Fatalf("unsafe css block should be removed: %s", blockedOut)
	}
	for _, blocked := range []string{"onclick", "onerror", "javascript:", "<script"} {
		if strings.Contains(strings.ToLower(out), blocked) {
			t.Fatalf("sanitized html kept unsafe %q: %s", blocked, out)
		}
	}
}

func TestMailSendQueuesSMTPFailureForRetry(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "127.0.0.1"
	a.cfg.SMTPPort = "1"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	payload := map[string]any{
		"to":      []string{"person@example.com"},
		"subject": "smtp failure should surface",
		"text":    "hello",
	}
	var sent MailMessage
	if code := admin.do("POST", "/api/mail/send", payload, &sent); code != http.StatusCreated {
		t.Fatalf("smtp queued send code=%d body=%+v", code, sent)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status, lastError string
	if err := a.db.QueryRow(`SELECT status,last_error FROM send_queue WHERE sent_message_id=?`, sent.ID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusFailed || lastError == "" {
		t.Fatalf("queue status=%q lastError=%q", status, lastError)
	}
	var auditCount int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM send_audit_events WHERE sent_message_id=? AND event=?`, sent.ID, sendAuditRetry).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("retry audit count=%d, want 1", auditCount)
	}
}

func TestMailSendRejectsUnauthorizedFrom(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	var errBody map[string]any
	if code := admin.do("POST", "/api/mail/send", map[string]any{
		"from":    "attacker@example.com",
		"to":      []string{"person@example.com"},
		"subject": "bad from",
		"text":    "hello",
	}, &errBody); code != http.StatusForbidden {
		t.Fatalf("unauthorized from code=%d body=%v", code, errBody)
	}
}

func TestMailSendRollsBackSentCopyWhenQueueInsertFails(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "postfix"
	a.cfg.SMTPPort = "25"
	user, mb := defaultAdminUserAndMailbox(t, a)
	if _, err := a.db.ExecContext(context.Background(), `DROP TABLE send_queue`); err != nil {
		t.Fatal(err)
	}
	_, err := a.sendMailNow(context.Background(), user, mb, mailComposeInput{
		To:      []string{"person@example.com"},
		Subject: "queue insert failure",
		Text:    "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "failed to enqueue delivery") {
		t.Fatalf("sendMailNow error=%v, want enqueue failure", err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE mailbox_id=? AND subject=?`, mb.ID, "queue insert failure").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("sent copy should be removed after enqueue failure, count=%d", count)
	}
}

func TestAPITokenManagementStoresHashAndRevokes(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	var created struct {
		Token string   `json:"token"`
		Item  APIToken `json:"item"`
	}
	if code := admin.do("POST", "/api/me/api-tokens", map[string]string{"name": "integration-test"}, &created); code != http.StatusCreated {
		t.Fatalf("create api token code=%d resp=%+v", code, created)
	}
	if !strings.HasPrefix(created.Token, "lq_") || created.Item.ID == "" || created.Item.Name != "integration-test" || created.Item.ExpiresAt == nil {
		t.Fatalf("created token response=%+v", created)
	}
	if remaining := time.Until(*created.Item.ExpiresAt); remaining < 89*24*time.Hour || remaining > 91*24*time.Hour {
		t.Fatalf("created token default expiry=%s, remaining=%s", created.Item.ExpiresAt, remaining)
	}
	var storedHash string
	if err := a.db.QueryRow(`SELECT token_hash FROM api_tokens WHERE id=?`, created.Item.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == created.Token || storedHash != hashToken(created.Token) {
		t.Fatalf("stored token hash=%q token=%q", storedHash, created.Token)
	}

	openAdmin := &testClient{t: t, server: ts, bearer: created.Token}
	var domains struct {
		Items []Domain `json:"items"`
	}
	if code := openAdmin.do("GET", "/api/open/domains", nil, &domains); code != http.StatusOK {
		t.Fatalf("open api with bearer token code=%d", code)
	}
	var listed struct {
		Items []APIToken `json:"items"`
	}
	if code := admin.do("GET", "/api/me/api-tokens", nil, &listed); code != http.StatusOK {
		t.Fatalf("list api tokens code=%d", code)
	}
	if len(listed.Items) != 1 || listed.Items[0].ID != created.Item.ID || listed.Items[0].LastUsedAt == nil {
		t.Fatalf("listed tokens=%+v", listed.Items)
	}

	disabled := true
	var updated APIToken
	if code := admin.do("POST", "/api/me/api-tokens/"+created.Item.ID, map[string]any{"disabled": disabled}, &updated); code != http.StatusOK {
		t.Fatalf("disable api token code=%d item=%+v", code, updated)
	}
	if !updated.Disabled {
		t.Fatalf("updated token should be disabled: %+v", updated)
	}
	if code := openAdmin.do("GET", "/api/open/domains", nil, &map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("disabled bearer token code=%d", code)
	}
	if code := admin.do("DELETE", "/api/me/api-tokens/"+created.Item.ID, nil, &map[string]any{}); code != http.StatusOK {
		t.Fatalf("delete api token code=%d", code)
	}
}

func TestPublicAPIDomainAndMailboxCRUD(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	adminToken := createTestAPIToken(t, admin, "admin-open-api")
	openAdmin := &testClient{t: t, server: ts, bearer: adminToken}

	var authErr map[string]any
	if code := admin.do("GET", "/api/open/domains", nil, &authErr); code != http.StatusUnauthorized {
		t.Fatalf("cookie-only open api code=%d body=%v", code, authErr)
	}

	var domain Domain
	if code := openAdmin.do("POST", "/api/open/domains", map[string]string{"name": "api.example.test"}, &domain); code != http.StatusCreated {
		t.Fatalf("create public api domain code=%d domain=%+v", code, domain)
	}
	if domain.Name != "api.example.test" || domain.DKIMPublicKey == "" {
		t.Fatalf("domain=%+v", domain)
	}
	var domains struct {
		Items []Domain `json:"items"`
	}
	if code := openAdmin.do("GET", "/api/open/domains", nil, &domains); code != http.StatusOK {
		t.Fatalf("list public api domains code=%d", code)
	}
	if len(domains.Items) < 2 {
		t.Fatalf("domains=%+v", domains.Items)
	}
	var disabled Domain
	if code := openAdmin.do("POST", "/api/open/domains/"+domain.ID, map[string]string{"status": "disabled"}, &disabled); code != http.StatusOK {
		t.Fatalf("update public api domain code=%d domain=%+v", code, disabled)
	}
	if disabled.Status != "disabled" {
		t.Fatalf("domain status=%q", disabled.Status)
	}
	if code := openAdmin.do("POST", "/api/open/domains/"+domain.ID, map[string]string{"status": "active"}, &domain); code != http.StatusOK {
		t.Fatalf("reactivate public api domain code=%d domain=%+v", code, domain)
	}

	var mailbox Mailbox
	if code := openAdmin.do("POST", "/api/open/mailboxes", map[string]any{
		"domainId":    domain.ID,
		"localPart":   "api-user",
		"displayName": "API User",
		"password":    "Password123!",
		"quotaMb":     256,
	}, &mailbox); code != http.StatusCreated {
		t.Fatalf("create public api mailbox code=%d mailbox=%+v", code, mailbox)
	}
	if mailbox.Address != "api-user@api.example.test" || mailbox.QuotaMB != 256 {
		t.Fatalf("mailbox=%+v", mailbox)
	}
	var mailboxes struct {
		Items []Mailbox `json:"items"`
	}
	if code := openAdmin.do("GET", "/api/open/mailboxes", nil, &mailboxes); code != http.StatusOK {
		t.Fatalf("list public api mailboxes code=%d", code)
	}
	if len(mailboxes.Items) < 2 {
		t.Fatalf("mailboxes=%+v", mailboxes.Items)
	}
	var updated Mailbox
	if code := openAdmin.do("POST", "/api/open/mailboxes/"+mailbox.ID, map[string]any{"displayName": "Renamed API User", "quotaMb": 512, "status": "disabled"}, &updated); code != http.StatusOK {
		t.Fatalf("update public api mailbox code=%d mailbox=%+v", code, updated)
	}
	if updated.DisplayName != "Renamed API User" || updated.QuotaMB != 512 || updated.Status != "disabled" {
		t.Fatalf("updated mailbox=%+v", updated)
	}
	var ok map[string]any
	if code := openAdmin.do("DELETE", "/api/open/mailboxes/"+mailbox.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete public api mailbox code=%d body=%v", code, ok)
	}
	if code := openAdmin.do("DELETE", "/api/open/domains/"+domain.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete public api domain code=%d body=%v", code, ok)
	}
}

func TestPublicAPISendStatusAndMailboxMessages(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}
	domainID := mustDefaultDomainID(t, a)
	sender := createTestMailbox(t, admin, domainID, "public-sender", "Public Sender", "Password123!", nil)
	recipient := createTestMailbox(t, admin, domainID, "public-recipient", "Public Recipient", "Password123!", nil)
	other := createTestMailbox(t, admin, domainID, "public-other", "Public Other", "Password123!", nil)

	senderClient := &testClient{t: t, server: ts}
	if code := senderClient.do("POST", "/api/auth/login", map[string]string{"email": sender.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("sender login code=%d body=%v", code, login)
	}
	senderToken := createTestAPIToken(t, senderClient, "sender-open-api")
	senderOpen := &testClient{t: t, server: ts, bearer: senderToken}
	if code := senderClient.do("POST", "/api/open/send", map[string]any{
		"mailboxId": sender.ID,
		"to":        []string{recipient.Address},
		"subject":   "cookie-only public api send",
		"text":      "this should not authenticate",
	}, &map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("cookie-only public api send code=%d", code)
	}
	var sent struct {
		ID             string    `json:"id"`
		QueueID        string    `json:"queueId"`
		Status         string    `json:"status"`
		MessageID      string    `json:"messageId"`
		RFCMessageID   string    `json:"rfcMessageId"`
		MailboxID      string    `json:"mailboxId"`
		MailboxAddress string    `json:"mailboxAddress"`
		Subject        string    `json:"subject"`
		CreatedAt      time.Time `json:"createdAt"`
	}
	if code := senderOpen.do("POST", "/api/open/send", map[string]any{
		"mailboxId": sender.ID,
		"to":        []string{recipient.Address},
		"subject":   "public api send",
		"text":      "hello from public api",
	}, &sent); code != http.StatusCreated {
		t.Fatalf("public api send code=%d body=%+v", code, sent)
	}
	if sent.ID == "" || sent.Status != sendAuditAccepted || sent.MessageID == "" || sent.MailboxAddress != sender.Address {
		t.Fatalf("sent response=%+v", sent)
	}

	var status struct {
		ID             string `json:"id"`
		QueueID        string `json:"queueId"`
		Status         string `json:"status"`
		MessageID      string `json:"messageId"`
		RFCMessageID   string `json:"rfcMessageId"`
		MailboxID      string `json:"mailboxId"`
		MailboxAddress string `json:"mailboxAddress"`
		Subject        string `json:"subject"`
	}
	if code := senderOpen.do("GET", "/api/open/send/"+sent.ID, nil, &status); code != http.StatusOK {
		t.Fatalf("public api send status code=%d status=%+v", code, status)
	}
	if status.ID != sent.ID || status.MessageID != sent.MessageID || status.Status != sendAuditAccepted {
		t.Fatalf("status=%+v sent=%+v", status, sent)
	}

	recipientClient := &testClient{t: t, server: ts}
	if code := recipientClient.do("POST", "/api/auth/login", map[string]string{"email": recipient.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("recipient login code=%d body=%v", code, login)
	}
	recipientToken := createTestAPIToken(t, recipientClient, "recipient-open-api")
	recipientOpen := &testClient{t: t, server: ts, bearer: recipientToken}
	var inbox struct {
		Items      []MailMessage `json:"items"`
		NextCursor string        `json:"nextCursor"`
	}
	if code := recipientOpen.do("GET", "/api/open/mailboxes/"+recipient.ID+"/messages?folder=Inbox", nil, &inbox); code != http.StatusOK {
		t.Fatalf("public api mailbox messages code=%d inbox=%+v", code, inbox)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Subject != "public api send" || inbox.Items[0].From != sender.Address {
		t.Fatalf("inbox=%+v", inbox.Items)
	}
	if code := recipientOpen.do("GET", "/api/open/mailboxes/"+other.ID+"/messages?folder=Inbox", nil, &map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("cross-user mailbox read code=%d", code)
	}
	if code := recipientOpen.do("GET", "/api/open/send/"+sent.ID, nil, &map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("cross-user send status code=%d", code)
	}
}

func TestSendQueueRecoversStaleSendingItems(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	host, port, received := startCapturingSMTP(t, 1)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	user, mb := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	mimeBytes := []byte("From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: stale\r\n\r\nbody")
	queueID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"person@example.com"},
		MIMEBytes:  mimeBytes,
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleAt := now.Add(-sendQueueStaleAfter - time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,attempt_count=1,updated_at=? WHERE id=?`, sendQueueStatusSending, staleAt, queueID); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("recovered queue item was not relayed")
	}
	var status string
	if err := a.db.QueryRow(`SELECT status FROM send_queue WHERE id=?`, queueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusDelivered {
		t.Fatalf("queue status=%q, want delivered", status)
	}
	var mimeBase64 string
	if err := a.db.QueryRow(`SELECT mime_base64 FROM send_queue WHERE id=?`, queueID).Scan(&mimeBase64); err != nil {
		t.Fatal(err)
	}
	if mimeBase64 != "" {
		t.Fatal("delivered queue item should not retain raw MIME")
	}
}

func TestSendQueueStaleDeliveredMarkerDoesNotRedeliver(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	host, port, received := startCapturingSMTP(t, 1)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	user, mb := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	mimeBytes := []byte("From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: marker\r\n\r\nbody")
	queueID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"person@example.com"},
		MIMEBytes:  mimeBytes,
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleAt := now.Add(-sendQueueStaleAfter - time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,attempt_count=1,updated_at=? WHERE id=?`, sendQueueStatusSending, staleAt, queueID); err != nil {
		t.Fatal(err)
	}
	if err := a.writeSendQueueDeliveredMarker(queueID); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-received:
		t.Fatalf("stale delivered marker should not redeliver, got %q", body)
	case <-time.After(200 * time.Millisecond):
	}
	var status, mimeBase64 string
	if err := a.db.QueryRow(`SELECT status,mime_base64 FROM send_queue WHERE id=?`, queueID).Scan(&status, &mimeBase64); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusDelivered {
		t.Fatalf("queue status=%q, want delivered", status)
	}
	if mimeBase64 != "" {
		t.Fatal("delivered marker recovery should clear raw MIME")
	}
	delivered, err := a.hasSendQueueDeliveredMarker(queueID)
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Fatal("delivered marker should be removed after database state is repaired")
	}
}

func TestSendQueueAPIPermissionIsolation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "127.0.0.1"
	a.cfg.SMTPPort = "25"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	aliceMB := createTestMailbox(t, admin, domainID, "queue-alice", "Queue Alice", "Password123!", nil)
	bobMB := createTestMailbox(t, admin, domainID, "queue-bob", "Queue Bob", "Password123!", nil)
	aliceUser, _, err := a.userByEmail(context.Background(), aliceMB.Address)
	if err != nil {
		t.Fatal(err)
	}
	bobUser, _, err := a.userByEmail(context.Background(), bobMB.Address)
	if err != nil {
		t.Fatal(err)
	}
	now := a.now().UTC()
	aliceQueueID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     aliceUser.ID,
		MailboxID:  aliceMB.ID,
		MessageID:  "<alice-queue@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   aliceMB.Address,
		HeaderFrom: aliceMB.Address,
		Recipients: []string{"person@example.test"},
		MIMEBytes:  []byte("From: queue-alice@example.test\r\nTo: person@example.test\r\nSubject: alice\r\n\r\nbody"),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     bobUser.ID,
		MailboxID:  bobMB.ID,
		MessageID:  "<bob-queue@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   bobMB.Address,
		HeaderFrom: bobMB.Address,
		Recipients: []string{"person@example.test"},
		MIMEBytes:  []byte("From: queue-bob@example.test\r\nTo: person@example.test\r\nSubject: bob\r\n\r\nbody"),
		Now:        now,
	}); err != nil {
		t.Fatal(err)
	}

	alice := &testClient{t: t, server: ts}
	if code := alice.do("POST", "/api/auth/login", map[string]string{"email": aliceMB.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("alice login code=%d", code)
	}
	var list struct {
		Items []SendQueueEntry `json:"items"`
	}
	if code := alice.do("GET", "/api/mail/send-queue?mailboxId="+aliceMB.ID, nil, &list); code != http.StatusOK {
		t.Fatalf("list own queue code=%d items=%+v", code, list.Items)
	}
	if len(list.Items) != 1 || list.Items[0].ID != aliceQueueID || list.Items[0].MailboxID != aliceMB.ID {
		t.Fatalf("own queue isolation failed: %+v", list.Items)
	}
	if code := alice.do("GET", "/api/mail/send-queue?mailboxId="+bobMB.ID, nil, &map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("listing another mailbox should be hidden, code=%d", code)
	}
	if code := alice.do("GET", "/api/mail/send-queue/"+list.Items[0].ID+"/audit", nil, &struct {
		Items []SendAuditEvent `json:"items"`
	}{}); code != http.StatusOK {
		t.Fatalf("own audit code=%d", code)
	}
}

func TestSendQueueAPIFiltersStableCursorAndMessageDetailLink(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "127.0.0.1"
	a.cfg.SMTPPort = "25"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	sentFolderID, err := a.ensureFolder(context.Background(), mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	sentMsg := storedMessage{
		MailboxID:  mb.ID,
		FolderID:   sentFolderID,
		MessageUID: "uid-queue-detail",
		MessageID:  "<queue-detail@example.test>",
		Subject:    "queue detail",
		From:       mb.Address,
		To:         []string{"detail@example.test"},
		SentAt:     now,
		ReceivedAt: now,
		Snippet:    "detail",
		BodyText:   "detail",
		IsRead:     true,
	}
	sentID, err := a.insertMessage(context.Background(), sentMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:        user.ID,
		MailboxID:     mb.ID,
		SentMessageID: sentID,
		MessageID:     "<queue-detail@example.test>",
		Source:        sendSourceWebmail,
		MailFrom:      mb.Address,
		HeaderFrom:    mb.Address,
		Recipients:    []string{"detail@example.test"},
		MIMEBytes:     []byte("Subject: detail\r\n\r\nbody"),
		Now:           now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		MessageID:  "<queue-other@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"other@example.test"},
		MIMEBytes:  []byte("Subject: other\r\n\r\nbody"),
		Now:        now.Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		MessageID:  "<queue-latest@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"latest@example.test"},
		MIMEBytes:  []byte("Subject: latest\r\n\r\nbody"),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 28; i++ {
		if _, err := a.enqueueSend(context.Background(), sendQueueInput{
			UserID:     user.ID,
			MailboxID:  mb.ID,
			MessageID:  fmt.Sprintf("<queue-extra-%02d@example.test>", i),
			Source:     sendSourceWebmail,
			MailFrom:   mb.Address,
			HeaderFrom: mb.Address,
			Recipients: []string{fmt.Sprintf("extra-%02d@example.test", i)},
			MIMEBytes:  []byte("Subject: extra\r\n\r\nbody"),
			Now:        now.Add(time.Duration(-24-i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var byMessage struct {
		Items []SendQueueEntry `json:"items"`
	}
	if code := client.do("GET", "/api/mail/send-queue?messageId="+url.QueryEscape("<queue-detail@example.test>"), nil, &byMessage); code != http.StatusOK || len(byMessage.Items) != 1 || byMessage.Items[0].ID != firstID {
		t.Fatalf("message filter code=%d items=%+v", code, byMessage.Items)
	}
	var byRecipient struct {
		Items []SendQueueEntry `json:"items"`
	}
	if code := client.do("GET", "/api/mail/send-queue?recipient="+url.QueryEscape("other@example.test"), nil, &byRecipient); code != http.StatusOK || len(byRecipient.Items) != 1 || byRecipient.Items[0].ID != secondID {
		t.Fatalf("recipient filter code=%d items=%+v", code, byRecipient.Items)
	}
	var byTime struct {
		Items []SendQueueEntry `json:"items"`
	}
	from := now.Add(-90 * time.Minute).Format(time.RFC3339Nano)
	to := now.Add(30 * time.Minute).Format(time.RFC3339Nano)
	if code := client.do("GET", "/api/mail/send-queue?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), nil, &byTime); code != http.StatusOK || len(byTime.Items) != 2 || byTime.Items[0].ID != thirdID || byTime.Items[1].ID != secondID {
		t.Fatalf("time filter code=%d items=%+v", code, byTime.Items)
	}
	var firstPage struct {
		Items      []SendQueueEntry `json:"items"`
		NextCursor string           `json:"nextCursor"`
	}
	if code := client.do("GET", "/api/mail/send-queue?cursor=0", nil, &firstPage); code != http.StatusOK || len(firstPage.Items) != 30 || firstPage.NextCursor == "" {
		t.Fatalf("first page code=%d cursor=%q items=%+v", code, firstPage.NextCursor, firstPage.Items)
	}
	if _, _, _, err := parseSendQueueCursor(firstPage.NextCursor); err != nil {
		t.Fatalf("next cursor is not stable cursor: %q err=%v", firstPage.NextCursor, err)
	}
	var detail MailMessage
	if code := client.do("GET", "/api/mail/messages/"+sentID+"?markRead=0", nil, &detail); code != http.StatusOK || detail.SendQueueID != firstID || detail.SendQueueStatus == "" {
		t.Fatalf("message detail queue link code=%d detail=%+v", code, detail)
	}
}

func TestSendQueueAPIRetryAndCancel(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startCapturingSMTP(t, 1)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC()
	failedID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		MessageID:  "<failed-retry-api@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"person@example.test"},
		MIMEBytes:  []byte("From: admin@lanqin.local\r\nTo: person@example.test\r\nSubject: retry\r\n\r\nbody"),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,attempt_count=3,last_error='temporary failure',next_attempt_at=? WHERE id=?`, sendQueueStatusFailed, now.Add(time.Hour).Format(time.RFC3339Nano), failedID); err != nil {
		t.Fatal(err)
	}
	var retried SendQueueEntry
	if code := client.do("POST", "/api/mail/send-queue/"+failedID+"/retry", nil, &retried); code != http.StatusOK {
		t.Fatalf("retry failed queue code=%d item=%+v", code, retried)
	}
	if retried.Status != sendQueueStatusQueued || retried.AttemptCount != 0 || retried.LastError != "" {
		t.Fatalf("retry did not reset queue item: %+v", retried)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "Subject: retry") {
			t.Fatalf("unexpected retried body: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retried queue item was not relayed")
	}

	deliveredID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		MessageID:  "<delivered-retry-api@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"person@example.test"},
		MIMEBytes:  []byte("From: admin@lanqin.local\r\nTo: person@example.test\r\nSubject: delivered\r\n\r\nbody"),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,delivered_at=?,mime_base64='' WHERE id=?`, sendQueueStatusDelivered, now.Format(time.RFC3339Nano), deliveredID); err != nil {
		t.Fatal(err)
	}
	if code := client.do("POST", "/api/mail/send-queue/"+deliveredID+"/retry", nil, &map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("delivered retry should be rejected, code=%d", code)
	}

	cancelID, err := a.enqueueSend(context.Background(), sendQueueInput{
		UserID:     user.ID,
		MailboxID:  mb.ID,
		MessageID:  "<cancel-api@example.test>",
		Source:     sendSourceWebmail,
		MailFrom:   mb.Address,
		HeaderFrom: mb.Address,
		Recipients: []string{"person@example.test"},
		MIMEBytes:  []byte("From: admin@lanqin.local\r\nTo: person@example.test\r\nSubject: cancel\r\n\r\nbody"),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var canceled SendQueueEntry
	if code := client.do("DELETE", "/api/mail/send-queue/"+cancelID, nil, &canceled); code != http.StatusOK {
		t.Fatalf("cancel queued item code=%d item=%+v", code, canceled)
	}
	if canceled.Status != sendQueueStatusCanceled {
		t.Fatalf("canceled status=%q", canceled.Status)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-received:
		if strings.Contains(body, "Subject: cancel") {
			t.Fatalf("canceled queue item was relayed: %q", body)
		}
	case <-time.After(200 * time.Millisecond):
	}
	var status string
	if err := a.db.QueryRow(`SELECT status FROM send_queue WHERE id=?`, cancelID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusCanceled {
		t.Fatalf("cancel status after worker=%q", status)
	}
}

func TestAdminSendAuditAccessAndFilters(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	domainID := mustDefaultDomainID(t, a)
	otherMB := createTestMailbox(t, admin, domainID, "audit-other", "Audit Other", "Password123!", nil)
	now := a.now().UTC()
	ctx := context.Background()
	sentFolderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,folder_id,recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"msg_audit_one", mb.ID, sentFolderID, "", "uid-audit-one", "<audit-one@example.test>", "audit one", mb.Address, "", jsonEncode([]string{"one@example.test"}), "[]", "[]", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), "audit", "", "", 1, 0, 0, 0, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO send_queue(id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"snd_audit_one", user.ID, mb.ID, "msg_audit_one", "<audit-one@example.test>", sendSourceWebmail, mb.Address, mb.Address, jsonEncode([]string{"one@example.test"}), "", sendQueueStatusQueued, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	events := []struct {
		id            string
		queueID       string
		mailboxID     string
		sentMessageID string
		event         string
		status        string
		recipients    []string
		errorText     string
		createdAt     time.Time
	}{
		{"audit_one", "snd_audit_one", mb.ID, "msg_audit_one", sendAuditQueued, sendQueueStatusQueued, []string{"one@example.test"}, "", now.Add(-2 * time.Hour)},
		{"audit_two", "snd_audit_one", mb.ID, "msg_audit_one", sendAuditFailed, sendQueueStatusFailed, []string{"one@example.test"}, "temporary failure", now.Add(-1 * time.Hour)},
		{"audit_other", "snd_audit_other", otherMB.ID, "", sendAuditDelivered, sendQueueStatusDelivered, []string{"two@example.test"}, "", now},
	}
	for _, item := range events {
		if _, err := a.db.ExecContext(ctx, `INSERT INTO send_audit_events(id,queue_id,user_id,mailbox_id,sent_message_id,source,event,status,mail_from,header_from,recipients_json,error,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.id, item.queueID, user.ID, item.mailboxID, item.sentMessageID, sendSourceWebmail, item.event, item.status, mb.Address, mb.Address, jsonEncode(item.recipients), item.errorText, item.createdAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	if code := (&testClient{t: t, server: ts}).do("GET", "/api/admin/send-audit", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated send audit code=%d", code)
	}
	regular := &testClient{t: t, server: ts}
	if code := regular.do("POST", "/api/auth/login", map[string]string{"email": otherMB.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("regular login code=%d", code)
	}
	if code := regular.do("GET", "/api/admin/send-audit", nil, nil); code != http.StatusForbidden {
		t.Fatalf("regular send audit code=%d", code)
	}
	updateRegularPermissionGroup(t, admin, []string{PermissionAdminOverview})
	if code := regular.do("GET", "/api/admin/send-audit", nil, nil); code != http.StatusForbidden {
		t.Fatalf("admin access without messages permission code=%d", code)
	}

	var all struct {
		Items []SendAuditEvent `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/send-audit", nil, &all); code != http.StatusOK || len(all.Items) != 3 {
		t.Fatalf("admin all audit code=%d items=%+v", code, all.Items)
	}
	var byMailbox struct {
		Items []SendAuditEvent `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/send-audit?mailboxId="+mb.ID, nil, &byMailbox); code != http.StatusOK || len(byMailbox.Items) != 2 {
		t.Fatalf("mailbox filter code=%d items=%+v", code, byMailbox.Items)
	}
	var byEvent struct {
		Items []SendAuditEvent `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/send-audit?event=failed", nil, &byEvent); code != http.StatusOK || len(byEvent.Items) != 1 || byEvent.Items[0].Error != "temporary failure" {
		t.Fatalf("event filter code=%d items=%+v", code, byEvent.Items)
	}
	var byMessage struct {
		Items []SendAuditEvent `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/send-audit?messageId="+url.QueryEscape("<audit-one@example.test>"), nil, &byMessage); code != http.StatusOK || len(byMessage.Items) != 2 {
		t.Fatalf("message filter code=%d items=%+v", code, byMessage.Items)
	}
	var byTime struct {
		Items []SendAuditEvent `json:"items"`
	}
	from := now.Add(-90 * time.Minute).Format(time.RFC3339Nano)
	if code := admin.do("GET", "/api/admin/send-audit?from="+url.QueryEscape(from), nil, &byTime); code != http.StatusOK || len(byTime.Items) != 2 {
		t.Fatalf("time filter code=%d items=%+v", code, byTime.Items)
	}
	if byEvent.Items[0].MailboxAddress != mb.Address || byEvent.Items[0].MessageID != "<audit-one@example.test>" {
		t.Fatalf("audit metadata missing: %+v", byEvent.Items[0])
	}
}

func TestSubmissionAuthRequiresMailboxPasswordAndSendPermission(t *testing.T) {
	a := newTestApp(t)
	user, mailbox, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatalf("authenticate submission: %v", err)
	}
	if user.Email != "admin@lanqin.local" || mailbox.Address != "admin@lanqin.local" {
		t.Fatalf("unexpected auth user=%+v mailbox=%+v", user, mailbox)
	}
	if _, _, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "wrong-password"); err == nil {
		t.Fatal("wrong password should fail")
	}

	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("Password123!"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	userID := newID("usr")
	domainID := mustDefaultDomainID(t, a)
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, userID, "nosend@lanqin.local", "No Send", "user", string(hash), 0, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,domain_id,local_part,address,display_name,password_hash,quota_mb,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, newID("mb"), userID, domainID, "nosend", "nosend@lanqin.local", "No Send", string(hash), 1024, "active", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE permission_groups SET permissions_json=?, updated_at=? WHERE id=?`, encodePermissions(withoutPermissions(regularUserDefaultPermissions(), PermissionMailSend)), now, PermissionGroupRegular); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.authenticateSubmission(ctx, "nosend@lanqin.local", "Password123!"); err == nil {
		t.Fatal("missing send permission should fail")
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE users SET disabled=1 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.authenticateSubmission(ctx, "nosend@lanqin.local", "Password123!"); err == nil {
		t.Fatal("disabled owner should fail")
	}
}

func TestSubmissionSendsRelayAndStoresSentCopy(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startCapturingSMTP(t, 2)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	raw := strings.Join([]string{
		"From: Admin <admin@lanqin.local>",
		"To: person@example.com",
		"Bcc: hidden@example.com",
		"Subject: Submission sent",
		"Message-ID: <submission-sent@example.test>",
		"Date: Tue, 24 Jun 2025 10:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"hello from submission",
	}, "\r\n")
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com", "hidden@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatalf("submit smtp message: %v", err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-received:
		if strings.Contains(strings.ToLower(body), "\r\nbcc:") || strings.Contains(body, "hidden@example.com") {
			t.Fatalf("relay body leaked bcc: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay message not received")
	}
	sentFolderID, err := a.ensureFolder(context.Background(), mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	var subject, bccJSON string
	var read int
	if err := a.db.QueryRow(`SELECT subject,bcc_addrs,is_read FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mb.ID, sentFolderID, "<submission-sent@example.test>").Scan(&subject, &bccJSON, &read); err != nil {
		t.Fatal(err)
	}
	if subject != "Submission sent" || read != 1 {
		t.Fatalf("unexpected sent message subject=%q read=%d", subject, read)
	}
	if got := jsonDecodeSlice(bccJSON); len(got) != 1 || got[0] != "hidden@example.com" {
		t.Fatalf("bcc json=%s", bccJSON)
	}
	var deliveredAudits int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM send_audit_events WHERE event=? AND status=?`, sendAuditDelivered, sendQueueStatusDelivered).Scan(&deliveredAudits); err != nil {
		t.Fatal(err)
	}
	if deliveredAudits != 1 {
		t.Fatalf("delivered audit count=%d, want 1", deliveredAudits)
	}
}

func TestSubmissionRejectsMismatchedSender(t *testing.T) {
	a := newTestApp(t)
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: attacker@example.com\r\nTo: person@example.com\r\nSubject: nope\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err == nil {
		t.Fatal("mismatched header From should fail")
	}
	raw = "From: admin@lanqin.local, attacker@example.com\r\nTo: person@example.com\r\nSubject: nope\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err == nil {
		t.Fatal("multiple header From addresses should fail")
	}
	raw = "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: nope\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, "attacker@example.com", []string{"person@example.com"}, strings.NewReader(raw)); err == nil {
		t.Fatal("mismatched MAIL FROM should fail")
	}
}

func TestSerializeMessageUsesStableHeaderOrder(t *testing.T) {
	header := textproto.MIMEHeader{
		"Subject":  {"stable"},
		"From":     {"admin@lanqin.local"},
		"Message":  {"custom"},
		"X-Zebra":  {"z"},
		"X-Answer": {"a"},
	}
	first := string(serializeMessage(header, []byte("body")))
	for i := 0; i < 20; i++ {
		if got := string(serializeMessage(header, []byte("body"))); got != first {
			t.Fatalf("serializeMessage is not stable:\nfirst=%q\ngot=%q", first, got)
		}
	}
	if !strings.HasPrefix(first, "From: admin@lanqin.local\r\n") {
		t.Fatalf("unexpected header order: %q", first)
	}
}

func TestSubmissionRelayFailureKeepsSentCopyAndRetries(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "127.0.0.1"
	a.cfg.SMTPPort = "1"
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: relay fail\r\nMessage-ID: <relay-fail@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatalf("submission should queue relay failure for retry: %v", err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE mailbox_id=? AND message_id=?`, mb.ID, "<relay-fail@example.test>").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sent copy should remain after queued relay failure, count=%d", count)
	}
	var status, lastError string
	if err := a.db.QueryRow(`SELECT status,last_error FROM send_queue WHERE mailbox_id=? AND sent_message_id <> ''`, mb.ID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusFailed || lastError == "" {
		t.Fatalf("queue status=%q lastError=%q", status, lastError)
	}
}

func TestSubmissionSentCopyDedupesByMessageID(t *testing.T) {
	a := newTestApp(t)
	host, port, _ := startCapturingSMTP(t, 4)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: dedupe\r\nMessage-ID: <dedupe@example.test>\r\n\r\nbody"
	for i := 0; i < 2; i++ {
		if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	sentFolderID, err := a.ensureFolder(context.Background(), mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mb.ID, sentFolderID, "<dedupe@example.test>").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sent copy count=%d, want 1", count)
	}
	var queueCount int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM send_queue WHERE mailbox_id=? AND message_id=?`, mb.ID, "<dedupe@example.test>").Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 1 {
		t.Fatalf("send queue count=%d, want 1", queueCount)
	}
}

func TestInsertSentMessageOnceFailsWhenDedupeKeyHasNoMessage(t *testing.T) {
	a := newTestApp(t)
	_, mb := defaultAdminUserAndMailbox(t, a)
	ctx := context.Background()
	sentFolderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	messageID := "<orphan-sent-dedupe@example.test>"
	if err := a.insertSentDedupeKey(ctx, mb.ID, sentFolderID, messageID); err != nil {
		t.Fatal(err)
	}
	now := a.now().UTC()
	sentID, inserted, err := a.insertSentMessageOnce(ctx, storedMessage{
		MailboxID:  mb.ID,
		MessageUID: newID("uid"),
		MessageID:  messageID,
		Subject:    "orphan dedupe",
		From:       mb.Address,
		To:         []string{"person@example.com"},
		SentAt:     now,
		ReceivedAt: now,
		BodyText:   "body",
		IsRead:     true,
	}, nil)
	if !errors.Is(err, errSentDedupeExists) {
		t.Fatalf("insertSentMessageOnce error=%v, want errSentDedupeExists", err)
	}
	if sentID != "" || inserted {
		t.Fatalf("sentID=%q inserted=%v, want empty false", sentID, inserted)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mb.ID, sentFolderID, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan dedupe should not create sent message, count=%d", count)
	}
}

func TestSubmissionRequeuesTerminalFailedDuplicateMessageID(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SMTPHost = "127.0.0.1"
	a.cfg.SMTPPort = "1"
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: requeue\r\nMessage-ID: <requeue@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,attempt_count=max_attempts,next_attempt_at=?,last_error='terminal' WHERE mailbox_id=? AND message_id=?`, sendQueueStatusFailed, a.now().UTC().Add(time.Hour).Format(time.RFC3339Nano), mb.ID, "<requeue@example.test>"); err != nil {
		t.Fatal(err)
	}

	host, port, received := startCapturingSMTP(t, 1)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("requeued terminal failure was not relayed")
	}
	var status string
	var attemptCount int
	if err := a.db.QueryRow(`SELECT status,attempt_count FROM send_queue WHERE mailbox_id=? AND message_id=?`, mb.ID, "<requeue@example.test>").Scan(&status, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusDelivered || attemptCount != 1 {
		t.Fatalf("queue status=%q attempts=%d, want delivered attempts=1", status, attemptCount)
	}
}

func TestSubmissionRequeuesDeliveredDuplicateMessageID(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startCapturingSMTP(t, 2)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: resend\r\nMessage-ID: <delivered-requeue@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery not received")
	}
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("requeued delivered message was not relayed")
	}
	var status string
	var attemptCount int
	if err := a.db.QueryRow(`SELECT status,attempt_count FROM send_queue WHERE mailbox_id=? AND message_id=?`, mb.ID, "<delivered-requeue@example.test>").Scan(&status, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusDelivered || attemptCount != 1 {
		t.Fatalf("queue status=%q attempts=%d, want delivered attempts=1", status, attemptCount)
	}
}

func TestSubmissionRequeuesCanceledDuplicateMessageID(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startCapturingSMTP(t, 1)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	user, mb, err := a.authenticateSubmission(context.Background(), "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: canceled resend\r\nMessage-ID: <canceled-requeue@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE send_queue SET status=?,updated_at=? WHERE mailbox_id=? AND message_id=?`, sendQueueStatusCanceled, a.now().UTC().Format(time.RFC3339Nano), mb.ID, "<canceled-requeue@example.test>"); err != nil {
		t.Fatal(err)
	}
	if err := a.submitSMTPMessage(context.Background(), user, mb, mb.Address, []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("requeued canceled message was not relayed")
	}
	var status string
	var attemptCount int
	if err := a.db.QueryRow(`SELECT status,attempt_count FROM send_queue WHERE mailbox_id=? AND message_id=?`, mb.ID, "<canceled-requeue@example.test>").Scan(&status, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if status != sendQueueStatusDelivered || attemptCount != 1 {
		t.Fatalf("queue status=%q attempts=%d, want delivered attempts=1", status, attemptCount)
	}
}

func TestSubmissionAllowsAuthorizedAliasSendAs(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	if _, err := a.db.ExecContext(ctx, `INSERT INTO aliases(id,domain_id,source,destination,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("als"), mustDefaultDomainID(t, a), "team@lanqin.local", "admin@lanqin.local", 1, a.now().UTC().Format(time.RFC3339Nano), a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	user, mb, err := a.authenticateSubmission(ctx, "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: Team <team@lanqin.local>\r\nTo: person@example.com\r\nSubject: alias send-as\r\nMessage-ID: <alias-send-as@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(ctx, user, mb, "team@lanqin.local", []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatalf("authorized alias send-as should submit: %v", err)
	}
	sentFolderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	var fromAddr string
	if err := a.db.QueryRow(`SELECT from_addr FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mb.ID, sentFolderID, "<alias-send-as@example.test>").Scan(&fromAddr); err != nil {
		t.Fatal(err)
	}
	if fromAddr != "team@lanqin.local" {
		t.Fatalf("from_addr=%q, want alias", fromAddr)
	}
}

func TestSubmissionAllowsMultiDestinationAliasSendAs(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	if _, err := a.db.ExecContext(ctx, `INSERT INTO aliases(id,domain_id,source,destination,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("als"), mustDefaultDomainID(t, a), "team-many@lanqin.local", "other@lanqin.local, admin@lanqin.local", 1, a.now().UTC().Format(time.RFC3339Nano), a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	user, mb, err := a.authenticateSubmission(ctx, "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: Team <team-many@lanqin.local>\r\nTo: person@example.com\r\nSubject: alias send-as\r\nMessage-ID: <multi-alias-send-as@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(ctx, user, mb, "team-many@lanqin.local", []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatalf("authorized multi-destination alias send-as should submit: %v", err)
	}
}

func TestSubmissionAllowsExplicitSendAsGrant(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	user, mb, err := a.authenticateSubmission(ctx, "admin@lanqin.local", "ChangeMe123!")
	if err != nil {
		t.Fatal(err)
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO send_as_grants(id,mailbox_id,address,display_name,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("sag"), mb.ID, "support@example.com", "Support", 1, now, now); err != nil {
		t.Fatal(err)
	}
	raw := "From: Support <support@example.com>\r\nTo: person@example.com\r\nSubject: explicit send-as\r\nMessage-ID: <explicit-send-as@example.test>\r\n\r\nbody"
	if err := a.submitSMTPMessage(ctx, user, mb, "support@example.com", []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatalf("explicit send-as grant should submit: %v", err)
	}
	sentFolderID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	var fromAddr, fromName string
	if err := a.db.QueryRow(`SELECT from_addr,from_name FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mb.ID, sentFolderID, "<explicit-send-as@example.test>").Scan(&fromAddr, &fromName); err != nil {
		t.Fatal(err)
	}
	if fromAddr != "support@example.com" || fromName != "Support" {
		t.Fatalf("from=%q name=%q, want explicit grant", fromAddr, fromName)
	}
}

func TestSentMessageDedupeTableExists(t *testing.T) {
	a := newTestApp(t)
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='sent_message_dedupe_keys'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sent message dedupe table count=%d, want 1", count)
	}
}

func TestSendQueueMessageIDMigrationDropsDuplicatesBeforeUniqueIndex(t *testing.T) {
	a := newTestApp(t)
	user, mb := defaultAdminUserAndMailbox(t, a)
	if _, err := a.db.Exec(`DROP INDEX IF EXISTS idx_send_queue_mailbox_source_message_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO send_queue(id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"dup_old", user.ID, mb.ID, "sent1", "<dup@example.test>", sendSourceSubmission, "admin@lanqin.local", "admin@lanqin.local", "[]", "bWVzc2FnZQ==", sendQueueStatusDelivered, a.now().UTC().Format(time.RFC3339Nano), "2026-06-24T00:00:00Z", "2026-06-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO send_queue(id,user_id,mailbox_id,sent_message_id,message_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"dup_keep", user.ID, mb.ID, "sent2", "<dup@example.test>", sendSourceSubmission, "admin@lanqin.local", "admin@lanqin.local", "[]", "bWVzc2FnZQ==", sendQueueStatusQueued, a.now().UTC().Format(time.RFC3339Nano), "2026-06-24T00:01:00Z", "2026-06-24T00:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateSendQueueMessageID(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(1) FROM send_queue WHERE mailbox_id=? AND source=? AND message_id='<dup@example.test>'`, mb.ID, sendSourceSubmission).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate queue rows count=%d, want 1", count)
	}
	var keptID string
	if err := a.db.QueryRow(`SELECT id FROM send_queue WHERE mailbox_id=? AND source=? AND message_id='<dup@example.test>'`, mb.ID, sendSourceSubmission).Scan(&keptID); err != nil {
		t.Fatal(err)
	}
	if keptID != "dup_keep" {
		t.Fatalf("kept queue id=%q, want dup_keep", keptID)
	}
}

func TestSubmissionTLSConfigRequiresCertificateFiles(t *testing.T) {
	a := newTestApp(t)
	a.cfg.SubmissionAddr = ":587"
	a.cfg.SubmissionTLSAddr = ":465"
	if _, err := LoadServerTLSConfig(a.cfg); err == nil {
		t.Fatal("submission TLS config should require certificate files")
	}
}

func TestSubmissionTLSConfigReloadsCertificateFiles(t *testing.T) {
	a := newTestApp(t)
	certPath, keyPath := writeTestCertificateFiles(t, "first.example.test")
	a.cfg.TLSCertFile = certPath
	a.cfg.TLSKeyFile = keyPath
	tlsConfig, err := LoadServerTLSConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := tlsConfig.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	firstLeaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	nextCertPath, nextKeyPath := writeTestCertificateFiles(t, "second.example.test")
	nextCert, err := os.ReadFile(nextCertPath)
	if err != nil {
		t.Fatal(err)
	}
	nextKey, err := os.ReadFile(nextKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, nextCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, nextKey, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := tlsConfig.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if firstLeaf.Subject.CommonName != "first.example.test" || secondLeaf.Subject.CommonName != "second.example.test" {
		t.Fatalf("cert reload common names first=%q second=%q", firstLeaf.Subject.CommonName, secondLeaf.Subject.CommonName)
	}
}

func TestSubmissionServersAcceptStartTLSAndImplicitTLS(t *testing.T) {
	a := newTestApp(t)
	host, port, received := startCapturingSMTP(t, 2)
	a.cfg.SMTPHost = host
	a.cfg.SMTPPort = port
	certPath, keyPath := writeTestCertificateFiles(t, "mail.example.test")
	a.cfg.TLSCertFile = certPath
	a.cfg.TLSKeyFile = keyPath
	tlsConfig, err := LoadServerTLSConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}

	startServer := func(t *testing.T, implicit bool) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := a.newSubmissionServer(ln.Addr().String(), tlsConfig)
		go func() {
			if implicit {
				_ = srv.Serve(tls.NewListener(ln, tlsConfig))
			} else {
				_ = srv.Serve(ln)
			}
		}()
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		return ln.Addr().String()
	}

	raw := "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: starttls\r\nMessage-ID: <starttls@example.test>\r\n\r\nbody"
	addr := startServer(t, false)
	client, err := smtpclient.DialStartTLS(addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Auth(sasl.NewPlainClient("", "admin@lanqin.local", "ChangeMe123!")); err != nil {
		t.Fatal(err)
	}
	if err := client.SendMail("admin@lanqin.local", []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("starttls relay not received")
	}

	raw = "From: admin@lanqin.local\r\nTo: person@example.com\r\nSubject: smtps\r\nMessage-ID: <smtps@example.test>\r\n\r\nbody"
	addr = startServer(t, true)
	client, err = smtpclient.DialTLS(addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Auth(sasl.NewPlainClient("", "admin@lanqin.local", "ChangeMe123!")); err != nil {
		t.Fatal(err)
	}
	if err := client.SendMail("admin@lanqin.local", []string{"person@example.com"}, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := a.processDueSendQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("implicit tls relay not received")
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

func TestAuthPolicyDovecotResponseFormat(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}

	var allowed map[string]any
	if code := client.do("POST", "/auth-policy?command=allow", map[string]string{"login": "admin@lanqin.local", "protocol": "smtp"}, &allowed); code != http.StatusOK {
		t.Fatalf("auth policy allow code=%d body=%v", code, allowed)
	}
	if allowed["status"] != float64(0) {
		t.Fatalf("expected numeric allow status 0, got %#v", allowed["status"])
	}

	var denied map[string]any
	if code := client.do("POST", "/auth-policy?command=allow", map[string]string{"login": "missing@lanqin.local", "protocol": "imap"}, &denied); code != http.StatusOK {
		t.Fatalf("auth policy deny code=%d body=%v", code, denied)
	}
	if denied["status"] != float64(-1) {
		t.Fatalf("expected numeric deny status -1, got %#v", denied["status"])
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

func TestUserMailSignaturesDefaultResolution(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}
	domainID := mustDefaultDomainID(t, a)
	mb1 := createTestMailbox(t, admin, domainID, "signer", "Signer", "Password123!", nil)
	mb2 := createTestMailbox(t, admin, domainID, "second", "Second", "Password123!", map[string]any{"ownerEmail": mb1.Address})

	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": mb1.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login code=%d", code)
	}
	var global MailSignature
	if code := user.do("POST", "/api/me/signatures", map[string]any{"name": "全局签名", "content": "Global Sig", "isDefault": true}, &global); code != http.StatusCreated || !global.IsDefault || global.MailboxID != "" {
		t.Fatalf("create global signature code=%d sig=%+v", code, global)
	}
	var bound MailSignature
	if code := user.do("POST", "/api/me/signatures", map[string]any{"mailboxId": mb1.ID, "name": "邮箱签名", "content": "Mailbox Sig", "isDefault": true}, &bound); code != http.StatusCreated || !bound.IsDefault || bound.MailboxID != mb1.ID {
		t.Fatalf("create bound signature code=%d sig=%+v", code, bound)
	}
	var defaultResp struct {
		Signature *MailSignature `json:"signature"`
	}
	if code := user.do("GET", "/api/me/signatures/default?mailboxId="+mb1.ID, nil, &defaultResp); code != http.StatusOK || defaultResp.Signature == nil || defaultResp.Signature.ID != bound.ID {
		t.Fatalf("bound default code=%d resp=%+v", code, defaultResp)
	}
	if code := user.do("GET", "/api/me/signatures/default?mailboxId="+mb2.ID, nil, &defaultResp); code != http.StatusOK || defaultResp.Signature == nil || defaultResp.Signature.ID != global.ID {
		t.Fatalf("global fallback code=%d resp=%+v", code, defaultResp)
	}
	var updated MailSignature
	if code := user.do("POST", "/api/me/signatures/"+bound.ID, map[string]any{"mailboxId": mb1.ID, "name": "更新签名", "content": "Updated Sig", "isDefault": false}, &updated); code != http.StatusOK || updated.IsDefault || updated.Content != "Updated Sig" {
		t.Fatalf("update signature code=%d sig=%+v", code, updated)
	}
	if code := user.do("GET", "/api/me/signatures/default?mailboxId="+mb1.ID, nil, &defaultResp); code != http.StatusOK || defaultResp.Signature == nil || defaultResp.Signature.ID != global.ID {
		t.Fatalf("fallback after update code=%d resp=%+v", code, defaultResp)
	}
	var ok map[string]any
	if code := user.do("DELETE", "/api/me/signatures/"+global.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete signature code=%d body=%v", code, ok)
	}
	if code := user.do("GET", "/api/me/signatures/default?mailboxId="+mb2.ID, nil, &defaultResp); code != http.StatusOK || defaultResp.Signature != nil {
		t.Fatalf("empty default code=%d resp=%+v", code, defaultResp)
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
	var domainID string
	if err := a.db.QueryRowContext(context.Background(), `SELECT id FROM domains WHERE name=?`, "lanqin.local").Scan(&domainID); err != nil {
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

func TestFixedRolesProtectAdminRoutesAndDefaultAdmin(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}

	var groups struct {
		Items []PermissionGroup `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/permission-groups", nil, &groups); code != http.StatusOK || len(groups.Items) != len(defaultPermissionGroups()) {
		t.Fatalf("fixed permission groups code=%d groups=%+v", code, groups.Items)
	}
	groupByID := map[string]PermissionGroup{}
	for _, group := range groups.Items {
		groupByID[group.ID] = group
	}
	for _, group := range defaultPermissionGroups() {
		if _, ok := groupByID[group.ID]; !ok {
			t.Fatalf("missing fixed permission group %s in %+v", group.ID, groups.Items)
		}
	}
	if groupByID[PermissionGroupRegular].Limits != defaultPermissionLimits() {
		t.Fatalf("regular group limits=%+v want %+v", groupByID[PermissionGroupRegular].Limits, defaultPermissionLimits())
	}
	if groups.Items[0].ID != PermissionGroupSuperAdmin || groups.Items[1].ID != PermissionGroupRegular {
		t.Fatalf("unexpected fixed permission groups: %+v", groups.Items)
	}

	var errBody map[string]any
	var users struct {
		Items []AdminUser `json:"items"`
	}
	var customGroup PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups", map[string]any{
		"name":        "Mailbox Viewers",
		"description": "Can view mailboxes only",
		"permissions": []string{PermissionAdminOverview, PermissionMailboxesView},
		"limits":      PermissionLimits{MaxAttachmentMB: 5, SMTPDailyLimit: 8, SMTPMinuteLimit: 2, IMAPMinuteLimit: 5, POP3MinuteLimit: 3},
	}, &customGroup); code != http.StatusCreated {
		t.Fatalf("custom permission group creation code=%d group=%+v", code, customGroup)
	}
	if customGroup.Limits.MaxAttachmentMB != 5 || customGroup.Limits.SMTPDailyLimit != 8 || customGroup.Limits.SMTPMinuteLimit != 2 || customGroup.Limits.IMAPMinuteLimit != 5 || customGroup.Limits.POP3MinuteLimit != 3 {
		t.Fatalf("custom permission group limits=%+v", customGroup.Limits)
	}
	if customGroup.System || customGroup.ID == "" || !userHasPermission(&User{Role: "user", Permissions: customGroup.Permissions}, PermissionMailboxesView) || userHasPermission(&User{Role: "user", Permissions: customGroup.Permissions}, PermissionMailboxesCreate) {
		t.Fatalf("custom permission group permissions=%+v", customGroup)
	}
	if code := admin.do("POST", "/api/admin/permission-groups/"+PermissionGroupSuperAdmin, map[string]any{
		"name":        "Changed",
		"description": "Should not change",
		"permissions": []string{PermissionMailboxesView},
	}, &errBody); code != http.StatusForbidden {
		t.Fatalf("system permission group update should be forbidden code=%d body=%v", code, errBody)
	}
	regularGroup := updateRegularPermissionGroup(t, admin, []string{PermissionAdminOverview})
	if !regularGroup.System || !userHasPermission(&User{Role: "user", Permissions: regularGroup.Permissions}, PermissionAdminOverview) {
		t.Fatalf("regular group update did not persist permissions=%+v", regularGroup)
	}
	if code := admin.do("DELETE", "/api/admin/permission-groups/"+PermissionGroupSuperAdmin, nil, &errBody); code != http.StatusForbidden {
		t.Fatalf("system permission group delete should be forbidden code=%d body=%v", code, errBody)
	}
	if code := admin.do("DELETE", "/api/admin/permission-groups/"+PermissionGroupRegular, nil, &errBody); code != http.StatusForbidden {
		t.Fatalf("regular user group delete should be forbidden code=%d body=%v", code, errBody)
	}
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email":              "invalid-group@lanqin.local",
		"displayName":        "Invalid Group",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{PermissionGroupSuperAdmin},
	}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("assigning super admin group should be rejected code=%d body=%v", code, errBody)
	}

	var mailboxAdminGroup PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups", map[string]any{
		"name":        "Mailbox Admins",
		"description": "Can manage mailboxes",
		"permissions": []string{
			PermissionAdminOverview,
			PermissionUsersView,
			PermissionDomainsView,
			PermissionMailboxesView,
			PermissionMailboxesCreate,
			PermissionMailboxesUpdate,
			PermissionMailboxesDelete,
		},
	}, &mailboxAdminGroup); code != http.StatusCreated {
		t.Fatalf("create mailbox admin group code=%d group=%+v", code, mailboxAdminGroup)
	}

	var userAdminGroup PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups", map[string]any{
		"name":        "User Admins",
		"description": "Can manage users",
		"permissions": []string{
			PermissionAdminOverview,
			PermissionUsersView,
			PermissionUsersCreate,
			PermissionUsersUpdate,
			PermissionUsersDelete,
			PermissionUsersResetPassword,
			PermissionGroupsView,
		},
	}, &userAdminGroup); code != http.StatusCreated {
		t.Fatalf("create user admin group code=%d group=%+v", code, userAdminGroup)
	}

	var mailboxUser AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email":              "mailbox-admin@lanqin.local",
		"displayName":        "Mailbox Admin",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{mailboxAdminGroup.ID},
	}, &mailboxUser); code != http.StatusCreated {
		t.Fatalf("create mailbox admin user code=%d user=%+v", code, mailboxUser)
	}
	if mailboxUser.Role != "user" || !containsString(mailboxUser.PermissionGroupIDs, PermissionGroupRegular) || !containsString(mailboxUser.PermissionGroupIDs, mailboxAdminGroup.ID) || !userHasPermission(&mailboxUser.User, PermissionMailboxesManage) || userHasPermission(&mailboxUser.User, PermissionSystemSettings) {
		t.Fatalf("mailbox admin authorization=%+v", mailboxUser.User)
	}

	var plainUser AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email":              "plain-user@lanqin.local",
		"displayName":        "Plain User",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{},
	}, &plainUser); code != http.StatusCreated {
		t.Fatalf("create plain user code=%d user=%+v", code, plainUser)
	}
	if len(plainUser.PermissionGroupIDs) != 1 || plainUser.PermissionGroupIDs[0] != PermissionGroupRegular || !userHasPermission(&plainUser.User, PermissionAdminOverview) {
		t.Fatalf("plain user should inherit regular permissions: %+v", plainUser.User)
	}

	var customUser AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email":              "mailbox-viewer@lanqin.local",
		"displayName":        "Mailbox Viewer",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{customGroup.ID},
	}, &customUser); code != http.StatusCreated {
		t.Fatalf("create custom group user code=%d user=%+v", code, customUser)
	}
	if !userHasPermission(&customUser.User, PermissionMailboxesView) || userHasPermission(&customUser.User, PermissionMailboxesCreate) {
		t.Fatalf("custom group user authorization=%+v", customUser.User)
	}
	if code := admin.do("DELETE", "/api/admin/permission-groups/"+customGroup.ID, nil, &errBody); code != http.StatusBadRequest {
		t.Fatalf("assigned custom permission group delete should be rejected code=%d body=%v", code, errBody)
	}

	mailboxAdmin := &testClient{t: t, server: ts}
	if code := mailboxAdmin.do("POST", "/api/auth/login", map[string]string{"email": "mailbox-admin@lanqin.local", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("mailbox admin login code=%d", code)
	}
	var mailboxList struct {
		Items []Mailbox `json:"items"`
	}
	if code := mailboxAdmin.do("GET", "/api/admin/mailboxes", nil, &mailboxList); code != http.StatusOK {
		t.Fatalf("mailbox admin should access mailboxes code=%d", code)
	}
	if code := mailboxAdmin.do("GET", "/api/admin/settings", nil, &errBody); code != http.StatusForbidden {
		t.Fatalf("mailbox admin settings should be forbidden code=%d body=%v", code, errBody)
	}
	if code := mailboxAdmin.do("GET", "/api/admin/users", nil, &errBody); code != http.StatusOK {
		t.Fatalf("mailbox admin should read users for mailbox ownership code=%d body=%v", code, errBody)
	}
	viewer := &testClient{t: t, server: ts}
	if code := viewer.do("POST", "/api/auth/login", map[string]string{"email": "mailbox-viewer@lanqin.local", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("mailbox viewer login code=%d", code)
	}
	if code := viewer.do("GET", "/api/admin/mailboxes", nil, &mailboxList); code != http.StatusOK {
		t.Fatalf("mailbox viewer should read mailboxes code=%d", code)
	}
	if code := viewer.do("POST", "/api/admin/mailboxes", map[string]any{
		"domainId":    mustDefaultDomainID(t, a),
		"localPart":   "blocked-create",
		"displayName": "Blocked Create",
		"password":    "Password123!",
		"quotaMb":     1024,
		"role":        "user",
	}, &errBody); code != http.StatusForbidden {
		t.Fatalf("mailbox viewer should not create mailboxes code=%d body=%v", code, errBody)
	}
	if code := mailboxAdmin.do("POST", "/api/admin/users", map[string]any{
		"email":              "blocked-by-mailbox-admin@lanqin.local",
		"displayName":        "Blocked",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{mailboxAdminGroup.ID},
	}, &errBody); code != http.StatusForbidden {
		t.Fatalf("mailbox admin should not create users code=%d body=%v", code, errBody)
	}

	var userManager AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email":              "user-admin@lanqin.local",
		"displayName":        "User Admin",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{userAdminGroup.ID},
	}, &userManager); code != http.StatusCreated {
		t.Fatalf("create user admin code=%d user=%+v", code, userManager)
	}
	userAdmin := &testClient{t: t, server: ts}
	if code := userAdmin.do("POST", "/api/auth/login", map[string]string{"email": "user-admin@lanqin.local", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user admin login code=%d", code)
	}
	if code := userAdmin.do("GET", "/api/admin/users", nil, &users); code != http.StatusOK {
		t.Fatalf("user admin users code=%d body=%v", code, users)
	}
	if code := userAdmin.do("POST", "/api/admin/users", map[string]any{
		"email":              "delegated-mailbox@lanqin.local",
		"displayName":        "Delegated Mailbox",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{mailboxAdminGroup.ID},
	}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("user admin should not assign mailbox admin group code=%d body=%v", code, errBody)
	}
	var regularUser AdminUser
	if code := userAdmin.do("POST", "/api/admin/users", map[string]any{
		"email":              "delegated-user@lanqin.local",
		"displayName":        "Delegated User",
		"role":               "user",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{userAdminGroup.ID},
	}, &regularUser); code != http.StatusCreated {
		t.Fatalf("user admin should assign own group code=%d user=%+v", code, regularUser)
	}
	if code := userAdmin.do("POST", "/api/admin/users", map[string]any{
		"email":              "delegated-super@lanqin.local",
		"displayName":        "Delegated Super",
		"role":               "admin",
		"password":           "Password123!",
		"disabled":           false,
		"permissionGroupIds": []string{},
	}, &errBody); code != http.StatusForbidden {
		t.Fatalf("user admin should not create super admin code=%d body=%v", code, errBody)
	}

	if code := admin.do("GET", "/api/admin/users", nil, &users); code != http.StatusOK || len(users.Items) == 0 {
		t.Fatalf("admin users code=%d items=%d", code, len(users.Items))
	}
	var defaultAdmin AdminUser
	for _, user := range users.Items {
		if user.Email == "admin@lanqin.local" {
			defaultAdmin = user
			break
		}
	}
	if defaultAdmin.ID == "" || !defaultAdmin.Protected || defaultAdmin.Role != "admin" {
		t.Fatalf("default admin should be protected super admin: %+v", defaultAdmin.User)
	}
	if code := admin.do("POST", "/api/admin/users/"+defaultAdmin.ID, map[string]any{
		"displayName": "LanQin Admin",
		"role":        "user",
		"disabled":    false,
	}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("default admin downgrade should be rejected code=%d body=%v", code, errBody)
	}
	if code := admin.do("POST", "/api/admin/users/"+defaultAdmin.ID, map[string]any{
		"displayName": "LanQin Admin",
		"role":        "admin",
		"disabled":    true,
	}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("default admin disable should be rejected code=%d body=%v", code, errBody)
	}
	if code := admin.do("DELETE", "/api/admin/users/"+defaultAdmin.ID, nil, &errBody); code != http.StatusBadRequest {
		t.Fatalf("default admin delete should be rejected code=%d body=%v", code, errBody)
	}
}

func TestLegacySystemPermissionGroupsAreCleanedUp(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	legacyIDs := []string{
		"pg_permission_manager",
		"pg_user_manager",
		"pg_system_operator",
		"pg_mail_operator",
	}

	for _, groupID := range legacyIDs {
		if _, err := a.db.ExecContext(ctx, `INSERT INTO permission_groups(id,name,description,permissions_json,system,created_at,updated_at)
			VALUES(?,?,?,?,1,?,?)`, groupID, "Legacy "+groupID, "", "[]", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ensureDefaultPermissionGroups(ctx); err != nil {
		t.Fatal(err)
	}
	for _, groupID := range legacyIDs {
		var count int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM permission_groups WHERE id=?`, groupID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("legacy permission group %s was not removed", groupID)
		}
	}
}

func TestRegularUserMailPermissionsAreEnforced(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d body=%v", code, login)
	}
	mb := createTestMailbox(t, admin, mustDefaultDomainID(t, a), "front-perm", "Front Permissions", "Password123!", nil)

	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": mb.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login code=%d", code)
	}
	var mine struct {
		Items []Mailbox `json:"items"`
	}
	if code := user.do("GET", "/api/mail/mailboxes", nil, &mine); code != http.StatusOK || len(mine.Items) != 1 || mine.Items[0].ID != mb.ID {
		t.Fatalf("regular user should access mail front code=%d items=%+v", code, mine.Items)
	}
	var errBody map[string]any
	if code := user.do("GET", "/api/admin/overview", nil, &errBody); code != http.StatusForbidden {
		t.Fatalf("regular mail permissions should not grant admin access code=%d body=%v", code, errBody)
	}

	updateRegularPermissionGroup(t, admin, withoutPermissions(regularUserDefaultPermissions(), PermissionMailAccess))
	noAccess := &testClient{t: t, server: ts}
	if code := noAccess.do("POST", "/api/auth/login", map[string]string{"email": mb.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("no access login code=%d", code)
	}
	if code := noAccess.do("GET", "/api/mail/mailboxes", nil, &errBody); code != http.StatusForbidden {
		t.Fatalf("missing mail access should block mailbox list code=%d body=%v", code, errBody)
	}

	updateRegularPermissionGroup(t, admin, withoutPermissions(regularUserDefaultPermissions(), PermissionMailSend))
	noSend := &testClient{t: t, server: ts}
	if code := noSend.do("POST", "/api/auth/login", map[string]string{"email": mb.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("no send login code=%d", code)
	}
	sendPayload := map[string]any{
		"mailboxId": mb.ID,
		"to":        []string{"someone@example.test"},
		"subject":   "blocked send",
		"text":      "body",
		"html":      "<p>body</p>",
	}
	if code := noSend.do("POST", "/api/mail/send", sendPayload, &errBody); code != http.StatusForbidden {
		t.Fatalf("missing send permission should block send code=%d body=%v", code, errBody)
	}
	schedulePayload := map[string]any{
		"mailboxId": mb.ID,
		"to":        []string{"someone@example.test"},
		"subject":   "blocked schedule",
		"text":      "body",
		"html":      "<p>body</p>",
		"sendAt":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if code := noSend.do("POST", "/api/mail/schedule-send", schedulePayload, &errBody); code != http.StatusForbidden {
		t.Fatalf("missing send permission should block scheduled send creation code=%d body=%v", code, errBody)
	}
	if code := noSend.do("GET", "/api/mail/scheduled-sends?mailboxId="+mb.ID, nil, &struct {
		Items []ScheduledSend `json:"items"`
	}{}); code != http.StatusOK {
		t.Fatalf("schedule management permission should remain usable code=%d", code)
	}
}

func TestMaildirSyncImportsRFC822(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	root := t.TempDir()
	a.cfg.MaildirRoot = root
	var domainID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM domains WHERE name=?`, "lanqin.local").Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	adminUser, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	// seed() already created mailbox admin@lanqin.local
	var mailboxID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id=? AND address=?`, adminUser.ID, "admin@lanqin.local").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id=?`, mailboxID); err != nil {
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

func TestMaildirImportStoresAuthenticationResults(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	root := t.TempDir()
	a.cfg.MaildirRoot = root
	ts := httptest.NewServer(a.Router())
	defer ts.Close()

	adminUser, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	var mailboxID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id=? AND address=?`, adminUser.ID, "admin@lanqin.local").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id=?`, mailboxID); err != nil {
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
		"Subject: auth import test",
		"Message-Id: <auth-import@example.test>",
		"Date: Sat, 13 Jun 2026 13:00:00 +0000",
		"Authentication-Results: mx.lanqin.local; spf=pass smtp.mailfrom=example.test; dkim=fail header.d=example.test; dmarc=temperror",
		"Received-SPF: pass (mx.lanqin.local: domain of sender@example.test designates 192.0.2.1 as permitted sender)",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"auth body",
	}, "\r\n")
	if err := os.WriteFile(filepath.Join(dir, "1749819601.M1P1.auth"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, err := a.syncMaildirOnce(ctx); err != nil || count != 1 {
		t.Fatalf("sync count=%d err=%v", count, err)
	}

	client := &testClient{t: t, server: ts}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	var list struct {
		Items []MailMessage `json:"items"`
	}
	if code := client.do("GET", "/api/mail/messages?folder=Inbox&q=auth%20import", nil, &list); code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("list code=%d items=%+v", code, list.Items)
	}
	var detail MailMessage
	if code := client.do("GET", "/api/mail/messages/"+list.Items[0].ID+"?markRead=0", nil, &detail); code != http.StatusOK {
		t.Fatalf("detail code=%d detail=%+v", code, detail)
	}
	if detail.Authentication.SPF != "pass" || detail.Authentication.DKIM != "fail" || detail.Authentication.DMARC != "temperror" {
		t.Fatalf("unexpected auth detail: %+v", detail.Authentication)
	}
	if !strings.Contains(detail.Authentication.AuthenticationResults, "spf=pass") || !strings.Contains(detail.Authentication.ReceivedSPF, "sender@example.test") {
		t.Fatalf("raw auth headers missing: %+v", detail.Authentication)
	}
}

func TestMaildirSyncHealthDisabled(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}

	var health maildirSyncHealthResponse
	if code := admin.do("GET", "/api/admin/maildir-sync/health", nil, &health); code != http.StatusOK {
		t.Fatalf("health code=%d body=%+v", code, health)
	}
	if health.Configured || health.Enabled || health.WorkerStarted || health.Running {
		t.Fatalf("unexpected disabled health: %+v", health)
	}
	if health.ScanSeconds != 30 {
		t.Fatalf("scan seconds=%d, want default 30", health.ScanSeconds)
	}
}

func TestMaildirSyncHealthAfterTrackedSync(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	root := t.TempDir()
	a.cfg.MaildirRoot = root
	a.cfg.MaildirScanSeconds = 45
	adminUser, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	var mailboxID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id=? AND address=?`, adminUser.ID, "admin@lanqin.local").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id=?`, mailboxID); err != nil {
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
		"Subject: Maildir health import",
		"Message-Id: <maildir-health@example.test>",
		"Date: Sat, 13 Jun 2026 15:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"hello from health test",
	}, "\r\n")
	if err := os.WriteFile(filepath.Join(dir, "1749826800.M1P1.health"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	counts, err := a.syncMaildirOnceTracked(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Imported != 1 || counts.FilesScanned != 1 {
		t.Fatalf("counts=%+v, want imported=1 filesScanned=1", counts)
	}
	health := a.maildirHealth.snapshot(a.cfg)
	if !health.Configured || !health.Enabled {
		t.Fatalf("configured health=%+v, want enabled", health)
	}
	if health.Running {
		t.Fatalf("health still running: %+v", health)
	}
	if health.LastRun == nil || health.LastRun.Status != "success" {
		t.Fatalf("last run=%+v, want success", health.LastRun)
	}
	if health.LastRun.Counts.Imported != 1 || health.Summary.Imported != 1 {
		t.Fatalf("health counts last=%+v summary=%+v", health.LastRun.Counts, health.Summary)
	}
	if health.NextRunAt == nil {
		t.Fatalf("next run is nil")
	}
}

func TestMaildirSyncImportsSentFolder(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	root := t.TempDir()
	a.cfg.MaildirRoot = root
	adminUser, _, err := a.userByEmail(ctx, "admin@lanqin.local")
	if err != nil {
		t.Fatal(err)
	}
	var mailboxID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id=? AND address=?`, adminUser.ID, "admin@lanqin.local").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id=?`, mailboxID); err != nil {
		t.Fatal(err)
	}
	sentFolderID, err := a.ensureFolder(ctx, mailboxID, "Sent")
	if err != nil {
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

	dir := filepath.Join(root, admin.Domain, admin.LocalPart, "Maildir", ".Sent", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := strings.Join([]string{
		"From: admin@lanqin.local",
		"To: recipient@example.test",
		"Subject: SMTP sent archive",
		"Message-Id: <smtp-sent-archive@example.test>",
		"Date: Sat, 13 Jun 2026 14:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"archived from smtp client",
	}, "\r\n")
	if err := os.WriteFile(filepath.Join(dir, "1749823200.M1P1.sent"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("imported=%d, want 1", count)
	}

	var folderID, subject string
	var read int
	err = a.db.QueryRow(`SELECT folder_id, subject, is_read FROM messages WHERE mailbox_id=? AND message_id='<smtp-sent-archive@example.test>'`, admin.ID).Scan(&folderID, &subject, &read)
	if err != nil {
		t.Fatal(err)
	}
	if folderID != sentFolderID || subject != "SMTP sent archive" || read != 1 {
		t.Fatalf("unexpected sent import folder=%q want=%q subject=%q read=%d", folderID, sentFolderID, subject, read)
	}
}

func TestWebmailSentWritesMaildirSent(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "maildir sent copy",
		Text:      "sent body",
		HTML:      "<p>sent body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	rawPath := maildirRawPathForTest(t, a, msg.ID)
	if !strings.Contains(filepath.ToSlash(rawPath), "/.Sent/cur/") {
		t.Fatalf("raw_path=%q, want .Sent/cur", rawPath)
	}
	if !strings.Contains(filepath.Base(rawPath), maildirFlagSeparator()+"S") {
		t.Fatalf("sent raw_path missing seen flag: %s", rawPath)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Subject: maildir sent copy") {
		t.Fatalf("sent maildir raw missing subject:\n%s", string(raw))
	}
	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("sync imported own sent copy=%d, want 0", count)
	}
}

func TestMaildirSyncBackfillsSQLiteOnlySent(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "legacy sent copy",
		Text:      "legacy body",
		HTML:      "<p>legacy body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldPath := maildirRawPathForTest(t, a, msg.ID)
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET raw_path='' WHERE id=?`, msg.ID); err != nil {
		t.Fatal(err)
	}
	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backfilled=%d, want 1", count)
	}
	newPath := maildirRawPathForTest(t, a, msg.ID)
	if !strings.Contains(filepath.ToSlash(newPath), "/.Sent/cur/") {
		t.Fatalf("raw_path=%q, want .Sent/cur", newPath)
	}
	var messages int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND message_id=?`, mb.ID, msg.MessageID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("messages with same Message-ID=%d, want 1", messages)
	}
}

func TestDraftWritesAndUpdatesMaildirDrafts(t *testing.T) {
	a := newTestApp(t)
	a.cfg.MaildirRoot = t.TempDir()
	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	client := &testClient{t: t, server: srv}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	_, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	var draft MailMessage
	payload := map[string]any{
		"mailboxId": mb.ID,
		"to":        []string{"recipient@example.test"},
		"subject":   "draft one",
		"text":      "draft body one",
		"html":      "<p>draft body one</p>",
	}
	if code := client.do("POST", "/api/mail/drafts", payload, &draft); code != http.StatusCreated {
		t.Fatalf("save draft code=%d draft=%+v", code, draft)
	}
	rawPath := maildirRawPathForTest(t, a, draft.ID)
	if !strings.Contains(filepath.ToSlash(rawPath), "/.Drafts/cur/") {
		t.Fatalf("raw_path=%q, want .Drafts/cur", rawPath)
	}
	if !strings.Contains(filepath.Base(rawPath), maildirFlagSeparator()+"S") {
		t.Fatalf("draft raw_path missing seen flag: %s", rawPath)
	}
	oldRawPath := rawPath

	payload["subject"] = "draft two"
	payload["text"] = "draft body two"
	payload["html"] = "<p>draft body two</p>"
	if code := client.do("POST", "/api/mail/drafts/"+draft.ID, payload, &draft); code != http.StatusOK {
		t.Fatalf("update draft code=%d draft=%+v", code, draft)
	}
	rawPath = maildirRawPathForTest(t, a, draft.ID)
	if _, err := os.Stat(oldRawPath); err == nil && oldRawPath != rawPath {
		t.Fatalf("old draft maildir file still exists: %s", oldRawPath)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Subject: draft two") {
		t.Fatalf("updated draft raw missing new subject:\n%s", string(raw))
	}
}

func TestMoveAndDeleteMessageUpdateMaildir(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	client := &testClient{t: t, server: srv}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "move me",
		Text:      "move body",
		HTML:      "<p>move body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	sentPath := maildirRawPathForTest(t, a, msg.ID)
	if code := client.do("POST", "/api/mail/messages/"+msg.ID+"/move", map[string]string{"folder": "Archive"}, nil); code != http.StatusOK {
		t.Fatalf("move code=%d", code)
	}
	archivePath := maildirRawPathForTest(t, a, msg.ID)
	if !strings.Contains(filepath.ToSlash(archivePath), "/.Archive/cur/") {
		t.Fatalf("raw_path=%q, want .Archive/cur", archivePath)
	}
	if _, err := os.Stat(sentPath); err == nil && sentPath != archivePath {
		t.Fatalf("old sent maildir file still exists: %s", sentPath)
	}
	if code := client.do("DELETE", "/api/mail/messages/"+msg.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("trash code=%d", code)
	}
	trashPath := maildirRawPathForTest(t, a, msg.ID)
	if !strings.Contains(filepath.ToSlash(trashPath), "/.Trash/cur/") {
		t.Fatalf("raw_path=%q, want .Trash/cur", trashPath)
	}
	if _, err := os.Stat(archivePath); err == nil && archivePath != trashPath {
		t.Fatalf("old archive maildir file still exists: %s", archivePath)
	}
	if code := client.do("DELETE", "/api/mail/messages/"+msg.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("permanent delete code=%d", code)
	}
	if _, err := os.Stat(trashPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trash maildir file exists after permanent delete err=%v", err)
	}
}

func TestMessageFlagsUpdateMaildir(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	client := &testClient{t: t, server: srv}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"admin@lanqin.local"},
		Subject:   "flag me",
		Text:      "flag body",
		HTML:      "<p>flag body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := client.do("POST", "/api/mail/messages/"+msg.ID+"/mark-read", map[string]bool{"read": false}, nil); code != http.StatusOK {
		t.Fatalf("mark unread code=%d", code)
	}
	unreadPath := maildirRawPathForTest(t, a, msg.ID)
	if strings.Contains(filepath.Base(unreadPath), maildirFlagSeparator()) {
		t.Fatalf("unread path should not have seen flag: %s", unreadPath)
	}
	if !strings.EqualFold(filepath.Base(filepath.Dir(unreadPath)), "new") {
		t.Fatalf("unread path dir=%s, want new", filepath.Dir(unreadPath))
	}
	if code := client.do("POST", "/api/mail/messages/"+msg.ID+"/star", map[string]bool{"starred": true}, nil); code != http.StatusOK {
		t.Fatalf("star code=%d", code)
	}
	starredPath := maildirRawPathForTest(t, a, msg.ID)
	if !strings.Contains(filepath.Base(starredPath), maildirFlagSeparator()+"F") {
		t.Fatalf("starred path missing F flag: %s", starredPath)
	}
	if !strings.EqualFold(filepath.Base(filepath.Dir(starredPath)), "cur") {
		t.Fatalf("starred path dir=%s, want cur", filepath.Dir(starredPath))
	}
}

func TestIMAPUIDAndModSeqProgression(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	client := &testClient{t: t, server: srv}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d body=%v", code, login)
	}
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)
	sentID, err := a.ensureFolder(ctx, mb.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	archiveID, err := a.ensureFolder(ctx, mb.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}

	first, err := a.sendMailNow(ctx, user, mb, mailComposeInput{MailboxID: mb.ID, To: []string{"one@example.test"}, Subject: "uid one", Text: "one", HTML: "<p>one</p>"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.sendMailNow(ctx, user, mb, mailComposeInput{MailboxID: mb.ID, To: []string{"two@example.test"}, Subject: "uid two", Text: "two", HTML: "<p>two</p>"})
	if err != nil {
		t.Fatal(err)
	}
	var firstUID, firstModSeq, secondUID int64
	if err := a.db.QueryRowContext(ctx, `SELECT imap_uid,imap_modseq FROM messages WHERE id=?`, first.ID).Scan(&firstUID, &firstModSeq); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRowContext(ctx, `SELECT imap_uid FROM messages WHERE id=?`, second.ID).Scan(&secondUID); err != nil {
		t.Fatal(err)
	}
	if firstUID <= 0 || secondUID != firstUID+1 {
		t.Fatalf("sent UIDs first=%d second=%d, want consecutive positive", firstUID, secondUID)
	}
	var sentUIDNext int64
	if err := a.db.QueryRowContext(ctx, `SELECT uid_next FROM folders WHERE id=?`, sentID).Scan(&sentUIDNext); err != nil {
		t.Fatal(err)
	}
	if sentUIDNext <= secondUID {
		t.Fatalf("sent uid_next=%d, second uid=%d", sentUIDNext, secondUID)
	}

	starred := true
	if err := a.updateMessageMaildirFlags(ctx, first.ID, nil, &starred); err != nil {
		t.Fatal(err)
	}
	modSeq, err := a.updateMessageModSeq(ctx, first.ID, sentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET is_starred=1,imap_modseq=? WHERE id=?`, modSeq, first.ID); err != nil {
		t.Fatal(err)
	}
	var afterFlagUID, afterFlagModSeq int64
	if err := a.db.QueryRowContext(ctx, `SELECT imap_uid,imap_modseq FROM messages WHERE id=?`, first.ID).Scan(&afterFlagUID, &afterFlagModSeq); err != nil {
		t.Fatal(err)
	}
	if afterFlagUID != firstUID || afterFlagModSeq <= firstModSeq {
		t.Fatalf("after flag uid/modseq=%d/%d, want uid %d and modseq > %d", afterFlagUID, afterFlagModSeq, firstUID, firstModSeq)
	}

	if err := a.moveMessageMaildir(ctx, first.ID, archiveID); err != nil {
		t.Fatal(err)
	}
	var archiveUID, archiveModSeq int64
	var folderID string
	if err := a.db.QueryRowContext(ctx, `SELECT folder_id,imap_uid,imap_modseq FROM messages WHERE id=?`, first.ID).Scan(&folderID, &archiveUID, &archiveModSeq); err != nil {
		t.Fatal(err)
	}
	if folderID != archiveID {
		t.Fatalf("folder after move=%s, want archive %s", folderID, archiveID)
	}
	if archiveUID <= 0 {
		t.Fatalf("archive uid=%d, want positive uid", archiveUID)
	}
	var archiveUIDNext, archiveHighestModSeq int64
	if err := a.db.QueryRowContext(ctx, `SELECT uid_next,highest_modseq FROM folders WHERE id=?`, archiveID).Scan(&archiveUIDNext, &archiveHighestModSeq); err != nil {
		t.Fatal(err)
	}
	if archiveUIDNext <= archiveUID {
		t.Fatalf("archive uid_next=%d, uid=%d", archiveUIDNext, archiveUID)
	}
	if archiveModSeq != archiveHighestModSeq {
		t.Fatalf("archive modseq=%d highest=%d, want equal", archiveModSeq, archiveHighestModSeq)
	}
}

func TestMaildirSyncUpdatesMovedMessageState(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "imap moved",
		Text:      "move body",
		HTML:      "<p>move body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	sentPath := maildirRawPathForTest(t, a, msg.ID)
	archiveID, err := a.ensureFolder(ctx, mb.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(sentPath))), ".Archive", "cur")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(archiveDir, filepath.Base(sentPath))
	if err := os.Rename(sentPath, archivePath); err != nil {
		t.Fatal(err)
	}
	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("sync count=%d, want no import/backfill", count)
	}
	var folderID, rawPath string
	if err := a.db.QueryRowContext(ctx, `SELECT folder_id,raw_path FROM messages WHERE id=?`, msg.ID).Scan(&folderID, &rawPath); err != nil {
		t.Fatal(err)
	}
	if folderID != archiveID || rawPath != archivePath {
		t.Fatalf("folder/raw after move=%q %q, want %q %q", folderID, rawPath, archiveID, archivePath)
	}
}

func TestMaildirSyncKeepsDistinctCopiesWithSameMessageID(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"admin@lanqin.local"},
		Subject:   "self copy",
		Text:      "self body",
		HTML:      "<p>self body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.syncMaildirOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var copies int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND message_id=?`, mb.ID, msg.MessageID).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if copies != 2 {
		t.Fatalf("copies with same Message-ID=%d, want Sent and Inbox copies", copies)
	}
}

func TestMaildirSyncUpdatesFlagsFromIMAP(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "imap flags",
		Text:      "flag body",
		HTML:      "<p>flag body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	rawPath := maildirRawPathForTest(t, a, msg.ID)
	flaggedPath := maildirPathWithFlags(rawPath, false, true)
	if err := os.MkdirAll(filepath.Dir(flaggedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rawPath, flaggedPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET updated_at=? WHERE id=?`, a.now().UTC().Add(-10*time.Minute).Format(time.RFC3339Nano), msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.syncMaildirOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var read, starred int
	var dbPath string
	if err := a.db.QueryRowContext(ctx, `SELECT is_read,is_starred,raw_path FROM messages WHERE id=?`, msg.ID).Scan(&read, &starred, &dbPath); err != nil {
		t.Fatal(err)
	}
	if read != 0 || starred != 1 || dbPath != flaggedPath {
		t.Fatalf("flags/path after sync read=%d starred=%d path=%q want 0 1 %q", read, starred, dbPath, flaggedPath)
	}
}

func TestMaildirSyncDeletesMissingMessage(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	a.cfg.MaildirRoot = t.TempDir()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)

	msg, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"recipient@example.test"},
		Subject:   "imap delete",
		Text:      "delete body",
		HTML:      "<p>delete body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	rawPath := maildirRawPathForTest(t, a, msg.ID)
	if err := os.Remove(rawPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET updated_at=? WHERE id=?`, a.now().UTC().Add(-10*time.Minute).Format(time.RFC3339Nano), msg.ID); err != nil {
		t.Fatal(err)
	}
	count, err := a.syncMaildirOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cleanup count=%d, want 1", count)
	}
	var remaining int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=?`, msg.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("message remaining=%d, want deleted", remaining)
	}
}

func TestMailboxQuotaRejectsNewMessage(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	user, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)
	if _, err := a.db.ExecContext(ctx, `UPDATE mailboxes SET quota_mb=1 WHERE id=?`, mb.ID); err != nil {
		t.Fatal(err)
	}
	_, err := a.sendMailNow(ctx, user, mb, mailComposeInput{
		MailboxID: mb.ID,
		To:        []string{"person@example.test"},
		Subject:   "quota overflow",
		Text:      strings.Repeat("x", 1024*1024+1),
	})
	if !errors.Is(err, errMailboxQuotaExceeded) {
		t.Fatalf("sendMailNow error=%v, want quota exceeded", err)
	}

	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}
	var login map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	var errBody map[string]any
	if code := client.do("POST", "/api/mail/send", map[string]any{
		"mailboxId": mb.ID,
		"to":        []string{"person@example.test"},
		"subject":   "quota overflow api",
		"text":      strings.Repeat("y", 1024*1024+1),
	}, &errBody); code != http.StatusInsufficientStorage {
		t.Fatalf("quota api code=%d body=%v", code, errBody)
	}
}

func TestMailStatsQuotaAndCleanupIsolation(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	domainID := mustDefaultDomainID(t, a)
	aliceMB := createTestMailbox(t, admin, domainID, "quota-alice", "Quota Alice", "Password123!", map[string]any{"quotaMb": 2})
	bobMB := createTestMailbox(t, admin, domainID, "quota-bob", "Quota Bob", "Password123!", map[string]any{"quotaMb": 2})
	aliceUser, _, err := a.userByEmail(ctx, aliceMB.Address)
	if err != nil {
		t.Fatal(err)
	}
	bobUser, _, err := a.userByEmail(ctx, bobMB.Address)
	if err != nil {
		t.Fatal(err)
	}
	aliceTrash, err := a.ensureFolder(ctx, aliceMB.ID, "Trash")
	if err != nil {
		t.Fatal(err)
	}
	bobTrash, err := a.ensureFolder(ctx, bobMB.ID, "Trash")
	if err != nil {
		t.Fatal(err)
	}
	attachment := AttachmentInput{Filename: "note.txt", ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello attachment"))}
	if _, err := a.insertMessage(ctx, storedMessage{MailboxID: aliceMB.ID, FolderID: aliceTrash, MessageUID: newID("uid"), MessageID: "<alice-trash@example.test>", Subject: "alice trash", From: "sender@example.test", To: []string{aliceMB.Address}, SentAt: a.now().UTC(), ReceivedAt: a.now().UTC(), Snippet: "body", BodyText: "body"}, []AttachmentInput{attachment}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.insertMessage(ctx, storedMessage{MailboxID: bobMB.ID, FolderID: bobTrash, MessageUID: newID("uid"), MessageID: "<bob-trash@example.test>", Subject: "bob trash", From: "sender@example.test", To: []string{bobMB.Address}, SentAt: a.now().UTC(), ReceivedAt: a.now().UTC(), Snippet: "body", BodyText: "body"}, nil); err != nil {
		t.Fatal(err)
	}

	alice := &testClient{t: t, server: ts}
	if code := alice.do("POST", "/api/auth/login", map[string]string{"email": aliceMB.Address, "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("alice login code=%d", code)
	}
	var stats MailStats
	if code := alice.do("GET", "/api/me/stats?mailboxId="+aliceMB.ID, nil, &stats); code != http.StatusOK {
		t.Fatalf("stats code=%d stats=%+v", code, stats)
	}
	if stats.QuotaBytes != int64(aliceMB.QuotaMB)*1024*1024 || stats.AttachmentBytes == 0 || stats.QuotaUsedPct <= 0 {
		t.Fatalf("stats quota/attachment not populated: %+v", stats)
	}
	var cleanup struct {
		OK       bool  `json:"ok"`
		Affected int64 `json:"affected"`
	}
	if code := alice.do("POST", "/api/me/cleanup", map[string]any{"mailboxId": aliceMB.ID, "target": "empty-trash"}, &cleanup); code != http.StatusOK || cleanup.Affected != 1 {
		t.Fatalf("cleanup code=%d body=%+v", code, cleanup)
	}
	var aliceRemaining, bobRemaining int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=?`, aliceMB.ID).Scan(&aliceRemaining); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=?`, bobMB.ID).Scan(&bobRemaining); err != nil {
		t.Fatal(err)
	}
	if aliceRemaining != 0 || bobRemaining != 1 {
		t.Fatalf("cleanup isolation alice=%d bob=%d, want 0/1", aliceRemaining, bobRemaining)
	}
	if aliceUser.ID == "" || bobUser.ID == "" {
		t.Fatal("test users were not created")
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

func containsString(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func folderListContains(items []MailFolder, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}

func customFolderNames(items []MailFolder) []string {
	names := []string{}
	for _, item := range items {
		if !isSystemFolderName(item.Name) {
			names = append(names, item.Name)
		}
	}
	return names
}

func withoutPermissions(items []string, removed ...string) []string {
	removedSet := map[string]bool{}
	for _, item := range removed {
		removedSet[item] = true
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !removedSet[item] {
			out = append(out, item)
		}
	}
	return out
}

func maildirRawPathForTest(t *testing.T, a *App, messageID string) string {
	t.Helper()
	var rawPath string
	if err := a.db.QueryRow(`SELECT raw_path FROM messages WHERE id=?`, messageID).Scan(&rawPath); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(rawPath) == "" {
		t.Fatalf("message %s raw_path is empty", messageID)
	}
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("raw_path %s stat error: %v", rawPath, err)
	}
	return rawPath
}

func clearMailboxMessagesForTest(t *testing.T, a *App, mailboxID string) {
	t.Helper()
	if _, err := a.db.Exec(`DELETE FROM messages WHERE mailbox_id=?`, mailboxID); err != nil {
		t.Fatal(err)
	}
}
