package app

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestExternalFreshSchemasCoverAllTables(t *testing.T) {
	t.Parallel()
	want := append([]string(nil), externalSchemaTables...)
	sort.Strings(want)

	for name, statements := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := schemaTableNames(statements)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("schema tables differ\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

func TestPostgresFreshSchemaPreservesConditionalIndexes(t *testing.T) {
	t.Parallel()
	schema := strings.Join(postgresFreshSchema(), "\n")
	for _, fragment := range []string{
		"idx_messages_mailbox_raw_path",
		"idx_messages_unregistered_raw_path",
		"idx_messages_folder_imap_uid",
		"idx_send_queue_mailbox_source_message_id",
		"WHERE raw_path<>''",
		"queue_id VARCHAR(64) NOT NULL REFERENCES send_queue(id) ON DELETE CASCADE",
		"mailbox_id VARCHAR(64) NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(schema, fragment) {
			t.Errorf("PostgreSQL schema is missing %q", fragment)
		}
	}
}

func TestMySQLFreshSchemaUsesPortableIndexesAndBinaryCollation(t *testing.T) {
	t.Parallel()
	statements := mysqlFreshSchema()
	schema := strings.Join(statements, "\n")

	for _, forbidden := range []string{
		"CREATE INDEX IF NOT EXISTS",
		"CREATE UNIQUE INDEX IF NOT EXISTS",
		"CREATE FUNCTION ",
		"CREATE TRIGGER ",
		"ALTER TABLE messages",
		"ALTER TABLE send_queue",
		"CREATE INDEX idx_messages_mailbox_raw_path ON messages(mailbox_id,raw_path)",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("MySQL schema contains unsupported SQL %q", forbidden)
		}
	}
	for _, statement := range statements {
		trimmed := strings.TrimSpace(statement)
		if !strings.HasPrefix(trimmed, "CREATE TABLE ") && !strings.HasPrefix(trimmed, "CREATE INDEX ") && !strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ") {
			t.Errorf("MySQL schema contains unexpected or privileged DDL: %.80s", trimmed)
		}
		if strings.HasPrefix(trimmed, "CREATE TABLE ") && !strings.HasSuffix(trimmed, "ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin") {
			t.Errorf("MySQL table is missing engine or binary collation: %.80s", trimmed)
		}
		if (strings.HasPrefix(trimmed, "CREATE INDEX ") || strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ")) && strings.Contains(trimmed, " WHERE ") {
			t.Errorf("MySQL schema contains a partial index: %s", trimmed)
		}
		for _, line := range strings.Split(statement, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, " REFERENCES ") && !strings.HasPrefix(line, "FOREIGN KEY ") {
				t.Errorf("MySQL inline REFERENCES would not enforce a foreign key: %s", line)
			}
		}
	}
	for _, fragment := range []string{
		"CREATE TABLE system_settings (\n\t\t\t`key` VARCHAR(255) PRIMARY KEY",
		"CREATE TABLE mail_templates (\n\t\t\t`key` VARCHAR(255) PRIMARY KEY",
		"`system` INTEGER NOT NULL DEFAULT 0",
		"mailbox_raw_path_key BINARY(32)",
		"unregistered_raw_path_key BINARY(32)",
		"positive_imap_uid BIGINT GENERATED ALWAYS",
		"dedupe_message_id VARCHAR(512)",
		"FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"FOREIGN KEY (queue_id) REFERENCES send_queue(id) ON DELETE CASCADE",
		"FOREIGN KEY (mailbox_id) REFERENCES mailboxes(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(schema, fragment) {
			t.Errorf("MySQL schema is missing %q", fragment)
		}
	}
	var messagesTable string
	for _, statement := range statements {
		if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE messages ") {
			messagesTable = statement
			break
		}
	}
	if generated, foreignKey := strings.Index(messagesTable, "mailbox_raw_path_key BINARY(32)"), strings.Index(messagesTable, "FOREIGN KEY (mailbox_id) REFERENCES mailboxes(id)"); generated < 0 || foreignKey < 0 || generated > foreignKey {
		t.Error("MySQL messages generated indexes must be defined before its foreign keys")
	}
	if count := strings.Count(messagesTable, ") VIRTUAL"); count != 3 || strings.Contains(messagesTable, ") STORED") {
		t.Errorf("MySQL messages generated columns must be virtual; virtual count=%d", count)
	}
}

