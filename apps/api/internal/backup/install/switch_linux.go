//go:build linux

package install

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func switchControlledParent(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 == 0
}

// Refuse unsupported kernels/filesystems rather than falling back to the
// existence-check + rename race that can overwrite a preexisting directory.
func renameSwitchNoReplace(from, to string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename-noreplace", Old: from, New: to, Err: err}
	}
	return nil
}
