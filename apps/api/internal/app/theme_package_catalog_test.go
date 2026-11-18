package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const themePackageTestPath = "/api/admin/settings/theme-packages"

const themePackageTestManifest = `{"id":"review-theme","name":"Review theme","version":"1.0.0","apiVersion":1,"surfaces":{"front":{"layout":"workbench","light":{"radius":"0.9rem"},"dark":{}},"admin":{"layout":"classic","light":{"radius":"1.1rem"},"dark":{}}}}`

func packThemePackageCatalogTest(t *testing.T, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)
	if err := archive.WriteHeader(&tar.Header{Name: "theme.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(archive, manifest); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(archive.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadThemePackageCatalogTest(t *testing.T, client *testClient, typ string, data []byte, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.server.URL+themePackageTestPath+"/upload", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", typ)
	if client.cookie != nil {
		req.AddCookie(client.cookie)
	}
	resp, err := client.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestThemePackageAccessAndLifecycle(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	guest := &testClient{t: t, server: server}
	admin := &testClient{t: t, server: server}
	const id = "review-theme"
	archive := packThemePackageCatalogTest(t, themePackageTestManifest)
	if got := guest.do(http.MethodGet, themePackageTestPath, nil, nil); got != http.StatusUnauthorized {
		t.Fatalf("guest list=%d", got)
	}
	if got := uploadThemePackageCatalogTest(t, guest, "application/gzip", archive, nil); got != http.StatusUnauthorized {
		t.Fatalf("guest upload=%d", got)
	}
	if got := admin.do(http.MethodPost, "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); got != http.StatusOK {
		t.Fatalf("admin login=%d", got)
	}

	group := createAssignableGroup(t, admin, "Theme catalog reader", []string{PermissionSettingsView})
	if got := admin.do(http.MethodPost, "/api/admin/users", map[string]any{
		"email": "theme-reader@example.test", "displayName": "Synthetic reader", "role": "user",
		"password": "Reader1234!", "permissionGroupIds": []string{group.ID},
	}, nil); got != http.StatusCreated {
		t.Fatalf("create reader=%d", got)
	}
	reader := &testClient{t: t, server: server}
	if got := reader.do(http.MethodPost, "/api/auth/login", map[string]string{"email": "theme-reader@example.test", "password": "Reader1234!"}, nil); got != http.StatusOK {
		t.Fatalf("reader login=%d", got)
	}
	var catalog themePackageCatalogResponse
	if got := reader.do(http.MethodGet, themePackageTestPath, nil, &catalog); got != http.StatusOK ||
		catalog.Active != (themePackageActiveIDs{"classic", "classic"}) || catalog.Packages == nil || len(catalog.Packages) != 0 {
		t.Fatalf("reader list=%d %#v", got, catalog)
	}
	for _, path := range []string{"/github", "/activate", "/rollback"} {
		if got := reader.do(http.MethodPost, themePackageTestPath+path, map[string]string{}, nil); got != http.StatusForbidden {
			t.Fatalf("reader POST %s=%d", path, got)
		}
	}
	if got := uploadThemePackageCatalogTest(t, reader, "application/gzip", archive, nil); got != http.StatusForbidden {
		t.Fatalf("reader upload=%d", got)
	}
	if got := reader.do(http.MethodDelete, themePackageTestPath+"/"+id, nil, nil); got != http.StatusForbidden {
		t.Fatalf("reader uninstall=%d", got)
	}

	if got := uploadThemePackageCatalogTest(t, admin, "text/plain", archive, nil); got != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported archive content type=%d", got)
	}
	if got := admin.doWithHeaders(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": "front", "id": "classic"}, map[string]string{"Content-Type": "text/plain"}, nil); got != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported JSON content type=%d", got)
	}
	if got := uploadThemePackageCatalogTest(t, admin, "application/gzip", make([]byte, themePackageMaxCompressed+1), nil); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload=%d", got)
	}
	if got := uploadThemePackageCatalogTest(t, admin, "application/gzip", archive, &catalog); got != http.StatusOK || len(catalog.Packages) != 1 ||
		catalog.Packages[0].ID != id || catalog.Packages[0].Source != "" || catalog.Packages[0].Version != "1.0.0" ||
		catalog.Active != (themePackageActiveIDs{"classic", "classic"}) {
		t.Fatalf("upload or implicit activation=%d %#v", got, catalog)
	}
	if got := uploadThemePackageCatalogTest(t, admin, "application/gzip", archive, nil); got != http.StatusConflict {
		t.Fatalf("duplicate upload=%d", got)
	}
	if got := admin.do(http.MethodPost, themePackageTestPath+"/github?replace=true", map[string]string{"url": "https://github.com/example/repo", "id": id}, nil); got != http.StatusConflict {
		t.Fatalf("replacement of local package=%d", got)
	}
	if got := admin.do(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": "front", "id": id}, nil); got != http.StatusBadRequest {
		t.Fatalf("activate without version=%d", got)
	}
	if got := admin.do(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": "front", "id": id, "version": "2.0.0"}, nil); got != http.StatusConflict {
		t.Fatalf("activate incorrect version=%d", got)
	}
	if got := admin.do(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": "front", "id": id, "version": "1.0.0"}, &catalog); got != http.StatusOK ||
		catalog.Active != (themePackageActiveIDs{id, "classic"}) || catalog.ActiveVersions.Front != "1.0.0" {
		t.Fatalf("activate front=%d %#v", got, catalog)
	}
	if got := admin.do(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": "admin", "id": id, "version": "1.0.0"}, nil); got != http.StatusOK {
		t.Fatalf("activate admin=%d", got)
	}

	var public map[string]json.RawMessage
	if got := guest.do(http.MethodGet, "/api/public/settings", nil, &public); got != http.StatusOK || len(public["themePackage"]) == 0 {
		t.Fatalf("public appearance=%d %s", got, public["themePackage"])
	}
	var front map[string]json.RawMessage
	if err := json.Unmarshal(public["themePackage"], &front); err != nil || len(front) != 3 || string(front["layout"]) != `"workbench"` ||
		string(front["light"]) != `{"radius":"0.9rem"}` {
		t.Fatalf("front data includes inappropriate information or wrong snapshot: %#v", front)
	}
	var site map[string]json.RawMessage
	if got := reader.do(http.MethodGet, "/api/me/site-layout", nil, &site); got != http.StatusOK || len(site) != 2 ||
		string(site["themePackage"]) != `{"layout":"classic","light":{"radius":"1.1rem"},"dark":{}}` {
		t.Fatalf("separate admin appearance=%d %s", got, site["themePackage"])
	}

	if got := admin.do(http.MethodDelete, themePackageTestPath+"/"+id, nil, nil); got != http.StatusConflict {
		t.Fatalf("active uninstall=%d", got)
	}
	for _, surface := range []string{"front", "admin"} {
		if got := admin.do(http.MethodPost, themePackageTestPath+"/activate", map[string]string{"surface": surface, "id": "classic"}, nil); got != http.StatusOK {
			t.Fatalf("disable %s=%d", surface, got)
		}
	}
	if got := admin.do(http.MethodDelete, themePackageTestPath+"/"+id, nil, nil); got != http.StatusOK {
		t.Fatalf("uninstall=%d", got)
	}
	public = nil
	if got := guest.do(http.MethodGet, "/api/public/settings", nil, &public); got != http.StatusOK || len(public["themePackage"]) != 0 {
		t.Fatalf("after uninstall, public appearance=%d %s", got, public["themePackage"])
	}
}

