package install

import (
	"os"
	"syscall"
)

// Refuse a root-run installer consuming a READY tree writable by another UID.
func materializePrivateInfo(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}
