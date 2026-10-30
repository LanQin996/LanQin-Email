package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"lanqin-email-api/internal/backup/storage"
)

type backupDriveTestTransport func(*http.Request) (*http.Response, error)

func (f backupDriveTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only tests reroute the fixed Google origin to a synthetic TLS server. The
// provider host/path/form and request headers are still validated by handlers.
func backupDriveTLSClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := server.Client()
	base := client.Transport
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = backupDriveTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "oauth2.googleapis.com" && r.URL.Host != "www.googleapis.com" {
			return nil, errors.New("unexpected test origin")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.Host = r.URL.Host
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(copy)
	})
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func backupDriveRefreshFixture(t *testing.T) (*App, []string) {
	t.Helper()
	a, _, _ := backupDriveOAuthFixture(t)
	state, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Items = nil
	ids := []string{"bkd_drive_a", "bkd_drive_b"}
	for _, id := range ids {
		state.Items = append(state.Items, backupDestinationRecord{
			Config:      storage.Destination{ID: id, Name: id, Type: "drive", Drive: &storage.DriveConfig{FolderID: id + "_folder", AccessTokenEnv: "BACKUP_DRIVE_TOKEN"}},
			Credentials: backupCredentials{DriveClientID: a.cfg.BackupDriveClientID, DriveAuthID: id + "_grant", DriveRefreshToken: id + "_refresh", DriveAccessToken: id + "_old_access", DriveTokenExpiry: time.Now().Add(time.Minute)},
		})
	}
	if err := a.saveBackupDestinationState(t.Context(), raw, state); err != nil {
		t.Fatal(err)
	}
	return a, ids
}

func TestBackupDriveRefreshBindsQueuedGrantBeforeAnyNetwork(t *testing.T) {
	a, ids := backupDriveRefreshFixture(t)
	vault, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	expected := backupTargetFingerprint(vault.Items[0])
	vault.Items[0].Credentials.DriveAuthID = "replacement-grant"
	vault.Items[0].Config.Drive.FolderID = "replacement-folder"
	if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.backupDriveOAuthClient = &http.Client{Transport: backupDriveTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected token request")
	})}
	if _, err := a.backupDriveToken(t.Context(), ids[0], expected); err == nil || calls.Load() != 0 {
		t.Fatal("new grant was paired with queued old folder", calls.Load(), err)
	}
}

func TestBackupDriveRefreshTLSIsolationRotationAndStableFingerprint(t *testing.T) {
	a, ids := backupDriveRefreshFixture(t)
	before, _, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "oauth2.googleapis.com" || r.URL.Path != "/token" || r.Method != "POST" {
			t.Error("wrong token origin/path/method")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != a.cfg.BackupDriveClientID || r.Form.Get("client_secret") != a.cfg.BackupDriveClientSecret {
			t.Error("refresh did not bind client")
		}
		refresh := r.Form.Get("refresh_token")
		if refresh != ids[0]+"_refresh" && refresh != ids[1]+"_refresh" {
			t.Error("credentials mixed between destinations")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": refresh + "_access", "refresh_token": refresh + "_rotated", "token_type": "Bearer", "expires_in": 3600}) // Scope omission retains validated initial grant.
	}))
	defer server.Close()
	a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	for _, id := range ids {
		token, err := a.backupDriveToken(t.Context(), id)
		if err != nil || token != id+"_refresh_access" {
			t.Fatal("refresh failed", err)
		}
	}
	after, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected retry", calls.Load())
	}
	for i, item := range after.Items {
		if backupTargetFingerprint(item) != backupTargetFingerprint(before.Items[i]) || item.Credentials.DriveRefreshToken != ids[i]+"_refresh_rotated" {
			t.Fatal("normal rotation invalidated grant generation")
		}
		if strings.Contains(raw, item.Credentials.DriveAccessToken) || strings.Contains(jsonEncode(backupDestinationResponse(after, raw, true)), item.Credentials.DriveRefreshToken) {
			t.Fatal("token escaped vault")
		}
	}
}

func TestBackupDriveRefreshMergesUnrelatedTargetChangeWithoutLosingRotatedGrant(t *testing.T) {
	a, ids := backupDriveRefreshFixture(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("a successful rotated token was discarded")
		}
		vault, raw, err := a.loadBackupDestinationState(r.Context())
		if err != nil {
			t.Error(err)
		}
		vault.Items[1].Config.Name = "concurrently renamed second target"
		if err := a.saveBackupDestinationState(r.Context(), raw, vault); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "only-valid-rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer server.Close()
	a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	if token, err := a.backupDriveToken(t.Context(), ids[0]); err != nil || token != "new-access" {
		t.Fatal(err)
	}
	vault, _, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if vault.Items[1].Config.Name != "concurrently renamed second target" || vault.Items[0].Credentials.DriveRefreshToken != "only-valid-rotated-refresh" {
		t.Fatal("unrelated edit/token lost")
	}
}