func TestThemePackageCatalogRevisionLifecycle(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	assertThemePackageDatabaseContract(t, a)
}

func assertThemePackageDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	ctx := t.Context()
	assertThemePackageConcurrentStorage(t, a)
	const id = "portable-theme"
	const source = "https://github.com/portable/theme"
	makeRev := func(version, radius string) themePackage {
		return themePackage{ID: id, Name: "Portable", Version: version, APIVersion: 1, Source: source, Surfaces: map[string]themePackageAppearance{"front": {Layout: "classic", Light: map[string]string{"radius": radius}, Dark: map[string]string{}}}}
	}
	change := func(fn func(*themePackageCatalog) error, want error) {
		t.Helper()
		if _, err := a.modifyThemePackageCatalog(ctx, fn); !errors.Is(err, want) {
			t.Fatalf("catalog mutation error=%v want=%v", err, want)
		}
	}
	change(func(c *themePackageCatalog) error { return c.install(makeRev("1.0.0", "0.5rem"), "") }, nil)
	change(func(c *themePackageCatalog) error { return c.activate("front", id, "1.0.0") }, nil)
	change(func(c *themePackageCatalog) error {
		return c.install(makeRev("2.0.0", "1rem"), "https://github.com/nonmatching/theme")
	}, errThemePackageBadReplacement)
	change(func(c *themePackageCatalog) error { return c.install(makeRev("2.0.0", "1rem"), source) }, nil)
	change(func(c *themePackageCatalog) error { return c.activate("front", id, "1.0.0") }, errThemePackagePreviewChanged)
	change(func(c *themePackageCatalog) error { return c.activate("admin", id, "2.0.0") }, errThemePackageSurface)
	change(func(c *themePackageCatalog) error { return c.install(makeRev("1.0.0", "1.2rem"), source) }, errThemePackageExistingVersion)
	change(func(c *themePackageCatalog) error { return c.install(makeRev("3.0.0", "1.5rem"), source) }, nil)
	check := func(ctx context.Context, latest, previous, active, radius string) {
		t.Helper()
		state, _, _, err := a.readThemePackageCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range state.response().Packages {
			if item.ID == id {
				found = item.Version == latest && item.PreviousVersion == previous
			}
		}
		live := a.readActiveThemePackage(ctx, "front")
		if !found || state.response().ActiveVersions.Front != active || live == nil || live.Light["radius"] != radius {
			t.Fatalf("catalog revision mismatch: %#v, live=%#v", state.response(), live)
		}
	}
	check(ctx, "3.0.0", "2.0.0", "1.0.0", "0.5rem")
	change(func(c *themePackageCatalog) error { return c.rollback(id, "3.0.0", "2.0.0") }, nil)
	change(func(c *themePackageCatalog) error { return c.rollback(id, "3.0.0", "2.0.0") }, errThemePackageConflict)
	check(ctx, "2.0.0", "3.0.0", "1.0.0", "0.5rem")
	change(func(c *themePackageCatalog) error { return c.activate("front", id, "2.0.0") }, nil)
	check(ctx, "2.0.0", "3.0.0", "2.0.0", "1rem")
	change(func(c *themePackageCatalog) error { return c.activate("front", "classic", "") }, nil)
	change(func(c *themePackageCatalog) error { return c.remove(id) }, nil)
}

