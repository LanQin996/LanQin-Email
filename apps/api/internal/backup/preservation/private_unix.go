//go:build unix

package preservation

import (
	"os"
	"syscall"
)

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func privateInfo(info os.FileInfo) bool { return owned(info) && info.Mode().Perm()&0o077 == 0 }
func safeParent(info os.FileInfo) bool  { return owned(info) && info.Mode().Perm()&0o022 == 0 }
func safeAncestor(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())) && (info.Mode().Perm()&0o022 == 0 || info.Mode()&os.ModeSticky != 0)
}

func syncRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return errOutput
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return errOutput
	}
	return nil
}
