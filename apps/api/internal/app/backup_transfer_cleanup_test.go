package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/storage"
)

func TestBackupCancellationClosesOnlyKnownIncompleteMultipartSessions(t *testing.T) {
	for _, scenario := range []string{"cancel", "cleanup-failed", "shutdown", "changed-destination", "completed-session"} {
		t.Run(scenario, func(t *testing.T) {
			a := newTestApp(t)
			entry, records, _ := backupJobFixture(t, a)
			job, err := a.enqueueBackupUpload(t.Context(), "synthetic-cleanup", "synthetic-cleanup-request", entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			parent, stop := context.WithCancel(t.Context())
			defer stop()
			abortCalls := 0
			a.backupAbortTransfer = func(ctx context.Context, d storage.Destination, state storage.Checkpoint, sha string, size int64, c storage.Credentials) error {
				abortCalls++
				if d.ID != records[0].Config.ID || d.Type != "s3" || state.UploadID != "synthetic-cleanup-session" || sha != entry.SHA256 || size != entry.Bytes || c.SecretAccessKey != "s3-synthetic-secret" {
					t.Fatal("cleanup crossed source or credential boundary")
				}
				if scenario == "cleanup-failed" {
					return errors.New("synthetic-provider-secret-must-not-be-returned")
				}
				return ctx.Err()
			}
			a.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, cfg storage.Config, opts storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
				results := []storage.Result{}
				for _, d := range cfg.Destinations {
					state := storage.Checkpoint{Version: 1, Type: d.Type, SHA256: entry.SHA256, Bytes: entry.Bytes, Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "uploading", UploadID: "synthetic-cleanup-session", Offset: entry.Bytes / 2}
					result := storage.Result{DestinationID: d.ID, State: "cancelled", ErrorCode: "cancelled"}
					if d.Type == "webdav" || scenario == "completed-session" {
						state.Phase = "completed"
						state.Offset = entry.Bytes
						now := a.now().UTC()
						result.State, result.ErrorCode, result.ObjectKey, result.CompletedAt = "succeeded", "", "lanqin-0123456789abcdef0123456789abcdef.age", &now
					}
					if err := opts.Checkpoints.Save(ctx, d.ID, state); err != nil {
						t.Fatal(err)
					}
					results = append(results, result)
				}
				if scenario == "shutdown" {
					stop()
				} else {
					if err := a.cancelBackupUpload(ctx, job.ID); err != nil {
						t.Fatal(err)
					}
					if scenario == "changed-destination" {
						vault, raw, _ := a.loadBackupDestinationState(ctx)
						vault.Items[0].Config.Disabled = true
						if err := a.saveBackupDestinationState(ctx, raw, vault); err != nil {
							t.Fatal(err)
						}
					}
				}
				return storage.Report{Destinations: results}, storage.ErrPartialFailure
			}
			a.runBackupUpload(parent, *lease)
			finished, _, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if scenario == "cancel" || scenario == "cleanup-failed" {
				wantCalls = 1
			}
			if abortCalls != wantCalls {
				t.Fatal("cancel cleanup touched wrong sessions", abortCalls, wantCalls)
			}
			var state storage.Checkpoint
			key := backupTransferPrefix + job.ID + "." + records[0].Config.ID
			if _, err := a.backupRead(t.Context(), key, &state); err != nil {
				t.Fatal(err)
			}
			if scenario == "cancel" && (state.Phase != "aborted" || finished.Transfers[records[0].Config.ID].Phase != "aborted" || state.Offset != 0) {
				t.Fatal("cleanup did not persist closure")
			}
			if scenario == "shutdown" && (state.Phase != "uploading" || finished.Status != "interrupted" || !canResumeBackupUpload(finished)) {
				t.Fatal("shutdown destroyed resumable state")
			}
			if scenario == "cleanup-failed" || scenario == "changed-destination" {
				if finished.Transfers[records[0].Config.ID].CleanupError == "" {
					t.Fatal("cleanup failure was hidden")
				}
			}
			if finished.Transfers[records[1].Config.ID].Phase != "completed" || len(finished.Results) != 2 || finished.Results[1].State != "succeeded" {
				t.Fatal("cancellation discarded successful backup")
			}
			if strings.Contains(jsonEncode(backupJobResponse(finished)), "synthetic-provider-secret") || strings.Contains(jsonEncode(backupJobResponse(finished)), "synthetic-cleanup-session") {
				t.Fatal("cleanup details leaked into report")
			}
		})
	}
}
