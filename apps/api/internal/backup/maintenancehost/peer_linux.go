//go:build linux

package maintenancehost

import (
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func peerUID(conn net.Conn) (int, error) {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, ErrUnavailable
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return -1, ErrUnavailable
	}
	var credential *unix.Ucred
	var inner error
	if raw.Control(func(fd uintptr) { credential, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || inner != nil || credential == nil {
		return -1, ErrUnavailable
	}
	return int(credential.Uid), nil
}

func trustedPath(name string, directory, private bool) bool {
	if !absolute(name) {
		return false
	}
	for p := name; ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil || i.Mode()&os.ModeSymlink != 0 {
			return false
		}
		s, ok := i.Sys().(*syscall.Stat_t)
		if !ok || s.Uid != 0 {
			return false
		}
		if p == name {
			if directory != i.IsDir() || !directory && !i.Mode().IsRegular() || private && i.Mode().Perm()&0o077 != 0 {
				return false
			}
		}
		if i.Mode().Perm()&0o022 != 0 && !(p != name && i.IsDir() && i.Mode()&os.ModeSticky != 0) {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
func lockFile(f *os.File) error {
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return ErrConflict
	}
	return nil
}
func unlockFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func rootHost() bool { return os.Geteuid() == 0 }

func socketOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}
