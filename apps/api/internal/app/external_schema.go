package app

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const externalSchemaVersion = 4

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
	"oauth_identities",
	"oauth_login_states",
	"oauth_registration_challenges",
	"registration_invites",
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
	"telegram_notification_outbox",
	"telegram_notification_settings",
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
		if err := migrateExternalSchemaV2(ctx, conn, driver, tables, migrationName); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		if err := migrateExternalSchemaV3(ctx, conn, driver, tables); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		if err := migrateExternalSchemaV4(ctx, conn, driver, tables); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
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
	for _, migration := range []struct {
		version int
		name    string
	}{{1, migrationName}, {2, "external_schema_v2_linuxdo_sso"}, {3, "external_schema_v3_registration_invites"}, {4, "external_schema_v4_telegram_notifications"}} {
		marker := fmt.Sprintf("INSERT INTO schema_migrations(version,name,applied_at) VALUES(%d,'%s',CURRENT_TIMESTAMP)", migration.version, migration.name)
		if _, err := executor.ExecContext(ctx, marker); err != nil {
			return fmt.Errorf("external schema: record v%d: %w", migration.version, err)
		}
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
	if count != externalSchemaVersion || version != externalSchemaVersion {
		return fmt.Errorf("external schema: unsupported migration history (count=%d, version=%d); automatic external database upgrades are disabled", count, version)
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read migration marker: %w", err)
	}
	if name != migrationName {
		return fmt.Errorf("external schema: database belongs to %q, expected %q", name, migrationName)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=2`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v2 migration marker: %w", err)
	}
	if name != "external_schema_v2_linuxdo_sso" {
		return fmt.Errorf("external schema: unexpected v2 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=3`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v3 migration marker: %w", err)
	}
	if name != "external_schema_v3_registration_invites" {
		return fmt.Errorf("external schema: unexpected v3 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=4`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v4 migration marker: %w", err)
	}
	if name != "external_schema_v4_telegram_notifications" {
		return fmt.Errorf("external schema: unexpected v4 migration %q", name)
	}
	return nil
}

func migrateExternalSchemaV2(ctx context.Context, conn *sql.Conn, driver string, actual []string, migrationName string) error {
	var count, version int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count >= 2 && version >= 2 {
		return nil
	}
	if count != 1 || version != 1 {
		return fmt.Errorf("external schema: unsupported migration history (count=%d, version=%d)", count, version)
	}
	legacy := make([]string, 0, len(externalSchemaTables)-6)
	for _, table := range externalSchemaTables {
		if table != "oauth_identities" && table != "oauth_login_states" && table != "oauth_registration_challenges" && table != "registration_invites" && table != "telegram_notification_settings" && table != "telegram_notification_outbox" {
			legacy = append(legacy, table)
		}
	}
	sort.Strings(legacy)
	got := append([]string(nil), actual...)
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(legacy, "\x00") {
		return fmt.Errorf("external schema: database does not match LanQin schema v1 (found tables: %s)", strings.Join(got, ", "))
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil || name != migrationName {
		return fmt.Errorf("external schema: invalid v1 migration marker")
	}
	statements := linuxDoExternalSchemaStatements()
	if driver == databaseDriverMySQL {
		for i := range statements {
			if strings.HasPrefix(strings.TrimSpace(statements[i]), "CREATE TABLE ") {
				statements[i] = postgresTableToMySQL(statements[i])
			}
		}
	}
	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		var beginErr error
		tx, beginErr = conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return fmt.Errorf("external schema: begin v2 PostgreSQL migration: %w", beginErr)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("external schema: apply v2 statement %d: %w", i+1, err)
		}
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(2,'external_schema_v2_linuxdo_sso',CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("external schema: record v2: %w", err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit v2 PostgreSQL migration: %w", err)
		}
	}
	return nil
}

