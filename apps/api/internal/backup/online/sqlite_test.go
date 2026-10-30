package online

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveSQLite(t *testing.T) (*sql.DB, string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "live.db")
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE state(id INTEGER PRIMARY KEY, generation INTEGER, payload BLOB); INSERT INTO state VALUES(1,0,zeroblob(1048576)),(2,0,zeroblob(1048576));`); err != nil {
		t.Fatal(err)
	}
	return db, name
}

func TestSQLiteOnlineSnapshotAllowsConcurrentAtomicWriter(t *testing.T) {
	db, source := liveSQLite(t)
	// Occupy the application's only pool slot for the entire export. Export must
	// not borrow it, checkpoint the source, or require closing this connection.
	writer, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	destination := filepath.Join(t.TempDir(), "snapshot.db")
	writes := 0
	err = snapshotSQLite(t.Context(), source, destination, 8<<20, func() {
		if writes == 3 {
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		tx, err := writer.BeginTx(ctx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := tx.ExecContext(ctx, "UPDATE state SET generation=generation+1"); err != nil {
			tx.Rollback()
			t.Error("mail writer blocked by export", err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
			return
		}
		writes++
	})
	if err != nil || writes != 3 {
		t.Fatalf("online export=%v concurrent writes=%d", err, writes)
	}
	uri, _ := sqliteURI(destination, "mode=ro&immutable=1")
	copy, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var lo, hi int
	if err := copy.QueryRow("SELECT min(generation),max(generation) FROM state").Scan(&lo, &hi); err != nil || lo != hi || lo != 0 {
		t.Fatalf("export tore an atomic transaction: %d %d %v", lo, hi, err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("export depends on sidecar", suffix)
		}
	}
	var mode string
	if err := writer.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("export changed source journal mode", mode, err)
	}
}

func TestSQLiteOnlineSnapshotDoesNotRestartForContinuousWriters(t *testing.T) {
	db, source := liveSQLite(t)
	writes := 0
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	err := snapshotSQLite(ctx, source, copyPath, 8<<20, func() {
		if _, err := db.ExecContext(ctx, "UPDATE state SET generation=generation+1"); err != nil {
			t.Error(err)
			return
		}
		writes++
	})
	if err != nil || writes < 2 || writes > 32 {
		t.Fatal("normal writer/heartbeat starved incremental export", writes, err)
	}
	uri, _ := sqliteURI(copyPath, "mode=ro&immutable=1")
	copy, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var generation int
	if err := copy.QueryRow("SELECT generation FROM state WHERE id=1").Scan(&generation); err != nil || generation != 0 {
		t.Fatal("read snapshot was not pinned", generation, err)
	}
}

func TestSQLiteOnlineSnapshotCancellationLimitsAndNoOverwrite(t *testing.T) {
	_, source := liveSQLite(t)
	for _, scenario := range []string{"cancelled", "size", "during-copy", "concurrent-output"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "copy.db")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			limit := int64(8 << 20)
			if scenario == "cancelled" {
				cancel()
			}
			if scenario == "size" {
				limit = 4096
			}
			calls := 0
			err := snapshotSQLite(ctx, source, out, limit, func() {
				calls++
				if scenario == "during-copy" {
					cancel()
				}
				if scenario == "concurrent-output" && calls == 1 {
					if err := os.WriteFile(out, []byte("other owner"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err == nil {
				t.Fatal("invalid export published")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "concurrent-output" {
				data, _ := os.ReadFile(out)
				if string(data) != "other owner" || len(entries) != 1 {
					t.Fatal("concurrent destination overwritten or temporary data leaked")
				}
			} else if len(entries) != 0 {
				t.Fatal("failed export left partial data")
			}
		})
	}
}

func TestSQLiteOnlineSnapshotRejectsNonWALWithoutChangingIt(t *testing.T) {
	source := filepath.Join(t.TempDir(), "delete.db")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE sample(id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	err = SnapshotSQLite(t.Context(), source, filepath.Join(t.TempDir(), "copy.db"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "WAL") {
		t.Fatal("non-WAL source was silently changed", err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatal("source setting was modified", mode, err)
	}
}
