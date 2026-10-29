// Package online contains live-source collectors. A database export alone is
// NOT a complete mail backup: Maildir, attachments and keys need coordination.
package online

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// SnapshotSQLite uses its own read-only connection, not the application's
// connection pool. The online backup API copies small page batches, releasing
// source read locks between steps. WAL writers remain enabled. Heavy concurrent
// changes may restart copying; the deadline bounds retries instead of stopping
// mail delivery. The existing source's journal mode is never changed.
//
// destination must be absent, with an existing private parent directory. Only a
// completed, integrity-checked, closed database is published. Callers must still
// collect and validate all referenced files before treating it as a mail backup.
func SnapshotSQLite(ctx context.Context, source, destination string, maxBytes int64) error {
	return snapshotSQLite(ctx, source, destination, maxBytes, nil)
}

func sqliteURI(name, query string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	name = filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: query}
	return u.String(), nil
}

func snapshotSQLite(ctx context.Context, source, destination string, maxBytes int64, afterStep func()) error {
	if maxBytes < 4096 {
		return errors.New("invalid SQLite export size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("SQLite source must be a regular database file")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("SQLite export destination must be absent")
	}
	// A private directory prevents other local users swapping the destination
	// between the driver's open and the final no-replace publication.
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".lanqin-sqlite-*")
	if err != nil {
		return errors.New("cannot create private SQLite export workspace")
	}
	defer os.RemoveAll(tmp) // Only the directory allocated by this call.
	copyPath := filepath.Join(tmp, "snapshot.db")
	f, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return errors.New("cannot create SQLite export")
	}
	if err := f.Close(); err != nil {
		return err
	}
	srcURI, err := sqliteURI(source, "mode=ro&_pragma=busy_timeout(100)&_pragma=trusted_schema(OFF)")
	if err != nil {
		return errors.New("cannot resolve SQLite source")
	}
	db, err := sql.Open("sqlite", srcURI)
	if err != nil {
		return errors.New("cannot open live SQLite source")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return errors.New("cannot connect to live SQLite source")
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		return errors.New("online SQLite export requires an existing WAL source; no source settings were changed")
	}
	var pages, pageSize int64
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return errors.New("cannot inspect SQLite source size")
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil || pageSize < 512 || pageSize > 65536 || pages > maxBytes/pageSize {
		return errors.New("SQLite export exceeds size limit")
	}
	dstURI, err := sqliteURI(copyPath, "mode=rw")
	if err != nil {
		return errors.New("cannot resolve SQLite export")
	}
	err = conn.Raw(func(raw any) (result error) {
		driver, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		copy, err := driver.NewBackup(dstURI)
		if err != nil {
			return errors.New("cannot initialize online SQLite export")
		}
		defer func() {
			if err := copy.Finish(); err != nil && result == nil {
				result = errors.New("cannot finish online SQLite export")
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := copy.Step(64)
			if err != nil {
				var se *sqlite.Error
				if !errors.As(err, &se) || (se.Code()&255 != 5 && se.Code()&255 != 6) {
					return errors.New("online SQLite export failed")
				}
				more = true // SQLITE_BUSY / SQLITE_LOCKED: retry, never stop writers.
			}
			if stat, statErr := os.Stat(copyPath); statErr != nil || stat.Size() > maxBytes {
				return errors.New("SQLite export exceeds size limit")
			}
			if afterStep != nil && err == nil {
				afterStep()
			}
			if !more {
				return nil
			}
			timer := time.NewTimer(2 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
	if err != nil {
		return err
	}
	// Convert only the new destination to DELETE mode and close its connection,
	// so consumers receive a standalone file with no required WAL/SHM sidecars.
	check, err := sql.Open("sqlite", dstURI+"&_pragma=trusted_schema(OFF)")
	if err != nil {
		return errors.New("cannot validate SQLite export")
	}
	check.SetMaxOpenConns(1)
	var integrity string
	err = check.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode)
	if err == nil && mode != "delete" {
		err = errors.New("SQLite export is not standalone")
	}
	if err == nil {
		err = check.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
	}
	closeErr := check.Close()
	if err != nil || closeErr != nil || integrity != "ok" {
		return errors.New("SQLite export failed integrity validation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err = os.OpenFile(copyPath, os.O_RDWR, 0)
	if err != nil {
		return errors.New("cannot flush SQLite export")
	}
	stat, statErr := f.Stat()
	err = f.Sync()
	closeErr = f.Close()
	if statErr != nil || stat.Size() > maxBytes || err != nil || closeErr != nil {
		return errors.New("cannot flush SQLite export within size limit")
	}
	if err := os.Link(copyPath, destination); err != nil {
		return errors.New("cannot publish SQLite export without replacement")
	}
	return nil
}
