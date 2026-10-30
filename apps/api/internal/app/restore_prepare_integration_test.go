package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

func TestRestorePrepareAcceptsCurrentApplicationSchemaAndRuntimeMapping(t *testing.T) {
	a := newTestApp(t)
	a.cfg.MaildirRoot = t.TempDir()
	if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
		t.Fatal(err)
	}
	captured, err := PrepareOnlineBackup(t.Context(), a.cfg, t.TempDir(), DefaultOnlineBackupOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if _, err := backup.WriteOnline(t.Context(), captured.Path, &encrypted, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encrypted.Bytes())
	stage := filepath.Join(t.TempDir(), "stage")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), stage, identity.String(), hex.EncodeToString(hash[:]), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	prepared, err := install.Prepare(t.Context(), stage, t.TempDir(), "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal("healthy current application schema rejected", err)
	}
	defer prepared.Close()
	db, err := openFrozenBackupDB(filepath.Join(prepared.Path, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT raw_path FROM messages WHERE raw_path<>''")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p, "/runtime/mail/") {
			t.Fatal("host path leaked into runtime DB mapping")
		}
	}
	rows.Close()
	if _, err := os.Stat(filepath.Join(stage, "snapshot", "data", "backup-online.json")); err != nil {
		t.Fatal("source stage changed", err)
	}
	if err := checkRestoreActivation(filepath.Join(prepared.Path, "data")); err == nil {
		t.Fatal("prepared data unexpectedly allowed startup")
	}
}
