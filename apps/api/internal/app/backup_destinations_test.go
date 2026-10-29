package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const backupTestKey = "c3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3M=" // Synthetic 32-byte fixture, never a deployment key.

func backupTestRequest(kind, name, revision string) backupDestinationRequest {
	req := backupDestinationRequest{Name: name, Type: kind, Revision: revision}
	if kind == "s3" {
		req.S3 = &backupS3Input{backupS3Settings: backupS3Settings{Endpoint: "https://s3.example.invalid", Region: "auto", Bucket: "mail-backups", Prefix: name, PathStyle: true}, AccessKeyID: name + "-synthetic-key", SecretAccessKey: name + "-synthetic-secret"}
	} else {
		req.WebDAV = &backupDAVInput{backupDAVSettings: backupDAVSettings{URL: "https://dav.example.invalid/" + name}, Username: name + "-synthetic-user", Password: name + "-synthetic-password"}
	}
	return req
}

func backupLoggedInAdmin(t *testing.T, a *App) *testClient {
	t.Helper()
	server := httptest.NewServer(a.Router())
	t.Cleanup(server.Close)
	client := &testClient{t: t, server: server}
	var body map[string]any
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": a.cfg.AdminEmail, "password": a.cfg.AdminPassword}, &body); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	return client
}

func backupList(t *testing.T, client *testClient) backupDestinationList {
	t.Helper()
	var list backupDestinationList
	if code := client.do("GET", "/api/admin/backups/destinations", nil, &list); code != http.StatusOK {
		t.Fatalf("list code=%d", code)
	}
	return list
}

func TestBackupDestinationAdminCRUDAndCredentialIsolation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	client := backupLoggedInAdmin(t, a)
	list := backupList(t, client)
	if !list.EncryptionConfigured || list.Revision != "0" || len(list.Items) != 0 {
		t.Fatalf("unexpected empty state: %#v", list)
	}
	var result map[string]any
	for _, entry := range []struct{ kind, name string }{{"s3", "r2"}, {"s3", "b2"}, {"s3", "s3_3"}, {"webdav", "dav1"}, {"webdav", "dav2"}} {
		if code := client.do("POST", "/api/admin/backups/destinations", backupTestRequest(entry.kind, entry.name, list.Revision), &result); code != http.StatusCreated {
			t.Fatalf("create %s code=%d body=%v", entry.name, code, result)
		}
		list = backupList(t, client)
	}
	if len(list.Items) != 5 || len(list.Revision) != 64 {
		t.Fatalf("multiple same-type targets did not persist: %#v", list)
	}
	encoded, _ := json.Marshal(list)
	for _, secret := range []string{"synthetic-key", "synthetic-secret", "synthetic-user", "synthetic-password", "BACKUP_SECRET", "credentials\""} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("list leaked credential material: %s", secret)
		}
	}
	state, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil || len(state.Items) != 5 || strings.Contains(raw, "synthetic") || strings.Contains(raw, "example.invalid") || strings.Contains(raw, "mail-backups") {
		t.Fatalf("database did not encrypt the complete destination records: %v", err)
	}
	first := list.Items[0]
	req := backupTestRequest("s3", "r2", list.Revision)
	req.Name, req.Disabled = "renamed", true
	req.S3.AccessKeyID, req.S3.SecretAccessKey = "", ""
	if code := client.do("POST", "/api/admin/backups/destinations/"+first.ID, req, &result); code != http.StatusOK {
		t.Fatalf("blank credential preserve failed: %d %v", code, result)
	}
	state, _, err = a.loadBackupDestinationState(t.Context())
	if err != nil || state.Items[0].Credentials.SecretAccessKey != "r2-synthetic-secret" || state.Items[1].Credentials.SecretAccessKey != "b2-synthetic-secret" || !state.Items[0].Config.Disabled {
		t.Fatal("editing one target changed or erased another target's credentials")
	}
	if code := client.do("DELETE", "/api/admin/backups/destinations/"+first.ID, map[string]string{"revision": list.Revision}, &result); code != http.StatusConflict {
		t.Fatalf("stale delete was accepted: %d", code)
	}
	list = backupList(t, client)
	req.Revision, req.S3.Endpoint = list.Revision, "https://different.example.invalid"
	if code := client.do("POST", "/api/admin/backups/destinations/"+first.ID, req, &result); code != http.StatusBadRequest {
		t.Fatalf("credentials silently moved to another endpoint: %d", code)
	}
	req.S3.AccessKeyID, req.S3.SecretAccessKey = "new-synthetic-key", "new-synthetic-secret"
	if code := client.do("POST", "/api/admin/backups/destinations/"+first.ID, req, &result); code != http.StatusOK {
		t.Fatalf("explicit endpoint/credential replacement failed: %d %v", code, result)
	}
	list = backupList(t, client)
	if code := client.do("DELETE", "/api/admin/backups/destinations/"+first.ID, map[string]string{"revision": list.Revision}, &result); code != http.StatusOK {
		t.Fatalf("delete code=%d", code)
	}
	state, _, err = a.loadBackupDestinationState(t.Context())
	if err != nil || len(state.Items) != 4 || state.Items[0].Credentials.SecretAccessKey != "b2-synthetic-secret" {
		t.Fatal("delete affected an unrelated target")
	}
}

