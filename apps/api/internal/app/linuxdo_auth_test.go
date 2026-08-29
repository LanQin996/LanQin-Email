package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeLinuxDoOAuthClient struct {
	profile linuxDoProfile
	err     error
	calls   int
}

func (f *fakeLinuxDoOAuthClient) AuthorizationURL(clientID, redirectURI, state string) string {
	values := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state}}
	return "https://linuxdo.test/authorize?" + values.Encode()
}

func (f *fakeLinuxDoOAuthClient) ExchangeProfile(context.Context, string, string, string, string) (linuxDoProfile, error) {
	f.calls++
	return f.profile, f.err
}

func enableLinuxDoForTest(a *App, registration bool, client linuxDoOAuthClient) {
	a.cfg.LinuxDoSSOEnabled = true
	a.cfg.LinuxDoRegistrationEnabled = registration
	a.cfg.LinuxDoClientID = "client-id"
	a.cfg.LinuxDoClientSecret = "client-secret"
	a.linuxDoOAuth = client
}

func serveLinuxDoRequest(t *testing.T, a *App, method, target string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response does not contain cookie %q", name)
	return nil
}

func redirectResult(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return location.Query().Get("linuxdo")
}

func TestLinuxDoSubjectNormalization(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "number", raw: "123456", want: "123456", ok: true},
		{name: "string", raw: "\"123456\"", want: "123456", ok: true},
		{name: "trimmed string", raw: "\"  abc-123  \"", want: "abc-123", ok: true},
		{name: "missing", raw: "null"},
		{name: "negative", raw: "-1"},
		{name: "object", raw: "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := linuxDoSubject(json.RawMessage(test.raw))
			if test.ok && (err != nil || got != test.want) {
				t.Fatalf("linuxDoSubject(%s)=(%q,%v), want %q", test.raw, got, err, test.want)
			}
			if !test.ok && err == nil {
				t.Fatalf("linuxDoSubject(%s) unexpectedly succeeded with %q", test.raw, got)
			}
		})
	}
}

func TestLinuxDoOAuthContractAndEligibility(t *testing.T) {
	t.Parallel()
	client := &linuxDoHTTPClient{}
	authorize, err := url.Parse(client.AuthorizationURL("client", "https://mail.example.test/api/auth/linuxdo/callback", "state"))
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Scheme+"://"+authorize.Host+authorize.Path != linuxDoAuthorizeURL || authorize.Query().Get("scope") != "user" || authorize.Query().Get("response_type") != "code" {
		t.Fatalf("unexpected authorize URL: %s", authorize.String())
	}
	active, silenced := true, false
	profile, err := linuxDoProfileFromUserInfo(linuxDoUserInfo{ID: json.RawMessage("123"), Username: "linux-user", Active: &active, Silenced: &silenced})
	if err != nil || profile.Subject != "123" || profile.DisplayName != "linux-user" {
		t.Fatalf("eligible profile=%+v err=%v", profile, err)
	}
	inactive := false
	if _, err := linuxDoProfileFromUserInfo(linuxDoUserInfo{ID: json.RawMessage("123"), Username: "linux-user", Active: &inactive, Silenced: &silenced}); !errors.Is(err, errLinuxDoIneligible) {
		t.Fatalf("inactive error=%v", err)
	}
	muted := true
	if _, err := linuxDoProfileFromUserInfo(linuxDoUserInfo{ID: json.RawMessage("123"), Username: "linux-user", Active: &active, Silenced: &muted}); !errors.Is(err, errLinuxDoIneligible) {
		t.Fatalf("silenced error=%v", err)
	}
	if _, err := linuxDoProfileFromUserInfo(linuxDoUserInfo{ID: json.RawMessage("123"), Username: "linux-user"}); err == nil {
		t.Fatal("missing active/silenced fields were accepted")
	}
}

