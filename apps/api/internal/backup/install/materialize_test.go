package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/online"
)

func TestPrepareIndependentlyExcludesRestoredBackupRuntime(t *testing.T) {
	f := newMaterializeFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE system_settings("key" TEXT PRIMARY KEY,value TEXT);
	INSERT INTO system_settings VALUES('backup.queue.v1','opaque'),('backup.schedules.v1','opaque'),('backup.job.v1.old','opaque'),('backup.drive.oauth.v1.actor','opaque'),('backup.destinations.v1','keep'),('backup.drive.oauth.v10.keep','keep');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	f.rehashDatabase(t)
	p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	copy, err := sql.Open("sqlite", filepath.Join(p.Path, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var count int
	if err := copy.QueryRow(`SELECT count(*) FROM system_settings`).Scan(&count); err != nil || count != 2 {
		t.Fatal("imported backup jobs/plans/oauth could replay", count, err)
	}
	planBytes, err := os.ReadFile(filepath.Join(p.Path, "data", "restore-install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan recoveryPlan
	if err := json.Unmarshal(planBytes, &plan); err != nil || plan.BackupRuntimeSettingsRemoved != 4 {
		t.Fatal("runtime cleanup missing from persistent plan", err)
	}
}

type materializeFixture struct {
	container, stage, workspace string
	report                      collectionReport
}

func newMaterializeFixture(t *testing.T) materializeFixture {
	t.Helper()
	base := t.TempDir()
	f := materializeFixture{container: filepath.Join(base, "restored"), stage: filepath.Join(base, "restored", "snapshot"), workspace: filepath.Join(base, "workspace")}
	for _, p := range []string{f.workspace, filepath.Join(f.stage, "data", "attachments", "m"), filepath.Join(f.stage, "mail", "example.test", "user", "Maildir", "cur"), filepath.Join(f.stage, "mail", "example.test", "user", "Maildir", ".Drafts", "cur"), filepath.Join(f.stage, "data", "backup-unindexed"), filepath.Join(f.stage, "dkim")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.container, "READY"), []byte("Synthetic authenticated restore fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
	CREATE TABLE messages(id TEXT PRIMARY KEY,raw_path TEXT NOT NULL,folder_id TEXT,subject TEXT DEFAULT '',from_addr TEXT DEFAULT '',from_name TEXT DEFAULT '',to_addrs TEXT DEFAULT '',recipient_addr TEXT DEFAULT '',snippet TEXT DEFAULT '',body_text TEXT DEFAULT '');
	CREATE TABLE attachments(id TEXT PRIMARY KEY,message_id TEXT REFERENCES messages(id),storage_path TEXT NOT NULL,size_bytes INTEGER);
	CREATE TABLE external_imap_messages(local_message_id TEXT);
	CREATE TABLE external_imap_folder_states(local_folder_id TEXT);
	CREATE TABLE send_queue(id TEXT PRIMARY KEY,status TEXT,mime_base64 TEXT,attempt_count INTEGER,max_attempts INTEGER,lease_token TEXT);
	CREATE TABLE scheduled_sends(id TEXT PRIMARY KEY,status TEXT,payload_json TEXT,send_at TEXT);
	CREATE TABLE delivery_events(queue_id TEXT);
	INSERT INTO messages(id,raw_path,folder_id) VALUES('m','/old/mail/example.test/user/Maildir/cur/m.eml','inbox'),('draft','','drafts'),('external','','external-folder');
	INSERT INTO external_imap_messages VALUES('external');
	INSERT INTO attachments VALUES('a','m','/old/data/attachments/m/a_file.txt',7);
	INSERT INTO send_queue VALUES('q','queued','synthetic-payload',0,5,'lease'),('s','sending','synthetic-payload',1,5,'lease'),('f','failed','synthetic-payload',1,5,''),('done','delivered','',1,5,''),('c','canceled','preserved',0,5,'');
	INSERT INTO scheduled_sends VALUES('p','pending','synthetic-schedule','2000-01-01T00:00:00Z'),('s','sending','synthetic-schedule','2000-01-01T00:00:00Z'),('done','sent','history','2000-01-01T00:00:00Z');
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, stmt := range knownTriggers {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f.report.Version, f.report.Scope, f.report.Complete = 1, "sqlite-reference-closure", true
	f.report.Summary = json.RawMessage(`{}`)
	f.report.Sources.Data, f.report.Sources.Mail = "/old/data", "/old/mail"
	date := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	items := []struct {
		kind, name, message, attachment, data string
		date                                  *time.Time
	}{
		{"database", "data/lanqin.db", "", "", "", nil},
		{"attachment", "data/attachments/m/a_file.txt", "m", "a", "example", nil},
		{"message", "mail/example.test/user/Maildir/cur/m.eml", "m", "", "Subject: Synthetic\r\n\r\nMail\r\n", &date},
		{"reconstructed-draft", "mail/example.test/user/Maildir/.Drafts/cur/d.eml", "draft", "", "Subject: Synthetic draft\r\n\r\nDraft\r\n", nil},
		{"unindexed-candidate", "data/backup-unindexed/candidate.eml", "", "", "Subject: Quarantined\r\n\r\nMail\r\n", &date},
		{"dkim", "dkim/example.test.key", "", "", "synthetic-not-a-real-key", nil},
	}
	for _, item := range items {
		name := filepath.Join(f.stage, filepath.FromSlash(item.name))
		if item.data != "" {
			if err := os.WriteFile(name, []byte(item.data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		f.report.Files = append(f.report.Files, reportFile{Kind: item.kind, MessageID: item.message, AttachmentID: item.attachment, InternalDate: item.date, CollectedFile: online.CollectedFile{Destination: item.name, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(data))}})
	}
	f.writeReport(t)
	return f
}

func (f materializeFixture) writeReport(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stage, filepath.FromSlash(reportName)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *materializeFixture) rehashDatabase(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	f.report.Files[0].SHA256, f.report.Files[0].Bytes = hex.EncodeToString(hash[:]), int64(len(data))
	f.writeReport(t)
}

func (f *materializeFixture) addRuntimeTables(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
	CREATE TABLE users(id TEXT PRIMARY KEY);
	INSERT INTO users VALUES('synthetic-user');
	CREATE TABLE sessions(id TEXT PRIMARY KEY,user_id TEXT REFERENCES users(id),token_hash TEXT,expires_at TEXT,created_at TEXT);
	INSERT INTO sessions VALUES('session-a','synthetic-user','synthetic-hash-a','9999-01-01T00:00:00Z','2020-01-01T00:00:00Z'),('session-b','synthetic-user','synthetic-hash-b','9999-01-01T00:00:00Z','2020-01-01T00:00:00Z');
	CREATE TABLE login_challenges(id TEXT PRIMARY KEY,user_id TEXT REFERENCES users(id),token_hash TEXT,expires_at TEXT,created_at TEXT);
	INSERT INTO login_challenges VALUES('challenge-a','synthetic-user','synthetic-hash-c','9999-01-01T00:00:00Z','2020-01-01T00:00:00Z');
	CREATE TABLE api_tokens(id TEXT PRIMARY KEY,user_id TEXT REFERENCES users(id),name TEXT,token_hash TEXT,last_used_at TEXT,expires_at TEXT,disabled INTEGER NOT NULL DEFAULT 0,scopes_json TEXT,created_at TEXT,updated_at TEXT);
	INSERT INTO api_tokens VALUES('active-token','synthetic-user','synthetic API integration','synthetic-token-hash',NULL,'9999-01-01T00:00:00Z',0,'["mail:read"]','2020-01-01T00:00:00Z','2020-01-01T00:00:00Z'),('disabled-token','synthetic-user','previously disabled','synthetic-disabled-hash',NULL,'9999-01-01T00:00:00Z',1,'[]','2020-01-01T00:00:00Z','2020-01-01T00:00:00Z');
	CREATE TABLE local_delivery_jobs(id VARCHAR(64) PRIMARY KEY,source_id VARCHAR(64) NOT NULL,target_mailbox_id VARCHAR(64) NOT NULL,payload TEXT NOT NULL,status VARCHAR(16) NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt_at VARCHAR(35) NOT NULL,lease_token VARCHAR(64) NOT NULL DEFAULT '',lease_until VARCHAR(35) NOT NULL DEFAULT '',message_id VARCHAR(64) NOT NULL DEFAULT '',last_error VARCHAR(255) NOT NULL DEFAULT '',created_at VARCHAR(35) NOT NULL,updated_at VARCHAR(35) NOT NULL,UNIQUE(source_id,target_mailbox_id));
	INSERT INTO local_delivery_jobs(id,source_id,target_mailbox_id,payload,status,next_attempt_at,lease_token,lease_until,created_at,updated_at) VALUES
	('local-q','source-q','mailbox','synthetic-local-queued','queued','2000-01-01T00:00:00Z','old-lease','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z'),
	('local-s','source-s','mailbox','synthetic-local-sending','sending','2000-01-01T00:00:00Z','old-lease','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z'),
	('local-d','source-d','mailbox','','delivered','2000-01-01T00:00:00Z','','','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z'),
	('local-f','source-f','mailbox','','failed','2000-01-01T00:00:00Z','','','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z'),
	('local-paused','source-p','mailbox','already-paused','restore_paused','2000-01-01T00:00:00Z','','','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"status_webhook_outbox", "telegram_notification_outbox"} {
		_, err := db.Exec("CREATE TABLE " + table + `(id TEXT PRIMARY KEY,payload_json TEXT NOT NULL,attempt_count INTEGER NOT NULL DEFAULT 0,next_attempt_at TEXT NOT NULL,last_error TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,delivered_at TEXT,lease_owner TEXT NOT NULL DEFAULT '',lease_token TEXT NOT NULL DEFAULT '',lease_until TEXT)`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("INSERT INTO " + table + `(id,payload_json,attempt_count,next_attempt_at,created_at,updated_at,delivered_at,lease_owner,lease_token,lease_until) VALUES
		('pending','synthetic-notification-payload',1,'2000-01-01T00:00:00Z','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z',NULL,'old-owner','old-token','2000-01-01T00:00:00Z'),
		('delivered','synthetic-delivered-history',1,'2000-01-01T00:00:00Z','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z','2000-01-02T00:00:00Z','','',NULL)`)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f.rehashDatabase(t)
}

func TestPrepareRevokesAuthenticationAndHoldsRuntimeWork(t *testing.T) {
	f := newMaterializeFixture(t)
	f.addRuntimeTables(t)
	p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	db, err := sql.Open("sqlite", filepath.Join(p.Path, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	for _, table := range []string{"sessions", "login_challenges"} {
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s not revoked: %d %v", table, count, err)
		}
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE disabled IS NOT 1").Scan(&count); err != nil || count != 0 {
		t.Fatalf("tokens active: %d %v", count, err)
	}
	var name, hash, scopes, updated string
	if err := db.QueryRow("SELECT name,token_hash,scopes_json,updated_at FROM api_tokens WHERE id='active-token'").Scan(&name, &hash, &scopes, &updated); err != nil || name != "synthetic API integration" || hash != "synthetic-token-hash" || scopes != `["mail:read"]` || updated != "2020-01-01T00:00:00Z" {
		t.Fatalf("token metadata lost: %v", err)
	}
	for _, item := range []struct{ id, payload string }{{"local-q", "synthetic-local-queued"}, {"local-s", "synthetic-local-sending"}} {
		var status, payload, lease string
		if err := db.QueryRow("SELECT status,payload,lease_token FROM local_delivery_jobs WHERE id=?", item.id).Scan(&status, &payload, &lease); err != nil || status != "restore_paused" || payload != item.payload || lease != "old-lease" {
			t.Fatalf("local job replay/payload loss: %q %q %q %v", status, payload, lease, err)
		}
	}
	now := "2026-10-31T00:00:00Z"
	// These predicates match local_delivery.go's due selection and cleanup.
	if err := db.QueryRow("SELECT COUNT(*) FROM local_delivery_jobs WHERE (status='queued' AND next_attempt_at<=?) OR (status='sending' AND lease_until<=?)", now, now).Scan(&count); err != nil || count != 0 {
		t.Fatalf("local worker sees restored work: %d %v", count, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM local_delivery_jobs WHERE status='restore_paused' AND status IN ('delivered','failed') AND updated_at<?", now).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cleanup sees paused work: %d %v", count, err)
	}
	for _, table := range []string{"status_webhook_outbox", "telegram_notification_outbox"} {
		var payload, next, lease string
		if err := db.QueryRow("SELECT payload_json,next_attempt_at,lease_token FROM "+table+" WHERE id='pending'").Scan(&payload, &next, &lease); err != nil || payload != "synthetic-notification-payload" || next != "9999-12-31T23:59:59Z" || lease != "old-token" {
			t.Fatalf("notification loss: %q %q %q %v", payload, next, lease, err)
		}
		// Both notification workers and their lease claim require this predicate.
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE delivered_at IS NULL AND attempt_count<? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?)", 20, now, now).Scan(&count); err != nil || count != 0 {
			t.Fatalf("outbox due: %d %v", count, err)
		}
		res, err := db.Exec("UPDATE "+table+" SET attempt_count=attempt_count+1 WHERE id='pending' AND delivered_at IS NULL AND attempt_count<? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?)", 20, now, now)
		if err != nil {
			t.Fatal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil || affected != 0 {
			t.Fatal("held outbox could be claimed")
		}
		if err := db.QueryRow("SELECT next_attempt_at FROM " + table + " WHERE id='delivered'").Scan(&next); err != nil || next != "2000-01-01T00:00:00Z" {
			t.Fatal("terminal outbox history modified")
		}
	}
	var plan recoveryPlan
	data, err := os.ReadFile(filepath.Join(p.Path, "INSTALL-PLAN.json"))
	if err != nil || json.Unmarshal(data, &plan) != nil || plan.SessionsRevoked != 2 || plan.LoginChallengesRevoked != 1 || plan.APITokensDisabled != 1 || plan.LocalDelivery["queued"] != 1 || plan.LocalDelivery["sending"] != 1 || plan.WebhookNotificationsHeld != 1 || plan.TelegramNotificationsHeld != 1 {
		t.Fatalf("runtime plan counts: %s %v", data, err)
	}
	if bytes.Contains(data, []byte("synthetic-token-hash")) || bytes.Contains(data, []byte("synthetic-local")) {
		t.Fatal("private payload leaked into plan")
	}
	source, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil || count != 2 {
		t.Fatal("source sessions modified")
	}
	if err := source.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE disabled=0").Scan(&count); err != nil || count != 1 {
		t.Fatal("source tokens modified")
	}
	if err := source.QueryRow("SELECT COUNT(*) FROM local_delivery_jobs WHERE status IN ('queued','sending')").Scan(&count); err != nil || count != 2 {
		t.Fatal("source local delivery modified")
	}
	for _, table := range []string{"status_webhook_outbox", "telegram_notification_outbox"} {
		if err := source.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE delivered_at IS NULL AND next_attempt_at='2000-01-01T00:00:00Z'").Scan(&count); err != nil || count != 1 {
			t.Fatal("source outbox modified")
		}
	}
}

func TestPrepareRuntimeTablesRejectUnknownTriggersWithoutWrites(t *testing.T) {
	for _, item := range []struct{ table, operation string }{{"sessions", "DELETE"}, {"login_challenges", "DELETE"}, {"api_tokens", "UPDATE"}, {"local_delivery_jobs", "UPDATE"}, {"status_webhook_outbox", "UPDATE"}, {"telegram_notification_outbox", "UPDATE"}} {
		t.Run(item.table, func(t *testing.T) {
			f := newMaterializeFixture(t)
			f.addRuntimeTables(t)
			file := filepath.Join(f.stage, "data", "lanqin.db")
			db, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("CREATE TABLE sentinel(value TEXT); INSERT INTO sentinel VALUES('untouched'); CREATE TRIGGER malicious AFTER " + item.operation + " ON " + item.table + " BEGIN UPDATE sentinel SET value='trigger-executed'; END")
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.rehashDatabase(t)
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(t.TempDir(), "candidate.db")
			if err := os.WriteFile(candidate, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := remapDatabase(t.Context(), candidate, f.report, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
				t.Fatal("unknown trigger accepted")
			}
			after, err := os.ReadFile(candidate)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected schema executed a write")
			}
			if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
				p.Close()
				t.Fatal("unsafe runtime trigger accepted")
			}
		})
	}
}

func TestPrepareRuntimeTablesRejectUnknownLocalStatus(t *testing.T) {
	f := newMaterializeFixture(t)
	f.addRuntimeTables(t)
	db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE local_delivery_jobs SET status='unexpected' WHERE id='local-q'")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.rehashDatabase(t)
	if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
		p.Close()
		t.Fatal("unknown local-delivery status accepted")
	}
}

func TestPrepareAuthenticatedOnlineArchive(t *testing.T) {
	f := newMaterializeFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if _, err := backup.WriteOnline(t.Context(), f.stage, &encrypted, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	receipt := sha256.Sum256(encrypted.Bytes())
	container := filepath.Join(filepath.Dir(f.container), "authenticated")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), container, identity.String(), hex.EncodeToString(receipt[:]), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	sourceReport, err := os.ReadFile(filepath.Join(f.stage, filepath.FromSlash(reportName)))
	if err != nil {
		t.Fatal(err)
	}
	renamedReport, err := os.ReadFile(filepath.Join(prepared.Path, filepath.FromSlash(sourceReportName)))
	if err != nil || !bytes.Equal(sourceReport, renamedReport) {
		t.Fatalf("original report not preserved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(prepared.Path, filepath.FromSlash(reportName))); !os.IsNotExist(err) {
		t.Fatal("prepared tree still presents an original online backup report")
	}
	marker, err := os.ReadFile(filepath.Join(prepared.Path, filepath.FromSlash(preparedMarkerName)))
	if err != nil || string(marker) != preparedMarkerContent {
		t.Fatalf("missing activation guard: %q %v", marker, err)
	}
	if prepared.Path == f.stage || !within(f.workspace, prepared.Path) {
		t.Fatal("not an isolated preparation")
	}
	db, err := sql.Open("sqlite", filepath.Join(prepared.Path, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw, storage, payload, status string
	if err := db.QueryRow("SELECT raw_path FROM messages WHERE id='m'").Scan(&raw); err != nil || raw != "/runtime/mail/example.test/user/Maildir/cur/m.eml" {
		t.Fatalf("raw mapping: %q %v", raw, err)
	}
	if err := db.QueryRow("SELECT raw_path FROM messages WHERE id='draft'").Scan(&raw); err != nil || raw != "/runtime/mail/example.test/user/Maildir/.Drafts/cur/d.eml" {
		t.Fatalf("draft mapping: %q %v", raw, err)
	}
	if err := db.QueryRow("SELECT raw_path FROM messages WHERE id='external'").Scan(&raw); err != nil || raw != "" {
		t.Fatalf("external cache activated: %q %v", raw, err)
	}
	if err := db.QueryRow("SELECT storage_path FROM attachments WHERE id='a'").Scan(&storage); err != nil || storage != "/runtime/data/attachments/m/a_file.txt" {
		t.Fatalf("attachment mapping: %q %v", storage, err)
	}
	for _, id := range []string{"q", "s", "f"} {
		if err := db.QueryRow("SELECT status,mime_base64 FROM send_queue WHERE id=?", id).Scan(&status, &payload); err != nil || status != "canceled" || payload != "synthetic-payload" {
			t.Fatalf("send replay/payload loss: %q %q %v", status, payload, err)
		}
	}
	for _, id := range []string{"p", "s"} {
		if err := db.QueryRow("SELECT status,payload_json FROM scheduled_sends WHERE id=?", id).Scan(&status, &payload); err != nil || status != "cancelled" || payload != "synthetic-schedule" {
			t.Fatalf("schedule replay/payload loss: %q %q %v", status, payload, err)
		}
	}
	info, err := os.Stat(filepath.Join(prepared.Path, "mail", "example.test", "user", "Maildir", "cur", "m.eml"))
	if err != nil || !info.ModTime().Equal(*f.report.Files[2].InternalDate) {
		t.Fatalf("INTERNALDATE lost: %v", err)
	}
	var plan recoveryPlan
	data, err := os.ReadFile(filepath.Join(prepared.Path, "INSTALL-PLAN.json"))
	if err != nil || json.Unmarshal(data, &plan) != nil || plan.SendQueue["queued"] != 1 || plan.Scheduled["sending"] != 1 || plan.Unindexed != 1 {
		t.Fatalf("plan: %s %v", data, err)
	}
	if bytes.Contains(data, []byte("old/data")) || bytes.Contains(data, []byte("synthetic-payload")) {
		t.Fatal("plan leaked private source/payload")
	}
	sourceDB, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceDB.Close()
	if err := sourceDB.QueryRow("SELECT status FROM send_queue WHERE id='q'").Scan(&status); err != nil || status != "queued" {
		t.Fatal("source modified")
	}
	if _, err := os.Stat(filepath.Join(prepared.Path, "mail", "candidate.eml")); !os.IsNotExist(err) {
		t.Fatal("candidate promoted")
	}
}

func TestPrepareBudgetsNewMetadataAndRejectsImportedActivationFiles(t *testing.T) {
	t.Run("metadata-limits", func(t *testing.T) {
		f := newMaterializeFixture(t)
		p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		entries := 0
		err = fs.WalkDir(os.DirFS(p.Path), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if name == "." {
				return nil
			}
			entries++
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				total += info.Size()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		for _, limits := range []backup.Limits{{MaxBytes: total - 1, MaxEntries: backup.DefaultLimits().MaxEntries}, {MaxBytes: backup.DefaultLimits().MaxBytes, MaxEntries: entries - 1}} {
			p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", limits)
			if err == nil {
				p.Close()
				t.Fatal("new activation metadata exceeded budget")
			}
			left, err := os.ReadDir(f.workspace)
			if err != nil || len(left) != 0 {
				t.Fatalf("limited preparation left output: %v %v", left, err)
			}
		}
		if _, err := os.Stat(filepath.Join(f.stage, filepath.FromSlash(reportName))); err != nil {
			t.Fatal("budget failure removed original report")
		}
		if _, err := os.Stat(filepath.Join(f.stage, filepath.FromSlash(preparedMarkerName))); !os.IsNotExist(err) {
			t.Fatal("budget failure wrote source marker")
		}
	})
	for _, name := range []string{sourceReportName, preparedMarkerName} {
		t.Run(name, func(t *testing.T) {
			f := newMaterializeFixture(t)
			if err := os.WriteFile(filepath.Join(f.stage, filepath.FromSlash(name)), []byte("must-not-overwrite"), 0o600); err != nil {
				t.Fatal(err)
			}
			if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
				p.Close()
				t.Fatal("imported activation metadata accepted")
			}
		})
	}
}

func TestPrepareRejectsUnsafeOrIncompleteStaging(t *testing.T) {
	tests := map[string]func(*testing.T, *materializeFixture){
		"missing-ready": func(t *testing.T, f *materializeFixture) {
			if err := os.Remove(filepath.Join(f.container, "READY")); err != nil {
				t.Fatal(err)
			}
		},
		"missing-report": func(t *testing.T, f *materializeFixture) {
			if err := os.Remove(filepath.Join(f.stage, filepath.FromSlash(reportName))); err != nil {
				t.Fatal(err)
			}
		},
		"incomplete": func(t *testing.T, f *materializeFixture) { f.report.Complete = false; f.writeReport(t) },
		"unknown-extra": func(t *testing.T, f *materializeFixture) {
			if err := os.WriteFile(filepath.Join(f.stage, "data", "extra"), []byte("extra"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"tampered": func(t *testing.T, f *materializeFixture) {
			if err := os.WriteFile(filepath.Join(f.stage, "data", "attachments", "m", "a_file.txt"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"duplicate": func(t *testing.T, f *materializeFixture) {
			f.report.Files = append(f.report.Files, f.report.Files[1])
			f.writeReport(t)
		},
		"report-self": func(t *testing.T, f *materializeFixture) {
			f.report.Files[0].Destination = reportName
			f.writeReport(t)
		},
		"traversal": func(t *testing.T, f *materializeFixture) {
			f.report.Files[1].Destination = "data/../escape"
			f.writeReport(t)
		},
		"wrong-root":    func(t *testing.T, f *materializeFixture) { f.report.Sources.Data = "/old/dat"; f.writeReport(t) },
		"wrong-message": func(t *testing.T, f *materializeFixture) { f.report.Files[2].MessageID = "absent"; f.writeReport(t) },
		"sidecar": func(t *testing.T, f *materializeFixture) {
			if err := os.WriteFile(filepath.Join(f.stage, "data", "lanqin.db-wal"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"trigger": func(t *testing.T, f *materializeFixture) {
			db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("CREATE TRIGGER malicious AFTER UPDATE OF raw_path ON messages BEGIN DELETE FROM attachments; END")
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.rehashDatabase(t)
		},
		"spoofed-known-trigger": func(t *testing.T, f *materializeFixture) {
			db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("DROP TRIGGER messages_fts_au; CREATE TRIGGER messages_fts_au AFTER UPDATE ON messages BEGIN DELETE FROM attachments; END")
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.rehashDatabase(t)
		},
		"unknown-work-state": func(t *testing.T, f *materializeFixture) {
			db, err := sql.Open("sqlite", filepath.Join(f.stage, "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("UPDATE send_queue SET status='unknown'")
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.rehashDatabase(t)
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newMaterializeFixture(t)
			change(t, &f)
			p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
			if err == nil {
				p.Close()
				t.Fatal("unsafe staging accepted")
			}
			entries, err := os.ReadDir(f.workspace)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed preparation left files: %v %v", entries, err)
			}
		})
	}
}

func TestPrepareRuntimeRootsLimitsAndCancellation(t *testing.T) {
	f := newMaterializeFixture(t)
	for _, root := range []string{"relative", "/", "C:/data", "/runtime/../data", "/runtime//data", "/data/", "/data\n", "/runtime\\data"} {
		if p, err := Prepare(t.Context(), f.container, f.workspace, root, "/runtime/mail", backup.DefaultLimits()); err == nil {
			p.Close()
			t.Fatalf("unsafe runtime root accepted: %q", root)
		}
	}
	for _, limits := range []backup.Limits{{MaxBytes: 1, MaxEntries: 100}, {MaxBytes: 1 << 30, MaxEntries: 3}} {
		if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", limits); err == nil {
			p.Close()
			t.Fatal("limits ignored")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Prepare(ctx, f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestPrepareRejectsSymlinksAndMaterializesHardlinksAsBytes(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		f := newMaterializeFixture(t)
		if err := os.Symlink(filepath.Join(f.stage, "data", "lanqin.db"), filepath.Join(f.stage, "data", "link")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
			p.Close()
			t.Fatal("symlink accepted")
		}
	})
	t.Run("independent-copy", func(t *testing.T) {
		f := newMaterializeFixture(t)
		source := filepath.Join(f.stage, "data", "attachments", "m", "a_file.txt")
		outside := filepath.Join(filepath.Dir(f.container), "outside-hardlink")
		if err := os.Link(source, outside); err != nil {
			t.Skipf("hardlink unavailable: %v", err)
		}
		p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		before, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(filepath.Join(p.Path, "data", "attachments", "m", "a_file.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(before, after) {
			t.Fatal("restored hardlink instead of bytes")
		}
	})
}

func TestPreparedCloseDoesNotFollowMutablePath(t *testing.T) {
	f := newMaterializeFixture(t)
	p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	owned := p.Path
	p.Path = f.container
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.container); err != nil {
		t.Fatal("cleanup removed caller source")
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("owned preparation not removed")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedCloseRefusesReplacedDirectory(t *testing.T) {
	f := newMaterializeFixture(t)
	p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	path := p.Path
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "not-owned")
	if err := os.WriteFile(marker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil {
		t.Fatal("cleanup accepted replaced directory")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("cleanup deleted replacement")
	}
}

func TestPrepareUnixPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses operator-managed workspace ACL, not Unix mode validation")
	}
	for _, which := range []string{"container", "snapshot", "workspace"} {
		t.Run(which, func(t *testing.T) {
			f := newMaterializeFixture(t)
			name := map[string]string{"container": f.container, "snapshot": f.stage, "workspace": f.workspace}[which]
			if err := os.Chmod(name, 0o755); err != nil {
				t.Fatal(err)
			}
			if p, err := Prepare(t.Context(), f.container, f.workspace, "/runtime/data", "/runtime/mail", backup.DefaultLimits()); err == nil {
				p.Close()
				t.Fatal("non-private tree accepted")
			}
		})
	}
}

func TestVerifyFilePinsIdentityAndContent(t *testing.T) {
	f := newMaterializeFixture(t)
	root, err := os.OpenRoot(f.stage)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	item := f.report.Files[1].CollectedFile
	file, err := root.Open(item.Destination)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := file.Stat()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(t.Context(), root, item, observed); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(f.stage, filepath.FromSlash(item.Destination))
	if err := os.WriteFile(name, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(t.Context(), root, item, observed); err == nil {
		t.Fatal("same-size, same-mtime tampering accepted")
	}
	if err := os.Rename(name, name+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("example"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(t.Context(), root, item, observed); err == nil {
		t.Fatal("replacement identity with identical bytes accepted")
	}
}

func TestSourceRootMappingIsLexicalAndPlatformIndependent(t *testing.T) {
	for _, tc := range []struct{ root, name, want string }{{"/data", "/data/attachments/a", "attachments/a"}, {`C:\\data`, `C:\\data\attachments\a`, "attachments/a"}} {
		got, err := sourceRelative(tc.root, tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("mapping %q: %q %v", tc.name, got, err)
		}
	}
	for _, name := range []string{"/data-other/a", "/data/../a", "/data/a\\b", "/data/a:stream"} {
		if _, err := sourceRelative("/data", name); err == nil {
			t.Fatalf("unsafe mapping: %s", name)
		}
	}
	if normalizedSQL(knownTriggers["messages_fts_au"]) == normalizedSQL(strings.ReplaceAll(knownTriggers["messages_fts_au"], "UPDATE OF subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text", "UPDATE OF raw_path")) {
		t.Fatal("trigger normalization obscures affected columns")
	}
}
