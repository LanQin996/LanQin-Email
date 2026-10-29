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
	results, code := a.executeBackupUpload(ctx, job)
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	<-done
	select {
	case why := <-reason:
		code = why
	default:
	}
	if parent.Err() != nil && code != "" {
		code = "server_shutdown"
	}
	if deadline && code != "" {
		code = "timeout"
	}
	// Persist termination before App.Close closes the pool. Fencing rejects a
	// stale process trying to finish after lease recovery by another instance.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := a.finishBackupUpload(finishCtx, lease, results, code); err != nil && !errors.Is(err, errBackupConflict) {
		a.log.Warn("backup result persistence failed; remote outcome may be uncertain")
	}
}

func (a *App) executeBackupUpload(ctx context.Context, job backupJob) ([]storage.Result, string) {
	key, err := a.backupMasterKey()
	if err != nil {
		return nil, "key_unavailable"
	}
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
	credentials := map[string]storage.Credentials{}
	for _, target := range job.Targets {
		record, ok := records[target.ID]
		if !ok || record.Config.Disabled || backupTargetFingerprint(record) != target.Fingerprint {
			results[target.ID] = storage.Result{DestinationID: target.ID, State: "failed", ErrorCode: "destination_changed"}
			continue
		}
		config := record.Config
		if config.S3 != nil {
			copy := *config.S3
			config.S3 = &copy
			if record.Credentials.SessionToken != "" {
				config.S3.SessionTokenEnv = "BACKUP_TOKEN"
			}
		}
		cfg.Destinations = append(cfg.Destinations, config)
		c := record.Credentials
		credentials[target.ID] = storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Username: c.Username, Password: c.Password}
	}
	code := ""
	if len(cfg.Destinations) > 0 {
		opts := storage.DefaultOptions()
		opts.MaxBytes = backupMaxBytes
		report, err := a.backupUploadFile(ctx, file, entry.SHA256, cfg, opts, func(id string) (storage.Credentials, error) {
			c, ok := credentials[id]
			if !ok {
				return c, errBackupAuth
			}
			return c, nil
		})
		if err != nil {
			code = "upload_failed"
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
		}
		ordered = append(ordered, result)
	}
	return ordered, code
}

func (a *App) finishBackupUpload(ctx context.Context, lease backupLease, results []storage.Result, code string) error {
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
		job.Results = results
		job.ErrorCode = code
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
			if r.State == "succeeded" && r.CompletedAt != nil {
				for _, t := range job.Targets {
					if t.ID == r.DestinationID && current[t.ID] == t.Fingerprint {
						registry.Success[t.ID] = backupSuccess{At: *r.CompletedAt, Fingerprint: t.Fingerprint, JobID: job.ID}
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
