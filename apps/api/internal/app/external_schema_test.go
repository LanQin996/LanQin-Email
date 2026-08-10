package app

import (
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
		"CREATE FUNCTION lanqin_delete_mailbox_webhooks",
		"CREATE FUNCTION lanqin_delete_queue_delivery_events",
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
		"CREATE INDEX idx_messages_mailbox_raw_path ON messages(mailbox_id,raw_path)",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("MySQL schema contains unsupported SQL %q", forbidden)
		}
	}
	for _, statement := range statements {
		trimmed := strings.TrimSpace(statement)
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
		"mailbox_raw_path_key BINARY(32)",
		"unregistered_raw_path_key BINARY(32)",
		"positive_imap_uid BIGINT GENERATED ALWAYS",
		"dedupe_message_id VARCHAR(512)",
		"FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"trg_mailbox_delete_status_webhook_outbox",
		"trg_send_queue_delete_delivery_events",
	} {
		if !strings.Contains(schema, fragment) {
			t.Errorf("MySQL schema is missing %q", fragment)
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
