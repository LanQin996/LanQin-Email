package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiteBrandingSettingsAccessAndIndependence(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	guest := &testClient{t: t, server: server}
	const path = "/api/admin/settings/branding"

	if status := guest.do(http.MethodPost, path, map[string]string{"siteTitle": "Guest"}, nil); status != http.StatusUnauthorized {
		t.Fatalf("guest POST: status=%d", status)
	}
	if status := admin.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	}, nil); status != http.StatusOK {
		t.Fatalf("admin login: status=%d", status)
	}
	readerGroup := createAssignableGroup(t, admin, "Branding read only", []string{PermissionSettingsView})
	if status := admin.do(http.MethodPost, "/api/admin/users", map[string]any{
		"email": "branding-reader@example.test", "displayName": "Synthetic reader", "role": "user",
		"password": "Reader1234!", "permissionGroupIds": []string{readerGroup.ID},
	}, nil); status != http.StatusCreated {
		t.Fatalf("create settings reader: status=%d", status)
	}
	reader := &testClient{t: t, server: server}
	if status := reader.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": "branding-reader@example.test", "password": "Reader1234!",
	}, nil); status != http.StatusOK {
		t.Fatalf("settings reader login: status=%d", status)
	}
	if status := reader.do(http.MethodPost, path, map[string]string{"siteTitle": "Reader"}, nil); status != http.StatusForbidden {
		t.Fatalf("settings.view without settings.update: status=%d", status)
	}

	for _, payload := range []any{
		map[string]any{},
		map[string]any{"siteTitle": nil},
		map[string]any{"siteTitle": 15},
		map[string]string{"siteTitle": "Valid", "smtpHost": "not-allowed.example.test"},
		map[string]string{"siteTitle": strings.Repeat("信", 81)},
	} {
		if status := admin.do(http.MethodPost, path, payload, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid branding payload %#v: status=%d", payload, status)
		}
	}

	var general SystemSettings
	if status := admin.do(http.MethodGet, "/api/admin/settings", nil, &general); status != http.StatusOK {
		t.Fatalf("load general settings: status=%d", status)
	}
	if general.SiteTitle != defaultSiteTitle {
		t.Fatalf("initial site title: %q", general.SiteTitle)
	}
	staleGeneral := systemSettingsPayload(general)
	staleGeneral["siteTitle"] = "Legacy title"
	staleGeneral["smtpHost"] = "simulation.example.test"
	if status := admin.do(http.MethodPost, "/api/admin/settings", staleGeneral, &general); status != http.StatusOK || general.SiteTitle != "Legacy title" {
		t.Fatalf("legacy explicit title: status=%d title=%q", status, general.SiteTitle)
	}
	delete(staleGeneral, "siteTitle")

	var branding map[string]string
	if status := admin.do(http.MethodPost, path, map[string]string{"siteTitle": "  审核站点  "}, &branding); status != http.StatusOK || len(branding) != 1 || branding["siteTitle"] != "审核站点" {
		t.Fatalf("save isolated title: status=%d body=%v", status, branding)
	}
	if status := reader.do(http.MethodGet, "/api/admin/settings", nil, &general); status != http.StatusOK || general.SiteTitle != "审核站点" || general.SMTPHost != "simulation.example.test" {
		t.Fatalf("site title save affected general settings: status=%d title=%q smtp=%q", status, general.SiteTitle, general.SMTPHost)
	}

	readTitleRow := func() (string, string) {
		t.Helper()
		key := keyColumnSQL(a.cfg.DBDriver)
		var value, updated string
		if err := a.db.QueryRowContext(t.Context(), "SELECT value,updated_at FROM system_settings WHERE "+key+"=?", siteTitleSettingKey).Scan(&value, &updated); err != nil {
			t.Fatal(err)
		}
		return value, updated
	}
	_, brandedAt := readTitleRow()

	staleGeneral["smtpHost"] = "new-simulation.example.test"
	if status := admin.do(http.MethodPost, "/api/admin/settings", staleGeneral, &general); status != http.StatusOK || general.SiteTitle != "审核站点" || general.SMTPHost != "new-simulation.example.test" {
		t.Fatalf("ordinary save clobbered title: status=%d title=%q smtp=%q", status, general.SiteTitle, general.SMTPHost)
	}
	if value, updated := readTitleRow(); value != "审核站点" || updated != brandedAt {
		t.Fatalf("ordinary save rewrote title row: value=%q updated=%q prior=%q", value, updated, brandedAt)
	}
	var public PublicSettings
	if status := guest.do(http.MethodGet, "/api/public/settings", nil, &public); status != http.StatusOK || public.SiteTitle != "审核站点" {
		t.Fatalf("public branding: status=%d title=%q", status, public.SiteTitle)
	}

	if status := admin.do(http.MethodPost, path, map[string]string{"siteTitle": "  "}, &branding); status != http.StatusOK || branding["siteTitle"] != defaultSiteTitle {
		t.Fatalf("reset dedicated title: status=%d body=%v", status, branding)
	}
	for _, reset := range []any{"", nil} {
		if status := admin.do(http.MethodPost, path, map[string]string{"siteTitle": "Before reset"}, &branding); status != http.StatusOK {
			t.Fatalf("reset fixture: status=%d", status)
		}
		staleGeneral["siteTitle"] = reset
		if status := admin.do(http.MethodPost, "/api/admin/settings", staleGeneral, &general); status != http.StatusOK || general.SiteTitle != defaultSiteTitle {
			t.Fatalf("reset legacy title %v: status=%d title=%q", reset, status, general.SiteTitle)
		}
	}
}

func assertSiteBrandingDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	for _, entry := range []struct{ body, want string }{
		{`{"siteTitle":"Portable title"}`, "Portable title"},
		{`{"siteTitle":""}`, defaultSiteTitle},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/branding", strings.NewReader(entry.body)).WithContext(t.Context())
		recorder := httptest.NewRecorder()
		a.handleUpdateSiteBranding(recorder, req)
		if got := a.siteTitle(t.Context()); recorder.Code != http.StatusOK || got != entry.want {
			t.Fatalf("save title %q: status=%d body=%s persisted=%q", entry.want, recorder.Code, recorder.Body.String(), got)
		}
	}
}
