//go:build linux

package storage

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkpointPrivateDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}

func checkpointLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func checkpointReplace(root *os.Root, from, to string) error {
	if err := root.Rename(from, to); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
