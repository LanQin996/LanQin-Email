//go:build linux

package maintenance

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func productionEnvironmentOps() environmentOps {
	return environmentOps{rename: func(source, target string) error {
		return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	}, syncDirectory: func(path string) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		return file.Sync()
	}}
}
func environmentOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func environmentFilePrivate(info os.FileInfo) bool {
	return environmentOwned(info) && info.Mode().Perm()&0o077 == 0
}
func environmentParentPrivate(info os.FileInfo) bool {
	return environmentOwned(info) && info.Mode().Perm()&0o022 == 0
}
