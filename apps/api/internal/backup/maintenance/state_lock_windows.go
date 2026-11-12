//go:build windows

package maintenance

import (
	"golang.org/x/sys/windows"
	"os"
)

func trustedMaintenanceInfo(os.FileInfo) bool   { return true }
func trustedMaintenanceParent(os.FileInfo) bool { return true }

func lockStateFile(file *os.File) error {
	var overlapped windows.Overlapped
	if windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped) != nil {
		return ErrLocked
	}
	return nil
}
func unlockStateFile(file *os.File) {
	var overlapped windows.Overlapped
	windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

// Directory durability is exercised by Linux production; Windows only runs
// callback/metadata tests and never exposes a production switching bypass.
func syncStateDirectory(string) error { return nil }
