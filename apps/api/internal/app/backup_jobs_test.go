package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

func backupJobFixture(t *testing.T, a *App) (catalog.Entry, []backupDestinationRecord, []byte) {
	t.Helper()
	a.cfg.BackupSecretKey = backupTestKey
	a.cfg.BackupArtifactDir = t.TempDir()
	key, err := a.backupMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("age-encryption.org/v1\nsynthetic job fixture, not a real mailbox")
	file := filepath.Join(t.TempDir(), "fixture.age")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	entry, err := catalog.Publish(t.Context(), a.cfg.BackupArtifactDir, file, hex.EncodeToString(hash[:]), key, backupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	records := []backupDestinationRecord{}
	for _, kind := range []string{"s3", "webdav"} {
		record, err := buildBackupDestination("bkd_"+kind, backupTestRequest(kind, kind, "0"), nil)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBackupDestinationState(t.Context(), raw, backupDestinationState{Version: 1, Items: records}); err != nil {
		t.Fatal(err)
	}
	return entry, records, data
}

func assertBackupUploadDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "contract-actor", "contract-queue-request-001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil || lease.ID != job.ID {
		t.Fatal("cross-database claim failed", err)
	}
	now := a.now().UTC()
	results := []storage.Result{{DestinationID: records[0].Config.ID, State: "succeeded", CompletedAt: &now, ObjectKey: "synthetic/fixture.age"}}
	if err := a.finishBackupUpload(t.Context(), *lease, results, ""); err != nil {
		t.Fatal("cross-database finish failed", err)
	}
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "succeeded" {
		t.Fatal("cross-database job state not persisted", err)
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || registry.Active != nil || registry.Success[records[0].Config.ID].JobID != job.ID {
		t.Fatal("cross-database result registry incorrect", err)
	}
}

func TestBackupUploadSQLiteDatabaseContract(t *testing.T) {
	assertBackupUploadDatabaseContract(t, newTestApp(t))
}

func TestBackupUploadCancelAfterCompletionIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-cancel-terminal", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if err := a.finishBackupUpload(t.Context(), *lease, nil, "upload_failed"); err != nil {
		t.Fatal(err)
	}
	before, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.cancelBackupUpload(t.Context(), job.ID); err != nil {
			t.Fatal("completion race returned a false configuration conflict", err)
		}
	}
	after, updated, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || raw != updated || before.Status != "failed" || after.Status != "failed" || after.CancelRequested {
		t.Fatal("late cancel rewrote a terminal result", err)
	}
}

