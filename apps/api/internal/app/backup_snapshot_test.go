package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
)

type onlineSnapshotFixture struct {
	db         *sql.DB
	cfg        Config
	workspace  string
	raw, draft string
	unindexed  string
	attachment string
	rawBytes   []byte
	privateKey []byte
}

func snapshotFixture(t *testing.T) onlineSnapshotFixture {
	t.Helper()
	draft := backupDraftFixture(t, nil)
	db := draft.live
	_, err := db.Exec(`ALTER TABLE mailboxes ADD COLUMN domain_id TEXT;
		ALTER TABLE mailboxes ADD COLUMN local_part TEXT;
		CREATE TABLE domains(id TEXT PRIMARY KEY,name TEXT,status TEXT,dkim_selector TEXT,dkim_public_key TEXT,dkim_private_key TEXT);
		UPDATE mailboxes SET domain_id='domain_fixture',local_part='owner';
		INSERT INTO folders VALUES('fld_inbox','mb_backup','Inbox','inbox');`)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := generateDKIMMaterial()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO domains VALUES('domain_fixture','capture.example.test','active','lanqin',?,?)`, public, private); err != nil {
		t.Fatal(err)
	}
	mail := t.TempDir()
	write := func(relative string, data []byte) string {
		t.Helper()
		name := filepath.Join(mail, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Minute)
		if err := os.Chtimes(name, past, past); err != nil {
			t.Fatal(err)
		}
		return name
	}
	rawBytes := []byte("From: sender@example.test\r\nTo: owner@capture.example.test\r\nSubject: Synthetic received\r\nMessage-ID: <original@example.test>\r\nX-Synthetic-Trace: retain-exactly\r\n\r\noriginal received body\r\n")
	raw := write("capture.example.test/owner/Maildir/new/received-stable", rawBytes)
	oldDraft := write("capture.example.test/owner/Maildir/.Drafts/cur/draft-stable", []byte("Subject: old live draft MIME\r\n\r\nnot the frozen draft body"))
	unindexed := write("capture.example.test/owner/Maildir/new/unindexed", []byte("Subject: not indexed yet\r\n\r\nsynthetic delivery"))
	catchall := write("capture.example.test/__unregistered__/Maildir/new/catchall", []byte("Subject: synthetic catchall\r\n\r\ncatchall bytes"))
	if _, err := db.Exec(`UPDATE messages SET raw_path=? WHERE id=?`, oldDraft, backupDraftFixtureID); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, mailbox, folder, recipient, raw string }{
		{"msg_received", "mb_backup", "fld_inbox", "", raw},
		{"msg_catchall", "", "", "missing@capture.example.test", catchall},
	} {
		_, err := db.Exec(`INSERT INTO messages VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.id, item.mailbox, item.folder, item.recipient, "uid_"+item.id, "<"+item.id+"@example.test>",
			"Synthetic received", "sender@example.test", "Synthetic sender", `["recipient@example.test"]`, `[]`, `[]`, "2026-10-29T08:00:00Z", "2026-10-29T08:00:00Z", "synthetic snippet", "original received body", "", 0, 0, item.raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	var seq int
	var schema, database string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &schema, &database); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(private)
	if err != nil {
		t.Fatal(err)
	}
	return onlineSnapshotFixture{db: db, cfg: Config{DBDriver: "sqlite", DBPath: database, DataDir: draft.dataDir, MaildirRoot: mail}, workspace: t.TempDir(), raw: raw, draft: oldDraft, unindexed: unindexed, attachment: filepath.Join(draft.dataDir, draft.attachment), rawBytes: rawBytes, privateKey: key}
}

