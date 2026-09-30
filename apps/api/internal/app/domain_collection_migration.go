package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func migrateExternalSchemaV12(ctx context.Context, conn *sql.Conn, driver string) error {
	var count, version int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations").Scan(&count, &version); err != nil {
		return err
	}
	if count >= 12 && version >= 12 {
		return nil
	}
	if count != 11 || version != 11 {
		return fmt.Errorf("unexpected schema before v12: %d/%d", count, version)
	}
	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		var err error
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		executor = tx
	}
	for _, stmt := range domainCollectionSchema() {
		if driver == databaseDriverMySQL && strings.HasPrefix(stmt, "CREATE TABLE ") {
			stmt = postgresTableToMySQL(stmt)
		}
		if _, err := executor.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := executor.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at) VALUES(12,'external_schema_v12_domain_collection',CURRENT_TIMESTAMP)"); err != nil {
		return err
	}
	if tx != nil {
		return tx.Commit()
	}
	return nil
}
