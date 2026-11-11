package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileCheckpointsEncryptedExclusiveAndBoundToNamespace(t *testing.T) {
	directory := t.TempDir()
	key := bytes.Repeat([]byte{0x53}, 32)
	store, err := OpenFileCheckpointStore(directory, "synthetic-operation", key)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenFileCheckpointStore(directory, "synthetic-operation", key); err == nil {
		other.Close()
		t.Fatal("two processes may write the same upload checkpoint")
	}
	state := Checkpoint{Version: 1, Type: "drive", Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "uploading", Session: "https://www.googleapis.com/upload/drive/v3/files?upload_id=synthetic-sensitive-session", FileID: "synthetic-file", Offset: 123}
	if err := store.Save(t.Context(), "first", state); err != nil {
		t.Fatal(err)
	}
	state.Offset = 456
	if err := store.Save(t.Context(), "first", state); err != nil {
		t.Fatal("atomic replacement failed", err)
	}
	name, _, _ := store.name("first")
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil || bytes.Contains(data, []byte("synthetic-sensitive-session")) || bytes.Contains(data, []byte("googleapis")) {
		t.Fatal("plaintext session persisted", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileCheckpointStore(directory, "synthetic-operation", key)
	if err != nil {
		t.Fatal("process lock was not released", err)
	}
	loaded, err := reopened.Load(t.Context(), "first")
	if err != nil || loaded.Offset != 456 || loaded.Session != state.Session {
		t.Fatal("durable checkpoint lost", err)
	}
	otherName, _, _ := reopened.name("second")
	if err := os.WriteFile(filepath.Join(directory, otherName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(t.Context(), "second"); err == nil {
		t.Fatal("ciphertext moved between destination IDs")
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(t.Context(), "first"); err == nil {
		t.Fatal("tampered checkpoint accepted")
	}
	reopened.Close()
	wrong, err := OpenFileCheckpointStore(directory, "synthetic-operation", bytes.Repeat([]byte{0x54}, 32))
	if err == nil {
		wrong.Close()
		t.Fatal("wrong key opened authenticated namespace index")
	}
	if strings.Contains(err.Error(), "synthetic-sensitive-session") {
		t.Fatal("session leaked in error")
	}
	valid, err := OpenFileCheckpointStore(directory, "synthetic-operation", key)
	if err != nil {
		t.Fatal(err)
	}
	defer valid.Close()
	if err := os.Remove(filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.Load(t.Context(), "first"); err == nil {
		t.Fatal("missing recorded checkpoint silently became a new upload")
	}
	if err := valid.Save(t.Context(), "first", state); err == nil {
		t.Fatal("missing recorded checkpoint could be overwritten")
	}
	if err := valid.Save(t.Context(), "../escape", state); err == nil {
		t.Fatal("checkpoint path escape accepted")
	}
	public, _ := json.Marshal(Result{DestinationID: "first", State: "failed", ErrorCode: "checkpoint_unavailable"})
	if bytes.Contains(public, []byte("synthetic-sensitive-session")) {
		t.Fatal("session entered public result")
	}
}
