package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/storage"
)

func TestUploadCLISelectionReportsAndSecretRedaction(t *testing.T) {
	directory := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("synthetic CLI cloud-upload fixture"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(directory, "snapshot.age")
	if err := os.WriteFile(input, ciphertext.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(ciphertext.Bytes())
	receipt := hex.EncodeToString(hash[:])
	cfg := storage.Config{Version: 1}
	for _, id := range []string{"r2", "b2", "s3_3"} {
		cfg.Destinations = append(cfg.Destinations, storage.Destination{ID: id, Name: id, Type: "s3", S3: &storage.S3Config{
			Endpoint: "https://s3.example.invalid", Bucket: "backup-bucket", Region: "auto",
			Prefix: id, PathStyle: true, AccessKeyIDEnv: "BACKUP_TEST_" + id + "_KEY", SecretAccessKeyEnv: "BACKUP_TEST_" + id + "_SECRET",
		}})
		t.Setenv("BACKUP_TEST_"+id+"_KEY", "") // Fail before constructing any network client.
	}
	cfg.Destinations[2].Disabled = true
	configFile := filepath.Join(directory, "destinations.json")
	encoded, _ := json.Marshal(cfg)
	if err := os.WriteFile(configFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"upload", "--config", configFile, "--in", input, "--expected-sha256", receipt, "--spool-dir", directory}
	var stdout, stderr bytes.Buffer
	err = run(t.Context(), args, &stdout, &stderr)
	if !errors.Is(err, storage.ErrPartialFailure) {
		t.Fatalf("expected isolated credential failures, got %v", err)
	}
	var report storage.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Destinations) != 3 || report.SHA256 != receipt {
		t.Fatalf("missing machine-readable partial report: %s %v", stdout.String(), err)
	}
	if report.Destinations[0].State != "failed" || report.Destinations[1].State != "failed" || report.Destinations[2].State != "skipped" {
		t.Fatal("incorrect target isolation or disabled handling")
	}
	stdout.Reset()
	err = run(t.Context(), append(args, "--destinations", "b2"), &stdout, &stderr)
	if !errors.Is(err, storage.ErrPartialFailure) {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Destinations) != 1 || report.Destinations[0].DestinationID != "b2" {
		t.Fatal("selected target was not isolated")
	}
	for _, selected := range []string{"missing", "r2,r2", "r2,"} {
		stdout.Reset()
		if err := run(t.Context(), append(args, "--destinations", selected), &stdout, &stderr); err == nil || stdout.Len() != 0 {
			t.Fatal("invalid selection uploaded or emitted success")
		}
	}
	if err := os.WriteFile(configFile, []byte(`{"version":1,"secret_access_key":"NEVER-ECHO-THIS"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err = run(t.Context(), args, &stdout, &stderr)
	if err == nil || strings.Contains(err.Error()+stdout.String()+stderr.String(), "NEVER-ECHO-THIS") {
		t.Fatal("misplaced credential accepted or leaked")
	}
	spool, _ := filepath.Glob(filepath.Join(directory, ".lanqin-upload-*"))
	if len(spool) != 0 {
		t.Fatal("CLI left private ciphertext spool behind")
	}
}

func TestUploadCLIRequiresReceiptAndExplicitConfig(t *testing.T) {
	for _, args := range [][]string{{"upload"}, {"upload", "--config", "unused", "--in", "unused"}, {"upload", "--config", "unused", "extra"}} {
		if err := run(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("upload accepted missing receipt/config or positional input")
		}
	}
}
