//go:build !linux

package handoff

import (
	"os"
	"runtime"
)

func publish(parent *os.Root, from, to string) error {
	if runtime.GOOS != "windows" {
		return errInvalid
	}
	// Windows directory rename refuses an existing destination; no Unix
	// overwrite-capable os.Rename fallback is used on other platforms.
	return parent.Rename(from, to)
}
