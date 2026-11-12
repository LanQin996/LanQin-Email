package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lanqin-email-api/internal/backup/nativedb"
)

type nativeForeignKey struct {
	Table, Name, Parent, Definition string
	Columns, ParentColumns          []string
}

func readNativeForeignKeys(ctx context.Context, db backupSnapshotQuery, driver string) ([]nativeForeignKey, error) {
	query := `SELECT k.table_name,k.constraint_name,k.column_name,k.referenced_table_name,k.referenced_column_name,k.ordinal_position,k.referenced_table_schema,''
		FROM information_schema.key_column_usage k WHERE k.table_schema=DATABASE() AND k.referenced_table_name IS NOT NULL ORDER BY k.table_name,k.constraint_name,k.ordinal_position`
	if driver == databaseDriverPostgres {
		query = `SELECT child.relname,c.conname,ca.attname,parent.relname,pa.attname,ord.n,pns.nspname,pg_get_constraintdef(c.oid)
		FROM pg_constraint c JOIN pg_class child ON child.oid=c.conrelid JOIN pg_namespace ns ON ns.oid=child.relnamespace
		JOIN pg_class parent ON parent.oid=c.confrelid JOIN pg_namespace pns ON pns.oid=parent.relnamespace
		CROSS JOIN LATERAL unnest(c.conkey,c.confkey) WITH ORDINALITY AS ord(childatt,parentatt,n)
		JOIN pg_attribute ca ON ca.attrelid=child.oid AND ca.attnum=ord.childatt
		JOIN pg_attribute pa ON pa.attrelid=parent.oid AND pa.attnum=ord.parentatt
		WHERE ns.nspname=current_schema() AND c.contype='f' ORDER BY child.relname,c.conname,ord.n`
	}
	var schema string
	schemaQuery := "SELECT DATABASE()"
	if driver == databaseDriverPostgres {
		schemaQuery = "SELECT current_schema()"
	}
	if err := db.QueryRowContext(ctx, schemaQuery).Scan(&schema); err != nil {
		return nil, errNativeDatabaseBackup
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer rows.Close()
	keys := []nativeForeignKey{}
	allowed := map[string]bool{}
	for _, name := range externalSchemaTables {
		allowed[name] = true
	}
	for rows.Next() {
		var table, name, column, parent, parentColumn, parentSchema, definition string
		var order int
		if rows.Scan(&table, &name, &column, &parent, &parentColumn, &order, &parentSchema, &definition) != nil || !allowed[table] || !allowed[parent] || parentSchema != schema || !nativeIdentifierPattern.MatchString(name) || !nativeIdentifierPattern.MatchString(column) || !nativeIdentifierPattern.MatchString(parentColumn) || len(definition) > 4096 {
			return nil, errNativeDatabaseBackup
		}
		if len(keys) == 0 || keys[len(keys)-1].Table != table || keys[len(keys)-1].Name != name {
			keys = append(keys, nativeForeignKey{Table: table, Name: name, Parent: parent, Definition: definition})
		}
		key := &keys[len(keys)-1]
		if order != len(key.Columns)+1 || key.Parent != parent {
			return nil, errNativeDatabaseBackup
		}
		key.Columns = append(key.Columns, column)
		key.ParentColumns = append(key.ParentColumns, parentColumn)
		if len(keys) > 1024 || len(key.Columns) > 32 {
			return nil, errNativeDatabaseBackup
		}
	}
	if rows.Err() != nil {
		return nil, errNativeDatabaseBackup
	}
	return keys, nil
}

func validateNativeForeignKeys(ctx context.Context, db backupSnapshotQuery, driver string, keys []nativeForeignKey) error {
	for _, key := range keys {
		table, _ := nativeIdentifier(driver, key.Table)
		parent, _ := nativeIdentifier(driver, key.Parent)
		nonnull, equalities := []string{}, []string{}
		for i, column := range key.Columns {
			child, _ := nativeIdentifier(driver, column)
			ref, _ := nativeIdentifier(driver, key.ParentColumns[i])
			nonnull = append(nonnull, "c."+child+" IS NOT NULL")
			equalities = append(equalities, "p."+ref+"=c."+child)
		}
		query := "SELECT COUNT(*) FROM " + table + " c WHERE " + strings.Join(nonnull, " AND ") + " AND NOT EXISTS (SELECT 1 FROM " + parent + " p WHERE " + strings.Join(equalities, " AND ") + ")"
		var invalid int64
		if err := db.QueryRowContext(ctx, query).Scan(&invalid); err != nil || invalid != 0 {
			return errNativeDatabaseBackup
		}
	}
	return nil
}

// restoreNativeDatabaseRows imports only into an explicitly selected EMPTY
// same-engine database. Tables are created by the trusted application schema,
// never from archived DDL. It does not activate the database or start workers.
func restoreNativeDatabaseRows(ctx context.Context, cfg Config, input string, maxBytes int64, expectedDatabaseSHA256 ...string) (nativedb.Header, int64, error) {
	var empty nativedb.Header
	cfg = normalizeDatabaseConfig(cfg)
	info, err := os.Lstat(input)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return empty, 0, errNativeDatabaseBackup
	}
	file, err := os.Open(input)
	if err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return empty, 0, errNativeDatabaseBackup
	}
	limits := nativedb.DefaultLimits()
	limits.MaxBytes = maxBytes
	firstDigest := sha256.New()
	header, expectedRows, err := nativedb.Read(ctx, io.TeeReader(file, firstDigest), limits, nil, nil)
	if err != nil || header.Driver != cfg.DBDriver || header.SchemaVersion != externalSchemaVersion {
		return empty, 0, errNativeDatabaseBackup
	}
	if len(expectedDatabaseSHA256) > 1 {
		return empty, 0, errNativeDatabaseBackup
	}
	if len(expectedDatabaseSHA256) == 1 {
		if hex.EncodeToString(firstDigest.Sum(nil)) != strings.ToLower(expectedDatabaseSHA256[0]) {
			return empty, 0, errNativeDatabaseBackup
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return empty, 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	defer conn.Close()
	unlock, err := lockExternalSchema(ctx, conn, cfg.DBDriver)
	if err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	defer unlock()
	family, err := nativeDatabaseFamily(ctx, conn, cfg.DBDriver)
	if err != nil || family != header.Family {
		return empty, 0, errNativeDatabaseBackup
	}
	if initializeExternalSchemaOnConn(ctx, conn, cfg.DBDriver, true) != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	actual, err := nativeSchemaHeader(ctx, conn, cfg.DBDriver)
	if err != nil || jsonEncode(actual) != jsonEncode(header) {
		return empty, 0, errNativeDatabaseBackup
	}
	keys, err := readNativeForeignKeys(ctx, conn, cfg.DBDriver)
	if err != nil {
		return empty, 0, err
	}
	if cfg.DBDriver == databaseDriverMySQL {
		if _, err := conn.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=0"); err != nil {
			return empty, 0, errNativeDatabaseBackup
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			conn.ExecContext(cleanup, "SET SESSION FOREIGN_KEY_CHECKS=1")
		}()
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	defer tx.Rollback()
	for _, table := range header.Tables {
		if table.Name == "schema_migrations" {
			continue
		}
		name, _ := nativeIdentifier(cfg.DBDriver, table.Name)
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name).Scan(&count); err != nil || count != 0 {
			return empty, 0, errNativeDatabaseBackup
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM schema_migrations"); err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	if cfg.DBDriver == databaseDriverPostgres {
		for _, key := range keys {
			table, _ := nativeIdentifier(cfg.DBDriver, key.Table)
			name, _ := nativeIdentifier(cfg.DBDriver, key.Name)
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+name); err != nil {
				return empty, 0, errNativeDatabaseBackup
			}
		}
	}
	statements := map[string]*sql.Stmt{}
	defer func() {
		for _, statement := range statements {
			statement.Close()
		}
	}()
	for _, table := range header.Tables {
		name, _ := nativeIdentifier(cfg.DBDriver, table.Name)
		columns, placeholders := []string{}, []string{}
		for _, column := range table.Columns {
			if !column.Generated {
				columnName, _ := nativeIdentifier(cfg.DBDriver, column.Name)
				columns = append(columns, columnName)
				placeholders = append(placeholders, "?")
			}
		}
		statement, err := tx.PrepareContext(ctx, "INSERT INTO "+name+"("+strings.Join(columns, ",")+") VALUES("+strings.Join(placeholders, ",")+")")
		if err != nil {
			return empty, 0, errNativeDatabaseBackup
		}
		statements[table.Name] = statement
	}
	secondDigest := sha256.New()
	_, rows, err := nativedb.Read(ctx, io.TeeReader(file, secondDigest), limits, func(other nativedb.Header) error {
		if jsonEncode(other) != jsonEncode(header) {
			return errNativeDatabaseBackup
		}
		return nil
	}, func(table string, cells []nativedb.Cell) error {
		values := make([]any, len(cells))
		for i, cell := range cells {
			value, err := cell.Value(limits.MaxCellBytes)
			if err != nil {
				return errNativeDatabaseBackup
			}
			values[i] = value
		}
		if _, err := statements[table].ExecContext(ctx, values...); err != nil {
			return errNativeDatabaseBackup
		}
		return nil
	})
	if err != nil || rows != expectedRows || hex.EncodeToString(firstDigest.Sum(nil)) != hex.EncodeToString(secondDigest.Sum(nil)) {
		return empty, 0, errNativeDatabaseBackup
	}
	if validateNativeForeignKeys(ctx, tx, cfg.DBDriver, keys) != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	var count, maxVersion int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations").Scan(&count, &maxVersion); err != nil || count != externalSchemaVersion || maxVersion != externalSchemaVersion {
		return empty, 0, errNativeDatabaseBackup
	}
	if validateExternalSchema(ctx, tx, externalSchemaTables, "external_schema_v1_"+cfg.DBDriver) != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	if cfg.DBDriver == databaseDriverPostgres {
		for _, key := range keys {
			table, _ := nativeIdentifier(cfg.DBDriver, key.Table)
			name, _ := nativeIdentifier(cfg.DBDriver, key.Name)
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+name+" "+key.Definition); err != nil {
				return empty, 0, errNativeDatabaseBackup
			}
		}
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
		return empty, 0, errNativeDatabaseBackup
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	marker, err := json.Marshal(map[string]any{"version": 1, "driver": cfg.DBDriver, "databaseSHA256": hex.EncodeToString(firstDigest.Sum(nil)), "operation": hex.EncodeToString(random[:]), "createdAt": time.Now().UTC()})
	if err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	key, _ := nativeIdentifier(cfg.DBDriver, "key")
	if _, err := tx.ExecContext(ctx, "INSERT INTO system_settings("+key+",value,updated_at) VALUES(?,?,?)", nativeRestorePendingKey, string(marker), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	if tx.Commit() != nil {
		return empty, 0, errNativeDatabaseBackup
	}
	return header, rows, nil
}

