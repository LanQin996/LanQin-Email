package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/online"
)

const backupDraftFixtureID = "msg_backup_draft"

type backupDraftTestFixture struct {
	live, frozen       *sql.DB
	dataDir, stagedDir string
	staged             *os.Root
	attachment         string
	content            []byte
}

func backupDraftFixture(t *testing.T, mutate func(*sql.DB, string)) backupDraftTestFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "live.db")
	live, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	live.SetMaxOpenConns(1)
	_, err = live.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE mailboxes(id TEXT PRIMARY KEY);
		CREATE TABLE folders(id TEXT PRIMARY KEY,mailbox_id TEXT,name TEXT,role TEXT);
		CREATE TABLE messages(id TEXT PRIMARY KEY,mailbox_id TEXT,folder_id TEXT,recipient_addr TEXT,message_uid TEXT,message_id TEXT,subject TEXT,
		from_addr TEXT,from_name TEXT,to_addrs TEXT,cc_addrs TEXT,bcc_addrs TEXT,sent_at TEXT,received_at TEXT,snippet TEXT,
		body_text TEXT,body_html TEXT,is_read INTEGER,is_starred INTEGER,raw_path TEXT);
		CREATE TABLE attachments(id TEXT PRIMARY KEY,message_id TEXT,filename TEXT,content_type TEXT,storage_path TEXT,size_bytes INTEGER);
		CREATE TABLE external_imap_messages(local_message_id TEXT);
		CREATE TABLE external_imap_folder_states(local_folder_id TEXT);
		INSERT INTO mailboxes VALUES('mb_backup');
		INSERT INTO folders VALUES('fld_backup','mb_backup','Drafts','drafts');`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = live.Exec(`INSERT INTO messages VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, backupDraftFixtureID, "mb_backup", "fld_backup", "", "uid_fixture", "<draft@example.test>",
		"冻结草稿标题", "sender@example.test", "Synthetic Sender", `["recipient@example.test"]`, `["cc@example.test"]`, `["hidden@example.test"]`,
		"2026-10-29T08:00:00Z", "2026-10-29T08:00:00Z", "synthetic snippet", "frozen draft body", "<p>frozen draft body</p>", 1, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	relative := filepath.Join("attachments", backupDraftFixtureID, "att_fixture_payload.txt")
	content := []byte("synthetic frozen attachment bytes")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dataDir, relative)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, relative), content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = live.Exec(`INSERT INTO attachments VALUES('att_fixture',?,'payload.txt','text/plain',?,?)`, backupDraftFixtureID, filepath.Join(dataDir, relative), len(content))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(live, dataDir)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := online.SnapshotSQLite(t.Context(), dbPath, snapshot, 1<<20); err != nil {
		t.Fatal(err)
	}
	uriPath := filepath.ToSlash(snapshot)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&immutable=1"}
	frozen, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	frozen.SetMaxOpenConns(1)
	t.Cleanup(func() { frozen.Close() })
	stagedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(stagedDir, relative)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, relative), content, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := os.OpenRoot(stagedDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { staged.Close() })
	return backupDraftTestFixture{live: live, frozen: frozen, dataDir: dataDir, stagedDir: stagedDir, staged: staged, attachment: relative, content: content}
}

func TestBackupDraftRebuildUsesFrozenDatabaseAndCapturedAttachments(t *testing.T) {
	f := backupDraftFixture(t, nil)
	if _, err := f.live.Exec(`UPDATE messages SET subject='new live subject',body_text='new live body',body_html='<p>new live HTML</p>',raw_path='a different live MIME'; UPDATE attachments SET size_bytes=1`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dataDir, f.attachment), []byte("new live attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	parser := App{policy: NewHTMLPolicy(), now: time.Now}
	message, attachments, err := parser.parseMaildirMessage(raw, "unused@example.test")
	if err != nil || message.Subject != "冻结草稿标题" || message.BodyText != "frozen draft body" || message.BodyHTML != "<p>frozen draft body</p>" || len(attachments) != 1 {
		t.Fatal("draft reconstruction read a live/old MIME or changed original content", err)
	}
	content, err := attachments[0].contentBytes()
	if err != nil || !bytes.Equal(content, f.content) || attachments[0].Filename != "payload.txt" {
		t.Fatal("draft reconstruction read the live attachment", err)
	}
	if message.MessageID != "<draft@example.test>" || !message.SentAt.Equal(time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)) || len(message.To) != 1 || message.To[0] != "recipient@example.test" {
		t.Fatal("frozen MIME metadata was not preserved")
	}
	var path, body string
	if err := f.frozen.QueryRow(`SELECT raw_path,body_text FROM messages WHERE id=?`, backupDraftFixtureID).Scan(&path, &body); err != nil || path != "" || body != "frozen draft body" {
		t.Fatal("draft reconstruction modified its database", err)
	}
}