func TestBackupDestinationRoutesRequireActualSuperAdmin(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	admin := backupLoggedInAdmin(t, a)
	group := createAssignableGroup(t, admin, "backup synthetic settings operator", []string{PermissionSettingsView, PermissionSettingsUpdate})
	var user AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{"email": "backup-ops@example.test", "displayName": "Synthetic operator", "role": "user", "password": "SyntheticPassword123!", "permissionGroupIds": []string{group.ID}}, &user); code != http.StatusCreated {
		t.Fatalf("create operator code=%d", code)
	}
	ops := &testClient{t: t, server: admin.server}
	var body map[string]any
	if code := ops.do("POST", "/api/auth/login", map[string]string{"email": user.Email, "password": "SyntheticPassword123!"}, &body); code != http.StatusOK {
		t.Fatalf("operator login code=%d", code)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/admin/backups/destinations"}, {"POST", "/api/admin/backups/destinations"}, {"POST", "/api/admin/backups/destinations/bkd_missing"}, {"DELETE", "/api/admin/backups/destinations/bkd_missing"},
		{"GET", "/api/admin/backups/artifacts"}, {"GET", "/api/admin/backups/jobs"}, {"POST", "/api/admin/backups/jobs"}, {"GET", "/api/admin/backups/jobs/bkj_missing"}, {"GET", "/api/admin/backups/jobs/bkj_missing?download=1"}, {"POST", "/api/admin/backups/jobs/bkj_missing/cancel"},
	} {
		if code := ops.do(route.method, route.path, map[string]string{"revision": "0"}, &body); code != http.StatusForbidden {
			t.Errorf("settings operator reached %s %s: %d", route.method, route.path, code)
		}
		anonymous := &testClient{t: t, server: admin.server}
		if code := anonymous.do(route.method, route.path, map[string]string{"revision": "0"}, &body); code != http.StatusUnauthorized {
			t.Errorf("anonymous reached backup route: %d", code)
		}
	}
}

