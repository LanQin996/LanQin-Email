package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUILayoutSettingsAccessAndPersistence(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	guest := &testClient{t: t, server: server}
	admin := &testClient{t: t, server: server}
	const path = "/api/admin/settings/layouts"
	const shellPath = "/api/me/site-layout"
	classic := uiLayoutSettings{"classic", "classic"}
	layouts := uiLayoutSettings{FrontLayoutID: "front-paper", AdminLayoutID: "admin-dense"}

	for _, get := range []string{path, shellPath} {
		if status := guest.do(http.MethodGet, get, nil, nil); status != http.StatusUnauthorized {
			t.Fatalf("guest GET %s: status=%d", get, status)
		}
	}
	if status := guest.do(http.MethodPost, path, layouts, nil); status != http.StatusUnauthorized {
		t.Fatalf("guest POST: status=%d", status)
	}
	if status := admin.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": "admin@lanqin.local", "password": "ChangeMe123!",
	}, nil); status != http.StatusOK {
		t.Fatalf("admin login: status=%d", status)
	}

	makeUser := func(name string, permissions []string) *testClient {
		t.Helper()
		body := map[string]any{
			"email": name + "@example.test", "displayName": "Synthetic layout tester", "role": "user", "password": "LayoutTest123!",
		}
		if len(permissions) != 0 {
			group := createAssignableGroup(t, admin, "Synthetic layout "+name, permissions)
			body["permissionGroupIds"] = []string{group.ID}
		}
		if status := admin.do(http.MethodPost, "/api/admin/users", body, nil); status != http.StatusCreated {
			t.Fatalf("create %s: status=%d", name, status)
		}
		client := &testClient{t: t, server: server}
		if status := client.do(http.MethodPost, "/api/auth/login", map[string]any{
			"email": body["email"], "password": body["password"],
		}, nil); status != http.StatusOK {
			t.Fatalf("login %s: status=%d", name, status)
		}
		return client
	}
	user := makeUser("regular", nil)
	operator := makeUser("operator", []string{PermissionAdminOverview})
	reader := makeUser("settings-reader", []string{PermissionSettingsView})
	for _, get := range []string{path, shellPath} {
		if status := user.do(http.MethodGet, get, nil, nil); status != http.StatusForbidden {
			t.Fatalf("regular GET %s: status=%d", get, status)
		}
	}
	if status := operator.do(http.MethodGet, path, nil, nil); status != http.StatusForbidden {
		t.Fatalf("delegated operator can read settings without settings.view: status=%d", status)
	}
	for _, client := range []*testClient{user, operator, reader} {
		if status := client.do(http.MethodPost, path, layouts, nil); status != http.StatusForbidden {
			t.Fatalf("POST without settings.update: status=%d", status)
		}
	}
	var got uiLayoutSettings
	if status := reader.do(http.MethodGet, path, nil, &got); status != http.StatusOK || got != classic {
		t.Fatalf("settings reader default: status=%d got=%+v", status, got)
	}

	// Existing installations can retain the existing style independently.
	if status := admin.do(http.MethodPost, "/api/admin/settings/theme", uiThemeSettings{ThemeID: "paper"}, nil); status != http.StatusOK {
		t.Fatalf("seed prior theme: status=%d", status)
	}
	assertPublic := func(wantFront string) {
		t.Helper()
		var data map[string]any
		if status := guest.do(http.MethodGet, "/api/public/settings", nil, &data); status != http.StatusOK || data["frontLayoutId"] != wantFront || data["themeId"] != "paper" {
			t.Fatalf("public layouts and old theme: status=%d front=%v theme=%v", status, data["frontLayoutId"], data["themeId"])
		}
		if _, leaks := data["adminLayoutId"]; leaks {
			t.Fatal("admin layout leaked in unauthenticated public settings")
		}
	}
	assertPublic("classic")

	for _, invalid := range []any{
		nil, map[string]any{}, map[string]any{"layoutId": "paper"},
		map[string]any{"frontLayoutId": "paper", "adminLayoutId": nil},
		map[string]any{"frontLayoutId": nil, "adminLayoutId": "classic"},
		map[string]any{"frontLayoutId": "", "adminLayoutId": "classic"},
		map[string]any{"frontLayoutId": "classic", "adminLayoutId": "../theme"},
		map[string]any{"frontLayoutId": strings.Repeat("x", 49)},
		map[string]any{"frontLayoutId": 15},
	} {
		if status := admin.do(http.MethodPost, path, invalid, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid layouts %+v: status=%d", invalid, status)
		}
	}
	for _, test := range []struct {
		body any
		want uiLayoutSettings
	}{
		{map[string]string{"adminLayoutId": "admin-dense"}, uiLayoutSettings{"classic", "admin-dense"}},
		{map[string]string{"frontLayoutId": "front-paper"}, layouts},
		{map[string]string{"frontLayoutId": "classic"}, uiLayoutSettings{"classic", "admin-dense"}},
		{layouts, layouts},
	} {
		if status := admin.do(http.MethodPost, path, test.body, &got); status != http.StatusOK || got != test.want {
			t.Fatalf("save layouts %+v: status=%d got=%+v want=%+v", test.body, status, got, test.want)
		}
	}
	if status := admin.do(http.MethodGet, path, nil, &got); status != http.StatusOK || got != layouts {
		t.Fatalf("read persisted layouts: status=%d got=%+v", status, got)
	}
	assertPublic("front-paper")
	var shell map[string]string
	if status := operator.do(http.MethodGet, shellPath, nil, &shell); status != http.StatusOK || len(shell) != 1 || shell["adminLayoutId"] != "admin-dense" {
		t.Fatalf("delegated shell layout: status=%d got=%v", status, shell)
	}

	// A stale/malformed stored value falls back, but a syntactically valid ID
	// can remain stored across rolling frontend upgrades for catalogue fallback.
	update := "UPDATE system_settings SET value=? WHERE " + keyColumnSQL(a.cfg.DBDriver) + "=?"
	if _, err := a.db.ExecContext(t.Context(), update, `{"frontLayoutId":"!!!","adminLayoutId":"admin-dense"}`, uiLayoutSettingKey); err != nil {
		t.Fatalf("seed invalid stored data: %v", err)
	}
	settings, err := a.readUILayouts(t.Context())
	if err != nil || settings != (uiLayoutSettings{"classic", "admin-dense"}) {
		t.Fatalf("invalid stored side fallback: %+v, %v", settings, err)
	}
	assertPublic("classic")
}