func TestBackupDraftRebuildRejectsNonlocalAndInvalidFrozenState(t *testing.T) {
	for _, scenario := range []string{"inbox", "wrong-role", "external-message", "external-folder", "invalid-addresses", "missing-date", "foreign-path", "other-message-path", "size-mismatch", "header-injection", "oversized-body", "oversized-attachment"} {
		t.Run(scenario, func(t *testing.T) {
			f := backupDraftFixture(t, func(db *sql.DB, dataDir string) {
				query, args := "", []any{}
				switch scenario {
				case "inbox":
					query = `UPDATE folders SET name='Inbox',role='inbox'`
				case "wrong-role":
					query = `UPDATE folders SET role='inbox'`
				case "external-message":
					query, args = `INSERT INTO external_imap_messages VALUES(?)`, []any{backupDraftFixtureID}
				case "external-folder":
					query = `INSERT INTO external_imap_folder_states VALUES('fld_backup')`
				case "invalid-addresses":
					query = `UPDATE messages SET to_addrs='{"email":"not an array"}'`
				case "missing-date":
					query = `UPDATE messages SET sent_at='',received_at=''`
				case "foreign-path":
					query, args = `UPDATE attachments SET storage_path=?`, []any{filepath.Join(filepath.Dir(dataDir), "outside-secret.txt")}
				case "other-message-path":
					query, args = `UPDATE attachments SET storage_path=?`, []any{filepath.Join(dataDir, "attachments", "msg_other", "att_fixture_payload.txt")}
				case "size-mismatch":
					query = `UPDATE attachments SET size_bytes=size_bytes+1`
				case "header-injection":
					query, args = `UPDATE attachments SET content_type=?`, []any{"text/plain\r\nX-Injected: true"}
				case "oversized-body":
					query, args = `UPDATE messages SET body_text=?`, []any{strings.Repeat("x", 700000)}
				case "oversized-attachment":
					query = `UPDATE attachments SET size_bytes=2000000`
				}
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			})
			_, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20)
			if err == nil || strings.Contains(err.Error(), f.dataDir) || strings.Contains(err.Error(), "frozen draft body") {
				t.Fatal("invalid draft was accepted or error exposed source data")
			}
			if strings.HasPrefix(scenario, "external-") || scenario == "inbox" || scenario == "wrong-role" {
				if !errors.Is(err, errBackupNotDraft) {
					t.Fatal("local-draft sentinel not returned", err)
				}
			}
		})
	}
}

func TestBackupDraftRebuildDoesNotFallBackToLiveAttachment(t *testing.T) {
	f := backupDraftFixture(t, nil)
	if err := os.Remove(filepath.Join(f.stagedDir, f.attachment)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20); !errors.Is(err, errBackupDraftInvalid) {
		t.Fatal("missing staged file was replaced by live data", err)
	}
}

func TestBackupDraftRebuildPreservesAttachmentFilenameQuotedPairs(t *testing.T) {
	for _, filename := range []string{`report "final" \2026.txt`, `report "final" \\2026.txt`, `report "final".txt`, `附件「最终版」.txt`, "payload.txt"} {
		t.Run(filename, func(t *testing.T) {
			f := backupDraftFixture(t, func(db *sql.DB, _ string) {
				if _, err := db.Exec(`UPDATE attachments SET filename=?`, filename); err != nil {
					t.Fatal(err)
				}
			})
			raw, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20)
			if err != nil {
				t.Fatal("legitimate filename prevented online draft capture", err)
			}
			assertBackupMIMEFilename(t, raw, filename)
			parser := App{policy: NewHTMLPolicy(), now: time.Now}
			_, attachments, err := parser.parseMaildirMessage(raw, "unused@example.test")
			if err != nil || len(attachments) != 1 || attachments[0].Filename != filepath.Base(filename) {
				t.Fatal("reconstructed attachment filename failed roundtrip", err)
			}
			content, err := attachments[0].contentBytes()
			if err != nil || !bytes.Equal(content, f.content) {
				t.Fatal("reconstructed filename changed attachment bytes", err)
			}
		})
	}
}

func TestBackupDraftRebuildRejectsStagedSymlinkAndCancellation(t *testing.T) {
	f := backupDraftFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rebuildBackupDraft(ctx, f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled reconstruction continued", err)
	}
	if _, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 4096); !errors.Is(err, errBackupDraftLimit) {
		t.Fatal("unsafe memory limit accepted", err)
	}
	name := filepath.Join(f.stagedDir, f.attachment)
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.dataDir, f.attachment), name); err != nil {
		t.Skip("symlink privilege unavailable on this host: " + err.Error())
	}
	if _, err := rebuildBackupDraft(t.Context(), f.frozen, backupDraftFixtureID, f.dataDir, f.staged, 1<<20); !errors.Is(err, errBackupDraftInvalid) {
		t.Fatal("staged symlink was followed", err)
	}
}
