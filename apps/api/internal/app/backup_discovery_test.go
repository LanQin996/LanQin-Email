package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/storage"
)

type discoveryFixture struct {
	a          *App
	client     *testClient
	record     backupDestinationRecord
	revision   string
	identity   *age.X25519Identity
	ciphertext []byte
	entry      storage.RecoveryIndexEntry
	draftID    string
}

type discoveryResultsFixture struct {
	Items []struct {
		ID                    string    `json:"id"`
		OriginalDestinationID string    `json:"originalDestinationId"`
		ObjectKey             string    `json:"objectKey"`
		SHA256                string    `json:"sha256"`
		Bytes                 int64     `json:"bytes"`
		CompletedAt           time.Time `json:"completedAt"`
	} `json:"items"`
	NextCursor string `json:"nextCursor"`
}

func newDiscoveryFixture(t *testing.T, kind string) discoveryFixture {
	t.Helper()
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	a.cfg.MaildirRoot = t.TempDir()
	a.cfg.BackupRestoreDir = privateBackupTestDir(t)
	// Independent discovery must work after the instance's original local
	// artifact catalog and upload history have gone away.
	a.cfg.BackupArtifactDir = filepath.Join(t.TempDir(), "absent-local-catalog")
	var record backupDestinationRecord
	var err error
	if kind == "drive" {
		a.cfg.PublicBaseURL = "https://mail.example.test"
		a.cfg.BackupDriveClientID = "synthetic-client"
		a.cfg.BackupDriveClientSecret = "synthetic-client-secret"
		record = backupDestinationRecord{Config: storage.Destination{ID: "bkd_new_drive", Name: "Synthetic reauthorized Drive", Type: "drive", Drive: &storage.DriveConfig{FolderID: "synthetic-new-current-folder", AccessTokenEnv: "BACKUP_DRIVE_TOKEN"}}, Credentials: backupCredentials{DriveClientID: a.cfg.BackupDriveClientID, DriveAuthID: "synthetic-current-grant", DriveRefreshToken: "synthetic-current-refresh", DriveAccessToken: "synthetic-current-token", DriveTokenExpiry: time.Now().Add(time.Hour)}}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "oauth2.googleapis.com" || r.URL.Path != "/token" || r.Method != "POST" {
				t.Error("unexpected Drive OAuth test origin")
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "synthetic-current-refresh" {
				t.Error("wrong discovery refresh authority", err)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-refreshed-token", "token_type": "Bearer", "expires_in": 3600})
		}))
		t.Cleanup(server.Close)
		a.backupDriveOAuthClient = backupDriveTLSClient(t, server)
	} else {
		record, err = buildBackupDestination("bkd_new_"+kind, backupTestRequest(kind, "discovery", "0"), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := a.saveBackupDestinationState(t.Context(), "", backupDestinationState{Version: 1, Items: []backupDestinationRecord{record}}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client := backupLoggedInAdmin(t, a)
	var mailboxID string
	if err := a.db.QueryRowContext(t.Context(), "SELECT id FROM mailboxes WHERE address=?", a.cfg.AdminEmail).Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	var draft MailMessage
	if status := client.do("POST", "/api/mail/drafts", map[string]any{"mailboxId": mailboxID, "subject": "Synthetic discovered backup draft", "to": []string{"recipient@example.test"}, "text": "synthetic discovery recovery payload", "attachments": []AttachmentInput{{Filename: "synthetic-discovery.bin", ContentType: "application/octet-stream", ContentBase64: base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255, 128, 65})}}}, &draft); status != 201 {
		t.Fatal("discovery mail/attachment fixture failed", status)
	}
	if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareOnlineBackup(t.Context(), a.cfg, privateBackupTestDir(t), DefaultOnlineBackupOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	if _, err := backup.WriteOnline(t.Context(), prepared.Path, &ciphertext, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256(ciphertext.Bytes())
	original := record.Config
	original.ID = "bkd_original_instance"
	object := "lanqin-0123456789abcdef0123456789abcdef.age"
	if original.S3 != nil {
		object = original.S3.Prefix + "/" + object
	}
	if original.Drive != nil {
		original.Drive = &storage.DriveConfig{FolderID: "synthetic-original-folder", AccessTokenEnv: "BACKUP_DRIVE_TOKEN"}
		object = "synthetic-original-ciphertext-file"
	}
	key, _ := a.backupMasterKey()
	document, err := storage.SealRecoveryIndex(original, object, hex.EncodeToString(sha[:]), int64(ciphertext.Len()), time.Now().UTC(), key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := storage.OpenRecoveryIndex(original, document, key)
	if err != nil {
		t.Fatal(err)
	}
	return discoveryFixture{a: a, client: client, record: record, revision: backupRevision(raw), identity: identity, ciphertext: append([]byte{}, ciphertext.Bytes()...), entry: entry, draftID: draft.ID}
}

func (f discoveryFixture) request(id string) map[string]any {
	request := map[string]any{"destinationId": f.record.Config.ID, "destinationsRevision": f.revision, "requestId": id}
	if f.record.Config.Type == "drive" {
		request["sourceFolderId"] = "synthetic-original-folder"
	}
	return request
}

func (f discoveryFixture) create(t *testing.T, id string) backupJobView {
	t.Helper()
	var created backupJobView
	if status := f.client.do("POST", "/api/admin/backups/discoveries", f.request(id), &created); status != http.StatusAccepted || created.Kind != "discover" || created.Status != "queued" {
		t.Fatal("independent discovery not admitted", status, created.Kind, created.Status)
	}
	return created
}

func (f discoveryFixture) run(t *testing.T, job backupJobView) backupJob {
	t.Helper()
	lease, err := f.a.claimBackupUpload(t.Context())
	if err != nil || lease == nil || lease.ID != job.ID {
		t.Fatal("discovery did not share backup queue", err)
	}
	f.a.runBackupUpload(t.Context(), *lease)
	stored, _, err := f.a.backupReadJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (f discoveryFixture) results(t *testing.T, job string, cursor string) (discoveryResultsFixture, int) {
	t.Helper()
	var page discoveryResultsFixture
	status := f.client.do("GET", "/api/admin/backups/jobs/"+job+"/discovery?cursor="+url.QueryEscape(cursor), nil, &page)
	return page, status
}

func TestBackupDiscoveryNewInstanceAuthenticatedRestoreWithoutUploadHistoryOrCatalog(t *testing.T) {
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			f := newDiscoveryFixture(t, kind)
			var oldJobs int
			if err := f.a.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM system_settings WHERE "+keyColumnSQL(f.a.cfg.DBDriver)+" LIKE ?", backupJobPrefix+"%").Scan(&oldJobs); err != nil || oldJobs != 0 {
				t.Fatal("discovery fixture reused upload history", err)
			}
			calls := 0
			f.a.backupDiscoverIndexes = func(ctx context.Context, d storage.Destination, key []byte, c storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				calls++
				if f.a.db.Stats().InUse != 0 || ctx.Err() != nil || d.ID != f.record.Config.ID {
					t.Fatal("discovery held business connection or changed selected destination")
				}
				if kind == "drive" {
					if d.Drive.FolderID != "synthetic-original-folder" || c.DriveAccessToken != "synthetic-refreshed-token" {
						t.Fatal("historical Drive folder did not use current selected grant")
					}
				} else if kind == "s3" && c.SecretAccessKey != f.record.Credentials.SecretAccessKey || kind == "webdav" && c.Password != f.record.Credentials.Password {
					t.Fatal("discovery credential isolation failed")
				}
				original := d
				original.ID = f.entry.DestinationID
				document, err := storage.SealRecoveryIndex(original, f.entry.ObjectKey, f.entry.SHA256, f.entry.Bytes, f.entry.CompletedAt, key)
				if err != nil {
					return nil, err
				}
				entry, err := storage.OpenRecoveryIndex(d, document, key)
				if err != nil {
					return nil, err
				}
				return []storage.RecoveryIndexEntry{entry}, nil
			}
			job := f.create(t, "new-instance-discovery-0001")
			businessCounts := map[string]int{}
			for _, table := range []string{"send_queue", "scheduled_sends", "local_delivery_jobs"} {
				var count int
				if err := f.a.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				businessCounts[table] = count
			}
			stored := f.run(t, job)
			if stored.Status != "succeeded" || stored.Discovery == nil || !stored.Discovery.Ready || stored.Discovery.Count != 1 || calls != 1 {
				t.Fatal("independent discovery did not complete", stored.Status, stored.ErrorCode)
			}
			page, status := f.results(t, job.ID, "")
			if status != 200 || len(page.Items) != 1 || page.NextCursor != "" {
				t.Fatal("discovery result unavailable", status)
			}
			item := page.Items[0]
			_, currentVault, err := f.a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			f.revision = backupRevision(currentVault)
			if item.OriginalDestinationID != f.entry.DestinationID || item.ObjectKey != f.entry.ObjectKey || item.SHA256 != f.entry.SHA256 || item.Bytes != f.entry.Bytes || item.ID == "" {
				t.Fatal("discovery lost authenticated source receipt")
			}
			request := backupRestoreRequest{SourceJobID: job.ID, DiscoveryEntryID: item.ID, DestinationID: f.record.Config.ID, ExpectedSHA256: item.SHA256, DestinationsRevision: f.revision, RequestID: "new-instance-restore-0001", Identity: f.identity.String(), Password: "wrong-synthetic-password", Confirm: true}
			var response backupJobView
			if status := f.client.do("POST", "/api/admin/backups/restores", request, &response); status != http.StatusForbidden || len(f.a.backupRestoreRequests) != 0 {
				t.Fatal("discovered backup skipped administrator reauthentication", status)
			}
			request.Password = f.a.cfg.AdminPassword
			f.a.backupDownloadFile = func(ctx context.Context, d storage.Destination, object, sha, output string, opts storage.Options, c storage.Credentials) (storage.DownloadReceipt, error) {
				if f.a.db.Stats().InUse != 0 || object != item.ObjectKey || sha != item.SHA256 || opts.MaxBytes != item.Bytes {
					t.Fatal("restoration used unsigned source or held business connection")
				}
				if kind == "drive" && (d.ID != item.OriginalDestinationID || d.Drive.FolderID != "synthetic-original-folder" || c.DriveAccessToken != "synthetic-refreshed-token") {
					t.Fatal("Drive restoration lost original identity/folder or current credential")
				}
				if err := os.WriteFile(output, f.ciphertext, 0o600); err != nil {
					return storage.DownloadReceipt{}, err
				}
				return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(f.ciphertext))}, nil
			}
			if status := f.client.do("POST", "/api/admin/backups/restores", request, &response); status != http.StatusAccepted {
				t.Fatal("discovered backup recovery rejected", status)
			}
			select {
			case execution := <-f.a.backupRestoreRequests:
				f.a.runBackupRestore(t.Context(), execution)
			default:
				t.Fatal("discovered restore did not dispatch")
			}
			restored, _, err := f.a.backupReadJob(t.Context(), response.ID)
			if err != nil || restored.Status != "succeeded" || restored.Restore == nil || !restored.Restore.Ready || restored.Restore.Manifest.Scope != "online-data" {
				t.Fatal("discovery to actual age/SQLite restore failed", restored.ErrorCode, err)
			}
			frozen, err := openFrozenBackupDB(filepath.Join(f.a.cfg.BackupRestoreDir, restored.Restore.Directory, "recovered", "snapshot", "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			var subject string
			queryErr := frozen.QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", f.draftID).Scan(&subject)
			frozen.Close()
			if queryErr != nil || subject != "Synthetic discovered backup draft" {
				t.Fatal("actual restored discovered archive lost synthetic mail", queryErr)
			}
			for table, want := range businessCounts {
				var count int
				if err := f.a.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
					t.Fatal("discovery/recovery mutated live sending or delivery state", table, err)
				}
			}
			registry, _, err := f.a.backupRegistry(t.Context())
			if err != nil || registry.Active != nil || len(registry.Success) != 0 {
				t.Fatal("discovery or restore contaminated upload last-success", err)
			}
			if _, err := os.Stat(f.a.cfg.BackupArtifactDir); !os.IsNotExist(err) {
				t.Fatal("independent discovery unexpectedly recreated catalog")
			}
			for _, secret := range []string{f.identity.String(), f.record.Credentials.SecretAccessKey, f.record.Credentials.Password, "synthetic-current-refresh", "synthetic-refreshed-token"} {
				if secret != "" && strings.Contains(jsonEncode(backupJobResponse(stored))+jsonEncode(page), secret) {
					t.Fatal("discovery DTO exposed secret")
				}
			}
		})
	}
}

