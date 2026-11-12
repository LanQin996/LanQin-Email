package app

import (
	"context"
	"errors"
	"time"

	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

func (a *App) backupUploadWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case execution := <-a.backupRestoreRequests:
			a.runBackupRestore(ctx, execution)
			continue
		default:
		}
		lease, err := a.claimBackupUpload(ctx)
		if err != nil && !errors.Is(err, errBackupConflict) && ctx.Err() == nil {
			a.log.Warn("backup queue unavailable")
			// A corrupted/unavailable queue must not produce a log storm or a
			// tight retry loop competing with normal mail database operations.
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		if lease != nil {
			a.runBackupUpload(ctx, *lease)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.backupWake:
		case execution := <-a.backupRestoreRequests:
			a.runBackupRestore(ctx, execution)
		}
	}
}

func (a *App) monitorBackupUpload(ctx context.Context, lease backupLease, cancel context.CancelFunc, reason chan<- string, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	nextRenew := a.now().Add(5 * time.Second)
	fail := func(code string) {
		select {
		case reason <- code:
		default:
		}
		cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		job, _, err := a.backupReadJob(ctx, lease.ID)
		if err != nil {
			if ctx.Err() == nil {
				fail("heartbeat_failed")
			}
			return
		}
		if job.Status != "running" || job.WorkerToken != lease.Token {
			fail("lease_lost")
			return
		}
		if job.CancelRequested {
			fail("user_cancelled")
			return
		}
		if a.now().Before(nextRenew) {
			continue
		}
		registry, raw, err := a.backupRegistry(ctx)
		if err != nil || registry.Active == nil || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			if ctx.Err() == nil {
				fail("lease_lost")
			}
			return
		}
		registry.Active.Until = a.now().Add(backupLeaseTTL)
		if err := a.backupCAS(ctx, a.db, backupRegistryKey, raw, registry); err != nil {
			if errors.Is(err, errBackupConflict) {
				continue
			}
			if ctx.Err() == nil {
				fail("heartbeat_failed")
			}
			return
		}
		nextRenew = a.now().Add(5 * time.Second)
	}
}

func (a *App) runBackupUpload(parent context.Context, lease backupLease) {
	job, _, err := a.backupReadJob(parent, lease.ID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	reason := make(chan string, 1)
	done := make(chan struct{})
	go a.monitorBackupUpload(ctx, lease, cancel, reason, done)
	var results []storage.Result
	var retention []backupRetentionResult
	code := ""
	if backupJobKind(job) == "discover" {
		results, code = a.executeBackupDiscovery(ctx, lease, job)
	}
	if backupJobKind(job) == "snapshot" && job.Artifact.ID == "" {
		job, code = a.createBackupArtifact(ctx, lease, job)
	}
	if code == "" && backupJobKind(job) != "discover" {
		results, code = a.executeBackupUpload(ctx, job, lease)
	}
	// Retention failures are reported separately; they must not turn a durable
	// successful upload into a failed upload or clear its trusted receipt.
	if ctx.Err() == nil && len(results) > 0 && backupJobKind(job) != "discover" {
		retention = a.runBackupRetention(ctx, lease, job, results)
	}
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	<-done
	select {
	case why := <-reason:
		code = why
	default:
	}
	if parent.Err() != nil && (code != "" || backupJobKind(job) == "discover") {
		code = "server_shutdown"
	}
	if deadline && (code != "" || backupJobKind(job) == "discover") {
		code = "timeout"
	}
	if parent.Err() == nil {
		current, _, err := a.backupReadJob(parent, lease.ID)
		if err == nil && current.CancelRequested && backupJobKind(current) != "discover" {
			a.cleanupCancelledTransfers(parent, lease, current)
		}
	}
	// Persist termination before App.Close closes the pool. Fencing rejects a
	// stale process trying to finish after lease recovery by another instance.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := a.finishBackupUpload(finishCtx, lease, results, code, retention); err != nil && !errors.Is(err, errBackupConflict) {
		a.log.Warn("backup result persistence failed; remote outcome may be uncertain")
	}
}

