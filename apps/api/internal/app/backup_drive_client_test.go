package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestBackupDriveClientEncryptedCRUDAndGenerationInvalidation(t *testing.T) {
	a, id, _ := backupDriveOAuthFixture(t)
	client := backupLoggedInAdmin(t, a)
	a.cfg.BackupDriveClientID, a.cfg.BackupDriveClientSecret = "", ""
	var view backupDriveClientView
	if status := client.do("GET", "/api/admin/backups/drive/client", nil, &view); status != http.StatusOK || view.Source != "none" || !view.Editable || view.Configured || view.Revision != "0" || view.RedirectURI != "https://mail.example.test/api/admin/backups/drive/oauth/callback" {
		t.Fatalf("empty client view: status=%d view=%+v", status, view)
	}
	request := map[string]string{"revision": view.Revision, "clientId": "synthetic-db-client", "clientSecret": "synthetic-db-secret"}
	var response map[string]bool
	if status := client.do("POST", "/api/admin/backups/drive/client", request, &response); status != http.StatusOK || !response["ok"] {
		t.Fatalf("save database client: %d", status)
	}
	if status := client.do("GET", "/api/admin/backups/drive/client", nil, &view); status != http.StatusOK || view.Source != "database" || !view.Configured || !view.Editable || !view.ClientSecretSet || view.ClientID != request["clientId"] {
		t.Fatalf("saved client view: status=%d view=%+v", status, view)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "synthetic-db-secret") {
		t.Fatal("Drive client GET exposed secret")
	}
	var persisted backupDriveClientSecret
	raw, err := a.backupRead(t.Context(), backupDriveClientSetting, &persisted)
	if err != nil || strings.Contains(raw, "synthetic-db-secret") || persisted.ClientSecret != request["clientSecret"] || !backupDriveClientGenerationPattern.MatchString(persisted.Generation) {
		t.Fatal("OAuth setting was not encrypted", err)
	}

	a.cfg.BackupDriveClientID = "synthetic-old-client"
	a.cfg.BackupDriveClientSecret = "synthetic-old-secret"
	state, _ := driveAuthorizationState(t, a, "rotation-actor", id, backupRevision(func() string {
		_, v, e := a.loadBackupDestinationState(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		return v
	}()))
	var before backupDriveOAuthState
	if _, err := a.backupRead(t.Context(), backupDriveStateKey("rotation-actor", id), &before); err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupDriveClientID = ""
	a.cfg.BackupDriveClientSecret = ""
	current, err := a.backupDriveClient(t.Context())
	if err != nil || current.Source != "database" {
		t.Fatal("database client not effective", err)
	}
	newState := backupDriveClientSecret{Version: 1, ClientID: "synthetic-new-client", ClientSecret: "synthetic-new-secret", Generation: newID("bdc")}
	if err := a.backupCAS(t.Context(), a.db, backupDriveClientSetting, raw, newState); err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	err = a.processBackupDriveCallback(t.Context(), "rotation-actor", state, "synthetic-code", "", func(context.Context, *oauth2.Config, string, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return nil, errors.New("must not exchange")
	})
	if err == nil || exchanges.Load() != 0 {
		t.Fatal("old callback reached provider after client rotation", err, exchanges.Load())
	}
}

func TestBackupDriveClientValidationAndEnvironmentLock(t *testing.T) {
	a, id, _ := backupDriveOAuthFixture(t)
	client := backupLoggedInAdmin(t, a)
	var view backupDriveClientView
	var out map[string]any
	if status := client.do("GET", "/api/admin/backups/drive/client", nil, &view); status != 200 || view.Source != "environment" || !view.Configured || view.Editable {
		t.Fatal("environment client precedence", status, view)
	}
	a.cfg.BackupDriveClientSecret = ""
	if status := client.do("GET", "/api/admin/backups/drive/client", nil, &view); status != 200 || view.Source != "environment" || view.Configured || view.Editable {
		t.Fatal("partial environment not locked", status, view)
	}
	if status := client.do("POST", "/api/admin/backups/drive/client", map[string]string{"revision": "environment", "clientId": "db-client", "clientSecret": "db-secret"}, &out); status != http.StatusConflict {
		t.Fatal("partial environment was overridden", status)
	}

	a.cfg.BackupDriveClientID, a.cfg.BackupDriveClientSecret = "", ""
	if err := a.saveBackupDriveClient(t.Context(), "0", "synthetic-client", "synthetic-secret"); err != nil {
		t.Fatal(err)
	}
	var stored backupDriveClientSecret
	raw, err := a.backupRead(t.Context(), backupDriveClientSetting, &stored)
	if err != nil {
		t.Fatal(err)
	}
	revision := backupRevision(raw)
	for _, bad := range []struct{ id, secret string }{{" changed", "new-secret"}, {"changed", ""}, {"client", "secret\n"}, {"bad\x00client", "new"}, {"client", "   "}} {
		if err := a.saveBackupDriveClient(t.Context(), revision, bad.id, bad.secret); err == nil {
			t.Fatal("invalid/partial client update accepted")
		}
	}
	if err := a.saveBackupDriveClient(t.Context(), "stale", "changed", "new-secret"); !errors.Is(err, errBackupDriveClientConflict) {
		t.Fatal("stale revision accepted", err)
	}
	if err := a.saveBackupDriveClient(t.Context(), revision, "synthetic-client", ""); err != nil {
		t.Fatal("blank same-client secret did not retain existing secret", err)
	}
	var after backupDriveClientSecret
	if _, err := a.backupRead(t.Context(), backupDriveClientSetting, &after); err != nil || after.ClientSecret != stored.ClientSecret || after.Generation != stored.Generation {
		t.Fatal("same-client blank secret changed grant generation", err)
	}
	_ = id
}

func TestBackupDriveClientRouteAuthorizationAndMasterKey(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = ""
	a.cfg.PublicBaseURL = "https://mail.example.test"
	admin := backupLoggedInAdmin(t, a)
	server := admin.server
	var out map[string]any
	if status := admin.do("GET", "/api/admin/backups/drive/client", nil, &out); status != http.StatusServiceUnavailable {
		t.Fatal("missing encryption key exposed editable database config", status)
	}
	if status := admin.do("POST", "/api/admin/backups/drive/client", map[string]string{"revision": "0", "clientId": "synthetic", "clientSecret": "synthetic"}, &out); status != http.StatusServiceUnavailable {
		t.Fatal("missing encryption key stored OAuth secret", status)
	}
	a.cfg.BackupSecretKey = backupTestKey
	group := createAssignableGroup(t, admin, "synthetic Drive operator", []string{PermissionSettingsView, PermissionSettingsUpdate})
	var user AdminUser
	if status := admin.do("POST", "/api/admin/users", map[string]any{"email": "drive-ops@example.test", "displayName": "Synthetic Drive operator", "role": "user", "password": "SyntheticDrive123!", "permissionGroupIds": []string{group.ID}}, &user); status != http.StatusCreated {
		t.Fatal(status)
	}
	operator := &testClient{t: t, server: server}
	if status := operator.do("POST", "/api/auth/login", map[string]string{"email": user.Email, "password": "SyntheticDrive123!"}, &out); status != 200 {
		t.Fatal(status)
	}
	if status := operator.do("GET", "/api/admin/backups/drive/client", nil, &out); status != http.StatusForbidden {
		t.Fatal("settings operator could read Drive OAuth status", status)
	}
	if status := operator.do("POST", "/api/admin/backups/drive/client", map[string]string{"revision": "0", "clientId": "synthetic", "clientSecret": "synthetic"}, &out); status != http.StatusForbidden {
		t.Fatal("settings operator could set Drive OAuth", status)
	}
	token := createTestAPIToken(t, admin, "synthetic Drive token")
	for _, denied := range []*testClient{{t: t, server: server}, {t: t, server: server, bearer: token}} {
		for _, method := range []string{"GET", "POST"} {
			if status := denied.do(method, "/api/admin/backups/drive/client", map[string]string{"revision": "0", "clientId": "synthetic", "clientSecret": "synthetic"}, &out); status != http.StatusUnauthorized && status != http.StatusForbidden {
				t.Fatal("non-session reached Drive client route", status)
			}
		}
	}
	if status := admin.do("POST", "/api/admin/backups/drive/client", map[string]string{"revision": "0", "clientId": "synthetic", "clientSecret": "synthetic", "endpoint": "https://synthetic-secret-do-not-echo.invalid"}, &out); status != http.StatusBadRequest || strings.Contains(jsonEncode(out), "synthetic-secret-do-not-echo") {
		t.Fatal("unknown configuration field accepted or echoed", status)
	}
}

func TestBackupDriveClientRotationDuringCallbackAndRefreshRejectsStaleResults(t *testing.T) {
	for _, phase := range []string{"before-callback", "during-exchange", "during-refresh"} {
		t.Run(phase, func(t *testing.T) {
			a, id, revision := backupDriveOAuthFixture(t)
			a.cfg.BackupDriveClientID, a.cfg.BackupDriveClientSecret = "", ""
			if err := a.saveBackupDriveClient(t.Context(), "0", "synthetic-database-client", "synthetic-original-secret"); err != nil {
				t.Fatal(err)
			}
			config, gen, err := a.backupDriveOAuthConfigFor(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			rotate := func() {
				view, err := a.backupDriveClient(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := a.saveBackupDriveClient(t.Context(), view.Revision, config.ClientID, "synthetic-rotated-secret"); err != nil {
					t.Fatal(err)
				}
			}
			var networkCalls atomic.Int32
			if phase == "during-refresh" {
				vault, raw, err := a.loadBackupDestinationState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				vault.Items[0].Config.Drive.FolderID = "synthetic-folder"
				vault.Items[0].Credentials.DriveClientID = config.ClientID
				vault.Items[0].Credentials.DriveClientGeneration = gen
				vault.Items[0].Credentials.DriveAuthID = "synthetic-grant"
				vault.Items[0].Credentials.DriveRefreshToken = "synthetic-refresh"
				vault.Items[0].Credentials.DriveAccessToken = "old-access"
				if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					networkCalls.Add(1)
					rotate()
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"access_token": "late-token-must-not-save", "refresh_token": "rotated-refresh-must-not-save", "token_type": "Bearer", "expires_in": 3600, "scope": backupDriveScope})
				}))
				defer server.Close()
				a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
				if token, err := a.backupDriveToken(t.Context(), id); err == nil || token != "" || networkCalls.Load() != 1 {
					t.Fatal("client secret rotation returned stale refreshed token", err, networkCalls.Load())
				}
				current, _, err := a.loadBackupDestinationState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if current.Items[0].Credentials.DriveAccessToken != "old-access" || current.Items[0].Credentials.DriveRefreshToken != "synthetic-refresh" {
					t.Fatal("stale refreshed credentials persisted")
				}
				if token, err := a.backupDriveToken(t.Context(), id); err == nil || token != "" || networkCalls.Load() != 1 {
					t.Fatal("stale stored generation made another provider request")
				}
				return
			}
			link, err := a.beginBackupDriveOAuth(t.Context(), "same-client-rotation", id, revision)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(link)
			if err != nil || parsed.Query().Get("client_id") != config.ClientID || parsed.Query().Get("code_challenge_method") != "S256" || strings.Contains(link, config.ClientSecret) {
				t.Fatal("database client did not generate a safe bound authorization URL", err)
			}
			state := parsed.Query().Get("state")
			if phase == "before-callback" {
				rotate()
			}
			a.backupDriveOAuthClient = &http.Client{Transport: backupDriveTestTransport(func(*http.Request) (*http.Response, error) {
				networkCalls.Add(1)
				return nil, errors.New("unexpected folder network")
			})}
			var exchanges atomic.Int32
			err = a.processBackupDriveCallback(t.Context(), "same-client-rotation", state, "synthetic-code", "", func(context.Context, *oauth2.Config, string, string) (*oauth2.Token, error) {
				exchanges.Add(1)
				rotate()
				return (&oauth2.Token{AccessToken: "late-token-must-not-bind", RefreshToken: "synthetic-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"scope": backupDriveScope}), nil
			})
			if err == nil || networkCalls.Load() != 0 || phase == "before-callback" && exchanges.Load() != 0 || phase == "during-exchange" && exchanges.Load() != 1 {
				t.Fatal("rotated callback bound token or contacted folder endpoint", err, networkCalls.Load(), exchanges.Load())
			}
			current, _, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if current.Items[0].Credentials.DriveRefreshToken != "" || current.Items[0].Config.Drive.FolderID != "" {
				t.Fatal("rotated OAuth callback changed destination")
			}
		})
	}
}

