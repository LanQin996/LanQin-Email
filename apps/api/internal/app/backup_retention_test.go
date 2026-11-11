package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

func assertBackupRetentionDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	_, records, _ := backupJobFixture(t, a)
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	req := backupRetentionRequest{Revision: backupRevision(raw), KeepLast: 3, Confirm: true}
	if err := a.saveBackupRetention(t.Context(), records[0].Config.ID, req); err != nil {
		t.Fatal(err)
	}
	state, updated, err := a.loadBackupDestinationState(t.Context())
	if err != nil || state.Items[0].KeepLast != 3 || updated == raw || backupTargetFingerprint(state.Items[0]) != backupTargetFingerprint(records[0]) {
		t.Fatal("retention persistence/revision/fingerprint contract", err)
	}
	if err := a.saveBackupRetention(t.Context(), records[0].Config.ID, req); !errors.Is(err, errBackupConflict) {
		t.Fatal("stale retention settings accepted", err)
	}
	// Ordinary name/credential edits must not accidentally disable retention.
	edit := backupTestRequest("s3", "renamed", backupRevision(updated))
	copy, err := buildBackupDestination(records[0].Config.ID, edit, &state.Items[0])
	if err != nil || copy.KeepLast != 3 {
		t.Fatal("ordinary edit cleared retention", err)
	}
}

func TestBackupRetentionSQLiteDatabaseContract(t *testing.T) {
	assertBackupRetentionDatabaseContract(t, newTestApp(t))
}

func TestBackupRetentionHTTPValidationAndConfirmation(t *testing.T) {
	a := newTestApp(t)
	_, records, _ := backupJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	list := backupList(t, client)
	path := "/api/admin/backups/destinations/" + records[0].Config.ID + "/retention"
	var body map[string]any
	for _, req := range []backupRetentionRequest{
		{Revision: list.Revision, KeepLast: -1, Confirm: true},
		{Revision: list.Revision, KeepLast: 101, Confirm: true},
		{Revision: list.Revision, KeepLast: 3},
	} {
		if code := client.do("POST", path, req, &body); code != http.StatusBadRequest {
			t.Fatal("invalid/destructive unconfirmed config accepted", code)
		}
	}
	if code := client.do("POST", path, backupRetentionRequest{Revision: list.Revision, KeepLast: 2, Confirm: true}, &body); code != http.StatusOK {
		t.Fatal("confirmed setting rejected", code)
	}
	list = backupList(t, client)
	if list.Items[0].KeepLast != 2 || list.Items[1].KeepLast != 0 {
		t.Fatal("retention crossed destinations")
	}
	if code := client.do("POST", path, backupRetentionRequest{Revision: list.Revision}, &body); code != http.StatusOK {
		t.Fatal("disabling retention rejected", code)
	}
}