func TestLinuxDoStateIsHashedExpiringAndSingleUse(t *testing.T) {
	a := newTestApp(t)
	enableLinuxDoForTest(a, false, &fakeLinuxDoOAuthClient{})
	rec := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/start", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	stateCookie := responseCookie(t, rec, a.linuxDoCookieName("state"))
	if !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.Path != "/api/auth/linuxdo/callback" || stateCookie.MaxAge != int(linuxDoOAuthStateTTL.Seconds()) {
		t.Fatalf("unexpected state cookie: %+v", stateCookie)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")
	if state == "" || state != stateCookie.Value {
		t.Fatal("state URL/cookie mismatch")
	}
	var stored string
	if err := a.db.QueryRow("SELECT token_hash FROM oauth_login_states").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == state || stored != hashToken(state) {
		t.Fatal("state was not stored only as a hash")
	}
	if _, err := a.consumeLinuxDoState(context.Background(), state); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := a.consumeLinuxDoState(context.Background(), state); err == nil {
		t.Fatal("replayed state was accepted")
	}
	expired := "expired-state"
	now := a.now().UTC()
	if _, err := a.db.Exec("INSERT INTO oauth_login_states(token_hash,purpose,user_id,expires_at,created_at) VALUES(?,?,?,?,?)", hashToken(expired), "login", nil, now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.consumeLinuxDoState(context.Background(), expired); err == nil {
		t.Fatal("expired state was accepted")
	}
	mismatch, err := a.createLinuxDoState(context.Background(), "login", "")
	if err != nil {
		t.Fatal(err)
	}
	mismatchResponse := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+mismatch+"&code=valid", nil, &http.Cookie{Name: a.linuxDoCookieName("state"), Value: "different"})
	if redirectResult(t, mismatchResponse) != "state" {
		t.Fatalf("mismatched cookie result=%q", redirectResult(t, mismatchResponse))
	}
	if _, err := a.consumeLinuxDoState(context.Background(), mismatch); err != nil {
		t.Fatalf("cookie mismatch consumed server-side state: %v", err)
	}
}

func TestLinuxDoCallbackFailuresDoNotLeakOrReuseState(t *testing.T) {
	a := newTestApp(t)
	fake := &fakeLinuxDoOAuthClient{err: errLinuxDoIneligible}
	enableLinuxDoForTest(a, false, fake)
	state, err := a.createLinuxDoState(context.Background(), "login", "")
	if err != nil {
		t.Fatal(err)
	}
	stateCookie := &http.Cookie{Name: a.linuxDoCookieName("state"), Value: state}
	rec := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+url.QueryEscape(state)+"&code=secret-code", nil, stateCookie)
	if rec.Code != http.StatusFound || redirectResult(t, rec) != "ineligible" {
		t.Fatalf("callback status=%d location=%s", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String()+rec.Header().Get("Location"), "secret-code") {
		t.Fatal("authorization code leaked in callback response")
	}
	rec = serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+url.QueryEscape(state)+"&code=again", nil, stateCookie)
	if redirectResult(t, rec) != "state" || fake.calls != 1 {
		t.Fatalf("replayed callback result=%q calls=%d", redirectResult(t, rec), fake.calls)
	}
	cancelState, err := a.createLinuxDoState(context.Background(), "login", "")
	if err != nil {
		t.Fatal(err)
	}
	rec = serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+url.QueryEscape(cancelState)+"&error=access_denied", nil, &http.Cookie{Name: a.linuxDoCookieName("state"), Value: cancelState})
	if redirectResult(t, rec) != "cancelled" || fake.calls != 1 {
		t.Fatalf("cancel result=%q calls=%d", redirectResult(t, rec), fake.calls)
	}
}

