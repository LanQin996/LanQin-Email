package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
)

type onlineCLITestFixture struct {
	db                              *sql.DB
	database, data, mail, work, out string
	relative, unindexed             string
	raw, candidate                  []byte
	identity                        *age.X25519Identity
}

func newOnlineCLIFixture(t *testing.T) onlineCLITestFixture {
	t.Helper()
	base := t.TempDir()
	f := onlineCLITestFixture{data: filepath.Join(base, "data"), mail: filepath.Join(base, "mail"), work: filepath.Join(base, "work"), out: filepath.Join(base, "snapshot.age")}
	for _, dir := range []string{f.data, f.mail, f.work} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.database = filepath.Join(f.data, "lanqin.db")
	f.relative = filepath.Join("example.test", "synthetic", "Maildir", "new", "1700000000.msg-fixture")
	f.unindexed = filepath.Join("example.test", "synthetic", "Maildir", "new", "1700000001.unindexed")
	f.raw = []byte("From: sender@example.test\r\nTo: synthetic@example.test\r\nSubject: Synthetic online CLI message\r\nMessage-ID: <fixture@example.test>\r\n\r\nSynthetic source mail body\r\n")
	f.candidate = []byte("From: second@example.test\r\nTo: synthetic@example.test\r\nSubject: Unindexed fixture\r\n\r\nSynthetic unindexed delivery\r\n")
	for relative, data := range map[string][]byte{f.relative: f.raw, f.unindexed: f.candidate} {
		name := filepath.Join(f.mail, relative)
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(name, past, past); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.db, err = sql.Open("sqlite", f.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.db.SetMaxOpenConns(1)
	_, err = f.db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE domains(id TEXT PRIMARY KEY,name TEXT,status TEXT,dkim_selector TEXT,dkim_public_key TEXT,dkim_private_key TEXT);
		CREATE TABLE mailboxes(id TEXT PRIMARY KEY,domain_id TEXT,local_part TEXT);
		CREATE TABLE folders(id TEXT PRIMARY KEY,mailbox_id TEXT,role TEXT);
		CREATE TABLE messages(id TEXT PRIMARY KEY,raw_path TEXT,mailbox_id TEXT,recipient_addr TEXT,folder_id TEXT);
		CREATE TABLE attachments(id TEXT PRIMARY KEY,message_id TEXT,storage_path TEXT,size_bytes INTEGER);
		CREATE TABLE external_imap_messages(local_message_id TEXT);
		CREATE TABLE external_imap_folder_states(local_folder_id TEXT);
		INSERT INTO domains VALUES('dom_fixture','example.test','disabled','','','');
		INSERT INTO mailboxes VALUES('mb_fixture','dom_fixture','synthetic');
		INSERT INTO folders VALUES('fld_fixture','mb_fixture','inbox');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO messages VALUES('msg_fixture',?,'mb_fixture','synthetic@example.test','fld_fixture')`, filepath.Join(f.mail, f.relative)); err != nil {
		t.Fatal(err)
	}
	f.identity, err = age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f onlineCLITestFixture) args(extra ...string) []string {
	args := []string{"create-online", "--database", f.database, "--data-dir", f.data, "--maildir-root", f.mail, "--workspace", f.work,
		"--recipient", f.identity.Recipient().String(), "--out", f.out, "--max-bytes", "1048576", "--max-entries", "1000", "--max-mime-bytes", "524288", "--attempts", "1"}
	return append(args, extra...)
}

func TestCreateOnlineCLIActualSQLiteEncryptionAndStagedRecovery(t *testing.T) {
	f := newOnlineCLIFixture(t)
	// An explicit online CLI must not use service configuration or start App.
	t.Setenv("LANQIN_DB_DRIVER", "postgres")
	t.Setenv("LANQIN_DATABASE_URL", "postgres://synthetic-env-secret@unreachable.invalid/never-used")
	t.Setenv("LANQIN_ADMIN_PASSWORD", "synthetic-env-admin-secret")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), f.args(), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(stdout.String()), "sha256=")
	if !ok || len(receipt) != 64 || !strings.Contains(stdout.String(), "messages=1 drafts=0 attachments=0 unindexed=1 dkim_keys=0 attempts=1 external_cached_only=0") {
		t.Fatal("online command did not return receipt and safe counts")
	}
	for _, secret := range []string{f.data, f.mail, f.work, "sender@example.test", "synthetic@example.test", "Synthetic source mail body", "synthetic-env-secret", "synthetic-env-admin-secret", f.identity.String()} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("online CLI exposed private snapshot data")
		}
	}
	ciphertext, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(ciphertext)
	if hex.EncodeToString(digest[:]) != receipt {
		t.Fatal("online command receipt did not cover actual ciphertext")
	}
	restoredDir := filepath.Join(t.TempDir(), "restored")
	manifest, err := backup.Restore(t.Context(), bytes.NewReader(ciphertext), restoredDir, f.identity.String(), receipt, backup.DefaultLimits())
	if err != nil || manifest.Scope != "online-data" {
		t.Fatal("online archive failed real authenticated recovery or has wrong scope", err)
	}
	recovered, err := os.ReadFile(filepath.Join(restoredDir, "snapshot", "mail", f.relative))
	if err != nil || !bytes.Equal(recovered, f.raw) {
		t.Fatal("original indexed MIME changed during online capture", err)
	}
	if _, err := os.Stat(filepath.Join(restoredDir, "snapshot", "mail", f.unindexed)); !os.IsNotExist(err) {
		t.Fatal("unindexed candidate was activated as ordinary mail")
	}
	reportData, err := os.ReadFile(filepath.Join(restoredDir, "snapshot", "data", "backup-online.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report app.OnlineBackupReport
	if err := json.Unmarshal(reportData, &report); err != nil || !report.Complete || report.Summary.Unindexed != 1 {
		t.Fatal("encrypted collection report missing or incomplete", err)
	}
	foundCandidate := false
	for _, file := range report.Files {
		if file.Kind == "unindexed-candidate" {
			foundCandidate = true
			candidate, err := os.ReadFile(filepath.Join(restoredDir, "snapshot", filepath.FromSlash(file.Destination)))
			if err != nil || !bytes.Equal(candidate, f.candidate) {
				t.Fatal("quarantined MIME candidate lost", err)
			}
		}
	}
	if !foundCandidate {
		t.Fatal("unindexed candidate absent from private mapping report")
	}
	var mode string
	if err := f.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("online capture changed or stopped the live database", err)
	}
	entries, err := os.ReadDir(f.work)
	if err != nil || len(entries) != 0 {
		t.Fatal("successful online CLI left plaintext staging files", err)
	}
}

func TestCreateOnlineCLIExternalCachedOnlyRecordPreservesLocalDataWithoutInventingMIME(t *testing.T) {
	f := newOnlineCLIFixture(t)
	relative := filepath.Join("attachments", "msg_external_cached", "att_cached_payload.txt")
	content := []byte("synthetic locally cached attachment")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.data, relative)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, relative), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO messages VALUES('msg_external_cached','','mb_fixture','synthetic@example.test','fld_fixture');
		INSERT INTO external_imap_messages VALUES('msg_external_cached')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO attachments VALUES('att_cached','msg_external_cached',?,?)`, filepath.Join(f.data, relative), len(content)); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), f.args(), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(stdout.String()), "sha256=")
	if !ok || len(receipt) != 64 || !strings.Contains(stdout.String(), "external_cached_only=1") || !strings.Contains(stdout.String(), "not original upstream MIME") {
		t.Fatal("CLI did not disclose the external cached-only scope")
	}
	if strings.Contains(stdout.String()+stderr.String(), "msg_external_cached") || strings.Contains(stdout.String()+stderr.String(), f.data) {
		t.Fatal("cached-only warning exposed private identifiers or paths")
	}
	ciphertext, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(t.TempDir(), "recovered")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(ciphertext), restoredDir, f.identity.String(), receipt, backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(restoredDir, "snapshot")
	recovered, err := os.ReadFile(filepath.Join(staged, "data", relative))
	if err != nil || !bytes.Equal(content, recovered) {
		t.Fatal("external local attachment cache was dropped", err)
	}
	metadata, err := os.ReadFile(filepath.Join(staged, "data", "backup-online.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report app.OnlineBackupReport
	if err := json.Unmarshal(metadata, &report); err != nil || report.Summary.ExternalCachedMessagesOnly != 1 || report.Summary.Attachments != 1 {
		t.Fatal("encrypted report lacks cached-only scope", err)
	}
	for _, file := range report.Files {
		if file.MessageID == "msg_external_cached" && file.Kind != "attachment" {
			t.Fatal("backup invented original MIME for a cached-only external message")
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(staged, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rawPath string
	if err := db.QueryRow(`SELECT raw_path FROM messages WHERE id='msg_external_cached'`).Scan(&rawPath); err != nil || rawPath != "" {
		t.Fatal("cached-only database record was dropped or raw path fabricated", err)
	}
}

func TestCreateOnlineCLIPreflightAndFailureNeverPublishesOutput(t *testing.T) {
	for _, scenario := range []string{"invalid-recipient", "external-driver", "size-limit", "entry-limit", "mime-limit", "attempts-limit", "source-output", "existing-output", "missing-source-file", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newOnlineCLIFixture(t)
			args := f.args()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "invalid-recipient":
				args = f.args("--recipient", "invalid")
			case "external-driver":
				args = f.args("--driver", "postgres")
			case "size-limit":
				args = f.args("--max-bytes", "4095")
			case "entry-limit":
				args = f.args("--max-entries", "100001")
			case "mime-limit":
				args = f.args("--max-mime-bytes", "8191")
			case "attempts-limit":
				args = f.args("--attempts", "4")
			case "source-output":
				f.out = filepath.Join(f.data, "must-not-create.age")
				args = f.args()
			case "existing-output":
				if err := os.WriteFile(f.out, []byte("another owner"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-source-file":
				if err := os.Remove(filepath.Join(f.mail, f.relative)); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			var stdout, stderr bytes.Buffer
			err := run(ctx, args, &stdout, &stderr)
			if err == nil || stdout.Len() != 0 {
				t.Fatal("failed capture returned success / trusted receipt")
			}
			if scenario == "existing-output" {
				data, err := os.ReadFile(f.out)
				if err != nil || string(data) != "another owner" {
					t.Fatal("existing output changed", err)
				}
			} else if _, err := os.Stat(f.out); !os.IsNotExist(err) {
				t.Fatal("failure published output")
			}
			entries, err := os.ReadDir(f.work)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed online capture left plaintext staging files", err)
			}
		})
	}
}
