package app

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const externalSchemaVersion = 1

var externalSchemaTables = []string{
	"aliases",
	"api_tokens",
	"attachments",
	"blocked_senders",
	"contacts",
	"delivery_events",
	"domains",
	"external_imap_accounts",
	"external_imap_folder_states",
	"external_imap_messages",
	"external_imap_sync_runs",
	"folders",
	"imap_events",
	"login_challenges",
	"mail_labels",
	"mail_rules",
	"mail_signatures",
	"mail_templates",
	"mailbox_share_audit_events",
	"mailbox_share_folders",
	"mailbox_share_labels",
	"mailbox_shares",
	"mailboxes",
	"message_labels",
	"messages",
	"permission_groups",
	"pop3_events",
	"scheduled_sends",
	"schema_migrations",
	"send_as_grants",
	"send_audit_events",
	"send_idempotency_keys",
	"send_queue",
	"sent_message_dedupe_keys",
	"sessions",
	"smtp_send_events",
	"status_webhook_outbox",
	"system_settings",
	"user_permission_groups",
	"users",
	"user_notifications",
}

// initializeExternalSchema initializes an empty PostgreSQL or MySQL database.
// Existing unversioned databases are intentionally rejected: importing SQLite
// data requires an explicit data migration and must never happen at startup.
func initializeExternalSchema(ctx context.Context, db *sql.DB, driver string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	var statements []string
	var migrationName string
	switch driver {
	case databaseDriverPostgres:
		statements = postgresFreshSchema()
		migrationName = "external_schema_v1_postgres"
	case databaseDriverMySQL:
		statements = mysqlFreshSchema()
		migrationName = "external_schema_v1_mysql"
	default:
		return fmt.Errorf("external schema: unsupported database driver %q", driver)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("external schema: reserve connection: %w", err)
	}
	defer conn.Close()

	unlock, err := lockExternalSchema(ctx, conn, driver)
	if err != nil {
		return err
	}
	defer unlock()

	tables, err := listExternalSchemaTables(ctx, conn, driver)
	if err != nil {
		return err
	}
	if len(tables) != 0 {
		return validateExternalSchema(ctx, conn, tables, migrationName)
	}

	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("external schema: begin PostgreSQL migration: %w", err)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("external schema: apply v%d statement %d: %w", externalSchemaVersion, i+1, err)
		}
	}
	marker := fmt.Sprintf(
		"INSERT INTO schema_migrations(version,name,applied_at) VALUES(%d,'%s',CURRENT_TIMESTAMP)",
		externalSchemaVersion,
		migrationName,
	)
	if _, err := executor.ExecContext(ctx, marker); err != nil {
		return fmt.Errorf("external schema: record v%d: %w", externalSchemaVersion, err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit PostgreSQL migration: %w", err)
		}
	}
	return nil
}

type externalSchemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func lockExternalSchema(ctx context.Context, conn *sql.Conn, driver string) (func(), error) {
	switch driver {
	case databaseDriverPostgres:
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(4810941213515201)`); err != nil {
			return nil, fmt.Errorf("external schema: acquire PostgreSQL lock: %w", err)
		}
		return func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock(4810941213515201)`)
		}, nil
	case databaseDriverMySQL:
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('lanqin_email_external_schema_v1', 30)`).Scan(&acquired); err != nil {
			return nil, fmt.Errorf("external schema: acquire MySQL lock: %w", err)
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return nil, fmt.Errorf("external schema: timed out acquiring MySQL initialization lock")
		}
		return func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(unlockCtx, `SELECT RELEASE_LOCK('lanqin_email_external_schema_v1')`)
		}, nil
	default:
		return nil, fmt.Errorf("external schema: unsupported database driver %q", driver)
	}
}

func listExternalSchemaTables(ctx context.Context, conn *sql.Conn, driver string) ([]string, error) {
	var query string
	switch driver {
	case databaseDriverPostgres:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' ORDER BY table_name`
	case databaseDriverMySQL:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' ORDER BY table_name`
	default:
		return nil, fmt.Errorf("external schema: unsupported database driver %q", driver)
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("external schema: inspect database: %w", err)
	}
	defer rows.Close()
	tables := make([]string, 0, len(externalSchemaTables))
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("external schema: inspect table name: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("external schema: inspect database: %w", err)
	}
	return tables, nil
}

func validateExternalSchema(ctx context.Context, conn *sql.Conn, actual []string, migrationName string) error {
	expected := append([]string(nil), externalSchemaTables...)
	sort.Strings(expected)
	actual = append([]string(nil), actual...)
	sort.Strings(actual)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf(
			"external schema: database is not empty and does not match LanQin schema v%d; use a new empty database or run an explicit import (found tables: %s)",
			externalSchemaVersion,
			strings.Join(actual, ", "),
		)
	}

	var count, version int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count != 1 || version != externalSchemaVersion {
		return fmt.Errorf("external schema: unsupported migration history (count=%d, version=%d); automatic external database upgrades are disabled", count, version)
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read migration marker: %w", err)
	}
	if name != migrationName {
		return fmt.Errorf("external schema: database belongs to %q, expected %q", name, migrationName)
	}
	return nil
}
