package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
	"lanqin-email-api/internal/backup/nativedb"
)

var errNativeDatabaseBackup = errors.New("native database snapshot cannot be safely created or restored")
var nativeLengthPattern = regexp.MustCompile(`length\(CAST\(([A-Za-z_][A-Za-z_0-9.]*) AS BLOB\)\)`)
var nativeServerFamily = regexp.MustCompile(`^([0-9]+)\.([0-9]+)`)
var nativeIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func backupNativeLengthSQL(query, driver string) string {
	if driver == databaseDriverMySQL || driver == databaseDriverPostgres {
		return nativeLengthPattern.ReplaceAllString(query, `OCTET_LENGTH($1)`)
	}
	return query
}

func nativeIdentifier(driver, name string) (string, error) {
	if !nativeIdentifierPattern.MatchString(name) {
		return "", errNativeDatabaseBackup
	}
	if driver == databaseDriverMySQL {
		return "`" + name + "`", nil
	}
	if driver == databaseDriverPostgres {
		return `"` + name + `"`, nil
	}
	return "", errNativeDatabaseBackup
}

func openNativeBackupDatabase(ctx context.Context, cfg Config) (*sql.DB, error) {
	cfg = normalizeDatabaseConfig(cfg)
	if cfg.DBDriver != databaseDriverMySQL && cfg.DBDriver != databaseDriverPostgres {
		return nil, errNativeDatabaseBackup
	}
	if cfg.DBDriver == databaseDriverMySQL {
		parsed, err := mysql.ParseDSN(cfg.DBDSN)
		if err != nil {
			return nil, errNativeDatabaseBackup
		}
		parsed.ParseTime = false
		cfg.DBDSN = parsed.FormatDSN()
	}
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	statement := "SET TIME ZONE 'UTC'"
	if cfg.DBDriver == databaseDriverMySQL {
		statement = "SET SESSION time_zone='+00:00'"
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		db.Close()
		return nil, errNativeDatabaseBackup
	}
	if cfg.DBDriver == databaseDriverMySQL {
		if _, err := db.ExecContext(ctx, "SET SESSION sql_mode='STRICT_ALL_TABLES,NO_ZERO_DATE,NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"); err != nil {
			db.Close()
			return nil, errNativeDatabaseBackup
		}
	}
	return db, nil
}

type nativeBackupCut struct {
	DB          *sql.DB
	Tx          *sql.Tx
	Header      nativedb.Header
	Rows        int64
	OmittedJobs int
}

func (c *nativeBackupCut) Close() error {
	if c == nil {
		return nil
	}
	if c.Tx != nil {
		c.Tx.Rollback()
		c.Tx = nil
	}
	if c.DB != nil {
		err := c.DB.Close()
		c.DB = nil
		return err
	}
	return nil
}

func nativeSchemaHeader(ctx context.Context, query backupSnapshotQuery, driver string) (nativedb.Header, error) {
	header := nativedb.Header{Version: 1, Driver: driver, SchemaVersion: externalSchemaVersion, Tables: []nativedb.Table{}}
	var err error
	header.Family, err = nativeDatabaseFamily(ctx, query, driver)
	if err != nil {
		return header, err
	}
	return nativeSchemaHeaderForFamily(ctx, query, header)
}

func nativeDatabaseFamily(ctx context.Context, query backupSnapshotQuery, driver string) (string, error) {
	var version string
	versionQuery := "SELECT VERSION()"
	if driver == databaseDriverPostgres {
		versionQuery = "SHOW server_version"
	}
	if err := query.QueryRowContext(ctx, versionQuery).Scan(&version); err != nil {
		return "", errNativeDatabaseBackup
	}
	parts := nativeServerFamily.FindStringSubmatch(version)
	if len(parts) != 3 {
		return "", errNativeDatabaseBackup
	}
	family := driver + ":" + parts[1]
	if driver == databaseDriverMySQL {
		family += "." + parts[2]
	}
	return family, nil
}

