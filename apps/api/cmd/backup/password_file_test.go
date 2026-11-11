package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup"
)

func passwordFileFixture(t *testing.T, value string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "private-password.txt")
	if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestPasswordFilePreservesSpacesAndRejectsUnsafeFiles(t *testing.T) {
	for _, ending := range []string{"", "\n", "\r\n"} {
		const password = "  synthetic password  "
		data, err := readBackupPasswordFile(passwordFileFixture(t, password+ending))
		if err != nil || string(data) != password {
			t.Fatal("password whitespace or delimiter changed", err)
		}
		clear(data)
	}
	for _, value := range []string{"", "\n", strings.Repeat("x", 1025), strings.Repeat("x", 1027)} {
		if _, err := readBackupPasswordFile(passwordFileFixture(t, value)); err == nil {
			t.Fatal("invalid password file size accepted")
		}
	}
	if runtime.GOOS != "windows" {
		name := passwordFileFixture(t, "synthetic password")
		if err := os.Chmod(name, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readBackupPasswordFile(name); err == nil {
			t.Fatal("world-readable password file accepted")
		}
	}
}

func TestPasswordCLIOnlineCreateAndIndependentRestore(t *testing.T) {
	f := newOnlineCLIFixture(t)
	const password = "abcdefgh"
	passwordFile := passwordFileFixture(t, password+"\r\n")
	var output, diagnostics bytes.Buffer
	args := f.args("--recipient=", "--password-file", passwordFile)
	if err := run(t.Context(), args, &output, &diagnostics); err != nil {
		t.Fatal("password create-online failed", err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(output.String()), "sha256=")
	if !ok || len(receipt) != 64 {
		t.Fatal("password CLI did not return trusted ciphertext receipt")
	}
	data, err := os.ReadFile(f.out)
	if err != nil || bytes.Contains(data, []byte(password)) || bytes.Contains(data, f.raw) {
		t.Fatal("password archive exposed private data", err)
	}
	recovered := filepath.Join(t.TempDir(), "recovered")
	output.Reset()
	// This independent invocation receives only the downloaded archive,
	// separately saved hash and password file. No application env key is used.
	t.Setenv("LANQIN_BACKUP_SECRET_KEY", "invalid-unrelated-server-key")
	if err := run(t.Context(), []string{"restore", "--in", f.out, "--out", recovered, "--password-file", passwordFile, "--expected-sha256", receipt}, &output, &diagnostics); err != nil {
		t.Fatal("independent password CLI recovery failed", err)
	}
	got, err := os.ReadFile(filepath.Join(recovered, "snapshot", "mail", f.relative))
	if err != nil || !bytes.Equal(got, f.raw) || !strings.Contains(output.String(), "report-sha256=") {
		t.Fatal("independent restore lost MIME or installation receipt", err)
	}
	for _, secret := range []string{password, passwordFile, "invalid-unrelated-server-key", string(f.raw)} {
		if strings.Contains(output.String()+diagnostics.String(), secret) {
			t.Fatal("password CLI output exposed secrets")
		}
	}
	output.Reset()
	wrong := passwordFileFixture(t, "wrong synthetic password")
	badOutput := filepath.Join(t.TempDir(), "wrong-password")
	if err := run(t.Context(), []string{"restore", "--in", f.out, "--out", badOutput, "--password-file", wrong, "--expected-sha256", receipt}, &output, &diagnostics); err == nil || strings.Contains(err.Error(), "wrong synthetic password") || output.Len() != 0 {
		t.Fatal("wrong backup password accepted or leaked")
	}
	if _, err := os.Stat(filepath.Join(badOutput, "READY")); !os.IsNotExist(err) {
		t.Fatal("wrong backup password published ready data")
	}
	if entries, err := os.ReadDir(f.work); err != nil || len(entries) != 0 {
		t.Fatal("password create left plaintext workspace", err)
	}
}

func TestPasswordCLINewPasswordValidationAndMutuallyExclusiveFlags(t *testing.T) {
	f := newOnlineCLIFixture(t)
	for _, value := range []string{"short", strings.Repeat("密", 7), strings.Repeat("🔐", 7), strings.Repeat("x", 129), strings.Repeat("🔐", 129), strings.Repeat(" ", 16), "synthetic\npassword", "synthetic\x00password", "synthetic invalid \xff"} {
		file := passwordFileFixture(t, value)
		for _, args := range [][]string{
			f.args("--recipient=", "--password-file", file),
			{"create", "--snapshot", "unused", "--confirm-offline-snapshot", "--out", f.out, "--password-file", file},
		} {
			var output, diagnostics bytes.Buffer
			if err := run(t.Context(), args, &output, &diagnostics); err == nil || strings.Contains(err.Error(), value) || output.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatal("new password policy bypassed or secret leaked")
			}
		}
	}
	file := passwordFileFixture(t, "synthetic password")
	for _, args := range [][]string{
		f.args("--password-file", file),
		{"create", "--snapshot", "unused", "--confirm-offline-snapshot", "--out", f.out, "--recipient", f.identity.Recipient().String(), "--password-file", file},
		{"restore", "--in", "unused", "--out", "unused", "--expected-sha256", strings.Repeat("a", 64), "--identity-file", "unused", "--password-file", file},
		{"restore", "--password=synthetic-do-not-echo"},
	} {
		var output, diagnostics bytes.Buffer
		if err := run(t.Context(), args, &output, &diagnostics); err == nil || strings.Contains(err.Error(), "synthetic-do-not-echo") || output.Len() != 0 || diagnostics.Len() != 0 {
			t.Fatal("mixed password/key flags accepted or value leaked")
		}
	}
}

func TestNewBackupPasswordCountsUnicodeCharacters(t *testing.T) {
	for _, password := range []string{"abcdefgh", "備份密碼測試八字", strings.Repeat("🔐", 8), strings.Repeat("x", 128), strings.Repeat("🔐", 128), "  abcdef  "} {
		if !validNewBackupPassword([]byte(password)) {
			t.Fatal("valid character-count password rejected")
		}
	}
	for _, password := range []string{"abcdefg", strings.Repeat("密", 7), strings.Repeat("🔐", 7), strings.Repeat("x", 129), strings.Repeat("🔐", 129), strings.Repeat("\u0085", 8), "abcdefgh\x00", "abcdefgh\u007f", "abcdefgh\xff"} {
		if validNewBackupPassword([]byte(password)) {
			t.Fatal("invalid character-count password accepted")
		}
	}
}

func TestPasswordCLIRestoreHistoricalShortPassword(t *testing.T) {
	f := newOnlineCLIFixture(t)
	// A historical offline archive can use a shorter password than today's
	// creation policy. Recovery must remain possible without re-encrypting it.
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for _, name := range []string{"data", "mail", "dkim"} {
		if err := os.Mkdir(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database, err := os.ReadFile(f.database)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data", "lanqin.db"), database, 0o600); err != nil {
		t.Fatal(err)
	}
	const password = "old"
	var encrypted bytes.Buffer
	if _, err := backup.WritePassword(t.Context(), source, &encrypted, password, backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "historical.age")
	if err := os.WriteFile(archive, encrypted.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encrypted.Bytes())
	var output, diagnostics bytes.Buffer
	if err := run(t.Context(), []string{"restore", "--in", archive, "--out", filepath.Join(t.TempDir(), "recovered"), "--password-file", passwordFileFixture(t, password), "--expected-sha256", hex.EncodeToString(digest[:])}, &output, &diagnostics); err != nil {
		t.Fatal("historical short-password recovery blocked", err)
	}
}
