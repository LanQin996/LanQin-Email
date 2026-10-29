package main

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestKeygenDoesNotPrintPrivateKeyOrOverwrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "recovery-key.txt")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"keygen", "--identity-file", file}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(key)))
	if err != nil || strings.TrimSpace(stdout.String()) != identity.Recipient().String() {
		t.Fatal("keygen did not return its public recipient")
	}
	if strings.Contains(stdout.String()+stderr.String(), "AGE-SECRET-KEY") {
		t.Fatal("keygen printed private recovery material")
	}
	if err := run(t.Context(), []string{"keygen", "--identity-file", file}, &stdout, &stderr); err == nil {
		t.Fatal("keygen overwrote a recovery key")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(key, after) {
		t.Fatal("existing recovery key changed")
	}
}

func TestPublishFailureAndConcurrentDestination(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "backup.age")
	if err := publish(output, func(file *os.File) error {
		file.WriteString("partial ciphertext")
		return errors.New("synthetic write failure")
	}); err == nil {
		t.Fatal("failed output was published")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed output is visible")
	}
	if err := publish(output, func(file *os.File) error {
		if err := os.WriteFile(output, []byte("concurrent owner"), 0o600); err != nil {
			return err
		}
		_, err := file.WriteString("new ciphertext")
		return err
	}); err == nil {
		t.Fatal("concurrent output was overwritten")
	}
	content, err := os.ReadFile(output)
	if err != nil || string(content) != "concurrent owner" {
		t.Fatal("concurrent output changed")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatal("partial temporary output was not cleaned up")
	}
}

func TestCreateRequiresOfflineAcknowledgement(t *testing.T) {
	if err := run(t.Context(), []string{"create", "--snapshot", "unused", "--recipient", "unused", "--out", "unused"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("create allowed an unacknowledged live directory")
	}
}

func TestCLIEncryptedSnapshotRoundTrip(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "offline")
	for _, name := range []string{"data", "mail", "dkim"} {
		if err := os.MkdirAll(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(source, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sample(body TEXT); INSERT INTO sample VALUES('synthetic CLI fixture')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(directory, "recovery.txt")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"keygen", "--identity-file", keyFile}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	recipient := strings.TrimSpace(stdout.String())
	stdout.Reset()
	archive := filepath.Join(directory, "snapshot.age")
	if err := run(t.Context(), []string{"create", "--confirm-offline-snapshot", "--snapshot", source, "--recipient", recipient, "--out", archive}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(stdout.String()), "sha256=")
	if !ok || len(receipt) != 64 {
		t.Fatal("create did not return a ciphertext receipt")
	}
	output := filepath.Join(directory, "recovered")
	if err := run(t.Context(), []string{"restore", "--in", archive, "--identity-file", keyFile, "--expected-sha256", receipt, "--out", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", filepath.Join(output, "snapshot", "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var body string
	if err := restored.QueryRow(`SELECT body FROM sample`).Scan(&body); err != nil || body != "synthetic CLI fixture" {
		t.Fatalf("restored database cannot be read: %v", err)
	}
	badOutput := filepath.Join(directory, "wrong-receipt")
	if err := run(t.Context(), []string{"restore", "--in", archive, "--identity-file", keyFile, "--expected-sha256", strings.Repeat("0", 64), "--out", badOutput}, &stdout, &stderr); err == nil {
		t.Fatal("untrusted receipt accepted")
	}
	if _, err := os.Stat(filepath.Join(badOutput, "READY")); !os.IsNotExist(err) {
		t.Fatal("wrong-receipt data was marked ready")
	}
	if err := run(t.Context(), []string{"create", "--confirm-offline-snapshot", "--snapshot", source, "--recipient", recipient, "--out", filepath.Join(source, "backup.age")}, &stdout, &stderr); err == nil {
		t.Fatal("backup was created inside its own source")
	}
}