func TestLinuxDoBoundLoginAndLocalTwoFactor(t *testing.T) {
	a := newTestApp(t)
	fake := &fakeLinuxDoOAuthClient{profile: linuxDoProfile{Subject: "42", Username: "linux-user", DisplayName: "Linux User"}}
	enableLinuxDoForTest(a, false, fake)
	user, _ := defaultAdminUserAndMailbox(t, a)
	if err := a.bindLinuxDoIdentity(context.Background(), user.ID, fake.profile); err != nil {
		t.Fatal(err)
	}
	state, err := a.createLinuxDoState(context.Background(), "login", "")
	if err != nil {
		t.Fatal(err)
	}
	rec := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+state+"&code=valid", nil, &http.Cookie{Name: a.linuxDoCookieName("state"), Value: state})
	if rec.Code != http.StatusFound || redirectResult(t, rec) != "success" {
		t.Fatalf("login status=%d location=%s", rec.Code, rec.Header().Get("Location"))
	}
	_ = responseCookie(t, rec, a.cfg.CookieName)

	secret := "JBSWY3DPEHPK3PXP"
	if _, err := a.db.Exec("UPDATE users SET two_factor_enabled=1,two_factor_secret=? WHERE id=?", secret, user.ID); err != nil {
		t.Fatal(err)
	}
	a.cfg.TwoFactorEnabled = false
	state, err = a.createLinuxDoState(context.Background(), "login", "")
	if err != nil {
		t.Fatal(err)
	}
	rec = serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+state+"&code=valid", nil, &http.Cookie{Name: a.linuxDoCookieName("state"), Value: state})
	if redirectResult(t, rec) != "2fa" {
		t.Fatalf("2fa callback location=%s", rec.Header().Get("Location"))
	}
	challengeCookie := responseCookie(t, rec, a.linuxDoCookieName("2fa"))
	wrong := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/2fa", map[string]string{"code": "000000"}, challengeCookie)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong 2fa status=%d", wrong.Code)
	}
	code, err := generateTOTP(secret, a.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ok := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/2fa", map[string]string{"code": code}, challengeCookie)
	if ok.Code != http.StatusOK {
		t.Fatalf("2fa status=%d body=%s", ok.Code, ok.Body.String())
	}
	_ = responseCookie(t, ok, a.cfg.CookieName)
	replay := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/2fa", map[string]string{"code": code}, challengeCookie)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed 2fa challenge status=%d", replay.Code)
	}
}

func TestLinuxDoLoginRejectsUnboundAndDisabledUsers(t *testing.T) {
	a := newTestApp(t)
	fake := &fakeLinuxDoOAuthClient{profile: linuxDoProfile{Subject: "unbound", Username: "unbound", DisplayName: "Unbound"}}
	enableLinuxDoForTest(a, false, fake)
	callback := func() *httptest.ResponseRecorder {
		state, err := a.createLinuxDoState(context.Background(), "login", "")
		if err != nil {
			t.Fatal(err)
		}
		return serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+state+"&code=valid", nil, &http.Cookie{Name: a.linuxDoCookieName("state"), Value: state})
	}
	if rec := callback(); redirectResult(t, rec) != "unbound" {
		t.Fatalf("unbound result=%q", redirectResult(t, rec))
	}
	user, _ := defaultAdminUserAndMailbox(t, a)
	if err := a.bindLinuxDoIdentity(context.Background(), user.ID, fake.profile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE users SET disabled=1 WHERE id=?", user.ID); err != nil {
		t.Fatal(err)
	}
	if rec := callback(); redirectResult(t, rec) != "disabled" {
		t.Fatalf("disabled result=%q", redirectResult(t, rec))
	}
}