func TestBackupDriveClientDatabaseConfigurationCompletesOAuthAndRefreshTLS(t *testing.T) {
	a, id, revision := backupDriveOAuthFixture(t)
	a.cfg.BackupDriveClientID, a.cfg.BackupDriveClientSecret = "", ""
	if err := a.saveBackupDriveClient(t.Context(), "0", "database-positive-client", "database-positive-secret"); err != nil {
		t.Fatal(err)
	}
	var folderCalls, refreshCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/drive/v3/files":
			folderCalls.Add(1)
			if r.Host != "www.googleapis.com" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer initial-database-access" {
				t.Error("unsafe folder creation request")
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "database-positive-folder"})
		case "/token":
			refreshCalls.Add(1)
			if r.ParseForm() != nil || r.Host != "oauth2.googleapis.com" || r.Method != "POST" || r.Form.Get("client_id") != "database-positive-client" || r.Form.Get("client_secret") != "database-positive-secret" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "database-positive-refresh" {
				t.Error("database client/secret not applied to refresh")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed-database-access", "refresh_token": "rotated-database-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": backupDriveScope})
		default:
			t.Error("unexpected Google API path")
			http.Error(w, "unexpected", 404)
		}
	}))
	defer server.Close()
	a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	link, err := a.beginBackupDriveOAuth(t.Context(), "database-positive-actor", id, revision)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Query().Get("client_id") != "database-positive-client" || parsed.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("database OAuth URL missing client or PKCE", err)
	}
	err = a.processBackupDriveCallback(t.Context(), "database-positive-actor", parsed.Query().Get("state"), "synthetic-code", "", func(_ context.Context, c *oauth2.Config, code, verifier string) (*oauth2.Token, error) {
		if c.ClientID != "database-positive-client" || c.ClientSecret != "database-positive-secret" || code != "synthetic-code" || verifier == "" {
			t.Fatal("database client was not used for authorization-code exchange")
		}
		return (&oauth2.Token{AccessToken: "initial-database-access", RefreshToken: "database-positive-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"scope": backupDriveScope}), nil
	})
	if err != nil || folderCalls.Load() != 1 {
		t.Fatal("database-backed OAuth failed", err, folderCalls.Load())
	}
	vault, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	bound := vault.Items[0]
	if bound.Credentials.DriveClientID != "database-positive-client" || bound.Credentials.DriveClientGeneration == "" || bound.Config.Drive.FolderID != "database-positive-folder" || strings.Contains(raw, "database-positive-refresh") {
		t.Fatal("database OAuth binding missing or plaintext")
	}
	fingerprint := backupTargetFingerprint(bound)
	if token, err := a.backupDriveToken(t.Context(), id, fingerprint); err != nil || token != "refreshed-database-access" || refreshCalls.Load() != 1 {
		t.Fatal("database-backed token refresh failed", err, refreshCalls.Load())
	}
	after, _, err := a.loadBackupDestinationState(t.Context())
	if err != nil || after.Items[0].Credentials.DriveRefreshToken != "rotated-database-refresh" || backupTargetFingerprint(after.Items[0]) != fingerprint {
		t.Fatal("refresh lost binding or changed authorization fingerprint", err)
	}
}
