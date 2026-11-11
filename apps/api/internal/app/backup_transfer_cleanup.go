package app

import (
	"context"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

// An explicit user cancellation closes known incomplete S3 multipart sessions.
// Shutdown/lease loss preserves resumable sessions. The original job still owns
// its lease; this short cleanup never deletes a completed backup object.
func (a *App) cleanupCancelledTransfers(parent context.Context, lease backupLease, job backupJob) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	store := &backupTransferStore{app: a, lease: lease, allowCancellation: true}
	vault, _, vaultErr := a.loadBackupDestinationState(ctx)
	abort := a.backupAbortTransfer
	if abort == nil {
		abort = storage.AbortCheckpoint
	}
	for _, target := range job.Targets {
		if ctx.Err() != nil {
			return
		}
		if target.Config.Type != "s3" {
			continue
		}
		state, err := store.Load(ctx, target.ID)
		if err != nil || state == nil || state.Phase == "completed" || state.Phase == "aborted" {
			continue
		}
		state.CleanupError = "multipart_cleanup_failed"
		var record *backupDestinationRecord
		if vaultErr == nil {
			for _, item := range vault.Items {
				if item.Config.ID == target.ID && !item.Config.Disabled && backupTargetFingerprint(item) == target.Fingerprint {
					copy := item
					record = &copy
				}
			}
		}
		if record != nil {
			config := record.Config
			if record.Credentials.SessionToken != "" {
				copy := *config.S3
				copy.SessionTokenEnv = "BACKUP_TOKEN"
				config.S3 = &copy
			}
			c := record.Credentials
			credentials := storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken}
			if err := abort(ctx, config, *state, job.Artifact.SHA256, job.Artifact.Bytes, credentials); err == nil {
				state.Phase, state.Offset, state.Parts, state.CleanupError = "aborted", 0, nil, ""
			}
		}
		if err := store.Save(ctx, target.ID, *state); err != nil {
			a.log.Warn("backup multipart cleanup status could not be persisted")
			return
		}
	}
}
