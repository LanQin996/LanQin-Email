package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedCatalogAndTamperRejection(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x71}, 32)
	data := []byte("age-encryption.org/v1\nsynthetic catalog ciphertext fixture")
	source := filepath.Join(t.TempDir(), "source.age")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	receipt := hex.EncodeToString(hash[:])
	entry, err := Publish(t.Context(), dir, source, receipt, key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	file, got, err := Open(dir, entry.ID, key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(read, data) || got.SHA256 != receipt {
		t.Fatal("published object mismatch")
	}
	listed, err := List(t.Context(), dir, key, 1<<20)
	if err != nil || len(listed) != 1 {
		t.Fatal("published catalog not listed", err)
	}
	for _, id := range []string{"../source", "bac_invalid", entry.ID + "/../other"} {
		if f, _, err := Open(dir, id, key, 1<<20); err == nil {
			f.Close()
			t.Fatal("unsafe ID accepted")
		}
	}
	if f, _, err := Open(dir, entry.ID, bytes.Repeat([]byte{0x72}, 32), 1<<20); err == nil {
		f.Close()
		t.Fatal("wrong MAC key accepted")
	}
	metadata := filepath.Join(dir, entry.ID+".json")
	raw, _ := os.ReadFile(metadata)
	var signed signedEntry
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Entry.SHA256 = strings.Repeat("0", 64)
	changed, _ := json.Marshal(signed)
	os.WriteFile(metadata, changed, 0o600)
	if f, _, err := Open(dir, entry.ID, key, 1<<20); err == nil {
		f.Close()
		t.Fatal("tampered receipt accepted")
	}
	os.WriteFile(metadata, raw, 0o600)
	os.WriteFile(filepath.Join(dir, entry.ID+".age"), data[:10], 0o600)
	if f, _, err := Open(dir, entry.ID, key, 1<<20); err == nil {
		f.Close()
		t.Fatal("wrong object size accepted")
	}
}

func TestPublishPreflightCleanup(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	for _, tc := range []struct {
		name   string
		data   []byte
		wrong  bool
		limit  int64
		cancel bool
	}{
		{"plaintext", []byte("synthetic private plaintext"), false, 1024, false},
		{"wrong receipt", []byte("age-encryption.org/v1\nfixture"), true, 1024, false},
		{"size limit", []byte("age-encryption.org/v1\nfixture"), false, 3, false},
		{"cancel", []byte("age-encryption.org/v1\nfixture"), false, 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "artifact")
			os.WriteFile(source, tc.data, 0o600)
			hash := sha256.Sum256(tc.data)
			receipt := hex.EncodeToString(hash[:])
			if tc.wrong {
				receipt = strings.Repeat("0", 64)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if _, err := Publish(ctx, dir, source, receipt, key, tc.limit); err == nil {
				t.Fatal("invalid artifact published")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("failed publication left artifacts")
			}
		})
	}
}