func (a *App) executeBackupUpload(ctx context.Context, job backupJob, leases ...backupLease) ([]storage.Result, string) {
	key, err := a.backupMasterKey()
	if err != nil {
		return nil, "key_unavailable"
	}
	defer clear(key)
	file, entry, err := catalog.Open(a.cfg.BackupArtifactDir, job.Artifact.ID, key, backupMaxBytes)
	if err != nil || entry.SHA256 != job.Artifact.SHA256 || entry.Bytes != job.Artifact.Bytes {
		if file != nil {
			file.Close()
		}
		return nil, "artifact_unavailable"
	}
	defer file.Close()
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return nil, "credentials_unavailable"
	}
	records := map[string]backupDestinationRecord{}
	for _, record := range vault.Items {
		records[record.Config.ID] = record
	}
	cfg := storage.Config{Version: 1, Destinations: []storage.Destination{}}
	results := map[string]storage.Result{}
	for _, result := range job.Results {
		if result.State == "succeeded" {
			results[result.DestinationID] = result
		}
	}
	credentials := map[string]storage.Credentials{}
	for _, target := range job.Targets {
		if previous, ok := results[target.ID]; ok && previous.State == "succeeded" {
			continue
		}
		record, ok := records[target.ID]
		if !ok || record.Config.Disabled || backupTargetFingerprint(record) != target.Fingerprint {
			results[target.ID] = storage.Result{DestinationID: target.ID, State: "failed", ErrorCode: "destination_changed"}
			continue
		}
		config := record.Config
		driveToken := ""
		if config.Type == "drive" {
			var tokenErr error
			driveToken, tokenErr = a.backupDriveToken(ctx, target.ID, target.Fingerprint)
			if tokenErr != nil {
				results[target.ID] = storage.Result{DestinationID: target.ID, State: "failed", ErrorCode: "reauthorization_required"}
				continue
			}
		}
		if config.S3 != nil {
			copy := *config.S3
			config.S3 = &copy
			if record.Credentials.SessionToken != "" {
				config.S3.SessionTokenEnv = "BACKUP_TOKEN"
			}
		}
		cfg.Destinations = append(cfg.Destinations, config)
		c := record.Credentials
		credentials[target.ID] = storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Username: c.Username, Password: c.Password, DriveAccessToken: driveToken}
	}
	code := ""
	if len(cfg.Destinations) > 0 {
		opts := storage.DefaultOptions()
		opts.MaxBytes = backupMaxBytes
		opts.RecoveryIndexKey = key
		opts.RecoveryIndexGuard = func(ctx context.Context, id string) error {
			if len(leases) != 1 {
				return errBackupConflict
			}
			for _, target := range job.Targets {
				if target.ID == id {
					return a.guardBackupRecoveryIndex(ctx, leases[0], target)
				}
			}
			return errBackupConflict
		}
		if len(leases) > 0 {
			opts.Checkpoints = &backupTransferStore{app: a, lease: leases[0]}
		}
		report, err := a.backupUploadFile(ctx, file, entry.SHA256, cfg, opts, func(id string) (storage.Credentials, error) {
			c, ok := credentials[id]
			if !ok {
				return c, errBackupAuth
			}
			return c, nil
		})
		if err != nil {
			code = "upload_failed"
			if errors.Is(err, storage.ErrRecoveryIndexFailure) {
				code = "recovery_index_publish_failed"
			}
		}
		for _, result := range report.Destinations {
			results[result.DestinationID] = result
		}
	}
	ordered := []storage.Result{}
	for _, target := range job.Targets {
		result, ok := results[target.ID]
		if !ok {
			result = storage.Result{DestinationID: target.ID, State: "failed", ErrorCode: "upload_failed"}
		}
		if result.State != "succeeded" {
			code = "upload_failed"
		} else if result.RecoveryIndexState != "" && result.RecoveryIndexState != "succeeded" && code == "" {
			code = "recovery_index_publish_failed"
		}
		ordered = append(ordered, result)
	}
	return ordered, code
}

