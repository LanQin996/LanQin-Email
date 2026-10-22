package app

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFaviconUploadServeAndDelete(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}

	data := []byte{0, 0, 1, 0, 1, 0, 16, 16, 0, 0, 1, 0, 32, 0, 1, 0, 0, 0, 22, 0, 0, 0, 0x42}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "site.ico")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/admin/settings/favicon", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(admin.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload code=%d", resp.StatusCode)
	}
	if saved, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "favicon.ico")); err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("saved ICO mismatch: err=%v", err)
	}
	if code := admin.do("GET", "/api/favicon.ico", nil, nil); code != http.StatusOK {
		t.Fatalf("public icon code=%d", code)
	}

	var invalid bytes.Buffer
	badWriter := multipart.NewWriter(&invalid)
	badPart, err := badWriter.CreateFormFile("file", "not.ico")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(badPart, "not an icon")
	_ = badWriter.Close()
	badReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/admin/settings/favicon", &invalid)
	badReq.Header.Set("Content-Type", badWriter.FormDataContentType())
	badReq.AddCookie(admin.cookie)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid upload code=%d", badResp.StatusCode)
	}
	if code := admin.do("DELETE", "/api/admin/settings/favicon", nil, nil); code != http.StatusOK {
		t.Fatalf("delete code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "favicon.ico")); !os.IsNotExist(err) {
		t.Fatalf("favicon remains after delete: %v", err)
	}
}

func TestSiteTitleSettingPersistsAndIsPublic(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	payload := systemSettingsPayload(settings)
	payload["siteTitle"] = "我的邮箱"
	if code := admin.do("POST", "/api/admin/settings", payload, &settings); code != http.StatusOK || settings.SiteTitle != "我的邮箱" {
		t.Fatalf("update title code=%d title=%q", code, settings.SiteTitle)
	}
	var public PublicSettings
	if code := admin.do("GET", "/api/public/settings", nil, &public); code != http.StatusOK || public.SiteTitle != "我的邮箱" {
		t.Fatalf("public title code=%d title=%q", code, public.SiteTitle)
	}
	a.cfg.SiteTitle = ""
	if err := a.loadPersistedSystemSettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.cfg.SiteTitle != "我的邮箱" {
		t.Fatalf("persisted title = %q", a.cfg.SiteTitle)
	}
	payload["siteTitle"] = strings.Repeat("a", 81)
	if code := admin.do("POST", "/api/admin/settings", payload, nil); code != http.StatusBadRequest {
		t.Fatalf("oversized title code=%d, want %d", code, http.StatusBadRequest)
	}
}
