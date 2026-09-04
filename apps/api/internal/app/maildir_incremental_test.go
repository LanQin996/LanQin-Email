package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaildirIncrementalScanSkipsUnchangedDirectories(t *testing.T) {
	a, mb, dir := prepareIncrementalMaildirTest(t)
	ctx := context.Background()
	writeIncrementalMaildirMessage(t, dir, "first", "<incremental-first@example.test>")

	first, err := a.syncMaildirOnceDetailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 1 || first.FilesScanned != 1 || first.DirectoriesScanned == 0 {
		t.Fatalf("first counts=%+v, want one imported and scanned file", first)
	}

	second, err := a.syncMaildirOnceDetailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.DirectoriesChecked == 0 || second.DirectoriesScanned != 0 || second.FilesScanned != 0 {
		t.Fatalf("unchanged counts=%+v, want directory checks without scans", second)
	}
	var messages int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=?`, mb.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("messages=%d, want 1", messages)
	}
}

func TestMaildirIncrementalScanDetectsChangeAndDeletion(t *testing.T) {
	a, mb, dir := prepareIncrementalMaildirTest(t)
	ctx := context.Background()
	firstPath := writeIncrementalMaildirMessage(t, dir, "first", "<incremental-delete@example.test>")
	if _, err := a.syncMaildirOnceDetailed(ctx); err != nil {
		t.Fatal(err)
	}

	secondPath := writeIncrementalMaildirMessage(t, dir, "second", "<incremental-second@example.test>")
	forceDirectoryChange(t, dir)
	changed, err := a.syncMaildirOnceDetailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Imported != 1 || changed.FilesScanned != 2 || changed.DirectoriesScanned == 0 {
		t.Fatalf("changed counts=%+v, want one import from changed directory", changed)
	}

	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	forceDirectoryChange(t, dir)
	if _, err := a.db.ExecContext(ctx, `UPDATE messages SET updated_at=? WHERE mailbox_id=? AND message_id=?`, a.now().UTC().Add(-10*time.Minute).Format(time.RFC3339Nano), mb.ID, "<incremental-delete@example.test>"); err != nil {
		t.Fatal(err)
	}
	deleted, err := a.syncMaildirOnceDetailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Cleaned != 1 || deleted.FilesScanned != 1 {
		t.Fatalf("deleted counts=%+v, want one cleanup and remaining file scan", deleted)
	}
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("remaining maildir file: %v", err)
	}
}

func TestMaildirIncrementalScanPerformsPeriodicFullScan(t *testing.T) {
	a, _, dir := prepareIncrementalMaildirTest(t)
	ctx := context.Background()
	writeIncrementalMaildirMessage(t, dir, "first", "<incremental-full-first@example.test>")
	if _, err := a.syncMaildirOnceDetailed(ctx); err != nil {
		t.Fatal(err)
	}

	secondPath := writeIncrementalMaildirMessage(t, dir, "second", "<incremental-full-second@example.test>")
	signature, err := statMaildirDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a mount that reports an unchanged directory signature.
	a.maildirDirs[filepath.Clean(dir)] = signature
	for a.maildirRuns < maildirFullScanEvery-1 {
		counts, err := a.syncMaildirOnceDetailed(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if counts.FilesScanned != 0 {
			t.Fatalf("pre-reconciliation counts=%+v, want no file scan", counts)
		}
	}
	full, err := a.syncMaildirOnceDetailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if full.Imported != 1 || full.FilesScanned != 2 || full.DirectoriesScanned != full.DirectoriesChecked {
		t.Fatalf("full reconciliation counts=%+v", full)
	}
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("second maildir file: %v", err)
	}
}

func TestTrackedMaildirSyncPublishesOnlyWhenMessagesChange(t *testing.T) {
	a, _, dir := prepareIncrementalMaildirTest(t)
	events, unsubscribe := a.subscribeSyncEvents()
	defer unsubscribe()
	writeIncrementalMaildirMessage(t, dir, "first", "<incremental-event@example.test>")

	counts, err := a.syncMaildirOnceTracked(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Imported != 1 {
		t.Fatalf("counts=%+v, want one imported message", counts)
	}
	select {
	case <-events:
	default:
		t.Fatal("maildir import did not publish a sync event")
	}

	if _, err := a.syncMaildirOnceTracked(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
		t.Fatal("unchanged maildir scan published a sync event")
	default:
	}
}

func prepareIncrementalMaildirTest(t *testing.T) (*App, *Mailbox, string) {
	t.Helper()
	a := newTestApp(t)
	a.cfg.MaildirRoot = t.TempDir()
	_, mb := defaultAdminUserAndMailbox(t, a)
	clearMailboxMessagesForTest(t, a, mb.ID)
	dir := filepath.Join(a.cfg.MaildirRoot, "lanqin.local", "admin", "Maildir", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return a, mb, dir
}

func writeIncrementalMaildirMessage(t *testing.T, dir, name, messageID string) string {
	t.Helper()
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: admin@lanqin.local",
		"Subject: " + name,
		"Message-Id: " + messageID,
		"Date: Sat, 13 Jun 2026 13:00:00 +0000",
		"Content-Type: text/plain; charset=utf-8",
		"",
		name + " body",
	}, "\r\n")
	path := filepath.Join(dir, name+".eml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// maildirDirChangeBase is far enough in the past that no real file operation can
// produce one of the timestamps forceDirectoryChange hands out.
var (
	maildirDirChangeBase = time.Now().Add(-24 * time.Hour)
	maildirDirChangeStep atomic.Int64
)

// forceDirectoryChange gives the directory an mtime that no earlier call produced.
//
// The incremental scan keys off the directory mtime (statMaildirDirectory) and records
// the value it saw. Deriving the new mtime from the current one is not enough: an
// intervening os.Remove resets the mtime to the real clock, so a fixed offset can land
// back on the value the previous scan already recorded — the scan then treats the
// directory as unchanged and skips the deletion the test just made. Stepping a shared
// counter instead makes every call produce a distinct signature.
func forceDirectoryChange(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	changed := maildirDirChangeBase.Add(time.Duration(maildirDirChangeStep.Add(1)) * time.Second)
	if err := os.Chtimes(dir, changed, changed); err != nil {
		t.Fatal(err)
	}
}
