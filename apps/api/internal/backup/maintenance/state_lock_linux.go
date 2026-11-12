//go:build linux

package maintenance

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func trustedMaintenanceInfo(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == uint32(os.Geteuid()) || stat.Uid == 0) && info.Mode().Perm()&0o022 == 0
}

func trustedMaintenanceParent(info os.FileInfo) bool {
	if trustedMaintenanceInfo(info) {
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	// A root-owned sticky temporary parent cannot let another uid replace this
	// helper-owned child; writable non-sticky ancestors remain forbidden.
	return ok && stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
}

func lockStateFile(file *os.File) error {
	if unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return ErrLocked
	}
	return nil
}
func unlockStateFile(file *os.File) { unix.Flock(int(file.Fd()), unix.LOCK_UN) }
func syncStateDirectory(name string) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