func TestBackupDestinationMissingWrongKeyAndCorruptionFailClosed(t *testing.T) {
	a := newTestApp(t)
	client := backupLoggedInAdmin(t, a)
	list := backupList(t, client)
	if list.EncryptionConfigured {
		t.Fatal("missing backup key marked configured")
	}
	var body map[string]any
	if code := client.do("POST", "/api/admin/backups/destinations", backupTestRequest("s3", "r2", "0"), &body); code != http.StatusServiceUnavailable {
		t.Fatal("persisted secrets without encryption key")
	}
	a.cfg.BackupSecretKey = backupTestKey
	if code := client.do("POST", "/api/admin/backups/destinations", backupTestRequest("s3", "r2", "0"), &body); code != http.StatusCreated {
		t.Fatalf("create code=%d", code)
	}
	_, original, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupSecretKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	for _, method := range []string{"GET", "POST"} {
		if code := client.do(method, "/api/admin/backups/destinations", backupTestRequest("s3", "other", "0"), &body); code != http.StatusServiceUnavailable {
			t.Errorf("wrong-key %s did not fail closed: %d", method, code)
		}
	}
	a.cfg.BackupSecretKey = backupTestKey
	if _, err := a.db.Exec(`UPDATE system_settings SET value=? WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, original[:len(original)-8]+"AAAAAAAA", backupDestinationSetting); err != nil {
		t.Fatal(err)
	}
	if code := client.do("GET", "/api/admin/backups/destinations", nil, &body); code != http.StatusServiceUnavailable {
		t.Fatal("tampered ciphertext did not fail closed")
	}
}

func TestBackupDestinationValidationDoesNotEchoSecrets(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	client := backupLoggedInAdmin(t, a)
	for _, change := range []func(*backupDestinationRequest){
		func(r *backupDestinationRequest) { r.S3.Endpoint = "https://127.0.0.1" },
		func(r *backupDestinationRequest) { r.S3.Endpoint = "https://user:DO-NOT-ECHO@example.test" },
		func(r *backupDestinationRequest) { r.S3.SecretAccessKey = "DO-NOT-ECHO\ninvalid" },
		func(r *backupDestinationRequest) { r.S3.Bucket = "../invalid" },
		func(r *backupDestinationRequest) { r.Type = "drive" },
	} {
		req := backupTestRequest("s3", "r2", "0")
		change(&req)
		var body map[string]any
		if code := client.do("POST", "/api/admin/backups/destinations", req, &body); code != http.StatusBadRequest || strings.Contains(jsonEncode(body), "DO-NOT-ECHO") {
			t.Fatalf("invalid config accepted or echoed: %d %v", code, body)
		}
	}
}

func TestBackupDestinationDatabaseContract(t *testing.T) {
	assertBackupDestinationDatabaseContract(t, newTestApp(t))
}

func TestBackupDestinationCredentialRotationAndEndpointBinding(t *testing.T) {
	req := backupTestRequest("s3", "r2", "0")
	req.S3.SessionToken = "synthetic-original-token"
	old, err := buildBackupDestination("bkd_one", req, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.S3.AccessKeyID, req.S3.SecretAccessKey, req.S3.SessionToken = "", "", ""
	preserved, err := buildBackupDestination("bkd_one", req, &old)
	if err != nil || preserved.Credentials != old.Credentials {
		t.Fatal("blank edit lost the credential pair or optional session token")
	}
	req.S3.AccessKeyID, req.S3.SecretAccessKey = "synthetic-new-key", "synthetic-new-secret"
	rotated, err := buildBackupDestination("bkd_one", req, &old)
	if err != nil || rotated.Credentials.SessionToken != "" {
		t.Fatal("key-pair rotation retained the old key's session token")
	}
	req.S3.Endpoint = "https://new.example.invalid"
	rotated, err = buildBackupDestination("bkd_one", req, &old)
	if err != nil || rotated.Credentials.SessionToken != "" || rotated.Credentials.AccessKeyID != "synthetic-new-key" {
		t.Fatal("endpoint change reused old credential material")
	}
	req.S3.AccessKeyID, req.S3.SecretAccessKey = "", ""
	if _, err := buildBackupDestination("bkd_one", req, &old); !errors.Is(err, errBackupAuth) {
		t.Fatal("endpoint change silently preserved old credentials")
	}
	davReq := backupTestRequest("webdav", "dav", "0")
	dav, err := buildBackupDestination("bkd_dav", davReq, nil)
	if err != nil {
		t.Fatal(err)
	}
	davReq.WebDAV.Username, davReq.WebDAV.Password = "", ""
	davReq.WebDAV.URL += "/different"
	if _, err := buildBackupDestination("bkd_dav", davReq, &dav); !errors.Is(err, errBackupAuth) {
		t.Fatal("WebDAV URL change silently preserved authentication")
	}
}

func TestBackupDestinationStorageLimitPreservesExistingData(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	record, err := buildBackupDestination("bkd_one", backupTestRequest("s3", "r2", "0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	state := backupDestinationState{Version: 1, Items: []backupDestinationRecord{record}}
	if err := a.saveBackupDestinationState(t.Context(), "", state); err != nil {
		t.Fatal(err)
	}
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Items[0].Credentials.SecretAccessKey = strings.Repeat("synthetic-secret", 5000)
	if err := a.saveBackupDestinationState(t.Context(), raw, state); !errors.Is(err, errBackupLimit) {
		t.Fatal("oversized encrypted config was saved")
	}
	loaded, current, err := a.loadBackupDestinationState(t.Context())
	if err != nil || current != raw || loaded.Items[0].Credentials.SecretAccessKey != record.Credentials.SecretAccessKey {
		t.Fatal("rejected update damaged existing destination state")
	}
}

func assertBackupDestinationDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	previousKey := a.cfg.BackupSecretKey
	a.cfg.BackupSecretKey = backupTestKey
	defer func() { a.cfg.BackupSecretKey = previousKey }()
	ctx := context.Background()
	record, err := buildBackupDestination("bkd_contract", backupTestRequest("s3", "contract", "0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	state := backupDestinationState{Version: 1, Items: []backupDestinationRecord{record}}
	if err := a.saveBackupDestinationState(ctx, "", state); err != nil {
		t.Fatal(err)
	}
	loaded, raw, err := a.loadBackupDestinationState(ctx)
	if err != nil || len(loaded.Items) != 1 || loaded.Items[0].Credentials.SecretAccessKey != "contract-synthetic-secret" {
		t.Fatalf("encrypted persistence failed: %v", err)
	}
	if err := a.saveBackupDestinationState(ctx, "", state); !errors.Is(err, errBackupConflict) {
		t.Fatalf("duplicate initial save did not conflict: %v", err)
	}
	other := &App{cfg: a.cfg, db: a.db, now: a.now}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, instance := range []*App{a, other} {
		wg.Go(func() {
			copy := backupDestinationState{Version: 1, Items: append([]backupDestinationRecord(nil), loaded.Items...)}
			copy.Items[0].Config.Disabled = i == 0
			results <- instance.saveBackupDestinationState(ctx, raw, copy)
		})
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errBackupConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("lost-update protection failed: success=%d conflicts=%d", successes, conflicts)
	}
	// Removing the last target persists an encrypted empty collection rather
	// than removing the CAS row and making old revision '0' valid again.
	_, current, err := other.loadBackupDestinationState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.saveBackupDestinationState(ctx, current, backupDestinationState{Version: 1, Items: []backupDestinationRecord{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.saveBackupDestinationState(ctx, "", state); !errors.Is(err, errBackupConflict) {
		t.Fatal("removing last target allowed stale initial save")
	}
}