func discoveryFixtureEntries(t *testing.T, f discoveryFixture, count int) []storage.RecoveryIndexEntry {
	t.Helper()
	d := f.record.Config
	d.ID = f.entry.DestinationID
	key, _ := f.a.backupMasterKey()
	defer clear(key)
	entries := make([]storage.RecoveryIndexEntry, 0, count)
	for i := 0; i < count; i++ {
		object := fmt.Sprintf("lanqin-%032x.age", i+1)
		if d.S3 != nil {
			object = d.S3.Prefix + "/" + object
		}
		if d.Drive != nil {
			settings := *d.Drive
			settings.FolderID = "synthetic-original-folder"
			d.Drive = &settings
			object = fmt.Sprintf("synthetic-original-file-%d", i)
		}
		document, err := storage.SealRecoveryIndex(d, object, f.entry.SHA256, f.entry.Bytes, f.entry.CompletedAt.Add(time.Duration(i)*time.Second), key)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := storage.OpenRecoveryIndex(d, document, key)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestBackupDiscoveryPaginatesAuthenticatedEncryptedPagesAndBindsCursors(t *testing.T) {
	f := newDiscoveryFixture(t, "s3")
	entries := discoveryFixtureEntries(t, f, 128)
	f.a.backupDiscoverIndexes = func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
		return entries, nil
	}
	job := f.create(t, "discovery-pages-0001")
	stored := f.run(t, job)
	if stored.Status != "succeeded" || stored.Discovery.Count != 128 || stored.Discovery.Pages != 4 {
		t.Fatal("discovery count/pages incorrect", stored.ErrorCode)
	}
	seen := map[string]bool{}
	cursor := ""
	secondCursor := ""
	for i := 0; i < 4; i++ {
		page, status := f.results(t, job.ID, cursor)
		if status != 200 || len(page.Items) != 32 {
			t.Fatal("invalid result page", i, status, len(page.Items))
		}
		for _, item := range page.Items {
			if seen[item.ID] || item.OriginalDestinationID != f.entry.DestinationID {
				t.Fatal("duplicate/lost discovery identity")
			}
			seen[item.ID] = true
		}
		if i == 0 {
			secondCursor = page.NextCursor
		}
		if (page.NextCursor != "") != (i < 3) {
			t.Fatal("pagination cursor omitted or invented")
		}
		cursor = page.NextCursor
		var pageValue backupDiscoveryPage
		raw, err := f.a.backupRead(t.Context(), backupDiscoveryPageKey(job.ID, i), &pageValue)
		if err != nil || len(raw) > maxBackupDestinationCiphertext || strings.Contains(raw, f.entry.ObjectKey) || strings.Contains(raw, f.entry.DestinationID) || strings.Contains(raw, f.entry.SHA256) || pageValue.JobID != job.ID || pageValue.Page != i || len(pageValue.Entries) != 32 {
			t.Fatal("discovery page not bounded/encrypted/job-bound", err)
		}
	}
	if len(seen) != 128 {
		t.Fatal("pagination lost records")
	}
	for _, badCursor := range []string{"bad-cursor", strings.Repeat("a", 257), base64.RawURLEncoding.EncodeToString([]byte(job.ID + ":4")), base64.RawURLEncoding.EncodeToString([]byte("bkj_00000000000000000000000000000000:1"))} {
		_, status := f.results(t, job.ID, badCursor)
		if status != http.StatusBadRequest {
			t.Fatal("invalid cursor accepted", status)
		}
	}
	_, currentVault, err := f.a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.revision = backupRevision(currentVault)
	second := f.create(t, "discovery-pages-0002")
	f.run(t, second)
	if _, status := f.results(t, second.ID, secondCursor); status != http.StatusBadRequest {
		t.Fatal("cursor crossed discovery job", status)
	}
}

func TestBackupDiscoveryRejectsInvalidTransportResultsWithoutPartialPages(t *testing.T) {
	for _, scenario := range []string{"too-many", "duplicate", "wrong-location", "wrong-sha", "provider-error", "cancel-during-network", "changed-destination", "expired-lease", "wrong-worker"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDiscoveryFixture(t, "webdav")
			job := f.create(t, "discovery-failure-0001")
			f.a.backupDiscoverIndexes = func(ctx context.Context, _ storage.Destination, _ []byte, _ storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				entries := []storage.RecoveryIndexEntry{f.entry}
				switch scenario {
				case "too-many":
					entries = discoveryFixtureEntries(t, f, 129)
				case "duplicate":
					entries = append(entries, f.entry)
				case "wrong-location":
					entries[0].Location = strings.Repeat("0", 64)
				case "wrong-sha":
					entries[0].SHA256 = "synthetic-secret-provider-response"
				case "provider-error":
					return nil, errors.New("synthetic-secret-provider-response session=do-not-echo")
				case "cancel-during-network":
					if err := f.a.cancelBackupUpload(ctx, job.ID); err != nil {
						t.Fatal(err)
					}
				case "changed-destination":
					vault, raw, err := f.a.loadBackupDestinationState(ctx)
					if err != nil {
						t.Fatal(err)
					}
					vault.Items[0].Credentials.Password = "synthetic-replacement-password"
					if err := f.a.saveBackupDestinationState(ctx, raw, vault); err != nil {
						t.Fatal(err)
					}
				case "expired-lease":
					registry, raw, err := f.a.backupRegistry(ctx)
					if err != nil {
						t.Fatal(err)
					}
					registry.Active.Until = f.a.now().Add(-time.Second)
					if err := f.a.backupCAS(ctx, f.a.db, backupRegistryKey, raw, registry); err != nil {
						t.Fatal(err)
					}
				case "wrong-worker":
					current, raw, err := f.a.backupReadJob(ctx, job.ID)
					if err != nil {
						t.Fatal(err)
					}
					current.WorkerToken = "synthetic-wrong-worker"
					if err := f.a.backupCAS(ctx, f.a.db, backupJobPrefix+job.ID, raw, current); err != nil {
						t.Fatal(err)
					}
				}
				return entries, nil
			}
			stored := f.run(t, job)
			if stored.Discovery.Ready || stored.Discovery.Count != 0 || stored.Status == "succeeded" {
				t.Fatal("invalid discovery published partial ready", scenario)
			}
			var count int
			if err := f.a.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM system_settings WHERE "+keyColumnSQL(f.a.cfg.DBDriver)+" LIKE ?", backupDiscoveryPrefix+job.ID+".%").Scan(&count); err != nil || count != 0 {
				t.Fatal("failed discovery left pages", err)
			}
			if _, status := f.results(t, job.ID, ""); status == 200 {
				t.Fatal("failed/incomplete discovery has readable inventory")
			}
			if strings.Contains(jsonEncode(backupJobResponse(stored)), "synthetic-secret-provider-response") {
				t.Fatal("provider secret entered discovery report")
			}
		})
	}
}

