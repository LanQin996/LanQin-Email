package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

const backupTransferPrefix = "backup.transfer.v1."

var errBackupResume = errors.New("backup_resume_unavailable")

type backupTransferProgress struct {
	Phase        string `json:"phase"`
	Bytes        int64  `json:"bytes"`
	Total        int64  `json:"total"`
	CleanupError string `json:"cleanupError,omitempty"`
}

type backupResumeAudit struct {
	At                time.Time `json:"at"`
	ActorID           string    `json:"actorId"`
	PreviousStatus    string    `json:"previousStatus"`
	PreviousErrorCode string    `json:"previousErrorCode,omitempty"`
}

type backupTransferStore struct {
	app               *App
	lease             backupLease
	allowCancellation bool
}

func (s *backupTransferStore) key(id string) (string, error) {
	if id == "" || len(id) > 64 {
		return "", errBackupInvalid
	}
	for _, c := range id {
		if c != '_' && c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", errBackupInvalid
		}
	}
	return backupTransferPrefix + s.lease.ID + "." + id, nil
}

func (s *backupTransferStore) active(ctx context.Context) (backupRegistry, string, backupJob, string, error) {
	a := s.app
	registry, registryRaw, err := a.backupRegistry(ctx)
	if err != nil {
		return registry, "", backupJob{}, "", err
	}
	if registry.Active == nil || registry.Active.ID != s.lease.ID || registry.Active.Token != s.lease.Token || !registry.Active.Until.After(a.now()) {
		return registry, "", backupJob{}, "", errBackupConflict
	}
	job, jobRaw, err := a.backupReadJob(ctx, s.lease.ID)
	if err != nil || job.Status != "running" || job.WorkerToken != s.lease.Token || job.CancelRequested && !s.allowCancellation || job.Kind == "restore" {
		return registry, "", job, "", errBackupConflict
	}
	return registry, registryRaw, job, jobRaw, nil
}

