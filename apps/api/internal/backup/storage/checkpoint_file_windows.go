//go:build windows

package storage

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Unix mode bits do not set a Windows DACL. The operator must supply a private
// ACL on this explicitly selected directory, like the other backup workspaces.
func checkpointPrivateDirectory(os.FileInfo) bool { return true }

func checkpointLock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func checkpointReplace(root *os.Root, from, to string) error {
	pinned, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(root.Name())
	if err != nil || !current.IsDir() || !os.SameFile(pinned, current) {
		return errCheckpointFile
	}
	source, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), from))
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), to))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