func migrateExternalSchemaV3(ctx context.Context, conn *sql.Conn, driver string, actual []string) error {
	var count, version int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count >= 3 && version >= 3 {
		return nil
	}
	if count != 2 || version != 2 {
		return fmt.Errorf("external schema: unsupported migration history before v3 (count=%d, version=%d)", count, version)
	}
	legacy := make([]string, 0, len(externalSchemaTables)-3)
	for _, table := range externalSchemaTables {
		if table != "registration_invites" && table != "telegram_notification_settings" && table != "telegram_notification_outbox" {
			legacy = append(legacy, table)
		}
	}
	sort.Strings(legacy)
	got := append([]string(nil), actual...)
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(legacy, "\x00") {
		return fmt.Errorf("external schema: database does not match LanQin schema v2 (found tables: %s)", strings.Join(got, ", "))
	}
	statements := registrationInviteExternalSchemaStatements()
	if driver == databaseDriverMySQL {
		for i := range statements {
			if strings.HasPrefix(strings.TrimSpace(statements[i]), "CREATE TABLE ") {
				statements[i] = postgresTableToMySQL(statements[i])
			}
		}
	}
	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		var err error
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("external schema: begin v3 PostgreSQL migration: %w", err)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("external schema: apply v3 statement %d: %w", i+1, err)
		}
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(3,'external_schema_v3_registration_invites',CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("external schema: record v3: %w", err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit v3 PostgreSQL migration: %w", err)
		}
	}
	return nil
}

func linuxDoExternalSchemaStatements() []string {
	all := postgresFreshSchema()
	out := make([]string, 0, 5)
	wanted := map[string]bool{"oauth_identities": true, "oauth_login_states": true, "oauth_registration_challenges": true}
	for _, statement := range all {
		fields := strings.Fields(statement)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE") && wanted[strings.Trim(fields[2], "`\"")] {
			out = append(out, statement)
			continue
		}
		if strings.Contains(statement, "idx_oauth_") {
			out = append(out, statement)
		}
	}
	return out
}

func registrationInviteExternalSchemaStatements() []string {
	all := postgresFreshSchema()
	out := make([]string, 0, 2)
	for _, statement := range all {
		fields := strings.Fields(statement)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE") && strings.Trim(fields[2], "`\"") == "registration_invites" {
			out = append(out, statement)
			continue
		}
		if strings.Contains(statement, "idx_registration_invites_") {
			out = append(out, statement)
		}
	}
	return out
}

func migrateExternalSchemaV4(ctx context.Context, conn *sql.Conn, driver string, actual []string) error {
	var count, version int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count >= 4 && version >= 4 {
		return nil
	}
	if count != 3 || version != 3 {
		return fmt.Errorf("external schema: unsupported migration history before v4 (count=%d, version=%d)", count, version)
	}
	legacy := make([]string, 0, len(externalSchemaTables)-2)
	for _, table := range externalSchemaTables {
		if table != "telegram_notification_settings" && table != "telegram_notification_outbox" {
			legacy = append(legacy, table)
		}
	}
	sort.Strings(legacy)
	got := append([]string(nil), actual...)
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(legacy, "\x00") {
		return fmt.Errorf("external schema: database does not match LanQin schema v3 (found tables: %s)", strings.Join(got, ", "))
	}
	statements := telegramNotificationExternalSchemaStatements()
	if driver == databaseDriverMySQL {
		for i := range statements {
			if strings.HasPrefix(strings.TrimSpace(statements[i]), "CREATE TABLE ") {
				statements[i] = postgresTableToMySQL(statements[i])
			}
		}
	}
	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		var err error
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("external schema: begin v4 PostgreSQL migration: %w", err)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("external schema: apply v4 statement %d: %w", i+1, err)
		}
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(4,'external_schema_v4_telegram_notifications',CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("external schema: record v4: %w", err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit v4 PostgreSQL migration: %w", err)
		}
	}
	return nil
}

func telegramNotificationExternalSchemaStatements() []string {
	all := postgresFreshSchema()
	out := make([]string, 0, 4)
	wanted := map[string]bool{"telegram_notification_settings": true, "telegram_notification_outbox": true}
	for _, statement := range all {
		fields := strings.Fields(statement)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE") && wanted[strings.Trim(fields[2], "`\"")] {
			out = append(out, statement)
			continue
		}
		if strings.Contains(statement, "idx_telegram_notification_") {
			out = append(out, statement)
		}
	}
	return out
}
