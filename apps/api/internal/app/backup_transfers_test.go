package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

func assertBackupTransferDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "transfer-contract", "transfer-contract-source", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	store := &backupTransferStore{app: a, lease: *lease}
	checkpoint := storage.Checkpoint{Version: 1, Type: "s3", SHA256: entry.SHA256, Bytes: entry.Bytes, Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "uploading", UploadID: "synthetic-sensitive-multipart-session", Offset: 1, Binding: "synthetic-source-binding"}
	if err := store.Save(t.Context(), records[0].Config.ID, checkpoint); err != nil {
		t.Fatal("checkpoint contract save", err)
	}
	key, _ := store.key(records[0].Config.ID)
	var ciphertext string
	if err := a.db.QueryRow(`SELECT value FROM system_settings WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, key).Scan(&ciphertext); err != nil || strings.Contains(ciphertext, checkpoint.UploadID) {
		t.Fatal("checkpoint was not encrypted", err)
	}
	if err := a.finishBackupUpload(t.Context(), *lease, nil, "upload_failed"); err != nil {
		t.Fatal(err)
	}
	_, vaultRaw, _ := a.loadBackupDestinationState(t.Context())
	resumed, err := a.resumeBackupUpload(t.Context(), job.ID, "synthetic-contract-admin", backupRevision(vaultRaw))
	if err != nil || resumed.Status != "queued" || resumed.ResumeCount != 1 {
		t.Fatal("checkpoint resume contract enqueue", err)
	}
	lease2, err := a.claimBackupUpload(t.Context())
	if err != nil || lease2 == nil || lease2.ID != job.ID {
		t.Fatal(err)
	}
	store2 := &backupTransferStore{app: a, lease: *lease2}
	loaded, err := store2.Load(t.Context(), records[0].Config.ID)
	if err != nil || loaded.UploadID != checkpoint.UploadID || loaded.Offset != 1 {
		t.Fatal("cross-instance checkpoint was lost", err)
	}
	if err := store.Save(t.Context(), records[0].Config.ID, checkpoint); !errors.Is(err, errBackupConflict) {
		t.Fatal("stale worker overwrote checkpoint", err)
	}
	if err := a.finishBackupUpload(t.Context(), *lease2, nil, "synthetic-contract-finish"); err != nil {
		t.Fatal(err)
	}
}

func TestBackupResumeDoesNotPretendToRetryOnlyFailedRecoveryIndexes(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "index-only", true: "mixed-cipher-and-index"}[mixed], func(t *testing.T) {
			a := newTestApp(t)
			entry, records, _ := backupJobFixture(t, a)
			ids := []string{records[0].Config.ID}
			if mixed {
				ids = append(ids, records[1].Config.ID)
			}
			job, err := a.enqueueBackupUpload(t.Context(), "index-resume-actor", "index-resume-request-0001", entry.ID, ids)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			store := &backupTransferStore{app: a, lease: *lease}
			completed := storage.Checkpoint{Version: 1, Type: records[0].Config.Type, SHA256: entry.SHA256, Bytes: entry.Bytes, Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "completed", Offset: entry.Bytes}
			if err := store.Save(t.Context(), records[0].Config.ID, completed); err != nil {
				t.Fatal(err)
			}
			now := a.now().UTC()
			indexedFailure := storage.Result{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &now, ObjectKey: records[0].Config.S3.Prefix + "/" + completed.Name, RecoveryIndexState: "failed", RecoveryIndexErrorCode: "recovery_index_publish_failed"}
			results := []storage.Result{indexedFailure}
			code := "recovery_index_publish_failed"
			if mixed {
				partial := completed
				partial.Type, partial.Phase, partial.Offset = records[1].Config.Type, "uploading", entry.Bytes/2
				if err := store.Save(t.Context(), records[1].Config.ID, partial); err != nil {
					t.Fatal(err)
				}
				results = append(results, storage.Result{DestinationID: records[1].Config.ID, State: "failed", ErrorCode: "network_error"})
				code = "upload_failed"
			}
			if err := a.finishBackupUpload(t.Context(), *lease, results, code); err != nil {
				t.Fatal(err)
			}
			stored, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil || stored.Status != "partial" || backupJobResponse(stored).CanResume != mixed {
				t.Fatal("DTO advertised the wrong continuation capability", err)
			}
			_, vaultRaw, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			client := backupLoggedInAdmin(t, a)
			var response backupJobView
			status := client.do("POST", "/api/admin/backups/jobs/"+job.ID+"/resume", map[string]string{"destinationsRevision": backupRevision(vaultRaw)}, &response)
			if !mixed {
				if status != http.StatusConflict {
					t.Fatal("index-only continuation API falsely accepted work", status)
				}
				if _, err := a.resumeBackupUpload(t.Context(), job.ID, "index-resume-actor", backupRevision(vaultRaw)); !errors.Is(err, errBackupResume) {
					t.Fatal("index-only continuation bypassed API capability check", err)
				}
				unchanged, _, err := a.backupReadJob(t.Context(), job.ID)
				if err != nil || unchanged.ResumeCount != 0 || len(unchanged.Results) != 1 || jsonEncode(unchanged.Results[0]) != jsonEncode(indexedFailure) {
					t.Fatal("rejected index-only continuation changed durable receipt", err)
				}
				return
			}
			if status != http.StatusAccepted || response.ResumeCount != 1 {
				t.Fatal("mixed transfer could not continue failed ciphertext", status)
			}
			resumed, err := a.claimBackupUpload(t.Context())
			if err != nil || resumed == nil {
				t.Fatal(err)
			}
			a.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, cfg storage.Config, opts storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
				if len(cfg.Destinations) != 1 || cfg.Destinations[0].ID != records[1].Config.ID {
					t.Fatal("index failure caused successful ciphertext to upload again")
				}
				checkpoint, err := opts.Checkpoints.Load(ctx, records[1].Config.ID)
				if err != nil || checkpoint.Offset != entry.Bytes/2 {
					t.Fatal("failed ciphertext checkpoint lost", err)
				}
				checkpoint.Offset, checkpoint.Phase = entry.Bytes, "completed"
				if err := opts.Checkpoints.Save(ctx, records[1].Config.ID, *checkpoint); err != nil {
					t.Fatal(err)
				}
				at := a.now().UTC()
				return storage.Report{Destinations: []storage.Result{{DestinationID: records[1].Config.ID, State: "succeeded", ObjectKey: completed.Name, CompletedAt: &at, RecoveryIndexState: "succeeded"}}}, nil
			}
			a.runBackupUpload(t.Context(), *resumed)
			finished, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil || finished.Status != "partial" || finished.ErrorCode != "recovery_index_publish_failed" || len(finished.Results) != 2 || jsonEncode(finished.Results[0]) != jsonEncode(indexedFailure) || finished.Results[1].RecoveryIndexState != "succeeded" || backupJobResponse(finished).CanResume {
				t.Fatal("mixed continuation cleared failed index or claimed it had been repaired", finished.Status, err)
			}
		})
	}
}

func TestBackupTransferSQLiteDatabaseContract(t *testing.T) {
	assertBackupTransferDatabaseContract(t, newTestApp(t))
}

func TestBackupResumeSkipsSuccessfulTargetsAndPreservesNewerSuccess(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	oldTime := a.now().UTC()
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-transfer", "synthetic-transfer-partial", entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	a.backupUploadFile = func(ctx context.Context, _ *os.File, hash string, cfg storage.Config, opts storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
		results := []storage.Result{}
		for _, destination := range cfg.Destinations {
			state := storage.Checkpoint{Version: 1, Type: destination.Type, SHA256: entry.SHA256, Bytes: entry.Bytes, Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "uploading", Offset: entry.Bytes / 2, UploadID: "synthetic-private-session"}
			result := storage.Result{DestinationID: destination.ID, State: "failed", ErrorCode: "network_error"}
			if destination.Type == "webdav" {
				state.Phase = "completed"
				state.Offset = entry.Bytes
				result.State = "succeeded"
				result.ErrorCode = ""
				result.CompletedAt = &oldTime
				result.ObjectKey = "lanqin-0123456789abcdef0123456789abcdef.age"
			}
			if err := opts.Checkpoints.Save(ctx, destination.ID, state); err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
		}
		return storage.Report{SHA256: hash, Bytes: entry.Bytes, Destinations: results}, storage.ErrPartialFailure
	}
	a.runBackupUpload(t.Context(), *lease)
	stored, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "partial" || !backupJobResponse(stored).CanResume {
		t.Fatal("partial transfer did not become resumable", err)
	}
	public := jsonEncode(backupJobResponse(stored))
	if strings.Contains(public, "synthetic-private-session") || strings.Contains(public, "uploadId") || strings.Contains(public, "fingerprint") {
		t.Fatal("private checkpoint appeared in public progress")
	}
	// An uploaded snapshot must resume the existing archive, never capture live
	// mail again. Keep the same task ID and source receipt.
	stored.Kind, stored.Recipient = "snapshot", "synthetic-public-recipient"
	if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+job.ID, raw, stored); err != nil {
		t.Fatal(err)
	}
	newerTime := oldTime.Add(2 * time.Minute)
	a.now = func() time.Time { return newerTime }
	newer, err := a.enqueueBackupUpload(t.Context(), "synthetic-transfer", "synthetic-transfer-newer", entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	newLease, err := a.claimBackupUpload(t.Context())
	if err != nil || newLease == nil {
		t.Fatal(err)
	}
	if err := a.finishBackupUpload(t.Context(), *newLease, []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &newerTime, ObjectKey: "s3/lanqin-11111111111111111111111111111111.age"}, {DestinationID: records[1].Config.ID, State: "succeeded", CompletedAt: &newerTime, ObjectKey: "lanqin-11111111111111111111111111111111.age"}}, ""); err != nil {
		t.Fatal(err)
	}
	vault, vaultRaw, _ := a.loadBackupDestinationState(t.Context())
	vault.Items[1].KeepLast = 1
	vault.Items[0].KeepLast = 1
	if err := a.saveBackupDestinationState(t.Context(), vaultRaw, vault); err != nil {
		t.Fatal(err)
	}
	_, vaultRaw, _ = a.loadBackupDestinationState(t.Context())
	a.now = func() time.Time { return oldTime.Add(3 * time.Minute) }
	resumed, err := a.resumeBackupUpload(t.Context(), job.ID, "synthetic-admin", backupRevision(vaultRaw))
	if err != nil || resumed.ResumeCount != 1 || resumed.ID != job.ID {
		t.Fatal(err)
	}
	duplicate, err := a.resumeBackupUpload(t.Context(), job.ID, "synthetic-admin", backupRevision(vaultRaw))
	if err != nil || duplicate.ResumeCount != 1 {
		t.Fatal("resume request appended twice", err)
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if len(registry.Pending) != 1 {
		t.Fatal("resume duplicated queue work")
	}
	second, err := a.claimBackupUpload(t.Context())
	if err != nil || second == nil {
		t.Fatal(err)
	}
	claimed, _, _ := a.backupReadJob(t.Context(), job.ID)
	if claimed.Phase != "uploading" {
		t.Fatal("resumed snapshot reentered collection")
	}
	restarted := &App{cfg: a.cfg, db: a.db, now: a.now, log: a.log}
	restarted.backupVerifyRemote = func(context.Context, storage.Destination, string, string, int64, storage.Credentials) error {
		t.Fatal("old successful target triggered retention after resume")
		return nil
	}
	restarted.backupPrepareSnapshot = func(context.Context, Config, string, OnlineBackupOptions) (*PreparedOnlineBackup, error) {
		t.Fatal("resume recollected mail")
		return nil, nil
	}
	restarted.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, cfg storage.Config, opts storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
		if len(cfg.Destinations) != 1 || cfg.Destinations[0].ID != records[0].Config.ID {
			t.Fatal("already successful destination uploaded again")
		}
		state, err := opts.Checkpoints.Load(ctx, records[0].Config.ID)
		if err != nil || state.UploadID != "synthetic-private-session" || state.Offset != entry.Bytes/2 {
			t.Fatal("new instance lost checkpoint", err)
		}
		state.Offset, state.Phase = entry.Bytes, "completed"
		if err := opts.Checkpoints.Save(ctx, records[0].Config.ID, *state); err != nil {
			t.Fatal(err)
		}
		now := a.now().UTC()
		return storage.Report{Destinations: []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &now, ObjectKey: "s3/lanqin-22222222222222222222222222222222.age"}}}, nil
	}
	restarted.runBackupUpload(t.Context(), *second)
	finished, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "succeeded" || len(finished.Results) != 2 || finished.ResumeCount != 1 || len(finished.ResumeAudit) != 1 {
		t.Fatal("resume lost final result or audit", err)
	}
	registry, _, _ = a.backupRegistry(t.Context())
	if registry.Success[records[1].Config.ID].JobID != newer.ID || !registry.Success[records[1].Config.ID].At.Equal(newerTime) {
		t.Fatal("resuming older partial task regressed recent success")
	}
	if registry.Success[records[0].Config.ID].JobID != newer.ID || !registry.Success[records[0].Config.ID].At.Equal(newerTime) {
		t.Fatal("late resumed success replaced a newer backup's recent-success record")
	}
}
