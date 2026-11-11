package app

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"lanqin-email-api/internal/backup/storage"
)

type backupRetentionResult struct {
	DestinationID string `json:"destinationId"`
	Deleted       int    `json:"deleted"`
	ErrorCode     string `json:"errorCode,omitempty"`
}

// A pending delete has an uncertain remote outcome after a crash. It is never
// counted toward the protected minimum, nor presented as a known recoverable copy.
type backupPruned struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
	JobID string    `json:"jobId"`
}

type backupRetentionRequest struct {
	Revision string `json:"revision"`
	KeepLast int    `json:"keepLast"`
	Confirm  bool   `json:"confirm"`
}

func (a *App) saveBackupRetention(ctx context.Context, id string, req backupRetentionRequest) error {
	if req.KeepLast < 0 || req.KeepLast > 100 || req.KeepLast > 0 && !req.Confirm {
		return errBackupInvalid
	}
	state, raw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return err
	}
	if req.Revision != backupRevision(raw) {
		return errBackupConflict
	}
	for i := range state.Items {
		if state.Items[i].Config.ID == id {
			state.Items[i].KeepLast = req.KeepLast
			return a.saveBackupDestinationState(ctx, raw, state)
		}
	}
	return errBackupMissing
}

func (a *App) handleSaveBackupRetention(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req backupRetentionRequest
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	if err := a.saveBackupRetention(r.Context(), chi.URLParam(r, "id"), req); err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Both the active lease and changed job are CAS-guarded in one short transaction.
// Filesystem/network work is deliberately outside this transaction.
func (a *App) mutateBackupRetention(ctx context.Context, lease backupLease, id string, change func(*backupJob) error) error {
	for range 5 {
		registry, registryRaw, err := a.backupRegistry(ctx)
		if err != nil {
			return err
		}
		if registry.Active == nil || registry.Active.ID != lease.ID || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			return errBackupConflict
		}
		active, _, err := a.backupReadJob(ctx, lease.ID)
		if err != nil || active.Status != "running" || active.WorkerToken != lease.Token || active.CancelRequested {
			return errBackupConflict
		}
		job, raw, err := a.backupReadJob(ctx, id)
		if err != nil {
			return err
		}
		if err := change(&job); err != nil {
			return err
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry)
		if err == nil {
			err = a.backupCAS(ctx, tx, backupJobPrefix+id, raw, job)
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

type backupRetentionCandidate struct {
	JobID, Object, SHA256 string
	Bytes                 int64
	At                    time.Time
	Pending               bool
}

// Stream authenticated history with a hard bound. If history is incomplete or
// corrupt, do not prune. No cloud listing is used to infer ownership.
func (a *App) backupRetentionCandidates(ctx context.Context, current backupJob, target backupTarget) ([]backupRetentionCandidate, error) {
	column := keyColumnSQL(a.cfg.DBDriver)
	items := []backupRetentionCandidate{}
	seen := map[string]bool{}
	count := 0
	cursor := ""
	for {
		// Release SQLite's single connection before decrypting each bounded page,
		// so history scans cannot starve business writes or our lease heartbeat.
		rows, err := a.db.QueryContext(ctx, `SELECT `+column+`,value FROM system_settings WHERE `+column+` LIKE ? AND `+column+`>? ORDER BY `+column+` LIMIT 100`, backupJobPrefix+"%", cursor)
		if err != nil {
			return nil, err
		}
		type record struct{ key, raw string }
		page := []record{}
		for rows.Next() {
			var record record
			if err := rows.Scan(&record.key, &record.raw); err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, record)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		cursor = page[len(page)-1].key
		for _, record := range page {
			count++
			if count > 10000 {
				return nil, errBackupLimit
			}
			key, raw := record.key, record.raw
			var job backupJob
			if err := a.backupOpen(key, raw, &job); err != nil {
				return nil, err
			}
			if key != backupJobPrefix+job.ID || !validBackupJob(job) {
				return nil, errBackupInvalid
			}
			if job.ID == current.ID || job.Status != "succeeded" && job.Status != "partial" {
				continue
			}
			bound := false
			for _, t := range job.Targets {
				if t.ID == target.ID && t.Fingerprint == target.Fingerprint {
					bound = true
				}
			}
			if !bound {
				continue
			}
			pruned := job.Pruned[target.ID]
			if pruned.State == "deleted" || pruned.State == "missing" {
				continue
			}
			for _, r := range job.Results {
				if r.DestinationID != target.ID || r.State != "succeeded" || r.CompletedAt == nil || r.ObjectKey == "" {
					continue
				}
				if seen[r.ObjectKey] {
					return nil, errBackupInvalid // Ambiguous receipt; never guess ownership.
				}
				seen[r.ObjectKey] = true
				items = append(items, backupRetentionCandidate{JobID: job.ID, Object: r.ObjectKey, SHA256: job.Artifact.SHA256, Bytes: job.Artifact.Bytes, At: *r.CompletedAt, Pending: pruned.State != ""})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].At.Equal(items[j].At) {
			return items[i].JobID > items[j].JobID
		}
		return items[i].At.After(items[j].At)
	})
	return items, nil
}

func (a *App) backupRetentionRecord(ctx context.Context, target backupTarget, keep int) (backupDestinationRecord, error) {
	state, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return backupDestinationRecord{}, err
	}
	for _, record := range state.Items {
		if record.Config.ID == target.ID && !record.Config.Disabled && record.KeepLast >= 1 && record.KeepLast <= 100 && (keep == 0 || record.KeepLast == keep) && backupTargetFingerprint(record) == target.Fingerprint {
			return record, nil
		}
	}
	return backupDestinationRecord{}, errBackupConflict
}

func (a *App) runBackupRetention(ctx context.Context, lease backupLease, job backupJob, results []storage.Result) []backupRetentionResult {
	// A resumed archive may predate newer snapshots completed in the meantime.
	// Complete its transfer, but let a normal NEW backup drive retention; do not
	// treat a late verification timestamp as permission to prune newer data.
	if job.ResumeCount > 0 {
		return nil
	}
	// Default-off policy: no history scan or network readback when not enabled.
	state, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return nil
	}
	enabled := map[string]bool{}
	for _, record := range state.Items {
		enabled[record.Config.ID] = record.KeepLast > 0
	}
	// Successful targets carried across an explicit resume were not newly
	// uploaded in this run; a failure elsewhere must not trigger their cleanup.
	for _, previous := range job.Results {
		if previous.State == "succeeded" {
			enabled[previous.DestinationID] = false
		}
	}
	work := false
	for _, r := range results {
		// A durable ciphertext with an unconfirmed discovery index must not
		// evict the last independently discoverable older backup. Persist ALL
		// ciphertext receipts below, including those whose index failed.
		if r.RecoveryIndexState != "" && r.RecoveryIndexState != "succeeded" {
			enabled[r.DestinationID] = false
		}
		work = work || r.State == "succeeded" && enabled[r.DestinationID]
	}
	if !work {
		return nil
	}
	// Save the new receipt BEFORE any old object can be touched. On restart the
	// existing lease recovery marks this job interrupted; no automatic delete replay.
	if err := a.mutateBackupRetention(ctx, lease, job.ID, func(current *backupJob) error {
		current.Results, current.Phase = results, "retaining"
		return nil
	}); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	summary := []backupRetentionResult{}
	for _, target := range job.Targets {
		for _, result := range results {
			if result.DestinationID != target.ID || result.State != "succeeded" || !enabled[target.ID] {
				continue
			}
			outcome := backupRetentionResult{DestinationID: target.ID}
			outcome.Deleted, outcome.ErrorCode = a.pruneBackupDestination(ctx, lease, job, target, result)
			summary = append(summary, outcome)
		}
	}
	// The caller persists this with the terminal upload result using a fresh
	// bounded context, including when readback/cleanup reached its deadline.
	return summary
}

func (a *App) pruneBackupDestination(ctx context.Context, lease backupLease, job backupJob, target backupTarget, result storage.Result) (int, string) {
	record, err := a.backupRetentionRecord(ctx, target, 0)
	if err != nil {
		return 0, "retention_configuration_changed"
	}
	keep := record.KeepLast
	items, err := a.backupRetentionCandidates(ctx, job, target)
	if err != nil {
		return 0, "retention_history_unavailable"
	}
	// New object is always protected, irrespective of provider clock or ordering.
	protected := 1
	candidates := []backupRetentionCandidate{}
	for _, item := range items {
		if item.Object == result.ObjectKey || result.CompletedAt == nil || !item.At.Before(*result.CompletedAt) {
			continue
		}
		if !item.Pending && protected < keep {
			protected++
			continue
		}
		candidates = append(candidates, item)
	}
	if len(candidates) == 0 {
		return 0, ""
	}
	c := record.Credentials
	credentials := storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Username: c.Username, Password: c.Password}
	if record.Config.Type == "drive" {
		credentials.DriveAccessToken, err = a.backupDriveToken(ctx, target.ID, target.Fingerprint)
		if err != nil {
			return 0, "retention_verification_failed"
		}
	}
	verify, remove := a.backupVerifyRemote, a.backupDeleteRemote
	if verify == nil {
		verify = storage.VerifyRemote
	}
	if remove == nil {
		remove = storage.DeleteVerifiedGuarded
	}
	if err := verify(ctx, record.Config, result.ObjectKey, job.Artifact.SHA256, job.Artifact.Bytes, credentials); err != nil {
		return 0, "retention_verification_failed"
	}
	deleted := 0
	guard := func(ctx context.Context) error {
		if _, err := a.backupRetentionRecord(ctx, target, keep); err != nil {
			return err
		}
		registry, _, err := a.backupRegistry(ctx)
		if err != nil {
			return err
		}
		if registry.Active == nil || registry.Active.ID != lease.ID || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			return errBackupConflict
		}
		current, _, err := a.backupReadJob(ctx, lease.ID)
		if err != nil || current.Status != "running" || current.WorkerToken != lease.Token || current.CancelRequested {
			return errBackupConflict
		}
		return ctx.Err()
	}
	for _, item := range candidates {
		if deleted >= 16 { // Bounded work; the next successful backup continues.
			return deleted, "retention_more_pending"
		}
		if _, err := a.backupRetentionRecord(ctx, target, keep); err != nil || ctx.Err() != nil {
			return deleted, "retention_configuration_changed"
		}
		mark := func(state string) error {
			return a.mutateBackupRetention(ctx, lease, item.JobID, func(old *backupJob) error {
				if old.Pruned == nil {
					old.Pruned = map[string]backupPruned{}
				}
				old.Pruned[target.ID] = backupPruned{State: state, At: a.now().UTC(), JobID: job.ID}
				return nil
			})
		}
		if err := mark("pending"); err != nil {
			return deleted, "retention_history_unavailable"
		}
		if err := remove(ctx, record.Config, item.Object, item.SHA256, item.Bytes, credentials, guard); err != nil {
			// A prior delete may have succeeded before its response/DB update was
			// lost. A missing object is unavailable, not a confirmed new deletion.
			if errors.Is(err, storage.ErrRemoteObjectMissing) {
				if err := mark("missing"); err != nil {
					return deleted, "retention_history_unavailable"
				}
				continue
			}
			return deleted, "retention_delete_failed"
		}
		if err := mark("deleted"); err != nil {
			return deleted, "retention_history_unavailable"
		}
		deleted++
	}
	return deleted, ""
}
