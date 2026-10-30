package app

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
)

func backupSnapshotJobFixture(t *testing.T, a *App) (string, []string, string) {
	t.Helper()
	entry, records, _ := backupJobFixture(t, a)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRecipient = identity.Recipient().String()
	a.cfg.BackupWorkspace = t.TempDir()
	a.cfg.MaildirRoot = t.TempDir()
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return entry.ID, []string{records[0].Config.ID, records[1].Config.ID}, backupRevision(raw)
}

func TestBackupSnapshotJobHTTPAdmissionIdempotencyAndRedaction(t *testing.T) {
	a := newTestApp(t)
	artifactID, ids, revision := backupSnapshotJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	payload := map[string]any{"destinationIds": ids, "requestId": "synthetic-snapshot-request-001", "destinationsRevision": revision, "expectedRecipient": a.cfg.BackupRecipient}
	var created backupJobView
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &created); code != http.StatusAccepted {
		t.Fatalf("create status=%d", code)
	}
	if created.Kind != "snapshot" || created.Phase != "queued" || created.Status != "queued" || created.Artifact.ID != "" || created.Recipient != a.cfg.BackupRecipient {
		t.Fatal("unexpected initial snapshot DTO", created.Kind, created.Phase, created.Status)
	}
	var duplicate backupJobView
	payload["destinationIds"] = []string{ids[1], ids[0]}
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &duplicate); code != http.StatusAccepted || duplicate.ID != created.ID {
		t.Fatal("retry created duplicate snapshot")
	}
	var body map[string]any
	payload["destinationIds"] = []string{ids[0]}
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusConflict {
		t.Fatal("changed request reused idempotency key")
	}
	var actor string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE email=?`, a.cfg.AdminEmail).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if _, err := a.enqueueBackupUpload(t.Context(), actor, "synthetic-snapshot-request-001", artifactID, ids); !errors.Is(err, errBackupConflict) {
		t.Fatal("cross-kind reused idempotency key", err)
	}
	originalRecipient := a.cfg.BackupRecipient
	rotated, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRecipient = rotated.Recipient().String()
	if _, err := a.enqueueBackupSnapshot(t.Context(), actor, "synthetic-snapshot-request-001", ids, revision, a.cfg.BackupRecipient); !errors.Is(err, errBackupConflict) {
		t.Fatal("recipient rotation reused a prior idempotency request", err)
	}
	a.cfg.BackupRecipient = originalRecipient
	job, raw, err := a.backupReadJob(t.Context(), created.ID)
	if err != nil || job.Recipient != a.cfg.BackupRecipient {
		t.Fatal(err)
	}
	if strings.Contains(raw, a.cfg.BackupRecipient) {
		t.Fatal("snapshot row not encrypted")
	}
	encoded := jsonEncode(created)
	for _, secret := range []string{a.cfg.DataDir, a.cfg.BackupWorkspace, "synthetic-secret", "synthetic-password", "workerToken", "fingerprint", "sources"} {
		if strings.Contains(encoded, secret) {
			t.Fatal("snapshot DTO leaked private metadata")
		}
	}
	entries, err := os.ReadDir(a.cfg.BackupWorkspace)
	if err != nil || len(entries) != 0 {
		t.Fatal("HTTP admission performed capture", err)
	}
	if err := a.cancelBackupUpload(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	job, _, err = a.backupReadJob(t.Context(), created.ID)
	if err != nil || job.Status != "cancelled" || job.Phase != "completed" {
		t.Fatal("queued snapshot cancellation invalid", err)
	}
}

func TestBackupSnapshotJobRejectsConfigPathsStaleStateAndKeyChanges(t *testing.T) {
	a := newTestApp(t)
	_, ids, revision := backupSnapshotJobFixture(t, a)
	client := backupLoggedInAdmin(t, a)
	base := func() map[string]any {
		return map[string]any{"destinationIds": ids, "requestId": "synthetic-snapshot-validation", "destinationsRevision": revision, "expectedRecipient": a.cfg.BackupRecipient}
	}
	var body map[string]any
	for _, field := range []string{"workspace", "database", "dataDir", "maildirRoot", "recipient", "artifactId"} {
		payload := base()
		payload[field] = "synthetic-untrusted-path"
		if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusBadRequest {
			t.Fatalf("accepted client field %s: %d", field, code)
		}
	}
	for _, field := range []string{"expectedRecipient", "destinationsRevision"} {
		payload := base()
		delete(payload, field)
		if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusBadRequest {
			t.Fatal("missing binding accepted", field, code)
		}
	}
	payload := base()
	payload["destinationsRevision"] = "0"
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusConflict {
		t.Fatal("stale destination revision accepted", code)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	payload = base()
	payload["expectedRecipient"] = other.Recipient().String()
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusConflict {
		t.Fatal("client redirected encryption recipient", code)
	}
	workspace := a.cfg.BackupWorkspace
	a.cfg.BackupWorkspace = ""
	if code := client.do("POST", "/api/admin/backups/snapshots", base(), &body); code != http.StatusServiceUnavailable {
		t.Fatal("missing server configuration accepted", code)
	}
	a.cfg.BackupWorkspace = workspace
	payload = base()
	payload["destinationIds"] = []string{ids[0], ids[0]}
	if code := client.do("POST", "/api/admin/backups/snapshots", payload, &body); code != http.StatusBadRequest {
		t.Fatal("duplicate target accepted", code)
	}
	registry, _, err := a.backupRegistry(t.Context())
	if err != nil || len(registry.Pending) != 0 {
		t.Fatal("rejected request mutated queue", err)
	}
}

func TestBackupSnapshotJobKindPhaseValidationAndLegacyDTO(t *testing.T) {
	a := newTestApp(t)
	artifactID, ids, revision := backupSnapshotJobFixture(t, a)
	upload, err := a.enqueueBackupUpload(t.Context(), "synthetic-actor", "synthetic-legacy-upload", artifactID, ids)
	if err != nil {
		t.Fatal(err)
	}
	upload.Kind, upload.Phase = "", ""
	view := backupJobResponse(upload)
	if !validBackupJob(upload) || view.Kind != "upload" || view.Phase != "queued" {
		t.Fatal("legacy upload no longer compatible")
	}
	upload.Kind = "unknown"
	if validBackupJob(upload) {
		t.Fatal("unknown kind accepted")
	}
	upload.Kind, upload.Phase = "upload", "collecting"
	if validBackupJob(upload) {
		t.Fatal("upload job accepted a snapshot-only phase")
	}
	job, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-actor", "synthetic-phase-snapshot", ids, revision, a.cfg.BackupRecipient)
	if err != nil {
		t.Fatal(err)
	}
	job.Phase = "bogus"
	if validBackupJob(job) {
		t.Fatal("unknown phase accepted")
	}
	job.Phase = "uploading"
	if validBackupJob(job) {
		t.Fatal("uploading snapshot without artifact accepted")
	}
}
