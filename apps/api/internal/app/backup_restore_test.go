package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/storage"
)

// TempDir's numbered directory follows the process umask. Private backup
// workspaces must satisfy the production permission check on Unix as well.
func privateBackupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func restoreJobFixture(t *testing.T, a *App) (backupJob, backupRestoreRequest, []byte) {
	t.Helper()
	_, records, _ := backupJobFixture(t, a)
	a.cfg.MaildirRoot = t.TempDir()
	a.cfg.BackupRestoreDir = privateBackupTestDir(t)
	if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOnlineBackupOptions()
	prepared, err := PrepareOnlineBackup(t.Context(), a.cfg, privateBackupTestDir(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	var data bytes.Buffer
	if _, err := backup.WriteOnline(t.Context(), prepared.Path, &data, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	archive := filepath.Join(t.TempDir(), "recovery.age")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	key, _ := a.backupMasterKey()
	entry, err := catalog.Publish(t.Context(), a.cfg.BackupArtifactDir, archive, hex.EncodeToString(hash[:]), key, backupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	job, err := a.enqueueBackupUpload(t.Context(), "restore-fixture", "restore-fixture-upload", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	now := a.now().UTC()
	if err := a.finishBackupUpload(t.Context(), *lease, []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &now, ObjectKey: records[0].Config.S3.Prefix + "/lanqin-0123456789abcdef0123456789abcdef.age"}}, ""); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := a.loadBackupDestinationState(t.Context())
	req := backupRestoreRequest{SourceJobID: job.ID, DestinationID: records[0].Config.ID, ExpectedSHA256: entry.SHA256, DestinationsRevision: backupRevision(raw), RequestID: "restore-fixture-request", Identity: identity.String(), Confirm: true, Password: a.cfg.AdminPassword}
	return job, req, append([]byte{}, data.Bytes()...)
}

func TestBackupRestoreMappedPrivateRootBindsAuthenticatedRecordAndCopiedPayload(t *testing.T) {
	a := newTestApp(t)
	_, req, ciphertext := restoreJobFixture(t, a)
	job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
	if err != nil || execution == nil {
		t.Fatal("restore admission failed", err)
	}
	a.backupDownloadFile = func(_ context.Context, _ storage.Destination, _, hash, output string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
		if err := os.WriteFile(output, ciphertext, 0o600); err != nil {
			return storage.DownloadReceipt{}, err
		}
		return storage.DownloadReceipt{SHA256: hash, Bytes: int64(len(ciphertext))}, nil
	}
	a.runBackupRestore(t.Context(), *execution)
	stored, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "succeeded" || stored.Restore == nil || !stored.Restore.Ready {
		t.Fatal("mapped-root fixture restore failed", err)
	}
	// The actual database/archive flow ran above. Emulate the authenticated
	// Linux container namespace independently of this Windows/Linux test host.
	const recordedRoot = "/synthetic-container/private-recovery"
	stored.Restore.Root = recordedRoot
	if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+job.ID, raw, stored); err != nil {
		t.Fatal(err)
	}
	originalRoot := a.cfg.BackupRestoreDir
	hostRoot := filepath.Join(t.TempDir(), "mapped-private-recovery")
	if err := os.Rename(originalRoot, hostRoot); err != nil {
		t.Fatal("cannot move complete private recovery root", err)
	}
	assertPrivateFailure := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("invalid mapped-root recovery accepted")
		}
		for _, secret := range []string{recordedRoot, originalRoot, hostRoot, a.cfg.DBPath, a.cfg.BackupSecretKey} {
			if secret != "" && strings.Contains(err.Error(), secret) {
				t.Fatal("mapped handoff error exposed source configuration")
			}
		}
	}
	_, _, err = ResolveWebRestore(t.Context(), a.cfg.DBPath, hostRoot, a.cfg.BackupSecretKey, job.ID)
	assertPrivateFailure(err)
	for _, root := range []string{"/wrong/recorded-private-root", "/", "relative", "/synthetic/../private", "/synthetic/"} {
		_, _, err := ResolveWebRestoreAt(t.Context(), a.cfg.DBPath, hostRoot, root, a.cfg.BackupSecretKey, job.ID)
		assertPrivateFailure(err)
	}
	container, receipt, err := ResolveWebRestoreAt(t.Context(), a.cfg.DBPath, hostRoot, recordedRoot, a.cfg.BackupSecretKey, job.ID)
	if err != nil || container != filepath.Join(hostRoot, stored.Restore.Directory, "recovered") || receipt != stored.Restore.ReportSHA256 {
		t.Fatal("explicit host/container mapping rejected", err)
	}
	if _, err := os.Lstat(originalRoot); !os.IsNotExist(err) {
		t.Fatal("mapped handoff recreated original recovery root")
	}
	// Resolving authenticates the report. Final installer preparation must still
	// validate the copied payload; mapping never grants permission to skip it.
	databasePath := filepath.Join(container, "snapshot", "data", "lanqin.db")
	database, err := os.ReadFile(databasePath)
	if err != nil || len(database) == 0 {
		t.Fatal("missing recovered SQLite fixture", err)
	}
	modified := append([]byte{}, database...)
	modified[len(modified)-1] ^= 1
	if err := os.WriteFile(databasePath, modified, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWebRestoreAt(t.Context(), a.cfg.DBPath, hostRoot, recordedRoot, a.cfg.BackupSecretKey, job.ID); err != nil {
		t.Fatal("report-only resolver unexpectedly replaced payload validation", err)
	}
	prepared, err := install.Prepare(t.Context(), container, privateBackupTestDir(t), "/runtime/data", "/runtime/mail", backup.DefaultLimits(), receipt)
	if err == nil {
		prepared.Close()
		t.Fatal("mapped handoff installed changed payload")
	}
	assertPrivateFailure(err)
	if err := os.WriteFile(databasePath, database, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err = install.Prepare(t.Context(), container, privateBackupTestDir(t), "/runtime/data", "/runtime/mail", backup.DefaultLimits(), receipt)
	if err != nil {
		t.Fatal("valid mapped payload could not be prepared", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(container, "snapshot", "data", "backup-online.json")
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(report, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ResolveWebRestoreAt(t.Context(), a.cfg.DBPath, hostRoot, recordedRoot, a.cfg.BackupSecretKey, job.ID)
	assertPrivateFailure(err)
}

func TestBackupRestoreRealArchiveAdmissionRecoveryAndPrivacy(t *testing.T) {
	a := newTestApp(t)
	source, req, data := restoreJobFixture(t, a)
	// New captures require a password, but explicit historical X25519 archive
	// metadata and its matching offline recovery identity remain supported.
	identity, err := age.ParseX25519Identity(req.Identity)
	if err != nil {
		t.Fatal(err)
	}
	storedSource, sourceRaw, err := a.backupReadJob(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedSource.Recipient = identity.Recipient().String()
	if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+source.ID, sourceRaw, storedSource); err != nil {
		t.Fatal(err)
	}
	job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
	if err != nil || execution == nil {
		t.Fatal("admission", err)
	}
	// Receipt must be bound before doing any slow network work, and repeats
	// cannot create a second private-key operation or mutate the active request.
	duplicate, next, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
	if err != nil || next != nil || duplicate.ID != job.ID {
		t.Fatal("idempotency", err)
	}
	other := req
	other.RequestID += "-different"
	if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", other); !errors.Is(err, errBackupRestoreBusy) {
		t.Fatal("concurrent recovery was admitted", err)
	}
	a.backupDownloadFile = func(ctx context.Context, d storage.Destination, object, hash, output string, opts storage.Options, c storage.Credentials) (storage.DownloadReceipt, error) {
		if a.db.Stats().InUse != 0 || hash != source.Artifact.SHA256 || c.SecretAccessKey != "s3-synthetic-secret" || d.ID != req.DestinationID || opts.MaxBytes != int64(len(data)) {
			t.Fatal("download held business connection or used unbound source")
		}
		if err := os.WriteFile(output, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return storage.DownloadReceipt{SHA256: hash, Bytes: int64(len(data))}, nil
	}
	a.runBackupRestore(t.Context(), *execution)
	if !bytes.Equal(execution.Identity, make([]byte, len(execution.Identity))) {
		t.Fatal("private identity buffer was retained")
	}
	stored, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "succeeded" || !stored.Restore.Ready || stored.Restore.Manifest.Scope != "online-data" {
		t.Fatal("actual age/SQLite/installer verification failed", stored.Status, stored.ErrorCode, err)
	}
	path := filepath.Join(a.cfg.BackupRestoreDir, stored.Restore.Directory)
	for _, name := range []string{"recovered/READY", "recovered/snapshot/data/lanqin.db", "recovered/snapshot/data/backup-online.json"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			t.Fatal("verified recovery missing", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(path, "ciphertext.age")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("redundant download retained")
	}
	public := jsonEncode(backupJobResponse(stored))
	for _, secret := range []string{req.Identity, req.Password, a.cfg.BackupRestoreDir, stored.Restore.Directory, "workerToken", "fingerprint", "synthetic-secret"} {
		if strings.Contains(raw, secret) || strings.Contains(public, secret) {
			t.Fatal("secret/local path leaked to report or unencrypted storage")
		}
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if registry.Active != nil || registry.Success[req.DestinationID].JobID != source.ID {
		t.Fatal("restore contaminated upload success registry")
	}
	// Source DB/session state stays live; recovery validation modifies only its
	// disposable prepared copy, not live or verified original staged data.
	if _, err := a.db.Exec("SELECT 1"); err != nil {
		t.Fatal("recovery touched live database", err)
	}
	resolved, receipt, err := ResolveWebRestore(t.Context(), a.cfg.DBPath, a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, job.ID)
	if err != nil || resolved != filepath.Join(path, "recovered") || receipt != stored.Restore.ReportSHA256 || len(receipt) != 64 {
		t.Fatal("authenticated maintenance handoff failed", err)
	}
	if _, _, err := ResolveWebRestore(t.Context(), a.cfg.DBPath, a.cfg.BackupRestoreDir, backupTestKey+"x", job.ID); err == nil {
		t.Fatal("handoff accepted incorrect master key")
	}
	if _, _, err := ResolveWebRestore(t.Context(), a.cfg.DBPath, t.TempDir(), a.cfg.BackupSecretKey, job.ID); err == nil {
		t.Fatal("handoff accepted another staging root")
	}
	report := filepath.Join(resolved, "snapshot", "data", "backup-online.json")
	content, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWebRestore(t.Context(), a.cfg.DBPath, a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, job.ID); err == nil {
		t.Fatal("handoff trusted a modified plaintext report")
	}
}

func TestBackupRestoreFailureCancellationAndRestartNeverReplay(t *testing.T) {
	for _, scenario := range []string{"wrong-key", "tampered", "download-failed", "cancel", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			a := newTestApp(t)
			_, req, data := restoreJobFixture(t, a)
			if scenario == "wrong-key" {
				key, _ := age.GenerateX25519Identity()
				req.Identity = key.String() // Upload receipts may predate public-key metadata.
			}
			job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "restart" {
				registry, raw, _ := a.backupRegistry(t.Context())
				registry.Active.Until = a.now().Add(-time.Second)
				if err := a.backupCAS(t.Context(), a.db, backupRegistryKey, raw, registry); err != nil {
					t.Fatal(err)
				}
				if lease, err := a.claimBackupUpload(t.Context()); err != nil || lease != nil {
					t.Fatal("private key job replayed", err)
				}
				duplicate, replay, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
				if err != nil || replay != nil || duplicate.Status != "interrupted" {
					t.Fatal("restart recovered secret or auto-replayed restore", err)
				}
				clear(execution.Identity)
				return
			}
			a.backupDownloadFile = func(ctx context.Context, _ storage.Destination, _, hash, output string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
				if scenario == "download-failed" {
					return storage.DownloadReceipt{}, errors.New("synthetic provider secret must not escape")
				}
				if scenario == "cancel" {
					if err := a.cancelBackupUpload(ctx, job.ID); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "tampered" {
					data[len(data)-1] ^= 1
				}
				if err := os.WriteFile(output, data, 0o600); err != nil {
					t.Fatal(err)
				}
				return storage.DownloadReceipt{SHA256: hash, Bytes: int64(len(data))}, nil
			}
			a.runBackupRestore(t.Context(), *execution)
			stored, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil || stored.Restore.Ready || stored.Status != "failed" && stored.Status != "cancelled" || stored.ErrorCode == "" {
				t.Fatal("failed restore advertised ready", stored.Status, stored.ErrorCode, err)
			}
			entries, err := os.ReadDir(a.cfg.BackupRestoreDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed recovery leaked plaintext workspace", err)
			}
		})
	}
}

func TestBackupRestoreHTTPReauthenticationAndSourceRestrictions(t *testing.T) {
	a := newTestApp(t)
	source, req, _ := restoreJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	var body map[string]any
	wrong := req
	wrong.Password = "wrong-synthetic-password"
	if code := client.do("POST", "/api/admin/backups/restores", wrong, &body); code != http.StatusForbidden {
		t.Fatal("restore accepted wrong administrator password", code)
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if registry.Active != nil || len(a.backupRestoreRequests) != 0 {
		t.Fatal("failed reauth dispatched private-key work")
	}
	// Pruned, altered receipts and unconfirmed operations are rejected before
	// network traffic or retaining the private key in a persistent work item.
	for _, mutate := range []func(*backupRestoreRequest){
		func(r *backupRestoreRequest) { r.Confirm = false },
		func(r *backupRestoreRequest) { r.ExpectedSHA256 = strings.Repeat("0", 64) },
		func(r *backupRestoreRequest) { r.DestinationsRevision = "stale" },
		func(r *backupRestoreRequest) { r.DestinationID = "unrelated-destination" },
	} {
		invalid := req
		mutate(&invalid)
		if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", invalid); err == nil {
			t.Fatal("invalid recovery admitted")
		}
	}
	stored, raw, _ := a.backupReadJob(t.Context(), source.ID)
	stored.Pruned = map[string]backupPruned{req.DestinationID: {State: "pending"}}
	if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+source.ID, raw, stored); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req); !errors.Is(err, errBackupRestoreSource) {
		t.Fatal("possibly deleted copy offered for recovery", err)
	}
}

func TestBackupRestoreHTTPRequiresFreshTwoFactorAndRateLimits(t *testing.T) {
	for _, scenario := range []string{"two-factor", "rate-limit"} {
		t.Run(scenario, func(t *testing.T) {
			a := newTestApp(t)
			_, req, _ := restoreJobFixture(t, a)
			client := backupLoggedInAdmin(t, a)
			var body map[string]any
			if scenario == "rate-limit" {
				req.Password = "synthetic-wrong-password"
				for i := 0; i < 6; i++ {
					code := client.do("POST", "/api/admin/backups/restores", req, &body)
					if i < 4 && code != http.StatusForbidden || i >= 4 && code != http.StatusTooManyRequests {
						t.Fatal("recovery reauthentication was not throttled", i, code)
					}
				}
				if len(a.backupRestoreRequests) != 0 {
					t.Fatal("incorrect authentication queued private key")
				}
				return
			}
			secret, err := newTOTPSecret()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec(`UPDATE users SET two_factor_secret=?,two_factor_enabled=1 WHERE email=?`, secret, a.cfg.AdminEmail); err != nil {
				t.Fatal(err)
			}
			if code := client.do("POST", "/api/admin/backups/restores", req, &body); code != http.StatusForbidden {
				t.Fatal("session bypassed second factor", code)
			}
			req.TwoFactorCode, err = generateTOTP(secret, a.now())
			if err != nil {
				t.Fatal(err)
			}
			if code := client.do("POST", "/api/admin/backups/restores", req, &body); code != http.StatusAccepted {
				t.Fatal("fresh reauthentication rejected", code)
			}
			execution := <-a.backupRestoreRequests
			defer clear(execution.Identity)
			if code := client.do("POST", "/api/admin/backups/restores", req, &body); code != http.StatusForbidden {
				t.Fatal("TOTP replay authorized another private-key operation", code)
			}
			if err := a.finishBackupUpload(t.Context(), execution.Lease, nil, "synthetic-stop"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupRestoreWorkspaceRejectsLiveOverlapAndRetainedDataLimit(t *testing.T) {
	a := newTestApp(t)
	_, req, _ := restoreJobFixture(t, a)
	original := a.cfg.BackupRestoreDir
	for _, directory := range []string{a.cfg.DataDir, a.cfg.MaildirRoot, a.cfg.BackupArtifactDir, filepath.Dir(a.cfg.DBPath)} {
		a.cfg.BackupRestoreDir = directory
		if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req); err == nil {
			t.Fatal("live/capture/storage root accepted as recovery workspace")
		}
	}
	a.cfg.BackupRestoreDir = original
	// Cloud recovery must not depend on having the original local catalog.
	a.cfg.BackupArtifactDir = filepath.Join(t.TempDir(), "lost-local-catalog")
	if _, err := a.backupRestoreRoot(); err != nil {
		t.Fatal("lost local upload catalog blocked independent cloud recovery", err)
	}
	for range 8 {
		if _, err := os.MkdirTemp(original, "synthetic-retained-"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req); !errors.Is(err, errBackupLimit) {
		t.Fatal("unbounded retained recovery workspace", err)
	}
}

// This contract tests encrypted metadata admission/CAS, not external database
// payload restoration. The synthetic fixture is intentionally never decrypted.
func assertBackupRestoreDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	entry, records, _ := backupJobFixture(t, a)
	a.cfg.BackupRestoreDir = privateBackupTestDir(t)
	source, err := a.enqueueBackupUpload(t.Context(), "restore-contract", "restore-contract-source", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	now := a.now().UTC()
	if err := a.finishBackupUpload(t.Context(), *lease, []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &now, ObjectKey: records[0].Config.S3.Prefix + "/lanqin-0123456789abcdef0123456789abcdef.age"}}, ""); err != nil {
		t.Fatal(err)
	}
	identity, _ := age.GenerateX25519Identity()
	_, raw, _ := a.loadBackupDestinationState(t.Context())
	req := backupRestoreRequest{SourceJobID: source.ID, DestinationID: records[0].Config.ID, ExpectedSHA256: entry.SHA256, DestinationsRevision: backupRevision(raw), RequestID: "restore-contract-admit", Identity: identity.String(), Confirm: true}
	job, execution, err := a.admitBackupRestore(t.Context(), "contract-actor", req)
	if err != nil || execution == nil {
		t.Fatal("cross-database admission failed", err)
	}
	defer clear(execution.Identity)
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Kind != "restore" || stored.Status != "running" || stored.Restore.SourceJobID != source.ID {
		t.Fatal("restore metadata contract lost", err)
	}
	if err := a.finishBackupUpload(t.Context(), execution.Lease, nil, "synthetic-stop"); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestoreSQLiteMetadataContract(t *testing.T) {
	assertBackupRestoreDatabaseContract(t, newTestApp(t))
}
