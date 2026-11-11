package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

func TestBackupWorkerKeepsCipherReceiptWhenRecoveryIndexFails(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	old := retentionHistoryFixture(t, a, entry, records, 1)
	state, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Items[0].KeepLast = 1
	if err := a.saveBackupDestinationState(t.Context(), raw, state); err != nil {
		t.Fatal(err)
	}
	job, err := a.enqueueBackupUpload(t.Context(), "index-actor", "index-failed-worker-0001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	wantKey, _ := base64.StdEncoding.DecodeString(backupTestKey)
	var receivedKey []byte
	a.backupUploadFile = func(ctx context.Context, _ *os.File, sha string, cfg storage.Config, opts storage.Options, resolve func(string) (storage.Credentials, error)) (storage.Report, error) {
		receivedKey = opts.RecoveryIndexKey
		if !bytes.Equal(receivedKey, wantKey) || opts.RecoveryIndexGuard == nil {
			t.Fatal("App did not provide recovery index authority")
		}
		if err := opts.RecoveryIndexGuard(ctx, records[0].Config.ID); err != nil {
			t.Fatal("current index publication guard rejected", err)
		}
		if err := opts.RecoveryIndexGuard(ctx, "unrelated-target"); err == nil {
			t.Fatal("index guard authorized unrelated destination")
		}
		credentials, err := resolve(records[0].Config.ID)
		if err != nil || credentials.SecretAccessKey != records[0].Credentials.SecretAccessKey {
			t.Fatal("index upload lost per-destination credentials", err)
		}
		now := time.Now().UTC()
		return storage.Report{SHA256: sha, Bytes: entry.Bytes, Destinations: []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", ObjectKey: records[0].Config.S3.Prefix + "/lanqin-0123456789abcdef0123456789abcdef.age", CompletedAt: &now, RecoveryIndexState: "failed", RecoveryIndexErrorCode: "recovery_index_publish_failed"}}}, errors.Join(storage.ErrPartialFailure, storage.ErrRecoveryIndexFailure)
	}
	a.backupVerifyRemote = func(context.Context, storage.Destination, string, string, int64, storage.Credentials) error {
		t.Fatal("unindexed new backup triggered old-backup deletion/readback")
		return nil
	}
	a.runBackupUpload(t.Context(), *lease)
	finished, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "partial" || finished.ErrorCode != "recovery_index_publish_failed" || len(finished.Results) != 1 || finished.Results[0].State != "succeeded" || finished.Results[0].RecoveryIndexState != "failed" || finished.Results[0].ObjectKey == "" {
		t.Fatal("index failure erased ciphertext receipt or claimed fully successful", finished.Status, err)
	}
	if !bytes.Equal(receivedKey, make([]byte, len(receivedKey))) {
		t.Fatal("App retained index key copy after worker completion")
	}
	previous, _, err := a.backupReadJob(t.Context(), old.ID)
	if err != nil || len(previous.Pruned) != 0 {
		t.Fatal("index failure pruned older discoverable backup", err)
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || registry.Success[records[0].Config.ID].JobID != job.ID {
		t.Fatal("durable ciphertext success receipt was lost", err)
	}
	view := jsonEncode(backupJobResponse(finished))
	if !strings.Contains(view, `"index_state":"failed"`) || strings.Contains(view, backupTestKey) {
		t.Fatal("index outcome missing from safe DTO or key leaked")
	}
}

func TestBackupRecoveryIndexGuardChecksLeaseCancellationAndCurrentDestination(t *testing.T) {
	for _, scenario := range []string{"valid", "canceled", "expired", "wrong-lease", "changed-credential", "disabled", "wrong-worker", "context-canceled"} {
		t.Run(scenario, func(t *testing.T) {
			a := newTestApp(t)
			entry, records, _ := backupJobFixture(t, a)
			job, err := a.enqueueBackupUpload(t.Context(), "index-actor", "index-guard-request-0001", entry.ID, []string{records[0].Config.ID})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "canceled":
				if err := a.cancelBackupUpload(ctx, job.ID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				registry, raw, err := a.backupRegistry(ctx)
				if err != nil {
					t.Fatal(err)
				}
				registry.Active.Until = a.now().Add(-time.Second)
				if err := a.backupCAS(ctx, a.db, backupRegistryKey, raw, registry); err != nil {
					t.Fatal(err)
				}
			case "wrong-lease":
				lease.Token += "wrong"
			case "changed-credential", "disabled":
				vault, raw, err := a.loadBackupDestinationState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "disabled" {
					vault.Items[0].Config.Disabled = true
				} else {
					vault.Items[0].Credentials.SecretAccessKey = "new-synthetic-secret"
				}
				if err := a.saveBackupDestinationState(ctx, raw, vault); err != nil {
					t.Fatal(err)
				}
			case "wrong-worker":
				current, raw, err := a.backupReadJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				current.WorkerToken = "wrong"
				if err := a.backupCAS(ctx, a.db, backupJobPrefix+job.ID, raw, current); err != nil {
					t.Fatal(err)
				}
			case "context-canceled":
				cancel()
			}
			err = a.guardBackupRecoveryIndex(ctx, *lease, job.Targets[0])
			if (err == nil) != (scenario == "valid") {
				t.Fatal("index publication fencing decision incorrect", scenario, err)
			}
		})
	}
}
