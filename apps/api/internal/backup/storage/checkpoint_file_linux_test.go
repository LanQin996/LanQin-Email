//go:build linux

package storage

import (
	"bytes"
	"os"
	"testing"
)

func TestFileCheckpointDirectoryMustBePrivate(t *testing.T) {
	directory := privateCheckpointDirectory(t)
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenFileCheckpointStore(directory, "synthetic-private", bytes.Repeat([]byte{0x53}, 32)); err == nil {
		store.Close()
		t.Fatal("public checkpoint directory accepted")
	}
}
