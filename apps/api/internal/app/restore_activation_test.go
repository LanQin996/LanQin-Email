package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreIncompleteMarkerPreventsOpeningDatabaseOrStartingWorkers(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, ".lanqin-restore-incomplete")
			switch kind {
			case "file":
				if err := os.WriteFile(marker, []byte("not an activation authorization"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-marker-target", marker); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			db := filepath.Join(dir, "unopened", "lanqin.db")
			a, err := New(Config{DBPath: db, DataDir: dir}, nil)
			if a != nil || !errors.Is(err, errRestoreIncomplete) {
				t.Fatal("partly restored data activated", err)
			}
			if _, err := os.Stat(filepath.Dir(db)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("startup modified incomplete restore before rejecting it")
			}
		})
	}
}

func TestRestoreStagedAndPreparedDataCannotStartApplication(t *testing.T) {
	for _, name := range []string{"backup-online.json", ".lanqin-restore-not-activated"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("untrusted marker content cannot authorize startup"), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := New(Config{DataDir: dir, DBPath: filepath.Join(dir, "not-opened.db")}, nil)
		if a != nil || !errors.Is(err, errRestoreIncomplete) {
			t.Fatal("staged/prepared backup was activated", name, err)
		}
	}
}