func retentionHistoryFixture(t *testing.T, a *App, entry catalog.Entry, records []backupDestinationRecord, index int) backupJob {
	t.Helper()
	job, err := a.enqueueBackupUpload(t.Context(), "retention-actor", fmt.Sprintf("retention-history-%04d", index), entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	at := a.now().UTC().Add(time.Duration(index-100) * time.Hour)
	results := []storage.Result{}
	for _, r := range records {
		object := fmt.Sprintf("lanqin-%032x.age", index)
		if r.Config.S3 != nil {
			object = r.Config.S3.Prefix + "/" + object
		}
		results = append(results, storage.Result{DestinationID: r.Config.ID, State: "succeeded", CompletedAt: &at, ObjectKey: object})
	}
	if err := a.finishBackupUpload(t.Context(), *lease, results, ""); err != nil {
		t.Fatal(err)
	}
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestBackupRetentionWorkerProtectsSuccessAndIsolatesFailures(t *testing.T) {
	for _, scenario := range []string{"success", "partial", "upload-failed", "verify-failed", "delete-failed", "missing", "configuration-changed", "changed-during-readback", "cancelled", "lease-lost", "corrupt-history", "off"} {
		t.Run(scenario, func(t *testing.T) {
			a := newTestApp(t)
			entry, records, _ := backupJobFixture(t, a)
			history := []backupJob{}
			for i := 1; i <= 4; i++ {
				history = append(history, retentionHistoryFixture(t, a, entry, records, i))
			}
			state, raw, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "off" {
				for i := range state.Items {
					state.Items[i].KeepLast = 2
				}
				if err := a.saveBackupDestinationState(t.Context(), raw, state); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt-history" {
				if _, err := a.db.Exec(`UPDATE system_settings SET value='corrupt' WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, backupJobPrefix+history[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			job, err := a.enqueueBackupUpload(t.Context(), "retention-actor", "retention-current-0001", entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			a.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, _ storage.Config, _ storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
				now := a.now().UTC()
				report := storage.Report{}
				for _, record := range records {
					r := storage.Result{DestinationID: record.Config.ID, State: "succeeded", ObjectKey: "lanqin-ffffffffffffffffffffffffffffffff.age", CompletedAt: &now}
					if record.Config.S3 != nil {
						r.ObjectKey = record.Config.S3.Prefix + "/" + r.ObjectKey
					}
					if scenario == "upload-failed" || scenario == "partial" && record.Config.Type == "webdav" {
						r.State, r.ObjectKey, r.CompletedAt = "failed", "", nil
					}
					report.Destinations = append(report.Destinations, r)
				}
				return report, nil
			}
			verified, deleted := map[string]bool{}, map[string]int{}
			a.backupVerifyRemote = func(ctx context.Context, d storage.Destination, object, hash string, size int64, c storage.Credentials) error {
				if a.db.Stats().InUse != 0 {
					t.Error("network readback holds DB connection")
				}
				stored, _, err := a.backupReadJob(ctx, job.ID)
				if err != nil || stored.Phase != "retaining" || len(stored.Results) != 2 || hash != entry.SHA256 || size != entry.Bytes {
					t.Fatal("new receipt was not durable before readback", err)
				}
				if scenario == "verify-failed" {
					return errors.New("synthetic failure")
				}
				if scenario == "configuration-changed" {
					vault, raw, _ := a.loadBackupDestinationState(ctx)
					for i := range vault.Items {
						vault.Items[i].KeepLast = 0
					}
					if err := a.saveBackupDestinationState(ctx, raw, vault); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "cancelled" {
					if err := a.cancelBackupUpload(ctx, job.ID); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "lease-lost" {
					registry, raw, _ := a.backupRegistry(ctx)
					registry.Active.Token = "another-worker"
					if err := a.backupCAS(ctx, a.db, backupRegistryKey, raw, registry); err != nil {
						t.Fatal(err)
					}
				}
				verified[d.ID] = true
				return nil
			}
			a.backupDeleteRemote = func(ctx context.Context, d storage.Destination, object, hash string, size int64, c storage.Credentials, guard func(context.Context) error) error {
				if !verified[d.ID] || a.db.Stats().InUse != 0 || strings.Contains(object, "ffffffff") || strings.Contains(object, fmt.Sprintf("%032x", 4)) {
					t.Fatal("deleted protected/newest copy or skipped readback")
				}
				for _, old := range history {
					for _, r := range old.Results {
						if r.DestinationID == d.ID && r.ObjectKey == object {
							stored, _, err := a.backupReadJob(ctx, old.ID)
							if err != nil || stored.Pruned[d.ID].State != "pending" || stored.Pruned[d.ID].JobID != job.ID {
								t.Fatal("delete intent not durable", err)
							}
						}
					}
				}
				if scenario == "delete-failed" {
					return errors.New("synthetic failure")
				}
				if scenario == "changed-during-readback" {
					vault, raw, _ := a.loadBackupDestinationState(ctx)
					for i := range vault.Items {
						vault.Items[i].KeepLast = 0
					}
					if err := a.saveBackupDestinationState(ctx, raw, vault); err != nil {
						t.Fatal(err)
					}
				}
				if err := guard(ctx); err != nil {
					return err
				}
				if scenario == "missing" {
					return storage.ErrRemoteObjectMissing
				}
				deleted[d.ID]++
				return nil
			}
			a.runBackupUpload(t.Context(), *lease)
			if scenario == "success" || scenario == "partial" {
				if deleted[records[0].Config.ID] != 3 || scenario == "success" && deleted[records[1].Config.ID] != 3 || scenario == "partial" && deleted[records[1].Config.ID] != 0 {
					t.Fatal("incorrect per-destination retention", deleted)
				}
			} else if len(deleted) != 0 {
				t.Fatal("failed or disabled flow deleted backups", deleted)
			}
			stored, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "verify-failed" || scenario == "delete-failed" || scenario == "corrupt-history" {
				if stored.Status != "succeeded" || len(stored.Retention) != 2 || stored.Retention[0].ErrorCode == "" {
					t.Fatal("retention failure hid upload success or warning", stored.Status, stored.Retention)
				}
			}
			if scenario == "success" {
				for _, old := range history[:3] {
					stored, _, _ := a.backupReadJob(t.Context(), old.ID)
					if stored.Pruned[records[0].Config.ID].State != "deleted" {
						t.Fatal("deleted receipt still advertised as available")
					}
				}
			}
			if scenario == "delete-failed" || scenario == "missing" {
				old, _, err := a.backupReadJob(t.Context(), history[2].ID)
				want := "pending"
				if scenario == "missing" {
					want = "missing"
				}
				if err != nil || old.Pruned[records[0].Config.ID].State != want {
					t.Fatal("uncertain/missing copy is still advertised as recoverable", old.Pruned, err)
				}
			}
		})
	}
}