func (a *App) guardBackupRecoveryIndex(ctx context.Context, lease backupLease, target backupTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return err
	}
	matched := false
	for _, record := range vault.Items {
		if record.Config.ID == target.ID && !record.Config.Disabled && backupTargetFingerprint(record) == target.Fingerprint {
			matched = true
			break
		}
	}
	if !matched {
		return errBackupConflict
	}
	registry, _, err := a.backupRegistry(ctx)
	if err != nil || registry.Active == nil || registry.Active.ID != lease.ID || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
		return errBackupConflict
	}
	job, _, err := a.backupReadJob(ctx, lease.ID)
	if err != nil || job.Status != "running" || job.WorkerToken != lease.Token || job.CancelRequested {
		return errBackupConflict
	}
	return ctx.Err()
}

func (a *App) finishBackupUpload(ctx context.Context, lease backupLease, results []storage.Result, code string, retention ...[]backupRetentionResult) error {
	for range 5 {
		job, raw, err := a.backupReadJob(ctx, lease.ID)
		if err != nil {
			return err
		}
		registry, registryRaw, err := a.backupRegistry(ctx)
		if err != nil {
			return err
		}
		if job.Status != "running" || job.WorkerToken != lease.Token || registry.Active == nil || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			return errBackupConflict
		}
		now := a.now().UTC()
		job.CompletedAt = &now
		job.Phase = "completed"
		job.Results = results
		if len(retention) > 0 {
			job.Retention = retention[0]
		}
		job.ErrorCode = code
		if job.Restore != nil && (code != "" || job.CancelRequested) {
			job.Restore.Ready = false
		}
		if job.Discovery != nil && (code != "" || job.CancelRequested) {
			job.Discovery.Ready = false
		}
		if job.Results == nil {
			job.Results = []storage.Result{}
		}
		job.Status = "failed"
		succeeded := 0
		for _, r := range results {
			if r.State == "succeeded" {
				succeeded++
			}
		}
		if succeeded == len(job.Targets) && code == "" {
			job.Status = "succeeded"
		} else if succeeded > 0 {
			job.Status = "partial"
		}
		if job.CancelRequested || code == "user_cancelled" {
			job.Status = "cancelled"
			job.ErrorCode = "user_cancelled"
		}
		if code == "server_shutdown" || code == "lease_lost" || code == "heartbeat_failed" {
			job.Status = "interrupted"
		}
		job.WorkerToken = ""
		// Last-success applies only to the same destination configuration. A
		// renamed/disabled target retains history; a new endpoint/key does not.
		vault, _, err := a.loadBackupDestinationState(ctx)
		if err != nil {
			return err
		}
		current := map[string]string{}
		for _, r := range vault.Items {
			current[r.Config.ID] = backupTargetFingerprint(r)
		}
		for id := range registry.Success {
			if _, ok := current[id]; !ok {
				delete(registry.Success, id)
			}
		}
		for _, r := range results {
			if backupJobKind(job) != "restore" && backupJobKind(job) != "discover" && r.State == "succeeded" && r.CompletedAt != nil {
				for _, t := range job.Targets {
					if t.ID == r.DestinationID && current[t.ID] == t.Fingerprint {
						previous := registry.Success[t.ID]
						if job.ResumeCount > 0 && previous.Fingerprint == t.Fingerprint && previous.JobID != "" && previous.JobID != job.ID {
							created := previous.CreatedAt
							if created.IsZero() {
								previousJob, _, err := a.backupReadJob(ctx, previous.JobID)
								if err != nil {
									continue
								}
								created = previousJob.CreatedAt
							}
							if created.After(job.CreatedAt) {
								continue
							}
						}
						if previous.Fingerprint != t.Fingerprint || !previous.At.After(*r.CompletedAt) {
							registry.Success[t.ID] = backupSuccess{At: *r.CompletedAt, CreatedAt: job.CreatedAt, Fingerprint: t.Fingerprint, JobID: job.ID}
						}
					}
				}
			}
		}
		registry.Active = nil
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry)
		if err == nil {
			err = a.backupCAS(ctx, tx, backupJobPrefix+job.ID, raw, job)
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
