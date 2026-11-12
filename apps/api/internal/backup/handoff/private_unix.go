//go:build unix

package handoff

import (
	"os"
	"syscall"
)

func sourcePrivate(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(uid) && info.Mode()&os.ModeSymlink == 0 && (info.IsDir() && info.Mode().Perm() == 0o700 || info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && stat.Nlink == 1)
}
func outputPrivate(info os.FileInfo) bool { return sourcePrivate(info, os.Geteuid()) }
func outputParent(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())) && info.Mode().Perm()&0o022 == 0
}
func trustedAncestor(info os.FileInfo, uid int, source bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())) && (info.Mode().Perm()&0o022 == 0 || info.Mode()&os.ModeSticky != 0) {
		return true
	}
	return source && stat.Uid == uint32(uid) && info.Mode().Perm() == 0o700
}
func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return errInvalid
	}
	defer file.Close()
	return file.Sync()
}
