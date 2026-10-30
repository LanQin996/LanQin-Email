package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

func TestBackupCaptureWorkerRealArchiveUploadAndRecoveryRuntimeIsolation(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRecipient = identity.Recipient().String()
	if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.backupPrepareSnapshot = func(ctx context.Context, cfg Config, workspace string, opts OnlineBackupOptions) (*PreparedOnlineBackup, error) {
		prepared, err := PrepareOnlineBackup(ctx, cfg, workspace, opts)
		if err != nil {
			t.Errorf("actual capture failed: %v", err)
		}
		return prepared, err
	}
	job, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-actor", "capture-worker-real-0001", ids, revision, a.cfg.BackupRecipient)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	var encrypted []byte
	a.backupUploadFile = func(ctx context.Context, f *os.File, receipt string, cfg storage.Config, _ storage.Options, resolve func(string) (storage.Credentials, error)) (storage.Report, error) {
		current, _, err := a.backupReadJob(ctx, job.ID)
		if err != nil || current.Phase != "uploading" || current.Summary == nil || current.Artifact.ID == "" {
			t.Error("upload started before fenced publication", err)
		}
		encrypted, err = io.ReadAll(f)
		if err != nil {
			return storage.Report{}, err
		}
		hash := sha256.Sum256(encrypted)
		if hex.EncodeToString(hash[:]) != receipt {
			t.Error("wrong receipt")
		}
		if a.db.Stats().InUse != 0 {
			t.Error("upload held business connection")
		}
		results := []storage.Result{}
		for _, d := range cfg.Destinations {
			if _, err := resolve(d.ID); err != nil {
				return storage.Report{}, err
			}
			now := time.Now().UTC()
			results = append(results, storage.Result{DestinationID: d.ID, State: "succeeded", CompletedAt: &now, ObjectKey: "synthetic/snapshot.age", Attempts: 1})
		}
		return storage.Report{SHA256: receipt, Bytes: int64(len(encrypted)), Destinations: results}, nil
	}
	a.runBackupUpload(t.Context(), *lease)
	finished, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "succeeded" || finished.Phase != "completed" || finished.Summary == nil || !finished.Summary.RuntimeBackupQueueCleared || finished.Summary.RuntimeBackupJobsOmitted != 1 {
		t.Fatalf("capture failed: %s %s %v", finished.Status, finished.ErrorCode, err)
	}
	key, err := a.backupMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	f, entry, err := catalog.Open(a.cfg.BackupArtifactDir, finished.Artifact.ID, key, backupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(encrypted), out, identity.String(), entry.SHA256, backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	frozen, err := openFrozenBackupDB(filepath.Join(out, "snapshot", "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Close()
	var count int
	if err := frozen.QueryRow(`SELECT count(*) FROM system_settings WHERE key='backup.queue.v1' OR key LIKE 'backup.job.v1.%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("restored backup would replay copied jobs", count, err)
	}
	if entries, err := os.ReadDir(a.cfg.BackupWorkspace); err != nil || len(entries) != 0 {
		t.Fatal("plaintext/cipher workspace leaked", err)
	}
	public := jsonEncode(backupJobResponse(finished))
	for _, secret := range []string{a.cfg.DataDir, a.cfg.BackupWorkspace, a.cfg.BackupSecretKey, identity.String(), "workerToken", "sources", "fingerprint"} {
		if strings.Contains(public, secret) {
			t.Fatal("private capture data leaked to DTO")
		}
	}
}

func TestBackupCaptureWorkerCancellationDuringCollectionKeepsAPIResponsive(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	job, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-actor", "capture-cancel-collect-001", ids, revision, a.cfg.BackupRecipient)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	a.backupPrepareSnapshot = func(ctx context.Context, _ Config, _ string, _ OnlineBackupOptions) (*PreparedOnlineBackup, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a.backupUploadFile = func(context.Context, *os.File, string, storage.Config, storage.Options, func(string) (storage.Credentials, error)) (storage.Report, error) {
		t.Error("cancelled capture uploaded")
		return storage.Report{}, errors.New("unexpected")
	}
	done := make(chan struct{})
	go func() { a.runBackupUpload(t.Context(), *lease); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("capture not started")
	}
	var response map[string]any
	if code := client.do("GET", "/api/me", nil, &response); code != http.StatusOK {
		t.Fatal("mail API blocked during collection")
	}
	if code := client.do("POST", "/api/admin/backups/jobs/"+job.ID+"/cancel", nil, &response); code != http.StatusAccepted {
		t.Fatal("cancel denied")
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("capture cancellation did not propagate")
	}
	finished, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "cancelled" || finished.Artifact.ID != "" || finished.Phase != "completed" {
		t.Fatal("bad cancellation state", finished.Status, err)
	}
}

func TestBackupCaptureWorkerKeyRotationAndStalePhaseFence(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	job, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-actor", "capture-rotate-fence-001", ids, revision, a.cfg.BackupRecipient)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	wrong := *lease
	wrong.Token = "stale-token"
	if _, err := a.updateBackupCapture(t.Context(), wrong, "encrypting", nil, nil); !errors.Is(err, errBackupConflict) {
		t.Fatal("stale worker mutated capture", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRecipient = identity.Recipient().String()
	a.backupPrepareSnapshot = func(context.Context, Config, string, OnlineBackupOptions) (*PreparedOnlineBackup, error) {
		t.Error("rotated recipient started collection")
		return nil, errors.New("unexpected")
	}
	a.runBackupUpload(t.Context(), *lease)
	finished, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "failed" || finished.ErrorCode != "capture_configuration_changed" || finished.Artifact.ID != "" {
		t.Fatal("queued recipient silently changed", err)
	}
}

func TestBackupCapabilitiesNeverEchoMisconfiguredPrivateKey(t *testing.T) {
	a := newTestApp(t)
	backupSnapshotJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRecipient = identity.String()
	var response map[string]any
	if code := client.do("GET", "/api/admin/backups/capabilities", nil, &response); code != http.StatusOK {
		t.Fatal(code)
	}
	if response["onlineBackupAvailable"] != false || response["recipient"] != nil || strings.Contains(jsonEncode(response), "AGE-SECRET") {
		t.Fatal("private key configuration leaked")
	}
}

func TestBackupCipherWriterHardLimit(t *testing.T) {
	var buffer bytes.Buffer
	w := backupCipherWriter{dst: &buffer, remaining: 3}
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("cd")); err == nil || buffer.String() != "ab" {
		t.Fatal("ciphertext limit did not stop publication")
	}
}
