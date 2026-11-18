package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIThemeSettingsAccessAndFallback(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	guest := &testClient{t: t, server: server}
	admin := &testClient{t: t, server: server}
	const path = "/api/admin/settings/theme"
	var public PublicSettings
	if code := guest.do(http.MethodGet, "/api/public/settings", nil, &public); code != http.StatusOK || public.ThemeID != defaultUIThemeID {
		t.Fatalf("missing theme: status=%d theme=%q", code, public.ThemeID)
	}
	if code := guest.do(http.MethodGet, path, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: status=%d", code)
	}
	if code := guest.do(http.MethodPost, path, uiThemeSettings{ThemeID: "paper"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous write: status=%d", code)
	}
	if code := admin.do(http.MethodPost, "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login: status=%d", code)
	}
	const userEmail = "theme-test@lanqin.local"
	const userPassword = "ThemeTest456!"
	if code := admin.do(http.MethodPost, "/api/admin/users", map[string]string{
		"email": userEmail, "displayName": "Theme test user", "password": userPassword, "role": "user",
	}, nil); code != http.StatusCreated {
		t.Fatalf("create user: status=%d", code)
	}
	user := &testClient{t: t, server: server}
	if code := user.do(http.MethodPost, "/api/auth/login", map[string]string{"email": userEmail, "password": userPassword}, nil); code != http.StatusOK {
		t.Fatalf("user login: status=%d", code)
	}
	if code := user.do(http.MethodPost, path, uiThemeSettings{ThemeID: "paper"}, nil); code != http.StatusForbidden {
		t.Fatalf("user write: status=%d", code)
	}

	for _, id := range []string{"", "../paper", "https://example.invalid", "Paper", "two--dashes", strings.Repeat("a", 49)} {
		if code := admin.do(http.MethodPost, path, uiThemeSettings{ThemeID: id}, nil); code != http.StatusBadRequest {
			t.Fatalf("invalid theme %q: status=%d", id, code)
		}
	}

	for _, id := range []string{"paper", "external-theme-2", defaultUIThemeID} {
		var theme uiThemeSettings
		if code := admin.do(http.MethodPost, path, uiThemeSettings{ThemeID: id}, &theme); code != http.StatusOK || theme.ThemeID != id {
			t.Fatalf("write theme %q: status=%d response=%q", id, code, theme.ThemeID)
		}
		if code := admin.do(http.MethodGet, path, nil, &theme); code != http.StatusOK || theme.ThemeID != id {
			t.Fatalf("read theme %q: status=%d response=%q", id, code, theme.ThemeID)
		}
		if code := guest.do(http.MethodGet, "/api/public/settings", nil, &public); code != http.StatusOK || public.ThemeID != id {
			t.Fatalf("public theme %q: status=%d response=%q", id, code, public.ThemeID)
		}
	}
}

func assertUIThemeDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	// The SQLite integration test covers auth; run the same storage handler
	// against both external drivers to cover insert and the conflict update.
	for _, id := range []string{"paper", defaultUIThemeID} {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/settings/theme", strings.NewReader(`{"themeId":"`+id+`"}`))
		recorder := httptest.NewRecorder()
		a.handleUpdateUITheme(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("write theme %q: status=%d body=%s", id, recorder.Code, recorder.Body.String())
		}
		got, err := a.readUIThemeID(t.Context())
		if err != nil || got != id {
			t.Fatalf("read theme %q: value=%q error=%v", id, got, err)
		}
	}
}
