package app

import (
	"regexp"
	"strings"
)

var mysqlLongTextDefault = regexp.MustCompile(`LONGTEXT NOT NULL DEFAULT ('[^']*')`)

func mysqlFreshSchema() []string {
	postgres := postgresFreshSchema()
	statements := make([]string, 0, len(postgres)+12)
	for _, statement := range postgres {
		trimmed := strings.TrimSpace(statement)
		switch {
		case strings.HasPrefix(trimmed, "CREATE FUNCTION "), strings.HasPrefix(trimmed, "CREATE TRIGGER "):
			continue
		case (strings.HasPrefix(trimmed, "CREATE INDEX ") || strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ")) && strings.Contains(trimmed, " WHERE "):
			continue
		case strings.HasPrefix(trimmed, "CREATE TABLE "):
			statements = append(statements, postgresTableToMySQL(statement))
		default:
			statements = append(statements, statement)
		}
	}

	// MySQL has no partial indexes. Nullable generated keys preserve the
	// uniqueness semantics without indexing LONGTEXT values directly.
	statements = append(statements,
		`ALTER TABLE messages
			ADD COLUMN mailbox_raw_path_key BINARY(32) GENERATED ALWAYS AS (
				CASE WHEN raw_path<>'' AND mailbox_id IS NOT NULL
				THEN UNHEX(SHA2(CONCAT(mailbox_id,CHAR(0),raw_path),256)) ELSE NULL END
			) STORED,
			ADD COLUMN unregistered_raw_path_key BINARY(32) GENERATED ALWAYS AS (
				CASE WHEN raw_path<>'' AND mailbox_id IS NULL
				THEN UNHEX(SHA2(raw_path,256)) ELSE NULL END
			) STORED,
			ADD COLUMN positive_imap_uid BIGINT GENERATED ALWAYS AS (
				CASE WHEN folder_id IS NOT NULL AND imap_uid>0 THEN imap_uid ELSE NULL END
			) STORED,
			ADD UNIQUE INDEX idx_messages_mailbox_raw_path (mailbox_id,mailbox_raw_path_key),
			ADD UNIQUE INDEX idx_messages_unregistered_raw_path (unregistered_raw_path_key),
			ADD UNIQUE INDEX idx_messages_folder_imap_uid (folder_id,positive_imap_uid)`,
		`CREATE INDEX idx_messages_mailbox_starred_received_id ON messages(mailbox_id,is_starred,received_at DESC,id DESC)`,
		`CREATE INDEX idx_messages_mailbox_message_id ON messages(mailbox_id,message_id)`,
		`CREATE INDEX idx_messages_maildir_backfill ON messages(created_at)`,
		`CREATE INDEX idx_messages_maildir_cleanup ON messages(updated_at)`,
		`CREATE INDEX idx_external_imap_messages_local ON external_imap_messages(local_message_id)`,
		`ALTER TABLE send_queue
			ADD COLUMN dedupe_message_id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin GENERATED ALWAYS AS (
				CASE WHEN message_id<>'' THEN message_id ELSE NULL END
			) STORED,
			ADD UNIQUE INDEX idx_send_queue_mailbox_source_message_id (mailbox_id,source,dedupe_message_id)`,
		`CREATE TRIGGER trg_mailbox_delete_status_webhook_outbox
			AFTER DELETE ON mailboxes FOR EACH ROW
			DELETE FROM status_webhook_outbox WHERE mailbox_id=OLD.id`,
		`CREATE TRIGGER trg_send_queue_delete_delivery_events
			AFTER DELETE ON send_queue FOR EACH ROW
			DELETE FROM delivery_events WHERE queue_id=OLD.id`,
	)
	return statements
}

func postgresTableToMySQL(statement string) string {
	statement = strings.ReplaceAll(statement, "TIMESTAMPTZ", "TIMESTAMP")
	statement = strings.ReplaceAll(statement, "VARCHAR(998)", "VARCHAR(512)")
	statement = strings.ReplaceAll(statement, "TEXT", "LONGTEXT")
	statement = mysqlLongTextDefault.ReplaceAllString(statement, "LONGTEXT NOT NULL DEFAULT ($1)")

	lines := strings.Split(statement, "\n")
	foreignKeys := make([]string, 0, 4)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "system ") {
			line = strings.Replace(line, "system", "`system`", 1)
			lines[i] = line
			trimmed = strings.TrimSpace(line)
		}
		referenceAt := strings.Index(trimmed, " REFERENCES ")
		if referenceAt < 0 {
			continue
		}
		columnDefinition := trimmed[:referenceAt]
		reference := strings.TrimSuffix(trimmed[referenceAt+1:], ",")
		fields := strings.Fields(columnDefinition)
		if len(fields) == 0 {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + columnDefinition + ","
		foreignKeys = append(foreignKeys, "\t\t\tFOREIGN KEY ("+fields[0]+") "+reference)
	}

	if len(foreignKeys) != 0 {
		closing := len(lines) - 1
		for closing >= 0 && strings.TrimSpace(lines[closing]) == "" {
			closing--
		}
		previous := closing - 1
		if !strings.HasSuffix(strings.TrimSpace(lines[previous]), ",") {
			lines[previous] += ","
		}
		for i := range foreignKeys[:len(foreignKeys)-1] {
			foreignKeys[i] += ","
		}
		withKeys := make([]string, 0, len(lines)+len(foreignKeys))
		withKeys = append(withKeys, lines[:closing]...)
		withKeys = append(withKeys, foreignKeys...)
		withKeys = append(withKeys, lines[closing:]...)
		lines = withKeys
	}
	return strings.Join(lines, "\n") + " ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
}
