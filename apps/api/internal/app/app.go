package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type App struct {
	cfg           Config
	db            *sql.DB
	log           *slog.Logger
	now           func() time.Time
	policy        *HTMLPolicy
	workerCancel  context.CancelFunc
	maildirHealth *maildirSyncHealthTracker
	externalIMAP  externalIMAPClientFactory
}

func New(cfg Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "attachments"), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	a := &App{cfg: cfg, db: db, log: logger, now: time.Now, policy: NewHTMLPolicy(), maildirHealth: newMaildirSyncHealthTracker()}
	a.externalIMAP = a
	if err := a.configureSQLite(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.ensureDefaultMailTemplates(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.loadPersistedSystemSettings(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.seed(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	a.workerCancel = cancel
	go a.scheduledSendWorker(workerCtx)
	if strings.TrimSpace(a.cfg.MaildirRoot) != "" {
		go a.maildirWorker(workerCtx)
	}
	go a.sendQueueWorker(workerCtx)
	go a.externalIMAPWorker(workerCtx)
	go a.smtpEventsCleanupWorker(workerCtx)
	return a, nil
}

func (a *App) Close() error {
	if a == nil || a.db == nil {
		return nil
	}
	if a.workerCancel != nil {
		a.workerCancel()
	}
	return a.db.Close()
}

func (a *App) configureSQLite(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	}
	for _, q := range pragmas {
		if _, err := a.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('admin','user')),
			password_hash TEXT NOT NULL,
			two_factor_secret TEXT NOT NULL DEFAULT '',
			two_factor_enabled INTEGER NOT NULL DEFAULT 0,
			disabled INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS permission_groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			permissions_json TEXT NOT NULL DEFAULT '[]',
			limits_json TEXT NOT NULL DEFAULT '{"maxAttachmentMb":25,"smtpDailyLimit":200,"smtpMinuteLimit":20,"imapMinuteLimit":200,"pop3MinuteLimit":150}',
			system INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_permission_groups (
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			group_id TEXT NOT NULL REFERENCES permission_groups(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL,
			PRIMARY KEY(user_id, group_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_permission_groups_group ON user_permission_groups(group_id, user_id)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS login_challenges (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mail_templates (
			key TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			subject TEXT NOT NULL,
			body_text TEXT NOT NULL,
			body_html TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS domains (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'active',
			dkim_selector TEXT NOT NULL,
			dkim_public_key TEXT NOT NULL,
			dkim_private_key TEXT NOT NULL,
			dns_status TEXT NOT NULL DEFAULT 'unchecked',
			dns_checked_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mailboxes (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
			local_part TEXT NOT NULL,
			address TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			quota_mb INTEGER NOT NULL DEFAULT 1024,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(domain_id, local_part)
		)`,
		`CREATE TABLE IF NOT EXISTS aliases (
			id TEXT PRIMARY KEY,
			domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
			source TEXT NOT NULL UNIQUE,
			destination TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS folders (
			id TEXT PRIMARY KEY,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			uid_validity INTEGER NOT NULL DEFAULT 0,
			uid_next INTEGER NOT NULL DEFAULT 1,
			highest_modseq INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			UNIQUE(mailbox_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			mailbox_id TEXT REFERENCES mailboxes(id) ON DELETE CASCADE,
			folder_id TEXT REFERENCES folders(id) ON DELETE CASCADE,
			recipient_addr TEXT NOT NULL DEFAULT '',
			message_uid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			subject TEXT NOT NULL,
			from_addr TEXT NOT NULL,
			from_name TEXT NOT NULL DEFAULT '',
			to_addrs TEXT NOT NULL,
			cc_addrs TEXT NOT NULL DEFAULT '[]',
			bcc_addrs TEXT NOT NULL DEFAULT '[]',
			sent_at TEXT NOT NULL,
			received_at TEXT NOT NULL,
			snippet TEXT NOT NULL,
			body_text TEXT NOT NULL,
			body_html TEXT NOT NULL,
			is_read INTEGER NOT NULL DEFAULT 0,
			is_starred INTEGER NOT NULL DEFAULT 0,
			has_attachments INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			auth_results TEXT NOT NULL DEFAULT '',
			auth_spf TEXT NOT NULL DEFAULT 'unknown',
			auth_dkim TEXT NOT NULL DEFAULT 'unknown',
			auth_dmarc TEXT NOT NULL DEFAULT 'unknown',
			received_spf TEXT NOT NULL DEFAULT '',
			raw_path TEXT NOT NULL DEFAULT '',
			imap_uid INTEGER NOT NULL DEFAULT 0,
			imap_modseq INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox_folder_received ON messages(mailbox_id, folder_id, received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_search ON messages(mailbox_id, subject, from_addr, from_name, snippet)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_mailbox_raw_path ON messages(mailbox_id, raw_path) WHERE raw_path <> '' AND mailbox_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_unregistered_raw_path ON messages(raw_path) WHERE raw_path <> '' AND mailbox_id IS NULL`,
		`CREATE TABLE IF NOT EXISTS sent_message_dedupe_keys (
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
			message_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(mailbox_id, folder_id, message_id)
		)`,
		`CREATE TABLE IF NOT EXISTS send_as_grants (
			id TEXT PRIMARY KEY,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			address TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(mailbox_id, address)
		)`,
		`CREATE TABLE IF NOT EXISTS send_queue (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			sent_message_id TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL,
			mail_from TEXT NOT NULL,
			header_from TEXT NOT NULL,
			recipients_json TEXT NOT NULL,
			mime_base64 TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			next_attempt_at TEXT NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			delivered_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_send_queue_due ON send_queue(status, next_attempt_at, created_at)`,
		`CREATE TABLE IF NOT EXISTS send_audit_events (
			id TEXT PRIMARY KEY,
			queue_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			mailbox_id TEXT NOT NULL DEFAULT '',
			sent_message_id TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL,
			event TEXT NOT NULL,
			status TEXT NOT NULL,
			mail_from TEXT NOT NULL DEFAULT '',
			header_from TEXT NOT NULL DEFAULT '',
			recipients_json TEXT NOT NULL DEFAULT '[]',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_send_audit_events_created ON send_audit_events(created_at)`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			storage_path TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scheduled_sends (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			draft_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
			payload_json TEXT NOT NULL,
			send_at TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			sent_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_sends_due ON scheduled_sends(status, send_at)`,
		`CREATE TABLE IF NOT EXISTS smtp_send_events (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_smtp_send_events_user_created ON smtp_send_events(user_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS imap_events (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_imap_events_user_created ON imap_events(user_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS pop3_events (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pop3_events_user_created ON pop3_events(user_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS external_imap_accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			tls_mode TEXT NOT NULL CHECK(tls_mode IN ('tls','starttls','plain')),
			username TEXT NOT NULL,
			password_ciphertext TEXT NOT NULL,
			auth_mode TEXT NOT NULL DEFAULT 'password' CHECK(auth_mode IN ('password','oauth2')),
			oauth_provider TEXT NOT NULL DEFAULT '',
			oauth_email TEXT NOT NULL DEFAULT '',
			oauth_access_token_ciphertext TEXT NOT NULL DEFAULT '',
			oauth_refresh_token_ciphertext TEXT NOT NULL DEFAULT '',
			oauth_expiry TEXT,
			storage_mode TEXT NOT NULL DEFAULT 'local' CHECK(storage_mode IN ('local','remote')),
			sync_read_state INTEGER NOT NULL DEFAULT 1,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_sync_at TEXT,
			last_status TEXT NOT NULL DEFAULT 'idle',
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_imap_accounts_user_mailbox ON external_imap_accounts(user_id, mailbox_id)`,
		`CREATE INDEX IF NOT EXISTS idx_external_imap_accounts_enabled ON external_imap_accounts(enabled, updated_at)`,
		`CREATE TABLE IF NOT EXISTS external_imap_folder_states (
			account_id TEXT NOT NULL REFERENCES external_imap_accounts(id) ON DELETE CASCADE,
			remote_folder TEXT NOT NULL,
			local_folder_id TEXT NOT NULL DEFAULT '',
			uid_validity INTEGER NOT NULL DEFAULT 0,
			last_uid INTEGER NOT NULL DEFAULT 0,
			last_sync_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(account_id, remote_folder)
		)`,
		`CREATE TABLE IF NOT EXISTS external_imap_messages (
			account_id TEXT NOT NULL REFERENCES external_imap_accounts(id) ON DELETE CASCADE,
			remote_folder TEXT NOT NULL,
			uid_validity INTEGER NOT NULL,
			uid INTEGER NOT NULL,
			message_id TEXT NOT NULL DEFAULT '',
			local_message_id TEXT NOT NULL DEFAULT '',
			is_read INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(account_id, remote_folder, uid_validity, uid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_imap_messages_local ON external_imap_messages(local_message_id) WHERE local_message_id <> ''`,
		`CREATE TABLE IF NOT EXISTS external_imap_sync_runs (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES external_imap_accounts(id) ON DELETE CASCADE,
			folder TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			imported INTEGER NOT NULL DEFAULT 0,
			skipped INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_imap_sync_runs_account_started ON external_imap_sync_runs(account_id, started_at DESC)`,

		`CREATE TABLE IF NOT EXISTS contacts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			email TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(user_id, email)
		)`,
		`CREATE TABLE IF NOT EXISTS mail_signatures (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			content TEXT NOT NULL,
			is_default INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mail_rules (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			match_mode TEXT NOT NULL DEFAULT 'all',
			conditions_json TEXT NOT NULL DEFAULT '[]',
			actions_json TEXT NOT NULL DEFAULT '[]',
			from_contains TEXT NOT NULL DEFAULT '',
			subject_contains TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			apply_to_existing INTEGER NOT NULL DEFAULT 0,
			stop_processing INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS blocked_senders (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(user_id, mailbox_id, email)
		)`,
		`CREATE TABLE IF NOT EXISTS mail_labels (
			id TEXT PRIMARY KEY,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			color TEXT NOT NULL DEFAULT '#64748b',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(mailbox_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS message_labels (
			message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			label_id TEXT NOT NULL REFERENCES mail_labels(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL,
			PRIMARY KEY(message_id, label_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contacts_user ON contacts(user_id, email)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_signatures_user_mailbox ON mail_signatures(user_id, mailbox_id, is_default)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_rules_user_mailbox ON mail_rules(user_id, mailbox_id, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_blocked_senders_user_mailbox ON blocked_senders(user_id, mailbox_id, email)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_labels_mailbox ON mail_labels(mailbox_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id, message_id)`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := a.migrateMessagesForUnregistered(ctx); err != nil {
		return err
	}
	if err := a.migrateMessagesFromName(ctx); err != nil {
		return err
	}
	if err := a.migrateMessageAuthentication(ctx); err != nil {
		return err
	}
	if err := a.rebuildHTMLOnlyMessageSnippets(ctx); err != nil {
		return err
	}
	if err := a.migrateUsersForTwoFactor(ctx); err != nil {
		return err
	}
	if err := a.migrateMailRulesBuilder(ctx); err != nil {
		return err
	}
	if err := a.migrateLegacyBootstrapMailbox(ctx); err != nil {
		return err
	}
	if err := a.migratePermissionGroupLimits(ctx); err != nil {
		return err
	}
	if err := a.migrateSendQueueMessageID(ctx); err != nil {
		return err
	}
	if err := a.migrateIMAPMetadata(ctx); err != nil {
		return err
	}
	if err := a.migrateFolderSortOrder(ctx); err != nil {
		return err
	}
	if err := a.migrateExternalIMAP(ctx); err != nil {
		return err
	}
	if err := a.ensureDefaultPermissionGroups(ctx); err != nil {
		return err
	}
	return nil
}

func (a *App) migrateMessageAuthentication(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	alter := []struct {
		name string
		sql  string
	}{
		{"auth_results", `ALTER TABLE messages ADD COLUMN auth_results TEXT NOT NULL DEFAULT ''`},
		{"auth_spf", `ALTER TABLE messages ADD COLUMN auth_spf TEXT NOT NULL DEFAULT 'unknown'`},
		{"auth_dkim", `ALTER TABLE messages ADD COLUMN auth_dkim TEXT NOT NULL DEFAULT 'unknown'`},
		{"auth_dmarc", `ALTER TABLE messages ADD COLUMN auth_dmarc TEXT NOT NULL DEFAULT 'unknown'`},
		{"received_spf", `ALTER TABLE messages ADD COLUMN received_spf TEXT NOT NULL DEFAULT ''`},
	}
	for _, item := range alter {
		if !columns[item.name] {
			if _, err := a.db.ExecContext(ctx, item.sql); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) rebuildHTMLOnlyMessageSnippets(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,body_html,snippet FROM messages WHERE trim(body_text)='' AND body_html<>''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type update struct {
		id      string
		snippet string
	}
	updates := []update{}
	for rows.Next() {
		var id, bodyHTML, current string
		if err := rows.Scan(&id, &bodyHTML, &current); err != nil {
			return err
		}
		next := snippetFrom("", bodyHTML)
		if next != current {
			updates = append(updates, update{id: id, snippet: next})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET snippet=?,updated_at=? WHERE id=?`, item.snippet, now, item.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) migrateSendQueueMessageID(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(send_queue)`)
	if err != nil {
		return err
	}
	hasMessageID := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "message_id" {
			hasMessageID = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasMessageID {
		if _, err := a.db.ExecContext(ctx, `ALTER TABLE send_queue ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM send_queue
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
					ROW_NUMBER() OVER (
						PARTITION BY mailbox_id, source, message_id
						ORDER BY
							CASE status
								WHEN 'queued' THEN 0
								WHEN 'sending' THEN 1
								WHEN 'failed' THEN 2
								WHEN 'delivered' THEN 3
								ELSE 4
							END,
							created_at DESC,
							id DESC
					) AS row_num
				FROM send_queue
				WHERE message_id <> ''
			)
			WHERE row_num > 1
		)`); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_send_queue_mailbox_source_message_id ON send_queue(mailbox_id, source, message_id) WHERE message_id <> ''`)
	return err
}

func (a *App) migratePermissionGroupLimits(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(permission_groups)`)
	if err != nil {
		return err
	}
	hasLimits := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "limits_json" {
			hasLimits = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasLimits {
		return nil
	}
	_, err = a.db.ExecContext(ctx, `ALTER TABLE permission_groups ADD COLUMN limits_json TEXT NOT NULL DEFAULT '{"maxAttachmentMb":25,"smtpDailyLimit":200,"smtpMinuteLimit":20,"imapMinuteLimit":200,"pop3MinuteLimit":150}'`)
	return err
}

// migrateLegacyBootstrapMailbox removes mailboxes created by an older version of seed()
// that implicitly created an admin mailbox with display_name "LanQin Admin".
// Current seed() creates mailboxes with display_name = admin email, so this migration
// has no effect on fresh installs. It only cleans up after upgrades from pre-v1.0 schema.
func (a *App) migrateLegacyBootstrapMailbox(ctx context.Context) error {
	adminEmail := normalizeEmail(a.cfg.AdminEmail)
	if adminEmail == "" || !strings.Contains(adminEmail, "@") {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT mb.id, mb.domain_id
		FROM mailboxes mb
		JOIN users u ON u.id=mb.user_id
		WHERE mb.address=?
		  AND mb.display_name='LanQin Admin'
		  AND u.email=?
		  AND u.role='admin'`, adminEmail, adminEmail)
	if err != nil {
		return err
	}
	type legacyMailbox struct {
		id       string
		domainID string
	}
	items := []legacyMailbox{}
	for rows.Next() {
		var item legacyMailbox
		if err := rows.Scan(&item.id, &item.domainID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		messageRows, err := a.db.QueryContext(ctx, `SELECT id FROM messages WHERE mailbox_id=?`, item.id)
		if err != nil {
			return err
		}
		messageIDs := []string{}
		for messageRows.Next() {
			var messageID string
			if err := messageRows.Scan(&messageID); err != nil {
				messageRows.Close()
				return err
			}
			messageIDs = append(messageIDs, messageID)
		}
		if err := messageRows.Err(); err != nil {
			messageRows.Close()
			return err
		}
		if err := messageRows.Close(); err != nil {
			return err
		}
		for _, messageID := range messageIDs {
			a.deleteMessage(ctx, messageID)
		}
		if _, err := a.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE id=?`, item.id); err != nil {
			return err
		}
		if _, err := a.db.ExecContext(ctx, `
			DELETE FROM domains
			WHERE id=?
			  AND NOT EXISTS (SELECT 1 FROM mailboxes WHERE domain_id=domains.id)
			  AND NOT EXISTS (SELECT 1 FROM aliases WHERE domain_id=domains.id)`, item.domainID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) migrateMailRulesBuilder(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(mail_rules)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		columns[name] = true
	}
	alter := []struct {
		name string
		sql  string
	}{
		{"match_mode", `ALTER TABLE mail_rules ADD COLUMN match_mode TEXT NOT NULL DEFAULT 'all'`},
		{"conditions_json", `ALTER TABLE mail_rules ADD COLUMN conditions_json TEXT NOT NULL DEFAULT '[]'`},
		{"actions_json", `ALTER TABLE mail_rules ADD COLUMN actions_json TEXT NOT NULL DEFAULT '[]'`},
		{"apply_to_existing", `ALTER TABLE mail_rules ADD COLUMN apply_to_existing INTEGER NOT NULL DEFAULT 0`},
		{"stop_processing", `ALTER TABLE mail_rules ADD COLUMN stop_processing INTEGER NOT NULL DEFAULT 0`},
	}
	for _, item := range alter {
		if !columns[item.name] {
			if _, err := a.db.ExecContext(ctx, item.sql); err != nil {
				return err
			}
		}
	}
	existing, err := a.db.QueryContext(ctx, `SELECT id,from_contains,subject_contains,action,conditions_json,actions_json FROM mail_rules`)
	if err != nil {
		return err
	}
	defer existing.Close()
	type update struct {
		id         string
		conditions string
		actions    string
	}
	updates := []update{}
	for existing.Next() {
		var id, fromContains, subjectContains, action, conditionsJSON, actionsJSON string
		if err := existing.Scan(&id, &fromContains, &subjectContains, &action, &conditionsJSON, &actionsJSON); err != nil {
			return err
		}
		if conditionsJSON != "" && conditionsJSON != "[]" && actionsJSON != "" && actionsJSON != "[]" {
			continue
		}
		conditions := []MailRuleCondition{}
		if strings.TrimSpace(fromContains) != "" {
			conditions = append(conditions, MailRuleCondition{Field: "from", Operator: "contains", Value: strings.TrimSpace(fromContains)})
		}
		if strings.TrimSpace(subjectContains) != "" {
			conditions = append(conditions, MailRuleCondition{Field: "subject", Operator: "contains", Value: strings.TrimSpace(subjectContains)})
		}
		actions := []MailRuleAction{}
		if strings.TrimSpace(action) != "" {
			actions = append(actions, MailRuleAction{Type: strings.TrimSpace(action)})
		}
		condBytes, err := json.Marshal(conditions)
		if err != nil {
			return err
		}
		actionBytes, err := json.Marshal(actions)
		if err != nil {
			return err
		}
		updates = append(updates, update{id: id, conditions: string(condBytes), actions: string(actionBytes)})
	}
	for _, item := range updates {
		if _, err := a.db.ExecContext(ctx, `UPDATE mail_rules SET conditions_json=?, actions_json=? WHERE id=?`, item.conditions, item.actions, item.id); err != nil {
			return err
		}
	}
	return existing.Err()
}

func (a *App) migrateUsersForTwoFactor(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !columns["two_factor_secret"] {
		if _, err := a.db.ExecContext(ctx, `ALTER TABLE users ADD COLUMN two_factor_secret TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !columns["two_factor_enabled"] {
		if _, err := a.db.ExecContext(ctx, `ALTER TABLE users ADD COLUMN two_factor_enabled INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) migrateMessagesForUnregistered(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasRecipientAddr := false
	mailboxNullable := false
	folderNullable := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		switch name {
		case "recipient_addr":
			hasRecipientAddr = true
		case "mailbox_id":
			mailboxNullable = notnull == 0
		case "folder_id":
			folderNullable = notnull == 0
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasRecipientAddr && mailboxNullable && folderNullable {
		return nil
	}

	if _, err := a.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer a.db.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_messages_mailbox_folder_received`,
		`DROP INDEX IF EXISTS idx_messages_search`,
		`DROP INDEX IF EXISTS idx_messages_mailbox_raw_path`,
		`DROP INDEX IF EXISTS idx_messages_unregistered_raw_path`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE messages_new (
		id TEXT PRIMARY KEY,
		mailbox_id TEXT REFERENCES mailboxes(id) ON DELETE CASCADE,
		folder_id TEXT REFERENCES folders(id) ON DELETE CASCADE,
		recipient_addr TEXT NOT NULL DEFAULT '',
		message_uid TEXT NOT NULL,
		message_id TEXT NOT NULL,
		subject TEXT NOT NULL,
		from_addr TEXT NOT NULL,
		from_name TEXT NOT NULL DEFAULT '',
		to_addrs TEXT NOT NULL,
		cc_addrs TEXT NOT NULL DEFAULT '[]',
		bcc_addrs TEXT NOT NULL DEFAULT '[]',
		sent_at TEXT NOT NULL,
		received_at TEXT NOT NULL,
		snippet TEXT NOT NULL,
		body_text TEXT NOT NULL,
		body_html TEXT NOT NULL,
		is_read INTEGER NOT NULL DEFAULT 0,
		is_starred INTEGER NOT NULL DEFAULT 0,
		has_attachments INTEGER NOT NULL DEFAULT 0,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		raw_path TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages_new(id,mailbox_id,folder_id,recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,raw_path,created_at,updated_at)
		SELECT id,mailbox_id,folder_id,'',message_uid,message_id,subject,from_addr,'',to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,has_attachments,size_bytes,raw_path,created_at,updated_at FROM messages`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE messages`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE messages_new RENAME TO messages`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, stmt := range messageIndexes() {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) migrateMessagesFromName(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasFromName := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "from_name" {
			hasFromName = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasFromName {
		if _, err := a.db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN from_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := a.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_messages_search`); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_search ON messages(mailbox_id, subject, from_addr, from_name, snippet)`); err != nil {
		return err
	}
	return nil
}

func messageIndexes() []string {
	return []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox_folder_received ON messages(mailbox_id, folder_id, received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_search ON messages(mailbox_id, subject, from_addr, from_name, snippet)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_mailbox_raw_path ON messages(mailbox_id, raw_path) WHERE raw_path <> '' AND mailbox_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_unregistered_raw_path ON messages(raw_path) WHERE raw_path <> '' AND mailbox_id IS NULL`,
	}
}

func (a *App) seed(ctx context.Context) error {
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return a.ensureConfiguredAdminSuperAdmin(ctx)
	}

	adminPassword := a.cfg.AdminPassword
	if adminPassword == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		adminPassword = base64.RawURLEncoding.EncodeToString(buf)
		a.log.Warn("LANQIN_ADMIN_PASSWORD not set; generated random password", "password", adminPassword)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	userID := newID("usr")
	adminEmail := normalizeEmail(a.cfg.AdminEmail)
	if adminEmail == "" || !strings.Contains(adminEmail, "@") {
		return errors.New("invalid admin email")
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, adminEmail, "LanQin Admin", "admin", string(passwordHash), 0, now, now); err != nil {
		return err
	}
	a.log.Warn("created default administrator; change LANQIN_ADMIN_PASSWORD in production", "email", adminEmail)

	// Create domain from admin email
	parts := strings.SplitN(adminEmail, "@", 2)
	localPart := parts[0]
	domainName := normalizeDomain(parts[1])
	var domainID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM domains WHERE name=?`, domainName).Scan(&domainID); err != nil {
		domainID, err = a.createDomainTx(ctx, nil, domainName)
		if err != nil {
			return err
		}
		a.log.Info("created domain for administrator", "domain", domainName)
	} else {
		a.log.Info("domain already exists for administrator", "domain", domainName)
	}

	// Create mailbox for admin
	mailboxID, err := a.createMailboxWithPasswordHash(ctx, userID, domainID, localPart, adminEmail, string(passwordHash), 1024, "active")
	if err != nil {
		return err
	}
	a.log.Info("created mailbox for administrator", "address", adminEmail)

	// Send welcome message
	if err := a.seedWelcomeMessage(ctx, mailboxID); err != nil {
		a.log.Warn("failed to create welcome message", "error", err)
	}
	return nil
}

func (a *App) ensureConfiguredAdminSuperAdmin(ctx context.Context) error {
	adminEmail := normalizeEmail(a.cfg.AdminEmail)
	if adminEmail == "" || !strings.Contains(adminEmail, "@") {
		return nil
	}
	_, err := a.db.ExecContext(ctx, `UPDATE users SET role='admin', disabled=0, updated_at=? WHERE email=?`,
		a.now().UTC().Format(time.RFC3339Nano), adminEmail)
	return err
}

func (a *App) createDomainTx(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	name = normalizeDomain(name)
	if name == "" || !strings.Contains(name, ".") {
		return "", errors.New("invalid domain")
	}
	selector := "lanqin"
	publicKey, privateKey, err := generateDKIMMaterial()
	if err != nil {
		return "", err
	}
	id := newID("dom")
	now := a.now().UTC().Format(time.RFC3339Nano)
	query := `INSERT INTO domains(id,name,status,dkim_selector,dkim_public_key,dkim_private_key,dns_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`
	args := []any{id, name, "active", selector, publicKey, privateKey, "unchecked", now, now}
	if tx != nil {
		_, err = tx.ExecContext(ctx, query, args...)
	} else {
		_, err = a.db.ExecContext(ctx, query, args...)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func generateDKIMMaterial() (string, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privDER := x509.MarshalPKCS1PrivateKey(key)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	return base64.StdEncoding.EncodeToString(pubDER), base64.StdEncoding.EncodeToString(privPEM), nil
}

func defaultFolderDefs() []struct{ name, role string } {
	return []struct{ name, role string }{
		{"Inbox", "inbox"},
		{"Sent", "sent"},
		{"Drafts", "drafts"},
		{"Archive", "archive"},
		{"Spam", "spam"},
		{"Trash", "trash"},
	}
}

func (a *App) createMailbox(ctx context.Context, userID, domainID, localPart, displayName, password string, quotaMB int, status string) (string, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return a.createMailboxWithPasswordHash(ctx, userID, domainID, localPart, displayName, string(passwordHash), quotaMB, status)
}

func (a *App) createMailboxWithPasswordHash(ctx context.Context, userID, domainID, localPart, displayName, passwordHash string, quotaMB int, status string) (string, error) {
	localPart = normalizeLocalPart(localPart)
	if localPart == "" {
		return "", errors.New("invalid local part")
	}
	if quotaMB <= 0 {
		quotaMB = 1024
	}
	if status == "" {
		status = "active"
	}
	var domain string
	if err := a.db.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=?`, domainID).Scan(&domain); err != nil {
		return "", err
	}
	address := localPart + "@" + domain
	if displayName == "" {
		displayName = address
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	id := newID("mbx")
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,domain_id,local_part,address,display_name,password_hash,quota_mb,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, userID, domainID, localPart, address, displayName, passwordHash, quotaMB, status, now, now)
	if err != nil {
		return "", err
	}
	for _, f := range defaultFolderDefs() {
		_, err = tx.ExecContext(ctx, `INSERT INTO folders(id,mailbox_id,name,role,sort_order,uid_validity,uid_next,highest_modseq,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, newID("fld"), id, f.name, f.role, 0, a.newUIDValidity(), 1, 1, now)
		if err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (a *App) seedWelcomeMessage(ctx context.Context, mailboxID string) error {
	folderID, err := a.ensureFolder(ctx, mailboxID, "Inbox")
	if err != nil {
		return err
	}
	now := a.now().UTC()
	subject := "欢迎使用 LanQin Email"
	bodyText := "你的自建邮箱 Webmail 已经初始化完成。请尽快修改默认管理员密码，并配置 MX/SPF/DKIM/DMARC。"
	bodyHTML := "<p>你的自建邮箱 Webmail 已经初始化完成。</p><p>请尽快修改默认管理员密码，并配置 MX/SPF/DKIM/DMARC。</p>"
	if tpl, err := a.mailTemplate(ctx, "welcome"); err == nil {
		rendered := renderMailTemplate(tpl, templateRenderData{
			To:             a.cfg.AdminEmail,
			From:           "system@lanqin.local",
			PublicHostname: a.cfg.PublicHostname,
			PublicBaseURL:  a.cfg.PublicBaseURL,
			Time:           now,
		})
		subject, bodyText, bodyHTML = rendered.Subject, rendered.Text, rendered.HTML
	}
	msg := storedMessage{
		MailboxID:  mailboxID,
		FolderID:   folderID,
		MessageUID: newID("uid"),
		MessageID:  fmt.Sprintf("<%s@lanqin.local>", newID("msg")),
		Subject:    subject,
		From:       "system@lanqin.local",
		FromName:   "LanQin Email",
		To:         []string{a.cfg.AdminEmail},
		SentAt:     now,
		ReceivedAt: now,
		Snippet:    snippetFrom(bodyText, bodyHTML),
		BodyText:   bodyText,
		BodyHTML:   bodyHTML,
		IsRead:     false,
	}
	_, err = a.insertMessage(ctx, msg, nil)
	return err
}
