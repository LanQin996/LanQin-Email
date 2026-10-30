//go:build !linux

package install

import (
	"os"
	"runtime"
)

func materializePrivateInfo(info os.FileInfo) bool {
	// Windows permissions are an operator-managed inherited workspace ACL.
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o077 == 0
}