func TestBackupDriveRefreshCASConflictReusesSuccessfulRotatedToken(t *testing.T) {
	a, ids := backupDriveRefreshFixture(t)
	// Stop unrelated fixture workers so the one-shot clock hook deterministically
	// runs inside the metadata save, after reload but before its CAS SQL executes.
	a.workerCancel()
	a.workerWG.Wait()
	var editAfterReload atomic.Bool
	var calls atomic.Int32
	a.now = func() time.Time {
		if editAfterReload.CompareAndSwap(true, false) {
			vault, raw, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Error(err)
			}
			vault.Items[1].Config.Name = "CAS-racing unrelated edit"
			if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
				t.Error(err)
			}
		}
		return time.Now()
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			t.Error("successful rotated token discarded on metadata CAS conflict")
		}
		editAfterReload.Store(true)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "CAS-refreshed-access", "refresh_token": "CAS-rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer server.Close()
	a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	if value, err := a.backupDriveToken(t.Context(), ids[0]); err != nil || value != "CAS-refreshed-access" {
		t.Fatal("CAS recovery lost grant", err)
	}
	vault, _, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || vault.Items[0].Credentials.DriveRefreshToken != "CAS-rotated-refresh" || vault.Items[1].Config.Name != "CAS-racing unrelated edit" {
		t.Fatal("token or concurrent edit lost")
	}
}

func TestBackupDriveRefreshRejectsChangedAuthorizationAndClient(t *testing.T) {
	for _, scenario := range []string{"client", "grant", "folder", "disabled", "deleted", "scope", "invalid-grant"} {
		t.Run(scenario, func(t *testing.T) {
			a, ids := backupDriveRefreshFixture(t)
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "grant" || scenario == "folder" || scenario == "disabled" || scenario == "deleted" {
					vault, raw, err := a.loadBackupDestinationState(r.Context())
					if err != nil {
						t.Error(err)
					}
					switch scenario {
					case "grant":
						vault.Items[0].Credentials.DriveAuthID = "new-grant"
						vault.Items[0].Credentials.DriveAccessToken = "new-grant-access"
					case "folder":
						vault.Items[0].Config.Drive.FolderID = "new-folder"
					case "disabled":
						vault.Items[0].Config.Disabled = true
					case "deleted":
						vault.Items = vault.Items[1:]
					}
					if err := a.saveBackupDestinationState(r.Context(), raw, vault); err != nil {
						t.Error(err)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "invalid-grant" {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":"invalid_grant","error_description":"synthetic-client-secret synthetic-refresh must not leak"}`)
					return
				}
				body := map[string]any{"access_token": "old-grant-result-must-not-save", "token_type": "Bearer", "expires_in": 3600}
				if scenario == "scope" {
					body["scope"] = "https://www.googleapis.com/auth/drive.metadata.readonly"
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
			if scenario == "client" {
				a.cfg.BackupDriveClientID = "changed-client"
			}
			value, err := a.backupDriveToken(t.Context(), ids[0])
			if value != "" || !errors.Is(err, errBackupDriveOAuth) || strings.Contains(err.Error(), "synthetic-client-secret") {
				t.Fatal("invalid refresh accepted or provider body leaked")
			}
			if scenario == "client" && calls.Load() != 0 {
				t.Fatal("wrong client contacted provider")
			}
			vault, _, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range vault.Items {
				if r.Credentials.DriveAccessToken == "old-grant-result-must-not-save" {
					t.Fatal("stale result overwrote a changed grant")
				}
			}
		})
	}
}

func TestBackupDriveOAuthCallbackCompletesWithLocalTLSFolderAndEncryptedBinding(t *testing.T) {
	a, id, revision := backupDriveOAuthFixture(t)
	state, _ := driveAuthorizationState(t, a, "actor", id, revision)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "www.googleapis.com" || r.URL.Path != "/drive/v3/files" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("unsafe folder request")
		}
		var metadata struct {
			MimeType   string            `json:"mimeType"`
			Properties map[string]string `json:"appProperties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil || metadata.MimeType != "application/vnd.google-apps.folder" || metadata.Properties["lanqinDestination"] != id {
			t.Error("folder not app-owned", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"synthetic-new-folder"}`)
	}))
	defer server.Close()
	a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	exchange := func(context.Context, *oauth2.Config, string, string) (*oauth2.Token, error) {
		return (&oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"scope": backupDriveScope}), nil
	}
	if err := a.processBackupDriveCallback(t.Context(), "actor", state, "synthetic-code", "", exchange); err != nil {
		t.Fatal(err)
	}
	vault, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r := vault.Items[0]
	if r.Config.Drive.FolderID != "synthetic-new-folder" || r.Credentials.DriveAuthID == "" || r.Credentials.DriveClientID != a.cfg.BackupDriveClientID || r.Credentials.DriveRefreshToken != "synthetic-refresh" {
		t.Fatal("grant not bound")
	}
	if strings.Contains(raw, "synthetic-refresh") || strings.Contains(jsonEncode(backupDestinationResponse(vault, raw, true)), "synthetic-access") {
		t.Fatal("binding leaked credentials")
	}
	if err := a.processBackupDriveCallback(t.Context(), "actor", state, "synthetic-code", "", exchange); err == nil {
		t.Fatal("callback replay succeeded")
	}
}