func TestBackupUploadShutdownPersistsInterruptedWithoutReplay(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-shutdown-001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	a.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, _ storage.Config, _ storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
		close(started)
		<-ctx.Done()
		return storage.Report{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { a.runBackupUpload(ctx, *lease); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown failed to cancel upload")
	}
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "interrupted" || stored.ErrorCode != "server_shutdown" {
		t.Fatal("shutdown lost its terminal state", stored.Status, stored.ErrorCode, err)
	}
	if lease, err := a.claimBackupUpload(t.Context()); err != nil || lease != nil {
		t.Fatal("uncertain remote upload automatically replayed", err)
	}
}

func TestBackupUploadRejectsArtifactModifiedAfterPublication(t *testing.T) {
	a := newTestApp(t)
	entry, records, data := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-tampered-001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1 // Same size and format prefix: the actual hash must catch it.
	if err := os.WriteFile(filepath.Join(a.cfg.BackupArtifactDir, entry.ID+".age"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	a.backupUploadFile = storage.UploadFile // Real preflight, not a mocked success.
	a.runBackupUpload(t.Context(), *lease)
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Status != "failed" || len(stored.Results) != 1 || stored.Results[0].Attempts != 0 || stored.Results[0].ObjectKey != "" {
		t.Fatal("modified bytes reached a remote attempt or became successful", err)
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || len(registry.Success) != 0 {
		t.Fatal("tampered artifact updated last-success state", err)
	}
}

func TestBackupUploadQueuePartialResultsIdempotencyAndRedaction(t *testing.T) {
	a := newTestApp(t)
	entry, records, data := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-request-0001", entry.ID, []string{records[0].Config.ID, records[1].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-request-0001", entry.ID, []string{records[1].Config.ID, records[0].Config.ID})
	if err != nil || duplicate.ID != job.ID {
		t.Fatal("retry created a second job", err)
	}
	if _, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-request-0001", entry.ID, []string{records[0].Config.ID}); !errors.Is(err, errBackupConflict) {
		t.Fatal("idempotency token reused with a different request")
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal("claim failed", err)
	}
	other := &App{cfg: a.cfg, db: a.db, now: a.now}
	if second, err := other.claimBackupUpload(t.Context()); err != nil || second != nil {
		t.Fatal("second instance claimed active work", err)
	}
	a.backupUploadFile = func(ctx context.Context, file *os.File, receipt string, cfg storage.Config, opts storage.Options, resolve func(string) (storage.Credentials, error)) (storage.Report, error) {
		got, err := io.ReadAll(file)
		if err != nil || !bytes.Equal(got, data) || receipt != entry.SHA256 {
			t.Error("wrong pinned artifact")
		}
		if a.db.Stats().InUse != 0 {
			t.Error("upload held a database connection")
		}
		now := a.now().UTC()
		report := storage.Report{SHA256: receipt, Bytes: int64(len(data)), Destinations: []storage.Result{}}
		for _, d := range cfg.Destinations {
			c, err := resolve(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			result := storage.Result{DestinationID: d.ID, State: "failed", Attempts: 1, ErrorCode: "access_denied"}
			if d.Type == "s3" {
				if c.SecretAccessKey != "s3-synthetic-secret" {
					t.Error("S3 credentials crossed targets")
				}
				result.State, result.ErrorCode, result.ObjectKey, result.CompletedAt = "succeeded", "", "s3/fixture.age", &now
			} else if c.Password != "webdav-synthetic-password" {
				t.Error("DAV credentials crossed targets")
			}
			report.Destinations = append(report.Destinations, result)
		}
		return report, storage.ErrPartialFailure
	}
	a.runBackupUpload(t.Context(), *lease)
	finished, raw, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || finished.Status != "partial" || len(finished.Results) != 2 {
		t.Fatalf("partial results lost: %#v %v", finished, err)
	}
	if strings.Contains(raw, "synthetic-secret") || strings.Contains(raw, "example.invalid") {
		t.Fatal("job row not encrypted")
	}
	encoded := jsonEncode(backupJobResponse(finished))
	for _, s := range []string{"synthetic-secret", "synthetic-password", "fingerprint", "workerToken", lease.Token} {
		if strings.Contains(encoded, s) {
			t.Fatal("private job fields leaked")
		}
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || registry.Active != nil || len(registry.Success) != 1 || registry.Success["bkd_s3"].At.IsZero() {
		t.Fatal("incorrect independent last-success state", err)
	}
	client := backupLoggedInAdmin(t, a)
	list := backupList(t, client)
	for _, d := range list.Items {
		if (d.ID == "bkd_s3") != (d.LastSuccessAt != nil) {
			t.Fatal("failed destination received a last-success timestamp")
		}
	}
}

func TestBackupUploadCancelAndMailboxAPIRemainsResponsive(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-running-0001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	a.backupUploadFile = func(ctx context.Context, _ *os.File, _ string, _ storage.Config, _ storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
		close(started)
		<-ctx.Done()
		return storage.Report{}, ctx.Err()
	}
	go func() { defer close(finished); a.runBackupUpload(t.Context(), *lease) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if a.db.Stats().InUse != 0 {
		t.Fatal("slow upload occupied SQLite connection")
	}
	// Real authenticated HTTP calls while the upload is blocked on network.
	client := backupLoggedInAdmin(t, a)
	var body map[string]any
	if code := client.do("GET", "/api/me", nil, &body); code != http.StatusOK {
		t.Fatalf("mail user request blocked: %d", code)
	}
	if code := client.do("POST", "/api/admin/backups/jobs/"+job.ID+"/cancel", nil, &body); code != http.StatusAccepted {
		t.Fatalf("cancel code=%d", code)
	}
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("cancel failed to abort active upload")
	}
	value, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || value.Status != "cancelled" {
		t.Fatal("cancel status not persisted", err)
	}
	queued, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-queued-0002", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.cancelBackupUpload(t.Context(), queued.ID); err != nil {
		t.Fatal(err)
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || len(registry.Pending) != 0 {
		t.Fatal("cancelled queued work remained pending")
	}
}

func TestBackupUploadChangedDestinationAndExpiredLease(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	job, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-changed-0001", entry.ID, []string{records[0].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	state, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Items[0].Config.S3.Endpoint = "https://changed.example.invalid"
	if err := a.saveBackupDestinationState(t.Context(), raw, state); err != nil {
		t.Fatal(err)
	}
	a.backupUploadFile = func(context.Context, *os.File, string, storage.Config, storage.Options, func(string) (storage.Credentials, error)) (storage.Report, error) {
		t.Error("changed destination uploaded with stale credentials")
		return storage.Report{}, errors.New("unexpected")
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	a.runBackupUpload(t.Context(), *lease)
	value, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || value.Status != "failed" || value.Results[0].ErrorCode != "destination_changed" {
		t.Fatal("changed target not fenced")
	}
	job, err = a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-expired-0002", entry.ID, []string{records[1].Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	lease, err = a.claimBackupUpload(t.Context())
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	registry, registryRaw, err := a.backupRegistry(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry.Active.Until = a.now().Add(-time.Second)
	if err := a.backupCAS(t.Context(), a.db, backupRegistryKey, registryRaw, registry); err != nil {
		t.Fatal(err)
	}
	if next, err := a.claimBackupUpload(t.Context()); err != nil || next != nil {
		t.Fatal("uncertain expired task replayed")
	}
	value, _, err = a.backupReadJob(t.Context(), job.ID)
	if err != nil || value.Status != "interrupted" {
		t.Fatal("expired upload not marked uncertain")
	}
	if err := a.finishBackupUpload(t.Context(), *lease, nil, ""); !errors.Is(err, errBackupConflict) {
		t.Fatal("stale worker could overwrite recovered job")
	}
}

func TestBackupUploadHTTPQueueHistoryAndLimits(t *testing.T) {
	a := newTestApp(t)
	entry, records, _ := backupJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	var response backupJobView
	_, vaultRaw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 23 {
		payload := map[string]any{"artifactId": entry.ID, "destinationIds": []string{records[0].Config.ID}, "requestId": fmt.Sprintf("synthetic-page-%04d", i), "destinationsRevision": backupRevision(vaultRaw)}
		if code := client.do("POST", "/api/admin/backups/jobs", payload, &response); code != http.StatusAccepted {
			t.Fatalf("enqueue code=%d", code)
		}
		if err := a.cancelBackupUpload(t.Context(), response.ID); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Items      []backupJobView `json:"items"`
		NextCursor string          `json:"nextCursor"`
	}
	if code := client.do("GET", "/api/admin/backups/jobs", nil, &page); code != http.StatusOK || len(page.Items) != 20 || page.NextCursor == "" {
		t.Fatal("history first page invalid")
	}
	seen := map[string]bool{}
	for _, job := range page.Items {
		seen[job.ID] = true
	}
	if code := client.do("GET", "/api/admin/backups/jobs?cursor="+page.NextCursor, nil, &page); code != http.StatusOK || len(page.Items) != 3 || page.NextCursor != "" {
		t.Fatal("history next page invalid")
	}
	for _, job := range page.Items {
		if seen[job.ID] {
			t.Fatal("pagination repeated a job")
		}
	}
	for i := range 16 {
		if _, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", fmt.Sprintf("synthetic-limit-%04d", i), entry.ID, []string{records[0].Config.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-limit-over", entry.ID, []string{records[0].Config.ID}); !errors.Is(err, errBackupQueue) {
		t.Fatal("queue limit not enforced")
	}
}
