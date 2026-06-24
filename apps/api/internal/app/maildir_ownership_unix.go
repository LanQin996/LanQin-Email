//go:build unix

package app

import "os"

const (
	maildirOwnerUID = 5000
	maildirOwnerGID = 5000
)

func applyMaildirOwnership(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, maildirOwnerUID, maildirOwnerGID)
}
