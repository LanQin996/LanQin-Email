package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStageFileUsesPinnedHandleWithoutChangingOffset(t *testing.T) {
	name, receipt, data := fixture(t)
	source, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// Renaming the path after catalog.Open must not select a different artifact.
	if err := os.Rename(name, name+".original"); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Log("Windows denies replacing an open file; pinned-path replacement case requires Unix")
	} else if err := os.WriteFile(name, []byte("replacement must never be uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.SpoolDir = t.TempDir()
	staged, err := stageFile(t.Context(), source, receipt, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged.file.Name())
	defer staged.file.Close()
	actual, err := io.ReadAll(io.NewSectionReader(staged.file, 0, staged.size))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("staged replacement or failed to rewind pinned source", err)
	}
	if offset, err := source.Seek(0, io.SeekCurrent); err != nil || offset != 7 {
		t.Fatal("caller file closed or its offset changed", offset, err)
	}
}

func TestUploadFilePreflightRunsBeforeCredentialResolution(t *testing.T) {
	name, receipt, _ := fixture(t)
	source, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("first")}}
	for _, scenario := range []string{"hash", "size", "plaintext", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := DefaultOptions()
			opts.SpoolDir = t.TempDir()
			file, hash := source, receipt
			switch scenario {
			case "hash":
				hash = strings.Repeat("0", 64)
			case "size":
				opts.MaxBytes = 1
			case "plaintext":
				plain := filepath.Join(t.TempDir(), "plain")
				if err := os.WriteFile(plain, []byte("synthetic secret"), 0o600); err != nil {
					t.Fatal(err)
				}
				file, err = os.Open(plain)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
			case "cancel":
				cancel()
			}
			_, err := UploadFile(ctx, file, hash, cfg, opts, func(string) (Credentials, error) {
				t.Error("credentials resolved / remote client built before preflight")
				return Credentials{}, nil
			})
			if err == nil {
				t.Fatal("invalid artifact accepted")
			}
			entries, err := os.ReadDir(opts.SpoolDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed stage left ciphertext spool", err)
			}
		})
	}
}