func TestBackupDiscoveryPendingCancelRestartAndIdempotencyNeverPublishInventory(t *testing.T) {
	for _, scenario := range []string{"cancel-pending", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDiscoveryFixture(t, "s3")
			f.a.backupDiscoverIndexes = func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				t.Fatal("canceled/interrupted discovery replayed")
				return nil, nil
			}
			job := f.create(t, "discovery-once-0001")
			repeated := f.create(t, "discovery-once-0001")
			if repeated.ID != job.ID {
				t.Fatal("discovery retry made duplicate job")
			}
			if scenario == "cancel-pending" {
				if err := f.a.cancelBackupUpload(t.Context(), job.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				lease, err := f.a.claimBackupUpload(t.Context())
				if err != nil || lease == nil {
					t.Fatal(err)
				}
				registry, raw, err := f.a.backupRegistry(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				registry.Active.Until = f.a.now().Add(-time.Second)
				if err := f.a.backupCAS(t.Context(), f.a.db, backupRegistryKey, raw, registry); err != nil {
					t.Fatal(err)
				}
			}
			if lease, err := f.a.claimBackupUpload(t.Context()); err != nil || lease != nil {
				t.Fatal("stopped discovery requeued", err)
			}
			stored, _, err := f.a.backupReadJob(t.Context(), job.ID)
			if err != nil || stored.Discovery.Ready || stored.Status != "cancelled" && stored.Status != "interrupted" {
				t.Fatal("stopped discovery state lost", err)
			}
			if _, status := f.results(t, job.ID, ""); status == 200 {
				t.Fatal("stopped discovery readable")
			}
		})
	}
}

