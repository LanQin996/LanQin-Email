package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/storage"
)

func TestUploadCheckpointFlagsRequirePairsAndKeepSecretsOutOfDiagnostics(t *testing.T) {
	base := []string{"--config=synthetic-config", "--in=synthetic-input", "--expected-sha256=" + strings.Repeat("a", 64)}
	for _, extra := range [][]string{{"--resume-dir=synthetic-directory"}, {"--resume-id=synthetic-operation"}, {"--unknown=synthetic-secret-do-not-echo"}, {"--concurrency=synthetic-secret-do-not-echo"}} {
		var out, diagnostic bytes.Buffer
		err := runUpload(t.Context(), append(append([]string{}, base...), extra...), &out, &diagnostic)
		if err == nil || out.Len() != 0 || diagnostic.Len() != 0 || strings.Contains(err.Error(), "synthetic-secret-do-not-echo") {
			t.Fatal("invalid checkpoint flags accepted or echoed")
		}
	}
}

func TestUploadCheckpointNamespaceLockReleasedAfterConfigFailure(t *testing.T) {
	key := bytes.Repeat([]byte{0x53}, 32)
	t.Setenv("LANQIN_BACKUP_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(config, []byte(`{"version":1,"privateCredential":"synthetic-do-not-echo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runUpload(t.Context(), []string{"--config", config, "--in=synthetic-input", "--expected-sha256=" + strings.Repeat("a", 64), "--resume-dir", root, "--resume-id=synthetic-cli-operation"}, io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "synthetic-do-not-echo") {
		t.Fatal("invalid config accepted or leaked")
	}
	store, err := storage.OpenFileCheckpointStore(root, "synthetic-cli-operation", key)
	if err != nil {
		t.Fatal("failed CLI retained OS namespace lock", err)
	}
	store.Close()
}