const nativeRestorePendingKey = "backup.native.restore.pending.v1"

func checkNativeRestoreActivation(ctx context.Context, db *sql.DB, driver string, roots ...string) error {
	key, _ := nativeIdentifier(driver, "key")
	var exists int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM system_settings WHERE "+key+"=?", nativeRestorePendingKey).Scan(&exists)
	if err != nil || exists != 0 {
		return errors.New("native restored database is not activated; complete verified maintenance installation before starting services")
	}
	active, _, activeErr := readNativeRestoreMarker(ctx, db, driver, nativeRestoreActiveKey, false)
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		return errNativeDatabaseBackup
	}
	if len(roots) < 2 {
		if activeErr == nil {
			return errNativeDatabaseBackup
		}
		return nil
	}
	planBytes, planErr := nativeFileBytes(filepath.Join(roots[0], "restore-install-plan.json"), 32<<10)
	if errors.Is(activeErr, sql.ErrNoRows) {
		if !errors.Is(planErr, os.ErrNotExist) {
			return errNativeDatabaseBackup
		}
		return nil
	}
	if planErr != nil || active.Phase != "active" || active.Plan == nil || !sameNativePlan(active, active.Plan.Targets) || active.Plan.RuntimeDataRoot != roots[0] || active.Plan.RuntimeMailRoot != roots[1] {
		return errNativeDatabaseBackup
	}
	digest := sha256.Sum256(planBytes)
	if hex.EncodeToString(digest[:]) != active.PlanSHA256 {
		return errNativeDatabaseBackup
	}
	var plan NativeInstallationPlan
	if json.Unmarshal(planBytes, &plan) != nil || plan.Driver != driver || plan.Operation != active.Operation {
		return errNativeDatabaseBackup
	}
	return nil
}