func TestBackupDiscoveryPageIntegrityAndRestoreSourceBinding(t *testing.T) {
	for _, scenario := range []string{"tampered-ciphertext", "wrong-job", "wrong-page", "missing-page", "duplicate-entry", "entry-not-in-job", "wrong-sha", "mixed-source-entry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDiscoveryFixture(t, "s3")
			entries := discoveryFixtureEntries(t, f, 2)
			f.a.backupDiscoverIndexes = func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				return entries, nil
			}
			job := f.create(t, "discovery-integrity-0001")
			stored := f.run(t, job)
			page, status := f.results(t, job.ID, "")
			if status != 200 || len(page.Items) != 2 {
				t.Fatal("missing integrity fixture inventory")
			}
			req := backupRestoreRequest{SourceJobID: job.ID, DiscoveryEntryID: page.Items[0].ID, DestinationID: f.record.Config.ID, ExpectedSHA256: page.Items[0].SHA256, DestinationsRevision: f.revision, RequestID: "discovery-tampered-restore", Identity: f.identity.String(), Confirm: true}
			key := backupDiscoveryPageKey(job.ID, 0)
			var persisted backupDiscoveryPage
			raw, err := f.a.backupRead(t.Context(), key, &persisted)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "tampered-ciphertext":
				if _, err := f.a.db.ExecContext(t.Context(), "UPDATE system_settings SET value=? WHERE "+keyColumnSQL(f.a.cfg.DBDriver)+"=?", raw[:len(raw)-8]+"AAAAAAAA", key); err != nil {
					t.Fatal(err)
				}
			case "wrong-job":
				persisted.JobID = "bkj_00000000000000000000000000000000"
			case "wrong-page":
				persisted.Page = 1
			case "missing-page":
				if _, err := f.a.db.ExecContext(t.Context(), "DELETE FROM system_settings WHERE "+keyColumnSQL(f.a.cfg.DBDriver)+"=?", key); err != nil {
					t.Fatal(err)
				}
			case "duplicate-entry":
				persisted.Entries[1] = persisted.Entries[0]
			case "entry-not-in-job":
				req.DiscoveryEntryID = "bdi_" + strings.Repeat("0", 64)
			case "wrong-sha":
				req.ExpectedSHA256 = strings.Repeat("0", 64)
			case "mixed-source-entry":
				req.SourceJobID = "bkj_00000000000000000000000000000000"
			}
			if scenario == "wrong-job" || scenario == "wrong-page" || scenario == "duplicate-entry" {
				if err := f.a.backupCAS(t.Context(), f.a.db, key, raw, persisted); err != nil {
					t.Fatal(err)
				}
			}
			if _, execution, err := f.a.admitBackupRestore(t.Context(), "integrity-actor", req); err == nil || execution != nil {
				t.Fatal("invalid discovery page/source admitted recovery", scenario)
			}
			if scenario == "tampered-ciphertext" || scenario == "wrong-job" || scenario == "wrong-page" || scenario == "missing-page" {
				if _, status := f.results(t, stored.ID, ""); status == 200 {
					t.Fatal("invalid job/page binding listed")
				}
			}
			registry, _, err := f.a.backupRegistry(t.Context())
			if err != nil || registry.Active != nil {
				t.Fatal("rejected discovered source consumed lease", err)
			}
		})
	}
}

