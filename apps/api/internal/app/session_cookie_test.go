package app

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecureCookiesPublicURL(t *testing.T) {
	for _, tc := range []struct {
		url           string
		allow, secure bool
	}{
		{"http://mail.example.test", false, false},
		{"https://mail.example.test", false, true},
		{"https://mail.example.test", true, true},
		{"", false, true},
		{"invalid", false, true},
		{"http://", false, true},
		{"", true, false},
	} {
		a := &App{cfg: Config{PublicBaseURL: tc.url, AllowInsecureHTTP: tc.allow}}
		if got := a.secureCookies(); got != tc.secure {
			t.Errorf("URL %q allow=%v: secure=%v want %v", tc.url, tc.allow, got, tc.secure)
		}
	}
}

func TestHTTPLoginCookieAuthenticatesMe(t *testing.T) {
	a := newTestApp(t)
	a.cfg.AllowInsecureHTTP = false
	a.cfg.PublicBaseURL = "http://mail.example.test"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	response, err := client.Post(ts.URL+"/api/auth/login", "application/json", strings.NewReader(`{"email":"admin@lanqin.local","password":"ChangeMe123!"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", response.StatusCode)
	}
	found := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == a.cfg.CookieName {
			found = true
			if cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
				t.Fatal("unexpected session cookie attributes")
			}
		}
	}
	if !found {
		t.Fatal("missing session cookie")
	}
	response, err = client.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("me: %d", response.StatusCode)
	}
}