func TestPrepareOnlineBackupConcurrentMoveDraftAndRealEncryptedRecovery(t *testing.T) {
	f := snapshotFixture(t)
	// The collector cannot borrow this only live pool connection. The simulated
	// writer still commits while the independently exported DB is being collected.
	writer, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	userFile := filepath.Join(f.workspace, "operator-owned.txt")
	if err := os.WriteFile(userFile, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(f.cfg.MaildirRoot, "capture.example.test", "owner", "Maildir", ".Archive", "cur", "received-stable"+maildirFlagSeparator()+"S")
	hooks := &backupSnapshotHooks{afterSnapshot: func(attempt int, _ string) error {
		if attempt != 1 {
			t.Fatal("unexpected retry")
		}
		if err := os.MkdirAll(filepath.Dir(moved), 0o700); err != nil {
			return err
		}
		if err := os.Rename(f.raw, moved); err != nil {
			return err
		}
		if err := os.WriteFile(f.draft, []byte("newer live MIME, must not be read"), 0o600); err != nil {
			return err
		}
		_, err := writer.ExecContext(t.Context(), `UPDATE messages SET body_text='newer live draft body',subject='new live subject' WHERE id=?;
			UPDATE domains SET dkim_private_key='rotated-live-key';`, backupDraftFixtureID)
		return err
	}}
	prepared, err := prepareOnlineBackup(t.Context(), f.cfg, f.workspace, DefaultOnlineBackupOptions(), hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	summary := prepared.Report.Summary
	if summary.Messages != 3 || summary.Drafts != 1 || summary.Attachments != 1 || summary.Unindexed != 1 || summary.DKIMKeys != 1 || summary.Attempts != 1 {
		t.Fatalf("wrong collection counts: %+v", summary)
	}
	for _, entry := range prepared.Report.Files {
		data, err := os.ReadFile(filepath.Join(prepared.Path, filepath.FromSlash(entry.Destination)))
		hash := sha256.Sum256(data)
		if err != nil || int64(len(data)) != entry.Bytes || hex.EncodeToString(hash[:]) != entry.SHA256 {
			t.Fatal("collection report does not describe actual bytes", entry.Kind, err)
		}
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	if _, err := backup.WriteOnline(t.Context(), prepared.Path, &ciphertext, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(ciphertext.Bytes())
	out := filepath.Join(t.TempDir(), "recovery")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(ciphertext.Bytes()), out, identity.String(), hex.EncodeToString(hash[:]), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(out, "snapshot")
	for _, entry := range prepared.Report.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Destination)))
		hash := sha256.Sum256(data)
		if err != nil || hex.EncodeToString(hash[:]) != entry.SHA256 {
			t.Fatal("recovered file differs", entry.Kind, err)
		}
		if entry.Kind == "message" && entry.MessageID == "msg_received" && !bytes.Equal(data, f.rawBytes) {
			t.Fatal("original RFC822 headers/body changed")
		}
		if entry.Kind == "reconstructed-draft" {
			parser := App{policy: NewHTMLPolicy(), now: time.Now}
			msg, attachments, err := parser.parseMaildirMessage(data, "unused@example.test")
			if err != nil || msg.BodyText != "frozen draft body" || len(attachments) != 1 {
				t.Fatal("draft did not use frozen state and captured attachments", err)
			}
		}
	}
	key, err := os.ReadFile(filepath.Join(root, "dkim", "capture.example.test.lanqin.key"))
	if err != nil || !bytes.Equal(key, f.privateKey) {
		t.Fatal("live key rotation changed frozen DKIM material", err)
	}
	rel, err := backupRelativePath(f.cfg.MaildirRoot, f.unindexed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "mail", filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unindexed candidate was automatically activated")
	}
	frozen, err := openFrozenBackupDB(filepath.Join(root, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	var body string
	err = frozen.QueryRow(`SELECT body_text FROM messages WHERE id=?`, backupDraftFixtureID).Scan(&body)
	frozen.Close()
	if err != nil || body != "frozen draft body" {
		t.Fatal("recovered DB is not frozen generation", err)
	}
	owned := prepared.Path
	prepared.Path = f.cfg.DataDir // Close must use its private captured path.
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal("Close is not idempotent", err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private plaintext workspace left behind")
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "must remain" {
		t.Fatal("cleanup removed operator-owned data", err)
	}
	if _, err := os.Stat(f.attachment); err != nil {
		t.Fatal("cleanup removed source attachment", err)
	}
}

func TestPrepareOnlineBackupRetriesScanReplacementAndDeletedAttachment(t *testing.T) {
	for _, scenario := range []string{"same-size-replacement", "deleted-attachment"} {
		t.Run(scenario, func(t *testing.T) {
			f := snapshotFixture(t)
			hooks := &backupSnapshotHooks{}
			if scenario == "same-size-replacement" {
				hooks.afterInventory = func(attempt int) error {
					if attempt != 1 {
						return nil
					}
					before, err := os.Stat(f.raw)
					if err != nil {
						return err
					}
					data := append([]byte(nil), f.rawBytes...)
					data[len(data)-3] ^= 1
					name := filepath.Join(filepath.Dir(f.raw), "replacement-tmp")
					if err := os.WriteFile(name, data, 0o600); err != nil {
						return err
					}
					if err := os.Chtimes(name, before.ModTime(), before.ModTime()); err != nil {
						return err
					}
					if err := os.Rename(name, f.raw); err != nil {
						return err
					}
					_, err = f.db.Exec(`UPDATE messages SET body_text='new live generation' WHERE id='msg_received'`)
					return err
				}
			} else {
				hooks.afterSnapshot = func(attempt int, _ string) error {
					if attempt != 1 {
						return nil
					}
					if err := os.Remove(f.attachment); err != nil {
						return err
					}
					_, err := f.db.Exec(`DELETE FROM attachments`)
					return err
				}
			}
			prepared, err := prepareOnlineBackup(t.Context(), f.cfg, f.workspace, DefaultOnlineBackupOptions(), hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if prepared.Report.Summary.Attempts != 2 {
				t.Fatal("a mixed generation was accepted instead of restarting from SQLite")
			}
			entries, err := os.ReadDir(f.workspace)
			if err != nil || len(entries) != 1 {
				t.Fatal("failed private attempt not discarded", err)
			}
			if scenario == "deleted-attachment" && prepared.Report.Summary.Attachments != 0 {
				t.Fatal("fresh snapshot did not use current attachment references")
			}
		})
	}
}

func TestPrepareOnlineBackupFailureBoundsAndSourceOwnership(t *testing.T) {
	for _, scenario := range []string{"missing-attachment", "missing-raw", "foreign-mailbox", "foreign-attachment", "entry-limit", "byte-limit", "mime-limit", "source-workspace", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := snapshotFixture(t)
			opts := DefaultOnlineBackupOptions()
			opts.Attempts = 2
			workspace := f.workspace
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "missing-attachment":
				if err := os.Remove(f.attachment); err != nil {
					t.Fatal(err)
				}
			case "missing-raw":
				if err := os.Remove(f.raw); err != nil {
					t.Fatal(err)
				}
			case "foreign-mailbox":
				_, err := f.db.Exec(`UPDATE messages SET raw_path=? WHERE id='msg_received'`, filepath.Join(f.cfg.MaildirRoot, "capture.example.test", "other-user", "Maildir", "new", "received-stable"))
				if err != nil {
					t.Fatal(err)
				}
			case "foreign-attachment":
				_, err := f.db.Exec(`UPDATE attachments SET storage_path=?`, filepath.Join(t.TempDir(), "outside.txt"))
				if err != nil {
					t.Fatal(err)
				}
			case "entry-limit":
				opts.MaxEntries = 4
			case "byte-limit":
				opts.MaxBytes = 4096
			case "mime-limit":
				opts.MaxMIMEBytes = 8192
				if _, err := f.db.Exec(`UPDATE messages SET body_text=? WHERE id=?`, strings.Repeat("x", 8192), backupDraftFixtureID); err != nil {
					t.Fatal(err)
				}
			case "source-workspace":
				workspace = f.cfg.DataDir
			case "cancelled":
				cancel()
			}
			prepared, err := PrepareOnlineBackup(ctx, f.cfg, workspace, opts)
			if err == nil || prepared != nil {
				t.Fatal("unsafe/incomplete snapshot accepted")
			}
			entries, err := os.ReadDir(f.workspace)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed collection left private plaintext", err)
			}
			if _, err := os.Stat(f.cfg.DataDir); err != nil {
				t.Fatal("source directory removed", err)
			}
		})
	}
}