func TestLinuxDoRegistrationIsTransactionalAndSingleUse(t *testing.T) {
	a := newTestApp(t)
	profile := linuxDoProfile{Subject: "registration-1", Username: "new-user", DisplayName: "Linux Name"}
	enableLinuxDoForTest(a, true, &fakeLinuxDoOAuthClient{profile: profile})
	token, err := a.createLinuxDoRegistrationChallenge(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	registrationCookie := &http.Cookie{Name: a.linuxDoCookieName("registration"), Value: token}
	pending := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/pending-registration", nil, registrationCookie)
	if pending.Code != http.StatusOK {
		t.Fatalf("pending status=%d body=%s", pending.Code, pending.Body.String())
	}
	var pendingBody struct {
		Username string
		Domains  []PublicDomain
	}
	if err := json.Unmarshal(pending.Body.Bytes(), &pendingBody); err != nil {
		t.Fatal(err)
	}
	if pendingBody.Username != profile.Username || len(pendingBody.Domains) == 0 {
		t.Fatalf("pending body=%+v", pendingBody)
	}
	payload := map[string]string{"domainId": pendingBody.Domains[0].ID, "localPart": "linux-user", "displayName": "", "password": "Password123!"}
	rec := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/register", payload, registrationCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	var userID, displayName, subject string
	if err := a.db.QueryRow("SELECT id,display_name FROM users WHERE email=?", "linux-user@"+pendingBody.Domains[0].Name).Scan(&userID, &displayName); err != nil {
		t.Fatal(err)
	}
	if displayName != profile.DisplayName {
		t.Fatalf("display name=%q want=%q", displayName, profile.DisplayName)
	}
	if err := a.db.QueryRow("SELECT subject FROM oauth_identities WHERE user_id=?", userID).Scan(&subject); err != nil || subject != profile.Subject {
		t.Fatalf("identity subject=%q err=%v", subject, err)
	}
	replay := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/register", payload, registrationCookie)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("registration replay status=%d", replay.Code)
	}
	a.cfg.ReservedMailboxPrefixes = "reserved"
	reservedProfile := linuxDoProfile{Subject: "registration-2", Username: "reserved-user", DisplayName: "Reserved"}
	reservedToken, err := a.createLinuxDoRegistrationChallenge(context.Background(), reservedProfile)
	if err != nil {
		t.Fatal(err)
	}
	reserved := serveLinuxDoRequest(t, a, http.MethodPost, "/api/auth/linuxdo/register", map[string]string{"domainId": pendingBody.Domains[0].ID, "localPart": "reserved", "password": "Password123!"}, &http.Cookie{Name: a.linuxDoCookieName("registration"), Value: reservedToken})
	if reserved.Code != http.StatusForbidden {
		t.Fatalf("reserved registration status=%d", reserved.Code)
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM users WHERE email=?", "reserved@"+pendingBody.Domains[0].Name).Scan(&count); err != nil || count != 0 {
		t.Fatalf("reserved registration count=%d err=%v", count, err)
	}
}

