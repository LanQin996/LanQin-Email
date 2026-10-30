package app

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var errBackupExportRuntime = errors.New("cannot safely exclude runtime backup jobs from export")

// sanitizeBackupExportRuntime operates ONLY on the private, standalone SQLite
// copy just produced by SnapshotSQLite, never on App.db or a live DB path.
// Backup queue/lease/job-history rows are execution state, not portable work:
// replaying a restored Pending queue could upload data or recursively capture
// another snapshot. Preserve the destination vault and all unrelated settings.
// Mail delivery and scheduled-send state require separate restore activation
// handling; this helper deliberately does not modify those business records.
func sanitizeBackupExportRuntime(ctx context.Context, copyPath string) (omittedJobs int, result error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Lstat(copyPath)
	if err != nil || !info.Mode().IsRegular() {
		return 0, errBackupExportRuntime
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(copyPath + suffix); !errors.Is(err, fs.ErrNotExist) {
			return 0, errBackupExportRuntime
		}
	}
	abs, err := filepath.Abs(copyPath)
	if err != nil {
		return 0, errBackupExportRuntime
	}
	name := filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=rw&_pragma=trusted_schema(OFF)&_pragma=busy_timeout(100)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, errBackupExportRuntime
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil && result == nil {
			omittedJobs, result = 0, errBackupExportRuntime
		}
		if result != nil {
			return
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(copyPath + suffix); !errors.Is(err, fs.ErrNotExist) {
				omittedJobs, result = 0, errBackupExportRuntime
				return
			}
		}
	}()
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
		return 0, errBackupExportRuntime // Never change the source journal mode.
	}
	var kind string
	err = db.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name='system_settings'`).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // Minimal supported fixtures have no runtime settings.
	}
	if err != nil || kind != "table" {
		return 0, errBackupExportRuntime
	}
	// The application has no triggers on this table. Do not execute arbitrary
	// imported trigger code while changing what should be an isolated copy.
	var triggers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND tbl_name='system_settings'`).Scan(&triggers); err != nil || triggers != 0 {
		return 0, errBackupExportRuntime
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errBackupExportRuntime
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_settings WHERE substr("key",1,?)=?`, len(backupJobPrefix), backupJobPrefix).Scan(&omittedJobs); err != nil {
		return 0, errBackupExportRuntime
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM system_settings WHERE "key"=? OR "key"=? OR substr("key",1,?)=? OR substr("key",1,?)=?`, backupRegistryKey, backupSchedulesKey, len(backupJobPrefix), backupJobPrefix, len(backupDriveOAuthPrefix), backupDriveOAuthPrefix); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errBackupExportRuntime
	}
	if err := tx.Commit(); err != nil {
		return 0, errBackupExportRuntime
	}
	return omittedJobs, nil
}