func (s *backupTransferStore) Load(ctx context.Context, id string) (*storage.Checkpoint, error) {
	key, err := s.key(id)
	if err != nil {
		return nil, err
	}
	_, _, job, _, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, target := range job.Targets {
		found = found || target.ID == id
	}
	if !found {
		return nil, errBackupInvalid
	}
	var checkpoint storage.Checkpoint
	_, err = s.app.backupRead(ctx, key, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		if _, known := job.Transfers[id]; known {
			return nil, errBackupInvalid
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func (s *backupTransferStore) Save(ctx context.Context, id string, checkpoint storage.Checkpoint) error {
	key, err := s.key(id)
	if err != nil {
		return err
	}
	a := s.app
	for range 5 {
		registry, registryRaw, job, jobRaw, err := s.active(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, target := range job.Targets {
			found = found || target.ID == id && target.Config.Type == checkpoint.Type
		}
		if !found || checkpoint.Version != 1 || checkpoint.SHA256 != job.Artifact.SHA256 || checkpoint.Bytes != job.Artifact.Bytes {
			return errBackupInvalid
		}
		var previous storage.Checkpoint
		previousRaw, err := a.backupRead(ctx, key, &previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, known := job.Transfers[id]; known {
				return errBackupInvalid
			}
			previousRaw = ""
		}
		progress := backupTransferProgress{Phase: checkpoint.Phase, Total: checkpoint.Bytes, Bytes: checkpoint.Offset, CleanupError: checkpoint.CleanupError}
		if checkpoint.Phase == "completed" {
			progress.Bytes = checkpoint.Bytes
		}
		if progress.Bytes < 0 || progress.Bytes > progress.Total {
			return errBackupInvalid
		}
		if job.Transfers == nil {
			job.Transfers = map[string]backupTransferProgress{}
		}
		job.Transfers[id] = progress
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry)
		if err == nil {
			err = a.backupCAS(ctx, tx, key, previousRaw, checkpoint)
		}
		if err == nil {
			err = a.backupCAS(ctx, tx, backupJobPrefix+job.ID, jobRaw, job)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if !errors.Is(err, errBackupConflict) {
			return err
		}
	}
	return errBackupConflict
}

func canResumeBackupUpload(job backupJob) bool {
	if job.Kind == "restore" || !catalog.ValidID(job.Artifact.ID) || len(job.Transfers) == 0 || job.CancelRequested || job.ResumeCount >= 10 || job.Status != "failed" && job.Status != "partial" && job.Status != "interrupted" {
		return false
	}
	// Resume only continues failed ciphertext transfers. Completed checkpoints
	// may remain when sidecar publication failed, but successful ciphertext is
	// deliberately skipped on resume; an index-only retry uses publish-index.
	succeeded := map[string]bool{}
	for _, result := range job.Results {
		succeeded[result.DestinationID] = result.State == "succeeded"
	}
	for _, target := range job.Targets {
		if !succeeded[target.ID] {
			return true
		}
	}
	return false
}

func (a *App) resumeBackupUpload(ctx context.Context, id, actor, revision string) (backupJob, error) {
	var empty backupJob
	if revision == "" || actor == "" || len(actor) > 128 {
		return empty, errBackupInvalid
	}
	job, raw, err := a.backupReadJob(ctx, id)
	if err != nil {
		return empty, err
	}
	if job.Kind != "restore" && (job.Status == "queued" || job.Status == "running") && job.ResumeCount > 0 {
		return job, nil
	}
	if !canResumeBackupUpload(job) {
		return empty, errBackupResume
	}
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return empty, err
	}
	if revision != backupRevision(vaultRaw) {
		return empty, errBackupConflict
	}
	succeeded := map[string]bool{}
	for _, result := range job.Results {
		succeeded[result.DestinationID] = result.State == "succeeded"
	}
	for _, target := range job.Targets {
		if succeeded[target.ID] {
			continue
		}
		found := false
		for _, record := range vault.Items {
			if record.Config.ID == target.ID && !record.Config.Disabled && backupTargetFingerprint(record) == target.Fingerprint && (record.Config.Type != "drive" || backupDriveBound(record)) {
				found = true
			}
		}
		if !found {
			return empty, errBackupResume
		}
		if _, known := job.Transfers[target.ID]; known {
			var checkpoint storage.Checkpoint
			_, err := a.backupRead(ctx, backupTransferPrefix+job.ID+"."+target.ID, &checkpoint)
			if err != nil || checkpoint.Version != 1 || checkpoint.Type != target.Config.Type || checkpoint.SHA256 != job.Artifact.SHA256 || checkpoint.Bytes != job.Artifact.Bytes || checkpoint.Phase == "aborted" {
				return empty, errBackupResume
			}
		}
	}
	master, err := a.backupMasterKey()
	if err != nil {
		return empty, err
	}
	file, entry, err := catalog.Open(a.cfg.BackupArtifactDir, job.Artifact.ID, master, backupMaxBytes)
	if err != nil {
		return empty, errBackupExecution
	}
	file.Close()
	if entry.SHA256 != job.Artifact.SHA256 || entry.Bytes != job.Artifact.Bytes {
		return empty, errBackupExecution
	}
	registry, registryRaw, err := a.backupRegistry(ctx)
	if err != nil {
		return empty, err
	}
	if len(registry.Pending) >= 16 {
		return empty, errBackupQueue
	}
	job.ResumeAudit = append(job.ResumeAudit, backupResumeAudit{At: a.now().UTC(), ActorID: actor, PreviousStatus: job.Status, PreviousErrorCode: job.ErrorCode})
	job.ResumeCount++
	job.Status, job.Phase, job.ErrorCode, job.WorkerToken = "queued", "queued", "", ""
	job.StartedAt, job.CompletedAt = nil, nil
	registry.Pending = append(registry.Pending, job.ID)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	for _, guard := range []struct {
		key, raw string
		value    any
	}{{backupRegistryKey, registryRaw, registry}, {backupDestinationSetting, vaultRaw, vault}, {backupJobPrefix + job.ID, raw, job}} {
		if err := a.backupCAS(ctx, tx, guard.key, guard.raw, guard.value); err != nil {
			return empty, err
		}
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	select {
	case a.backupWake <- struct{}{}:
	default:
	}
	return job, nil
}

func (a *App) handleResumeBackupUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		DestinationsRevision string `json:"destinationsRevision"`
	}
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	job, err := a.resumeBackupUpload(r.Context(), chi.URLParam(r, "id"), currentUser(r).ID, req.DestinationsRevision)
	if err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, backupJobResponse(job))
}
