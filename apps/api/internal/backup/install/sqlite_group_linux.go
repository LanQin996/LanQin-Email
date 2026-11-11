//go:build linux

package install

import (
	"os"
	"syscall"
)

func sqliteSingleFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1
}
func sqliteGroupDirectory(info os.FileInfo, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && int64(stat.Gid) == int64(gid) && info.Mode().Perm() == 0o750 && info.Mode()&os.ModeSetgid != 0 && info.Mode()&(os.ModeSetuid|os.ModeSticky) == 0
}
func sqliteGroupFile(info, parent os.FileInfo, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	owner, pok := parent.Sys().(*syscall.Stat_t)
	return ok && pok && sqliteSingleFile(info) && stat.Uid == owner.Uid && int64(stat.Gid) == int64(gid) && info.Mode().Perm() == 0o640 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}