func TestLinuxDoExternalSchemaContract(t *testing.T) {
	t.Parallel()
	postgres := strings.Join(postgresFreshSchema(), "\n")
	mysql := strings.Join(mysqlFreshSchema(), "\n")
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		for _, fragment := range []string{
			"CREATE TABLE oauth_identities",
			"PRIMARY KEY(provider,subject)",
			"UNIQUE(user_id,provider)",
			"CREATE TABLE oauth_login_states",
			"CREATE TABLE oauth_registration_challenges",
			"idx_oauth_login_states_expires",
			"idx_oauth_registration_challenges_expires",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s Linux.do schema is missing %q", name, fragment)
			}
		}
	}
	statements := linuxDoExternalSchemaStatements()
	if len(statements) != 6 {
		t.Fatalf("v2 migration has %d statements, want 6", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "oauth_") {
			t.Errorf("v2 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestRegistrationInviteExternalSchemaContract(t *testing.T) {
	t.Parallel()
	postgres := strings.Join(postgresFreshSchema(), "\n")
	mysql := strings.Join(mysqlFreshSchema(), "\n")
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		for _, fragment := range []string{"CREATE TABLE registration_invites", "code VARCHAR(64) NOT NULL UNIQUE", "CHECK(max_uses > 0)", "idx_registration_invites_created"} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s registration invite schema is missing %q", name, fragment)
			}
		}
	}
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		if !strings.Contains(schema, "CHECK(used_count >= 0)") || !strings.Contains(schema, "CHECK(used_count <= max_uses)") {
			t.Errorf("%s registration invite schema is missing portable usage constraints", name)
		}
	}
	statements := registrationInviteExternalSchemaStatements()
	if len(statements) != 2 {
		t.Fatalf("v3 migration has %d statements, want 2", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "registration_invites") {
			t.Errorf("v3 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestTelegramNotificationExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]string{
		"postgres": strings.Join(postgresFreshSchema(), "\n"),
		"mysql":    strings.Join(mysqlFreshSchema(), "\n"),
	} {
		for _, fragment := range []string{
			"CREATE TABLE telegram_notification_settings",
			"CREATE TABLE telegram_notification_outbox",
			"event_key VARCHAR(255) NOT NULL UNIQUE",
			"idx_telegram_notification_outbox_due",
			"idx_telegram_notification_outbox_user",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s Telegram notification schema is missing %q", name, fragment)
			}
		}
	}

	statements := telegramNotificationExternalSchemaStatements()
	if len(statements) != 4 {
		t.Fatalf("v4 migration has %d statements, want 4", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "telegram_notification_") {
			t.Errorf("v4 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestLoginRateLimitExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]string{
		"postgres": strings.Join(postgresFreshSchema(), "\n"),
		"mysql":    strings.Join(mysqlFreshSchema(), "\n"),
	} {
		for _, fragment := range []string{
			"CREATE TABLE login_rate_limits",
			"PRIMARY KEY(scope,subject_hash)",
			"CHECK(scope IN ('account','ip'))",
			"idx_login_rate_limits_updated",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s login rate-limit schema is missing %q", name, fragment)
			}
		}
	}

	statements := loginRateLimitExternalSchemaStatements()
	if len(statements) != 2 {
		t.Fatalf("v5 migration has %d statements, want 2", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "login_rate_limits") {
			t.Errorf("v5 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestQueueLeaseExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		for _, table := range []string{"send_queue", "status_webhook_outbox", "telegram_notification_outbox"} {
			fragment := ""
			for _, statement := range schema {
				if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE "+table+" ") {
					fragment = statement
					break
				}
			}
			if fragment == "" {
				t.Fatalf("%s schema is missing %s", name, table)
			}
			for _, column := range []string{"lease_owner", "lease_token", "lease_until"} {
				if !strings.Contains(fragment, column) {
					t.Errorf("%s %s is missing %s", name, table, column)
				}
			}
		}
	}

	statements := queueLeaseExternalSchemaStatements()
	if len(statements) != 9 {
		t.Fatalf("v6 migration has %d statements, want 9", len(statements))
	}
	for _, table := range []string{"send_queue", "status_webhook_outbox", "telegram_notification_outbox"} {
		count := 0
		for _, statement := range statements {
			if strings.Contains(statement, "ALTER TABLE "+table+" ADD COLUMN lease_") {
				count++
			}
		}
		if count != 3 {
			t.Errorf("v6 migration has %d lease columns for %s, want 3", count, table)
		}
	}
}

func schemaTableNames(statements []string) []string {
	names := make([]string, 0, len(externalSchemaTables))
	for _, statement := range statements {
		fields := strings.Fields(statement)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE") {
			names = append(names, strings.Trim(fields[2], "`\""))
		}
	}
	return names
}

// TestEveryMigratedColumnExistsInFreshSchema is a structural guard against the trap
// CLAUDE.md warns about: adding a column to the migration chain but not to the fresh
// schema leaves upgraded databases correct while every *new* MySQL/PostgreSQL install
// crashes on the missing column. Local SQLite development cannot surface it, and the
// per-feature contract tests only cover columns somebody remembered to add them for.
//
// The migration statements live as literals inside external_schema.go, so they are read
// back from the source. That keeps the check automatic for future migrations rather than
// something each one has to opt into.
func TestEveryMigratedColumnExistsInFreshSchema(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("external_schema.go")
	if err != nil {
		t.Fatal(err)
	}
	alter := regexp.MustCompile("ALTER TABLE ([a-z_]+) ADD COLUMN ([a-z_]+)")
	matches := alter.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no ALTER TABLE ... ADD COLUMN statements; has the migration chain moved?")
	}
	for name, schema := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		for _, match := range matches {
			table, column := match[1], match[2]
			fragment := ""
			for _, statement := range schema {
				if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE "+table+" ") {
					fragment = statement
					break
				}
			}
			if fragment == "" {
				t.Errorf("%s fresh schema is missing table %s, which a migration alters", name, table)
				continue
			}
			if !columnDeclaredIn(fragment, column) {
				t.Errorf("%s fresh schema: %s.%s is added by a migration but absent from CREATE TABLE — new installs will fail on it", name, table, column)
			}
		}
	}
}

// columnDeclaredIn looks for a column declaration rather than a bare substring, so a
// name that merely appears inside another column, an index or a default value does not
// count as declared.
func columnDeclaredIn(createTable, column string) bool {
	for _, line := range strings.Split(createTable, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == column {
			return true
		}
	}
	return false
}
