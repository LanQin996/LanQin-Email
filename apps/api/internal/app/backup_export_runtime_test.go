package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/online"
)

func exportRuntimeFixture(t *testing.T, schema string) (*sql.DB, string, string) {
	t.Helper()
	livePath := filepath.Join(t.TempDir(), "live.db")
	live, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	if _, err := live.Exec("PRAGMA journal_mode=WAL;" + schema); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := online.SnapshotSQLite(t.Context(), livePath, copyPath, 1<<20); err != nil {
		t.Fatal(err)
	}
	return live, livePath, copyPath
}

func TestBackupExportRuntimeExcludesOnlyBackupExecutionStateFromCopy(t *testing.T) {
	schema := `CREATE TABLE system_settings("key" TEXT PRIMARY KEY,value TEXT,updated_at TEXT);
		CREATE TABLE send_queue(id TEXT,status TEXT);
		CREATE TABLE scheduled_sends(id TEXT,status TEXT);
		INSERT INTO send_queue VALUES('synthetic-send','pending');
		INSERT INTO scheduled_sends VALUES('synthetic-scheduled','pending');
		INSERT INTO system_settings VALUES
		('backup.queue.v1','opaque-encrypted-active-A-pending-B','synthetic'),
		('backup.job.v1.bkj_active','opaque-encrypted-running-A','synthetic'),
		('backup.job.v1.bkj_pending','opaque-encrypted-queued-B','synthetic'),
		('backup.job.v1.bkj_complete','opaque-encrypted-history','synthetic'),
		('backup.schedules.v1','opaque-encrypted-enabled-plan','synthetic'),
		('backup.drive.oauth.v1.actor','opaque-encrypted-verifier','synthetic'),
		('backup.drive.oauth.v10.keep','unrelated-oauth-version','synthetic'),
		('backup.destinations.v1','opaque-encrypted-vault','synthetic'),
		('backup.job.v10.keep','unrelated-prefix','synthetic'),
		('site.title','Synthetic site','synthetic');`
	live, livePath, copyPath := exportRuntimeFixture(t, schema)
	jobs, err := sanitizeBackupExportRuntime(t.Context(), copyPath)
	if err != nil || jobs != 3 {
		t.Fatal("runtime history/queue not excluded", jobs, err)
	}
	copy, err := openFrozenBackupDB(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	// Exercise the real claim path against the recovered copy. The missing
	// registry yields an empty queue without needing to decrypt stale job rows.
	restored := &App{db: copy, cfg: Config{DBDriver: databaseDriverSQLite}, now: time.Now}
	if lease, err := restored.claimBackupUpload(t.Context()); err != nil || lease != nil {
		t.Fatal("recovered copy could claim a captured backup job", lease, err)
	}
	var count int
	if err := copy.QueryRow(`SELECT count(*) FROM system_settings WHERE "key"='backup.queue.v1' OR "key"='backup.schedules.v1' OR substr("key",1,?)=? OR substr("key",1,?)=?`, len(backupJobPrefix), backupJobPrefix, len(backupDriveOAuthPrefix), backupDriveOAuthPrefix).Scan(&count); err != nil || count != 0 {
		t.Fatal("restored copy still has executable backup queue state", count, err)
	}
	for key, expected := range map[string]string{"backup.destinations.v1": "opaque-encrypted-vault", "backup.job.v10.keep": "unrelated-prefix", "backup.drive.oauth.v10.keep": "unrelated-oauth-version", "site.title": "Synthetic site"} {
		var value string
		if err := copy.QueryRow(`SELECT value FROM system_settings WHERE "key"=?`, key).Scan(&value); err != nil || value != expected {
			t.Fatal("unrelated setting changed", key, err)
		}
	}
	for _, table := range []string{"send_queue", "scheduled_sends"} {
		if err := copy.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE status='pending'`).Scan(&count); err != nil || count != 1 {
			t.Fatal("business delivery state changed", table, err)
		}
	}
	if err := live.QueryRow(`SELECT count(*) FROM system_settings`).Scan(&count); err != nil || count != 10 {
		t.Fatal("source settings mutated", count, err)
	}
	var journal string
	if err := live.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatal("source journal changed", err)
	}
	if _, err := sanitizeBackupExportRuntime(t.Context(), livePath); !errors.Is(err, errBackupExportRuntime) {
		t.Fatal("active WAL source accepted", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(copyPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("copy not closed/standalone", suffix)
		}
	}
}

func TestBackupExportRuntimeMinimalCopyAndUnexpectedTriggers(t *testing.T) {
	_, _, minimal := exportRuntimeFixture(t, `CREATE TABLE synthetic(id INTEGER); INSERT INTO synthetic VALUES(1);`)
	if count, err := sanitizeBackupExportRuntime(t.Context(), minimal); err != nil || count != 0 {
		t.Fatal("minimal fixture rejected", err)
	}
	_, _, triggered := exportRuntimeFixture(t, `CREATE TABLE system_settings("key" TEXT PRIMARY KEY,value TEXT,updated_at TEXT);
		CREATE TABLE synthetic(id INTEGER);
		INSERT INTO synthetic VALUES(1);
		INSERT INTO system_settings VALUES('backup.queue.v1','opaque','synthetic');
		CREATE TRIGGER unexpected_runtime_delete AFTER DELETE ON system_settings BEGIN DELETE FROM synthetic; END;`)
	if _, err := sanitizeBackupExportRuntime(t.Context(), triggered); !errors.Is(err, errBackupExportRuntime) {
		t.Fatal("unexpected trigger accepted", err)
	}
	db, err := openFrozenBackupDB(triggered)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM synthetic`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unexpected trigger executed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sanitizeBackupExportRuntime(ctx, minimal); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}