func TestBackupDiscoveryRoutesRejectSettingsOperatorsTokensAndArbitraryInputs(t *testing.T) {
	f := newDiscoveryFixture(t, "s3")
	group := createAssignableGroup(t, f.client, "synthetic discovery settings operator", []string{PermissionSettingsView, PermissionSettingsUpdate})
	var operator AdminUser
	if status := f.client.do("POST", "/api/admin/users", map[string]any{"email": "discovery-operator@example.test", "displayName": "Synthetic settings operator", "role": "user", "password": "SyntheticDiscovery123!", "permissionGroupIds": []string{group.ID}}, &operator); status != 201 {
		t.Fatal("operator fixture failed", status)
	}
	ops := &testClient{t: t, server: f.client.server}
	var response map[string]any
	if status := ops.do("POST", "/api/auth/login", map[string]string{"email": operator.Email, "password": "SyntheticDiscovery123!"}, &response); status != 200 {
		t.Fatal("operator fixture login failed", status)
	}
	token := createTestAPIToken(t, f.client, "synthetic discovery token")
	tokenClient := &testClient{t: t, server: f.client.server, bearer: token}
	anonymous := &testClient{t: t, server: f.client.server}
	for _, route := range []struct{ method, path string }{{"POST", "/api/admin/backups/discoveries"}, {"GET", "/api/admin/backups/jobs/bkj_00000000000000000000000000000000/discovery"}} {
		if status := ops.do(route.method, route.path, f.request("unauthorized-discovery"), &response); status != 403 {
			t.Fatal("delegated settings operator accessed discovery", status)
		}
		for _, client := range []*testClient{tokenClient, anonymous} {
			if status := client.do(route.method, route.path, f.request("unauthorized-discovery"), &response); status != 401 && status != 403 {
				t.Fatal("API token or anonymous accessed discovery", status)
			}
		}
	}
	for _, mutation := range []func(map[string]any){
		func(r map[string]any) { r["objectKey"] = "arbitrary-private-object" },
		func(r map[string]any) { r["endpoint"] = "https://user:synthetic-secret@example.invalid" },
		func(r map[string]any) { r["indexKey"] = "synthetic-secret-master" },
		func(r map[string]any) { r["destinationsRevision"] = "stale-revision" },
		func(r map[string]any) { r["sourceFolderId"] = "arbitrary-drive-folder-on-s3" },
		func(r map[string]any) { r["destinationId"] = "unrelated-destination" },
	} {
		request := f.request("invalid-discovery-input")
		mutation(request)
		status := f.client.do("POST", "/api/admin/backups/discoveries", request, &response)
		if status == 202 || strings.Contains(jsonEncode(response), "synthetic-secret") {
			t.Fatal("arbitrary discovery source/key accepted or echoed", status)
		}
	}
	registry, _, err := f.a.backupRegistry(t.Context())
	if err != nil || registry.Active != nil || len(registry.Pending) != 0 {
		t.Fatal("rejected discovery consumed shared queue", err)
	}
}

func assertBackupDiscoveryDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	previousKey, previousCatalog, previousDiscover := a.cfg.BackupSecretKey, a.cfg.BackupArtifactDir, a.backupDiscoverIndexes
	a.cfg.BackupSecretKey, a.cfg.BackupArtifactDir = backupTestKey, ""
	defer func() {
		a.cfg.BackupSecretKey, a.cfg.BackupArtifactDir, a.backupDiscoverIndexes = previousKey, previousCatalog, previousDiscover
	}()
	record, err := buildBackupDestination("bkd_discovery_contract", backupTestRequest("s3", "discovery-contract", "0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	vault, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	vault.Items = append(vault.Items, record)
	if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
		t.Fatal(err)
	}
	_, raw, err = a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	original := record.Config
	original.ID = "bkd_previous_instance"
	key, _ := a.backupMasterKey()
	defer clear(key)
	document, err := storage.SealRecoveryIndex(original, original.S3.Prefix+"/lanqin-0123456789abcdef0123456789abcdef.age", strings.Repeat("a", 64), 12345, time.Now().UTC(), key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := storage.OpenRecoveryIndex(record.Config, document, key)
	if err != nil {
		t.Fatal(err)
	}
	a.backupDiscoverIndexes = func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
		return []storage.RecoveryIndexEntry{entry}, nil
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	beforeSuccess := jsonEncode(registry.Success)
	job, err := a.enqueueBackupDiscovery(t.Context(), "contract-actor", backupDiscoveryRequest{DestinationID: record.Config.ID, DestinationsRevision: backupRevision(raw), RequestID: "discovery-contract-request-0001"})
	if err != nil {
		t.Fatal("cross-database discovery admission", err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil || lease.ID != job.ID {
		t.Fatal("cross-database discovery claim", err)
	}
	a.runBackupUpload(t.Context(), *lease)
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "succeeded" || stored.Discovery.Count != 1 || !stored.Discovery.Ready {
		t.Fatal("cross-database discovery did not commit pages", err)
	}
	actual, page, err := a.readBackupDiscoveryEntry(t.Context(), stored, discoveryEntryID(entry))
	if err != nil || actual != entry || page.value.JobID != job.ID || strings.Contains(page.raw, entry.ObjectKey) {
		t.Fatal("cross-database encrypted discovery persistence", err)
	}
	registry, _, err = a.backupRegistry(t.Context())
	if err != nil || registry.Active != nil || jsonEncode(registry.Success) != beforeSuccess {
		t.Fatal("cross-database discovery polluted last success", err)
	}
}

func TestBackupDiscoverySQLiteDatabaseContract(t *testing.T) {
	assertBackupDiscoveryDatabaseContract(t, newTestApp(t))
}
