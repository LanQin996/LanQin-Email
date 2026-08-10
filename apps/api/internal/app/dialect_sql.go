package app

import "strings"

func insertIgnoreSQL(driver, insert, conflictTarget string) string {
	switch sqlDialect(driver) {
	case databaseDriverMySQL:
		return strings.Replace(insert, "INSERT INTO", "INSERT IGNORE INTO", 1)
	case databaseDriverPostgres:
		return insert + " ON CONFLICT " + conflictTarget + " DO NOTHING"
	default:
		return strings.Replace(insert, "INSERT INTO", "INSERT OR IGNORE INTO", 1)
	}
}

func upsertSQL(driver, insert, conflictTarget, update, mysqlUpdate string) string {
	if sqlDialect(driver) == databaseDriverMySQL {
		return insert + " ON DUPLICATE KEY UPDATE " + mysqlUpdate
	}
	return insert + " ON CONFLICT " + conflictTarget + " DO UPDATE SET " + update
}

func scalarMaxSQL(driver, left, right string) string {
	if sqlDialect(driver) == databaseDriverSQLite {
		return "MAX(" + left + "," + right + ")"
	}
	return "GREATEST(" + left + "," + right + ")"
}

func permissionGroupRenameSQL(driver string) string {
	if sqlDialect(driver) == databaseDriverMySQL {
		return `UPDATE permission_groups SET name=CONCAT(name,' (',id,')') WHERE name=? AND id<>?`
	}
	return `UPDATE permission_groups SET name=name || ' (' || id || ')' WHERE name=? AND id<>?`
}

func permissionGroupSystemColumnSQL(driver string) string {
	if sqlDialect(driver) == databaseDriverMySQL {
		return "`system`"
	}
	return "system"
}

func keyColumnSQL(driver string) string {
	if sqlDialect(driver) == databaseDriverMySQL {
		return "`key`"
	}
	return "key"
}

func apiTokenLastUsedUpdateSQL(driver string) string {
	if sqlDialect(driver) == databaseDriverSQLite {
		return `UPDATE api_tokens SET last_used_at=?
			WHERE id=? AND (last_used_at IS NULL OR julianday(last_used_at) < julianday(?))`
	}
	return `UPDATE api_tokens SET last_used_at=?
		WHERE id=? AND (last_used_at IS NULL OR last_used_at < ?)`
}

func sqlDialect(driver string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "mysql":
		return databaseDriverMySQL
	case "pg", "pgsql", "postgres", "postgresql":
		return databaseDriverPostgres
	default:
		return databaseDriverSQLite
	}
}
