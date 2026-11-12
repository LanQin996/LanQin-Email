package app

import (
	"errors"
	"os"
	"path/filepath"
)

var errRestoreIncomplete = errors.New("restore data is not activated; complete explicit install or rollback before starting services")

// This marker is owned by the offline restore transaction. Presence always
// blocks API startup, regardless of its content/type. It is NOT a replacement
// for stopping all other mail writers before directory switching.
func checkRestoreActivation(dataDir string) error {
	for _, name := range []string{".lanqin-restore-incomplete", ".lanqin-restore-not-activated", "backup-online.json"} {
		_, err := os.Lstat(filepath.Join(dataDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errors.New("cannot verify restore activation state")
		}
		return errRestoreIncomplete
	}
	return nil
}
