package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sqliteAccessFixture(t *testing.T) switchFixture {
	t.Helper()
	f := newSwitchFixture(t)
	mustSwitchWrite(t, filepath.Join(f.source, "data", "lanqin.db"), "synthetic SQLite data")
	if runtime.GOOS == "linux" {
		uid, gid := os.Geteuid(), os.Getegid()
		if uid == 0 {
			gid = 103
		}
		if gid <= 0 {
			t.Skip("explicit positive data group required")
		}
		owner := Owner{uid, gid}
		f.owners = Owners{owner, owner, owner}
		f.ops = productionSwitchOps()
	}
	return f
}

func TestSQLiteGroupAccessExplicitPolicyAndLegacyJournal(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "explicit"}[enabled], func(t *testing.T) {
			f := sqliteAccessFixture(t)
			info, err := applyWithOps(context.Background(), f.source, f.targets, f.owners, f.journal, true, f.ops, Access{SQLiteGroupRead: enabled})
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteGroupAccess(t.Context(), info.Path, f.targets, enabled); err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteGroupAccess(t.Context(), info.Path, f.targets, !enabled); err == nil {
				t.Fatal("journal policy mismatch accepted")
			}
			j, _, err := readSwitchJournal(info.Path)
			if err != nil || j.SQLiteGroupRead != enabled {
				t.Fatal("policy not bound", err)
			}
			data, err := os.ReadFile(info.Path)
			if err != nil {
				t.Fatal(err)
			}
			first := strings.SplitN(string(data), "\n", 2)[0]
			if !enabled && (strings.Contains(first, "SQLiteGroupRead") || strings.Contains(first, "DataReadGID")) {
				t.Fatal("legacy journal encoding changed")
			}
			for _, name := range []string{"nested/payload", "restore-install-plan.json"} {
				p := filepath.Join(f.targets.Data, filepath.FromSlash(name))
				if stat, err := os.Lstat(p); err == nil && runtime.GOOS == "linux" && stat.Mode().Perm() != 0o600 {
					t.Fatal("unrelated data became group-readable", name)
				}
			}
			if runtime.GOOS == "linux" {
				data, _ := os.Stat(f.targets.Data)
				db, _ := os.Stat(filepath.Join(f.targets.Data, "lanqin.db"))
				if enabled {
					if data.Mode().Perm() != 0o750 || data.Mode()&os.ModeSetgid == 0 || db.Mode().Perm() != 0o640 {
						t.Fatal("incorrect explicit modes")
					}
				} else if data.Mode().Perm() != 0o700 || db.Mode().Perm() != 0o600 {
					t.Fatal("legacy modes weakened")
				}
			}
			if err := rollbackWithOps(t.Context(), info.Path, f.targets, true, f.ops); err != nil {
				t.Fatal(err)
			}
			assertOldSwitchRoots(t, f)
			if err := VerifySQLiteGroupAccess(t.Context(), info.Path, f.targets, enabled); err == nil {
				t.Fatal("rolled-back new policy treated as active")
			}
		})
	}
}

func TestSQLiteGroupAccessRejectsAmbiguousOrNonSQLiteInputs(t *testing.T) {
	for _, damage := range []string{"duplicate-options", "zero-gid", "negative-gid", "native", "wal", "shm", "journal", "missing-db"} {
		t.Run(damage, func(t *testing.T) {
			f := sqliteAccessFixture(t)
			options := []Access{{SQLiteGroupRead: true}}
			switch damage {
			case "duplicate-options":
				options = append(options, Access{})
			case "zero-gid":
				f.owners.Data.GID = 0
			case "negative-gid":
				f.owners.Data.GID = -1
			case "native":
				mustSwitchWrite(t, filepath.Join(f.source, "data/lanqin-native.jsonl"), "native")
			case "wal", "shm", "journal":
				mustSwitchWrite(t, filepath.Join(f.source, "data/lanqin.db-"+damage), "sidecar")
			case "missing-db":
				if err := os.Remove(filepath.Join(f.source, "data/lanqin.db")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := applyWithOps(t.Context(), f.source, f.targets, f.owners, f.journal, true, f.ops, options...); err == nil {
				t.Fatal("unsafe group-read source accepted")
			}
			assertOldSwitchRoots(t, f)
		})
	}
}

func TestSQLiteGroupAccessRejectsMalformedJournalPolicy(t *testing.T) {
	for _, state := range []struct {
		Enabled bool
		GID     int
	}{{false, 103}, {true, 0}, {true, -1}} {
		f := newSwitchFixture(t)
		txn := strings.Repeat("a", 32)
		j := switchJournal{Version: 1, Txn: txn, SQLiteGroupRead: state.Enabled, DataReadGID: state.GID}
		data, _ := json.Marshal(j)
		path := filepath.Join(f.journal, "restore-"+txn+".journal")
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readSwitchJournal(path); err == nil {
			t.Fatal("malformed journal policy accepted")
		}
	}
}
