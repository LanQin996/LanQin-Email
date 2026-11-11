package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

func TestBackupPasswordCaptureAndRealRestoreWithoutAgeEnvironmentKey(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	a.cfg.BackupRecipient = ""
	a.cfg.BackupRestoreDir = t.TempDir()
	const password = "  synthetic portable backup password  "
	setBackupTestPassword(t, a, password)
	configuration, err := a.backupEncryptionConfiguration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
		t.Fatal(err)
	}
	source, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-admin", "password-capture-request", ids[:1], revision, configuration.Recipient)
	if err != nil {
		t.Fatal("password capture admission failed without age key", err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal("capture lease unavailable", err)
	}
	captured, code := a.createBackupArtifact(t.Context(), *lease, source)
	if code != "" {
		t.Fatal("actual password capture failed", code)
	}
	master, err := a.backupMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	file, artifact, err := catalog.Open(a.cfg.BackupArtifactDir, captured.Artifact.ID, master, backupMaxBytes)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := io.ReadAll(file)
	file.Close()
	if err != nil || bytes.Contains(ciphertext, []byte(password)) {
		t.Fatal("invalid encrypted password artifact", err)
	}
	now := a.now().UTC()
	if err := a.finishBackupUpload(t.Context(), *lease, []storage.Result{{DestinationID: ids[0], State: "succeeded", ObjectKey: "synthetic/lanqin-0123456789abcdef0123456789abcdef.age", CompletedAt: &now}}, ""); err != nil {
		t.Fatal(err)
	}
	request := backupRestoreRequest{SourceJobID: source.ID, DestinationID: ids[0], ExpectedSHA256: artifact.SHA256, DestinationsRevision: revision, RequestID: "password-recovery-request", EncryptionMode: "password", BackupPassword: password, Confirm: true}
	invalid := request
	invalid.Identity = "AGE-SECRET-KEY-DO-NOT-ACCEPT-BOTH-MODES"
	if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", invalid); err == nil {
		t.Fatal("mixed recovery secrets accepted")
	}
	job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", request)
	if err != nil || execution == nil || len(execution.Identity) != 0 {
		t.Fatal("password restore admission failed", err)
	}
	changed := request
	changed.BackupPassword = "different synthetic backup password"
	if _, _, err := a.admitBackupRestore(t.Context(), "synthetic-admin", changed); err == nil {
		t.Fatal("changed password reused recovery idempotency key")
	}
	a.backupDownloadFile = func(_ context.Context, _ storage.Destination, _, hash, output string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
		if err := os.WriteFile(output, ciphertext, 0o600); err != nil {
			return storage.DownloadReceipt{}, err
		}
		return storage.DownloadReceipt{SHA256: hash, Bytes: int64(len(ciphertext))}, nil
	}
	a.runBackupRestore(t.Context(), *execution)
	if !bytes.Equal(execution.Secret, make([]byte, len(execution.Secret))) {
		t.Fatal("restore password buffer was retained")
	}
	restored, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || restored.Status != "succeeded" || restored.Restore == nil || !restored.Restore.Ready {
		t.Fatal("real password archive recovery failed", restored.Status, restored.ErrorCode, err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.BackupRestoreDir, restored.Restore.Directory, "recovered", "READY")); err != nil {
		t.Fatal("password recovery did not publish verified data", err)
	}
	for _, value := range []string{raw, jsonEncode(restored), jsonEncode(backupJobResponse(restored)), jsonEncode(backupJobResponse(captured))} {
		if strings.Contains(value, password) || strings.Contains(value, "backupPassword") {
			t.Fatal("backup password entered persisted job or response")
		}
	}
	// A wrong password is admitted as a bounded operation, then fails archive
	// authentication without publishing data or retaining the supplied secret.
	request.RequestID = "password-recovery-wrong-request"
	request.BackupPassword = "wrong synthetic backup password"
	_, destinationRaw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request.DestinationsRevision = backupRevision(destinationRaw)
	wrongJob, wrongExecution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", request)
	if err != nil || wrongExecution == nil {
		t.Fatal("wrong-password operation not admitted", err)
	}
	a.runBackupRestore(t.Context(), *wrongExecution)
	failed, _, err := a.backupReadJob(t.Context(), wrongJob.ID)
	if err != nil || failed.Status != "failed" || failed.ErrorCode != "restore_verification_failed" || failed.Restore.Ready {
		t.Fatal("wrong password advertised recoverable data", err)
	}
	if !bytes.Equal(wrongExecution.Secret, make([]byte, len(wrongExecution.Secret))) {
		t.Fatal("failed restore retained backup password")
	}
}

func TestBackupPasswordRotationInvalidatesQueuedCapture(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	const password = "synthetic original backup password"
	setBackupTestPassword(t, a, password)
	before, _ := a.backupEncryptionConfiguration(t.Context())
	job, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-admin", "password-rotation-request", ids[:1], revision, before.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	const next = "synthetic rotated backup password"
	if err := a.saveBackupEncryption(t.Context(), before.Revision, "password", next, next, password); err != nil {
		t.Fatal(err)
	}
	if _, code := a.createBackupArtifact(t.Context(), *lease, job); code != "capture_configuration_changed" {
		t.Fatal("queued capture silently switched its password", code)
	}
}