func TestPrepareOnlineBackupExplicitRootAliasesAndReportContract(t *testing.T) {
	f := snapshotFixture(t)
	aliasParent := t.TempDir()
	dataAlias, mailAlias := filepath.Join(aliasParent, "data-alias"), filepath.Join(aliasParent, "mail-alias")
	if err := os.Symlink(f.cfg.DataDir, dataAlias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(f.cfg.MaildirRoot, mailAlias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE attachments SET storage_path=replace(storage_path,?,?)`, f.cfg.DataDir, dataAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE messages SET raw_path=replace(raw_path,?,?)`, f.cfg.MaildirRoot, mailAlias); err != nil {
		t.Fatal(err)
	}
	f.cfg.DataDir, f.cfg.MaildirRoot = dataAlias, mailAlias
	prepared, err := PrepareOnlineBackup(t.Context(), f.cfg, f.workspace, DefaultOnlineBackupOptions())
	if err != nil {
		t.Fatal("configured alias was confused with a forbidden child symlink", err)
	}
	defer prepared.Close()
	data, err := os.ReadFile(filepath.Join(prepared.Path, "data", "backup-online.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report OnlineBackupReport
	if err := json.Unmarshal(data, &report); err != nil || !report.Complete || report.Scope != "sqlite-reference-closure" || report.Sources.Data != dataAlias || len(report.Files) != len(prepared.Report.Files) {
		t.Fatal("report contract incorrect", err)
	}
}

func TestPrepareOnlineBackupFutureInternalDateIsNotAWriteTimestamp(t *testing.T) {
	f := snapshotFixture(t)
	future := time.Date(2035, 6, 7, 8, 9, 10, 0, time.UTC)
	if err := os.Chtimes(f.raw, future, future); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareOnlineBackup(t.Context(), f.cfg, f.workspace, DefaultOnlineBackupOptions())
	if err != nil {
		t.Fatal("legitimate future IMAP INTERNALDATE prevented backup", err)
	}
	defer prepared.Close()
	found := false
	for _, file := range prepared.Report.Files {
		if file.MessageID == "msg_received" {
			found = file.InternalDate != nil && file.InternalDate.Equal(future)
		}
	}
	if !found {
		t.Fatal("original INTERNALDATE omitted from encrypted recovery metadata")
	}
}

func TestPreparedOnlineBackupCloseRefusesReplacementDirectory(t *testing.T) {
	f := snapshotFixture(t)
	prepared, err := PrepareOnlineBackup(t.Context(), f.cfg, f.workspace, DefaultOnlineBackupOptions())
	if err != nil {
		t.Fatal(err)
	}
	original := prepared.Path
	moved := original + ".moved-by-owner"
	if !backupPathWithin(f.workspace, moved) {
		t.Fatal("test target escaped private workspace")
	}
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(original, "replacement-owned.txt")
	if err := os.WriteFile(file, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err == nil {
		t.Fatal("cleanup accepted a replacement directory identity")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "do not delete" {
		t.Fatal("cleanup deleted data it did not create", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "data", "lanqin.db")); err != nil {
		t.Fatal("original private staging directory was altered", err)
	}
}