func TestLinuxDoLinkAndUnlinkRequireReauthentication(t *testing.T) {
	a := newTestApp(t)
	fake := &fakeLinuxDoOAuthClient{profile: linuxDoProfile{Subject: "linked-1", Username: "linked-user", DisplayName: "Linked"}}
	enableLinuxDoForTest(a, false, fake)
	user, _ := defaultAdminUserAndMailbox(t, a)
	sessionRecorder := httptest.NewRecorder()
	if err := a.issueSession(sessionRecorder, httptest.NewRequest(http.MethodGet, "/", nil), user.ID); err != nil {
		t.Fatal(err)
	}
	sessionCookie := responseCookie(t, sessionRecorder, a.cfg.CookieName)
	wrong := serveLinuxDoRequest(t, a, http.MethodPost, "/api/me/auth/linuxdo/link", map[string]string{"currentPassword": "wrong"}, sessionCookie)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password link status=%d", wrong.Code)
	}
	start := serveLinuxDoRequest(t, a, http.MethodPost, "/api/me/auth/linuxdo/link", map[string]string{"currentPassword": "ChangeMe123!"}, sessionCookie)
	if start.Code != http.StatusOK {
		t.Fatalf("link start status=%d body=%s", start.Code, start.Body.String())
	}
	var startBody struct{ URL string }
	if err := json.Unmarshal(start.Body.Bytes(), &startBody); err != nil {
		t.Fatal(err)
	}
	authorizeURL, err := url.Parse(startBody.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := authorizeURL.Query().Get("state")
	stateCookie := responseCookie(t, start, a.linuxDoCookieName("state"))
	callback := serveLinuxDoRequest(t, a, http.MethodGet, "/api/auth/linuxdo/callback?state="+state+"&code=valid", nil, sessionCookie, stateCookie)
	if redirectResult(t, callback) != "linked" {
		t.Fatalf("link callback location=%s", callback.Header().Get("Location"))
	}
	var boundUserID string
	if err := a.db.QueryRow("SELECT user_id FROM oauth_identities WHERE provider=? AND subject=?", linuxDoProvider, fake.profile.Subject).Scan(&boundUserID); err != nil || boundUserID != user.ID {
		t.Fatalf("bound user=%q err=%v", boundUserID, err)
	}
	wrong = serveLinuxDoRequest(t, a, http.MethodDelete, "/api/me/auth/linuxdo", map[string]string{"currentPassword": "wrong"}, sessionCookie)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password unlink status=%d", wrong.Code)
	}
	unlink := serveLinuxDoRequest(t, a, http.MethodDelete, "/api/me/auth/linuxdo", map[string]string{"currentPassword": "ChangeMe123!"}, sessionCookie)
	if unlink.Code != http.StatusOK {
		t.Fatalf("unlink status=%d body=%s", unlink.Code, unlink.Body.String())
	}
	if _, err := a.userIDForLinuxDoIdentity(context.Background(), fake.profile.Subject); !errors.Is(err, errNotFound) {
		t.Fatalf("identity still linked: %v", err)
	}
}

func TestLinuxDoSettingsHideAndPreserveClientSecret(t *testing.T) {
	a := newTestApp(t)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	var login map[string]any
	if code := admin.do(http.MethodPost, "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("login status=%d body=%v", code, login)
	}
	var settings SystemSettings
	if code := admin.do(http.MethodGet, "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("settings status=%d", code)
	}
	payload := systemSettingsPayload(settings)
	payload["linuxDoSSOEnabled"] = true
	payload["linuxDoRegistrationEnabled"] = true
	payload["linuxDoClientId"] = "linuxdo-client"
	payload["linuxDoClientSecret"] = "top-secret"
	var updated map[string]any
	if code := admin.do(http.MethodPost, "/api/admin/settings", payload, &updated); code != http.StatusOK {
		t.Fatalf("update settings status=%d body=%v", code, updated)
	}
	if _, exposed := updated["linuxDoClientSecret"]; exposed {
		t.Fatal("settings response exposed linuxDoClientSecret")
	}
	if updated["linuxDoClientSecretSet"] != true || updated["linuxDoCallbackUrl"] != "http://localhost:5173/api/auth/linuxdo/callback" {
		t.Fatalf("unexpected Linux.do settings response: %v", updated)
	}
	payload["linuxDoClientSecret"] = ""
	if code := admin.do(http.MethodPost, "/api/admin/settings", payload, &updated); code != http.StatusOK {
		t.Fatalf("blank secret update status=%d body=%v", code, updated)
	}
	if a.cfg.LinuxDoClientSecret != "top-secret" {
		t.Fatal("blank Client Secret did not preserve the configured value")
	}
	var stored string
	if err := a.db.QueryRow("SELECT value FROM system_settings WHERE key=?", "linuxDoClientSecret").Scan(&stored); err != nil || stored != "top-secret" {
		t.Fatalf("stored secret=%q err=%v", stored, err)
	}
	var public PublicSettings
	if code := admin.do(http.MethodGet, "/api/public/settings", nil, &public); code != http.StatusOK || !public.LinuxDoSSOEnabled || !public.LinuxDoRegistrationEnabled {
		t.Fatalf("public settings status=%d body=%+v", code, public)
	}
}