func nativeSchemaHeaderForFamily(ctx context.Context, query backupSnapshotQuery, header nativedb.Header) (nativedb.Header, error) {
	driver := header.Driver
	var count, maxVersion int
	if err := query.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations").Scan(&count, &maxVersion); err != nil || count != externalSchemaVersion || maxVersion != externalSchemaVersion {
		return header, errNativeDatabaseBackup
	}
	where := "table_schema=current_schema()"
	if driver == databaseDriverMySQL {
		where = "table_schema=DATABASE()"
	}
	rows, err := query.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE "+where+" AND table_type='BASE TABLE' ORDER BY table_name")
	if err != nil {
		return header, errNativeDatabaseBackup
	}
	tables := []string{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			rows.Close()
			return header, errNativeDatabaseBackup
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	expected := append([]string{}, externalSchemaTables...)
	sort.Strings(expected)
	if err != nil || strings.Join(tables, "\x00") != strings.Join(expected, "\x00") {
		return header, errNativeDatabaseBackup
	}
	if validateExternalSchema(ctx, query, tables, "external_schema_v1_"+driver) != nil {
		return header, errNativeDatabaseBackup
	}
	var triggers int
	triggerWhere := "trigger_schema=current_schema()"
	if driver == databaseDriverMySQL {
		triggerWhere = "trigger_schema=DATABASE()"
	}
	if err := query.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.triggers WHERE "+triggerWhere).Scan(&triggers); err != nil || triggers != 0 {
		return header, errNativeDatabaseBackup
	}
	if driver == databaseDriverMySQL {
		var unsupported int
		if err := query.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' AND engine<>'InnoDB'").Scan(&unsupported); err != nil || unsupported != 0 {
			return header, errNativeDatabaseBackup
		}
	} else {
		var unsupported int
		if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind IN ('r','p') AND (c.relkind<>'r' OR c.relpersistence<>'p' OR c.relrowsecurity OR c.relforcerowsecurity)`).Scan(&unsupported); err != nil || unsupported != 0 {
			return header, errNativeDatabaseBackup
		}
	}
	generated := "is_generated"
	if driver == databaseDriverMySQL {
		generated = "CASE WHEN generation_expression<>'' THEN 'ALWAYS' ELSE 'NEVER' END"
	}
	rows, err = query.QueryContext(ctx, "SELECT table_name,column_name,data_type,is_nullable,"+generated+",COALESCE(character_maximum_length,0),COALESCE(numeric_precision,0),COALESCE(numeric_scale,0),COALESCE(collation_name,''),COALESCE(datetime_precision,0) FROM information_schema.columns WHERE "+where+" ORDER BY table_name,column_name")
	if err != nil {
		return header, errNativeDatabaseBackup
	}
	defer rows.Close()
	index := map[string]int{}
	for _, name := range tables {
		index[name] = len(header.Tables)
		header.Tables = append(header.Tables, nativedb.Table{Name: name, Columns: []nativedb.Column{}})
	}
	for rows.Next() {
		var name, nullable, generation string
		var column nativedb.Column
		if rows.Scan(&name, &column.Name, &column.Type, &nullable, &generation, &column.MaxLength, &column.Precision, &column.Scale, &column.Collation, &column.DatetimePrecision) != nil {
			return header, errNativeDatabaseBackup
		}
		i, ok := index[name]
		if !ok {
			return header, errNativeDatabaseBackup
		}
		column.Nullable = nullable == "YES"
		column.Generated = generation != "NEVER"
		header.Tables[i].Columns = append(header.Tables[i].Columns, column)
	}
	if rows.Err() != nil || header.Validate() != nil {
		return header, errNativeDatabaseBackup
	}
	return header, nil
}

// prepareNativeDatabaseCut owns an independent read-only repeatable-read
// connection. Row export and later file-reference queries share this exact cut.
// It never starts application workers or writes to the original database.
func prepareNativeDatabaseCut(ctx context.Context, cfg Config, output string, maxBytes int64) (*nativeBackupCut, error) {
	cfg = normalizeDatabaseConfig(cfg)
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cut := &nativeBackupCut{DB: db}
	ok := false
	defer func() {
		if !ok {
			cut.Close()
		}
	}()
	cut.Tx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	cut.Header, err = nativeSchemaHeader(ctx, cut.Tx, cfg.DBDriver)
	if err != nil {
		return nil, err
	}
	key, _ := nativeIdentifier(cfg.DBDriver, "key")
	var pending int
	if err := cut.Tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM system_settings WHERE "+key+"=?", nativeRestorePendingKey).Scan(&pending); err != nil || pending != 0 {
		return nil, errNativeDatabaseBackup
	}
	// Hold metadata/access-share locks for all known tables. Ordinary business
	// writes remain possible; concurrent schema changes are unsupported.
	for _, table := range cut.Header.Tables {
		name, _ := nativeIdentifier(cfg.DBDriver, table.Name)
		rows, err := cut.Tx.QueryContext(ctx, "SELECT * FROM "+name+" LIMIT 0")
		if err != nil {
			return nil, errNativeDatabaseBackup
		}
		rows.Close()
	}
	confirmed, err := nativeSchemaHeader(ctx, cut.Tx, cfg.DBDriver)
	if err != nil || jsonEncode(confirmed) != jsonEncode(cut.Header) {
		return nil, errNativeDatabaseBackup
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer file.Close()
	limits := nativedb.DefaultLimits()
	limits.MaxBytes = maxBytes
	writer, err := nativedb.NewWriter(file, cut.Header, limits)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	for _, table := range cut.Header.Tables {
		name, _ := nativeIdentifier(cfg.DBDriver, table.Name)
		columns := []nativedb.Column{}
		quoted := []string{}
		sizes := []string{}
		for _, column := range table.Columns {
			if !column.Generated {
				columns = append(columns, column)
				q, _ := nativeIdentifier(cfg.DBDriver, column.Name)
				quoted = append(quoted, q)
				if nativeColumnVariableWidth(column.Type) {
					sizes = append(sizes, "COALESCE(OCTET_LENGTH("+q+"),0)")
				}
			}
		}
		if len(sizes) > 0 {
			var largest int64
			if err := cut.Tx.QueryRowContext(ctx, "SELECT COALESCE(MAX("+strings.Join(sizes, "+")+"),0) FROM "+name).Scan(&largest); err != nil || largest > int64(limits.MaxRecordBytes-65536)*3/4 {
				return nil, errNativeDatabaseBackup
			}
		}
		where := ""
		if table.Name == "system_settings" {
			key, _ := nativeIdentifier(cfg.DBDriver, "key")
			where = " WHERE " + key + " NOT IN ('backup.queue.v1','backup.schedules.v1') AND " + key + " NOT LIKE 'backup.job.v1.%' AND " + key + " NOT LIKE 'backup.transfer.v1.%' AND " + key + " NOT LIKE 'backup.discovery.v1.%' AND " + key + " NOT LIKE 'backup.drive.oauth.v1.%' AND " + key + " NOT LIKE 'backup.native.restore.%'"
			if err := cut.Tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name+" WHERE "+key+" LIKE 'backup.job.v1.%'").Scan(&cut.OmittedJobs); err != nil {
				return nil, errNativeDatabaseBackup
			}
		}
		rows, err := cut.Tx.QueryContext(ctx, "SELECT "+strings.Join(quoted, ",")+" FROM "+name+where)
		if err != nil {
			return nil, errNativeDatabaseBackup
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if ctx.Err() != nil {
				rows.Close()
				return nil, ctx.Err()
			}
			if rows.Scan(pointers...) != nil {
				rows.Close()
				return nil, errNativeDatabaseBackup
			}
			cells := make([]nativedb.Cell, len(columns))
			for i, value := range values {
				cell, err := nativedb.EncodeCell(value, nativeColumnTextual(columns[i].Type), limits.MaxCellBytes)
				if err != nil {
					rows.Close()
					return nil, errNativeDatabaseBackup
				}
				cells[i] = cell
			}
			if writer.Row(table.Name, cells) != nil {
				rows.Close()
				return nil, errNativeDatabaseBackup
			}
			cut.Rows++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errNativeDatabaseBackup
		}
	}
	if writer.Close() != nil || file.Sync() != nil || file.Close() != nil {
		return nil, errNativeDatabaseBackup
	}
	ok = true
	return cut, nil
}

func nativeColumnTextual(kind string) bool {
	return strings.Contains(kind, "char") || strings.Contains(kind, "text") || strings.Contains(kind, "json") || strings.Contains(kind, "enum") || strings.Contains(kind, "date") || strings.Contains(kind, "time")
}
func nativeColumnBinary(kind string) bool {
	return strings.Contains(kind, "blob") || strings.Contains(kind, "binary") || kind == "bytea"
}

func nativeColumnVariableWidth(kind string) bool {
	return strings.Contains(kind, "char") || strings.Contains(kind, "text") || strings.Contains(kind, "json") || strings.Contains(kind, "enum") || nativeColumnBinary(kind)
}