func assertThemePackageConcurrentStorage(t *testing.T, a *App) {
	t.Helper()
	ctx := t.Context()
	key := keyColumnSQL(a.cfg.DBDriver)
	if _, err := a.db.ExecContext(ctx, "DELETE FROM system_settings WHERE "+key+"=?", themePackageSettingKey); err != nil {
		t.Fatal(err)
	}
	race := func(first, second func(*themePackageCatalog) error) {
		t.Helper()
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		results := make(chan error, 2)
		for _, mutate := range []func(*themePackageCatalog) error{first, second} {
			go func(mutate func(*themePackageCatalog) error) {
				var firstRead sync.Once
				_, err := a.modifyThemePackageCatalog(ctx, func(c *themePackageCatalog) error {
					firstRead.Do(func() {
						ready <- struct{}{}
						<-release
					})
					return mutate(c)
				})
				results <- err
			}(mutate)
		}
		for range 2 {
			select {
			case <-ready:
			case err := <-results:
				close(release)
				t.Fatalf("catalog write completed before contention: %v", err)
			}
		}
		close(release)
		if err := errors.Join(<-results, <-results); err != nil {
			t.Fatalf("independent concurrent catalog writes: %v", err)
		}
	}
	pack := func(id, surface string) themePackage {
		return themePackage{ID: id, Name: "Concurrent", Version: "1.0.0", APIVersion: 1,
			Surfaces: map[string]themePackageAppearance{surface: {Layout: "classic", Light: map[string]string{}, Dark: map[string]string{}}}}
	}
	front, admin := pack("temporary-front", "front"), pack("temporary-admin", "admin")
	race(
		func(c *themePackageCatalog) error { return c.install(front, "") },
		func(c *themePackageCatalog) error { return c.install(admin, "") },
	)
	race(
		func(c *themePackageCatalog) error { return c.activate("front", front.ID, front.Version) },
		func(c *themePackageCatalog) error { return c.activate("admin", admin.ID, admin.Version) },
	)
	state, _, _, err := a.readThemePackageCatalog(ctx)
	if err != nil || len(state.Packages) != 2 || state.response().Active != (themePackageActiveIDs{front.ID, admin.ID}) {
		t.Fatalf("concurrent update lost state: %#v, %v", state, err)
	}
	if _, err := a.db.ExecContext(ctx, "DELETE FROM system_settings WHERE "+key+"=?", themePackageSettingKey); err != nil {
		t.Fatal(err)
	}
}
