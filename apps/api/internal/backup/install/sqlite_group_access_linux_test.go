//go:build linux

package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteGroupAccessLinuxRejectsLinksAndRuntimeModeDrift(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("real owner/group mutations require root")
	}
	for _, damage := range []string{"root-mode", "root-gid", "db-mode", "db-gid", "db-owner", "db-link", "db-hardlink", "wal-mode", "wal-gid", "wal-owner", "wal-link", "wal-hardlink", "shm-mode"} {
		t.Run(damage, func(t *testing.T) {
			f := sqliteAccessFixture(t)
			info, err := applyWithOps(t.Context(), f.source, f.targets, f.owners, f.journal, true, f.ops, Access{true})
			if err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(f.targets.Data, "lanqin.db")
			wal := db + "-wal"
			shm := db + "-shm"
			for _, name := range []string{wal, shm} {
				if err := os.WriteFile(name, []byte("synthetic sidecar"), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chown(name, f.owners.Data.UID, f.owners.Data.GID); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifySQLiteGroupAccess(t.Context(), info.Path, f.targets, true); err != nil {
				t.Fatal("valid runtime sidecars refused", err)
			}
			must := func(err error) {
				if err != nil {
					t.Fatal(err)
				}
			}
			switch damage {
			case "root-mode":
				must(os.Chmod(f.targets.Data, 0o750))
			case "root-gid":
				must(os.Chown(f.targets.Data, 0, f.owners.Data.GID+1))
			case "db-mode":
				must(os.Chmod(db, 0o644))
			case "db-gid":
				must(os.Chown(db, 0, f.owners.Data.GID+1))
			case "db-owner":
				must(os.Chown(db, 1002, f.owners.Data.GID))
			case "db-link":
				must(os.Rename(db, db+".parked"))
				must(os.Symlink(db+".parked", db))
			case "db-hardlink":
				must(os.Link(db, db+".hardlink"))
			case "wal-mode":
				must(os.Chmod(wal, 0o660))
			case "wal-gid":
				must(os.Chown(wal, 0, f.owners.Data.GID+1))
			case "wal-owner":
				must(os.Chown(wal, 1002, f.owners.Data.GID))
			case "wal-link":
				must(os.Rename(wal, wal+".parked"))
				must(os.Symlink(wal+".parked", wal))
			case "wal-hardlink":
				must(os.Link(wal, wal+".hardlink"))
			case "shm-mode":
				must(os.Chmod(shm, 0o600))
			}
			if err := VerifySQLiteGroupAccess(t.Context(), info.Path, f.targets, true); err == nil {
				t.Fatal("runtime access drift accepted")
			}
		})
	}
}

func TestSQLiteGroupAccessLinuxDoesNotAlterSourceOldOrUnrelatedModes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("real owner/group mappings require root")
	}
	f := sqliteAccessFixture(t)
	if err := os.Chmod(f.targets.Data, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(f.targets.Data, "old")
	if err := os.Chmod(old, 0o640); err != nil {
		t.Fatal(err)
	}
	sourceDB := filepath.Join(f.source, "data/lanqin.db")
	sourceBefore, _ := os.Stat(sourceDB)
	info, err := applyWithOps(t.Context(), f.source, f.targets, f.owners, f.journal, true, f.ops, Access{true})
	if err != nil {
		t.Fatal(err)
	}
	sourceAfter, _ := os.Stat(sourceDB)
	if !os.SameFile(sourceBefore, sourceAfter) || sourceAfter.Mode().Perm() != sourceBefore.Mode().Perm() {
		t.Fatal("source changed")
	}
	for _, name := range []string{"nested", "nested/payload"} {
		stat, err := os.Lstat(filepath.Join(f.targets.Data, name))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if stat.IsDir() {
			want = 0o700
		}
		if stat.Mode().Perm() != want {
			t.Fatal("unrelated data exposed", name)
		}
	}
	if err := rollbackWithOps(t.Context(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatal(err)
	}
	root, err := os.Stat(f.targets.Data)
	if err != nil || root.Mode().Perm() != 0o755 || root.Mode()&os.ModeSetgid != 0 {
		t.Fatal("old directory modes changed", err)
	}
	file, err := os.Stat(old)
	if err != nil || file.Mode().Perm() != 0o640 {
		t.Fatal("old file modes changed", err)
	}
}

func TestSQLiteGroupAccessLinuxRejectsPreparedDatabaseHardlink(t *testing.T) {
	f := sqliteAccessFixture(t)
	if err := os.Link(filepath.Join(f.source, "data/lanqin.db"), filepath.Join(f.source, "data/copy.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := applyWithOps(t.Context(), f.source, f.targets, f.owners, f.journal, true, f.ops, Access{true}); err == nil {
		t.Fatal("prepared database hardlink accepted")
	}
	assertOldSwitchRoots(t, f)
}
