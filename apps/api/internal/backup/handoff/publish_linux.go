package handoff

import (
	"os"

	"golang.org/x/sys/unix"
)

func publish(parent *os.Root, from, to string) error {
	directory, err := parent.Open(".")
	if err != nil {
		return errInvalid
	}
	defer directory.Close()
	return unix.Renameat2(int(directory.Fd()), from, int(directory.Fd()), to, unix.RENAME_NOREPLACE)
}
