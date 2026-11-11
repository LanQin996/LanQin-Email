//go:build !linux

package maintenancehost

import (
	"net"
	"os"
)

func peerUID(net.Conn) (int, error) { return -1, ErrUnavailable }

// Non-Linux supports in-process protocol tests only, never service execution.
func trustedPath(name string, directory, private bool) bool {
	i, e := os.Lstat(name)
	return e == nil && i.Mode()&os.ModeSymlink == 0 && i.IsDir() == directory && (directory || i.Mode().IsRegular())
}
func lockFile(*os.File) error { return ErrUnavailable }
func unlockFile(*os.File)     {}
func syncDir(string) error    { return nil }
func rootHost() bool          { return false }

func socketOwned(os.FileInfo) bool { return false }
