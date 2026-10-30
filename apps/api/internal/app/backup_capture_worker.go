package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/catalog"
)

// Only stable capability reasons cross the API boundary. Invalid recipient
// configuration must never be echoed: it might accidentally contain a private key.
func (a *App) backupCaptureCapability() (string, string) {
	if normalizeDatabaseConfig(a.cfg).DBDriver != databaseDriverSQLite {
		return "", "sqlite_required"
	}
	key, err := age.ParseX25519Recipient(strings.TrimSpace(a.cfg.BackupRecipient))
	if err != nil || a.cfg.BackupWorkspace == "" || a.cfg.BackupArtifactDir == "" {
		return "", "configuration_required"
	}
	if _, err := a.backupMasterKey(); err != nil {
		return "", "configuration_required"
	}
	data, err := resolvedBackupDirectory(a.cfg.DataDir)
	if err != nil {
		return "", "workspace_unavailable"
	}
	mail, err := resolvedBackupDirectory(a.cfg.MaildirRoot)
	if err != nil {
		return "", "workspace_unavailable"
	}
	work, err := resolvedBackupDirectory(a.cfg.BackupWorkspace)
	if err != nil {
		return "", "workspace_unavailable"
	}
	objects, err := resolvedBackupDirectory(a.cfg.BackupArtifactDir)
	if err != nil || backupPathWithin(data, work) || backupPathWithin(mail, work) || backupPathWithin(data, objects) || backupPathWithin(mail, objects) {
		return "", "workspace_unavailable"
	}
	info, err := os.Stat(work)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", "workspace_unavailable"
	}
	return key.String(), ""
}

func (a *App) backupCaptureConfiguration() (string, error) {
	recipient, reason := a.backupCaptureCapability()
	if reason != "" {
		return "", errBackupExecution
	}
	return recipient, nil
}

func (a *App) handleBackupCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	recipient, reason := a.backupCaptureCapability()
	response := map[string]any{"onlineBackupAvailable": reason == ""}
	_, driveErr := a.backupDriveOAuthConfig()
	response["driveOAuthAvailable"] = driveErr == nil
	if reason != "" {
		response["reason"] = reason
	} else {
		response["recipient"] = recipient
	}
	respondJSON(w, http.StatusOK, response)
}

// Updates only short-lived metadata. All collection, hashing, encryption and
// filesystem publication must happen OUTSIDE this function and DB transactions.
func (a *App) updateBackupCapture(ctx context.Context, lease backupLease, phase string, artifact *catalog.Entry, summary *OnlineBackupSummary) (backupJob, error) {
	for range 5 {
		job, raw, err := a.backupReadJob(ctx, lease.ID)
		if err != nil {
			return job, err
		}
		registry, _, err := a.backupRegistry(ctx)
		if err != nil {
			return job, err
		}
		if job.Status != "running" || job.WorkerToken != lease.Token || registry.Active == nil || registry.Active.ID != lease.ID || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			return job, errBackupConflict
		}
		if job.CancelRequested {
			return job, context.Canceled
		}
		job.Phase = phase
		if artifact != nil {
			job.Artifact = *artifact
		}
		if summary != nil {
			copy := *summary
			job.Summary = &copy
		}
		if err := ctx.Err(); err != nil {
			return job, err
		}
		err = a.backupCAS(ctx, a.db, backupJobPrefix+job.ID, raw, job)
		if !errors.Is(err, errBackupConflict) {
			return job, err
		}
	}
	return backupJob{}, errBackupConflict
}

// Returns only safe stage codes. The prepared private report with paths and
// message IDs is never persisted in task JSON or returned by HTTP.
func (a *App) createBackupArtifact(ctx context.Context, lease backupLease, job backupJob) (backupJob, string) {
	recipient, err := a.backupCaptureConfiguration()
	if err != nil || recipient != job.Recipient {
		return job, "capture_configuration_changed"
	}
	prepare := a.backupPrepareSnapshot
	if prepare == nil {
		prepare = PrepareOnlineBackup
	}
	opts := DefaultOnlineBackupOptions()
	// Reserve transport/container overhead, with a second hard ciphertext cap.
	// Long PAX paths can still exceed the cap; such output is safely rejected.
	opts.MaxBytes = backupMaxBytes - (128 << 20)
	prepared, err := prepare(ctx, a.cfg, a.cfg.BackupWorkspace, opts)
	if err != nil {
		return job, "snapshot_failed"
	}
	defer prepared.Close()
	job, err = a.updateBackupCapture(ctx, lease, "encrypting", nil, &prepared.Report.Summary)
	if err != nil {
		return job, "capture_interrupted"
	}
	file, err := os.CreateTemp(a.cfg.BackupWorkspace, ".lanqin-encrypted-*")
	if err != nil {
		return job, "encryption_failed"
	}
	defer os.Remove(file.Name()) // Only our own EXCL ciphertext temporary file.
	defer file.Close()
	digest := sha256.New()
	bounded := backupCipherWriter{dst: io.MultiWriter(file, digest), remaining: backupMaxBytes}
	_, err = backup.WriteOnline(ctx, prepared.Path, &bounded, recipient, backup.Limits{MaxBytes: opts.MaxBytes, MaxEntries: opts.MaxEntries})
	if err != nil {
		return job, "encryption_failed"
	}
	if err := prepared.Close(); err != nil {
		return job, "snapshot_cleanup_failed"
	}
	if err := file.Sync(); err != nil {
		return job, "encryption_failed"
	}
	if err := file.Close(); err != nil {
		return job, "encryption_failed"
	}
	// Fence before filesystem publication and again before allowing upload. An
	// interruption between publication and attachment may leave a valid orphan
	// ciphertext. Never delete catalog objects based on an uncertain DB outcome.
	job, err = a.updateBackupCapture(ctx, lease, "encrypting", nil, nil)
	if err != nil {
		return job, "capture_interrupted"
	}
	key, err := a.backupMasterKey()
	if err != nil {
		return job, "key_unavailable"
	}
	entry, err := catalog.Publish(ctx, a.cfg.BackupArtifactDir, file.Name(), hex.EncodeToString(digest.Sum(nil)), key, backupMaxBytes)
	if err != nil {
		return job, "artifact_publication_failed"
	}
	job, err = a.updateBackupCapture(ctx, lease, "uploading", &entry, nil)
	if err != nil {
		return job, "capture_interrupted"
	}
	return job, ""
}

type backupCipherWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *backupCipherWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("encrypted backup exceeds artifact size limit")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