func TestUILayoutPatchRequiresSingleJSONValue(t *testing.T) {
	body := strings.NewReader(`{"adminLayoutId":"classic"} {"frontLayoutId":"classic"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/admin/settings/layouts", body)
	if _, err := decodeUILayoutPatch(request); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestUILayoutDisjointUpdates(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	assertUILayoutDatabaseContract(t, a)
}

func assertUILayoutDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	ctx := t.Context()
	key := keyColumnSQL(a.cfg.DBDriver)
	if _, err := a.db.ExecContext(ctx, "DELETE FROM system_settings WHERE "+key+"=?", uiLayoutSettingKey); err != nil {
		t.Fatal(err)
	}
	for _, want := range []uiLayoutSettings{
		{FrontLayoutID: "front-paper", AdminLayoutID: "admin-dense"},
		{FrontLayoutID: "classic", AdminLayoutID: "admin-wide"},
	} {
		// Simultaneous insert (first case) and disjoint update (second) must
		// both preserve the other side even when based on the same old row.
		start := make(chan struct{})
		done := make(chan error, 2)
		for _, patch := range []uiLayoutSettings{{FrontLayoutID: want.FrontLayoutID}, {AdminLayoutID: want.AdminLayoutID}} {
			go func(patch uiLayoutSettings) {
				<-start
				_, err := a.updateUILayouts(ctx, patch)
				done <- err
			}(patch)
		}
		close(start)
		if err := errors.Join(<-done, <-done); err != nil {
			t.Fatalf("disjoint layout writes: %v", err)
		}
		got, err := a.readUILayouts(ctx)
		if err != nil || got != want {
			t.Fatalf("layout contract: got=%+v want=%+v error=%v", got, want, err)
		}
	}
}
