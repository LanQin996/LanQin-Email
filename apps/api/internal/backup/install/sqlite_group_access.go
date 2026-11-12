package install

import (
	"context"
	"os"
	"path/filepath"
)

func validateSQLiteGroupSource(data string) error {
	info, err := pinnedInfo(filepath.Join(data, "lanqin.db"), false)
	if err != nil || !sqliteSingleFile(info) {
		return ErrSwitchUnsafe
	}
	for _, name := range []string{"lanqin-native.jsonl", "lanqin.db-wal", "lanqin.db-shm", "lanqin.db-journal"} {
		if requireAbsent(filepath.Join(data, name)) != nil {
			return ErrSwitchUnsafe
		}
	}
	return nil
}

func setSQLiteGroupAccess(data string, owner Owner, ops switchOps) error {
	if err := validateSQLiteGroupSource(data); err != nil {
		return err
	}
	db, err := os.OpenFile(filepath.Join(data, "lanqin.db"), os.O_RDWR, 0)
	if err != nil {
		return ErrSwitchUnsafe
	}
	defer db.Close()
	info, err := db.Stat()
	if err != nil || !sqliteSingleFile(info) {
		return ErrSwitchUnsafe
	}
	if err := db.Chmod(0o640); err != nil {
		return err
	}
	if err := db.Sync(); err != nil {
		return err
	}
	// Chown has already completed during cloning, before setting setgid. New
	// runtime sidecars must inherit the explicitly approved database group.
	if err := os.Chmod(data, os.ModeSetgid|0o750); err != nil {
		return err
	}
	parent, err := pinnedInfo(data, true)
	current, statErr := db.Stat()
	if err != nil || statErr != nil || !sqliteGroupDirectory(parent, owner.GID) || !sqliteGroupFile(current, parent, owner.GID) {
		return ErrSwitchUnsafe
	}
	return ops.syncDir(data)
}

// VerifySQLiteGroupAccess checks policy bound to the exact installed NEW tree.
// It never changes modes or repairs runtime sidecars. Callers stop writers
// before using it; a missing or unsafe sidecar is never silently rewritten.
func VerifySQLiteGroupAccess(ctx context.Context, journalFile string, targets Targets, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j, _, err := readSwitchJournal(journalFile)
	if err != nil || j.SQLiteGroupRead != enabled {
		return ErrSwitchUnsafe
	}
	if _, err := VerifyCommitted(ctx, journalFile, targets); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if _, err := switchRealDirectory(targets.Data); err != nil {
		return ErrSwitchUnsafe
	}
	root, err := os.OpenRoot(targets.Data)
	if err != nil {
		return ErrSwitchUnsafe
	}
	defer root.Close()
	data, err := root.Stat(".")
	if err != nil || !sqliteGroupDirectory(data, j.DataReadGID) {
		return ErrSwitchUnsafe
	}
	for _, name := range []string{"lanqin.db", "lanqin.db-wal", "lanqin.db-shm"} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		before, err := root.Lstat(name)
		if os.IsNotExist(err) && name != "lanqin.db" {
			continue
		}
		if err != nil || !sqliteGroupFile(before, data, j.DataReadGID) {
			return ErrSwitchUnsafe
		}
		file, err := root.Open(name)
		if err != nil {
			return ErrSwitchUnsafe
		}
		opened, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(before, opened) || !sqliteGroupFile(opened, data, j.DataReadGID) {
			return ErrSwitchUnsafe
		}
	}
	return nil
}