func TestBackupDriveWorkerRefreshAndFailureIsolationWithInjectedUploader(t *testing.T) {
	for _, scenario := range []string{"success", "refresh-failure"} {
		t.Run(scenario, func(t *testing.T) {
			a, driveIDs := backupDriveRefreshFixture(t)
			drives, _, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			entry, records, data := backupJobFixture(t, a) // Synthetic ciphertext-prefix fixture; uploader success below is injected, not cloud verification.
			vault, raw, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			vault.Items = append(vault.Items, drives.Items...)
			if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "oauth2.googleapis.com" || r.URL.Path != "/token" {
					t.Error("unexpected provider request")
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != driveIDs[0]+"_refresh" {
					t.Error("wrong destination refresh", err)
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "refresh-failure" {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":"invalid_grant","error_description":"synthetic-secret-do-not-leak"}`)
					return
				}
				fmt.Fprint(w, `{"access_token":"worker-fresh-access","token_type":"Bearer","expires_in":3600}`)
			}))
			defer server.Close()
			a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
			job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "worker-drive-refresh-0001", entry.ID, []string{records[0].Config.ID, driveIDs[0]})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			uploaded := map[string]bool{}
			a.backupUploadFile = func(ctx context.Context, f *os.File, receipt string, cfg storage.Config, _ storage.Options, resolve func(string) (storage.Credentials, error)) (storage.Report, error) {
				actual, err := io.ReadAll(f)
				if err != nil || string(actual) != string(data) || receipt != entry.SHA256 {
					t.Error("wrong pinned artifact")
				}
				result := storage.Report{SHA256: receipt, Bytes: int64(len(data))}
				for _, d := range cfg.Destinations {
					c, err := resolve(d.ID)
					if err != nil {
						return storage.Report{}, err
					}
					if d.Type == "drive" && (scenario != "success" || c.DriveAccessToken != "worker-fresh-access" || c.SecretAccessKey != "") {
						t.Error("uploader received stale/mixed Drive credentials")
					}
					if d.Type == "s3" && (c.DriveAccessToken != "" || c.SecretAccessKey != "s3-synthetic-secret") {
						t.Error("Drive token crossed into S3")
					}
					uploaded[d.ID] = true
					now := time.Now().UTC()
					result.Destinations = append(result.Destinations, storage.Result{DestinationID: d.ID, State: "succeeded", CompletedAt: &now, ObjectKey: "synthetic-object", Attempts: 1})
				}
				return result, nil
			}
			a.runBackupUpload(t.Context(), *lease)
			stored, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !uploaded[records[0].Config.ID] {
				t.Fatal("Drive refresh failure stopped unrelated S3")
			}
			if scenario == "success" {
				if stored.Status != "succeeded" || !uploaded[driveIDs[0]] {
					t.Fatal("Drive refresh did not reach upload", stored.Status)
				}
			} else {
				if stored.Status != "partial" || uploaded[driveIDs[0]] {
					t.Fatal("failed grant was uploaded or marked fully successful", stored.Status)
				}
				found := false
				for _, r := range stored.Results {
					if r.DestinationID == driveIDs[0] {
						found = r.State == "failed" && r.ErrorCode == "reauthorization_required" && r.Attempts == 0
					}
				}
				if !found {
					t.Fatal("failure did not preserve independent safe result")
				}
			}
			public := jsonEncode(backupJobResponse(stored))
			for _, secret := range []string{"worker-fresh-access", "synthetic-secret-do-not-leak", driveIDs[0] + "_refresh", "workerToken", "fingerprint"} {
				if strings.Contains(public, secret) {
					t.Fatal("worker report leaked credentials/provider detail")
				}
			}
		})
	}
}
