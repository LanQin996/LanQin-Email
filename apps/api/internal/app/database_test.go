package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "mysql duplicate", err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, want: true},
		{name: "postgres unique", err: &pgconn.PgError{Code: "23505"}, want: true},
		{name: "sqlite unique", err: errors.New("UNIQUE constraint failed: users.email"), want: true},
		{name: "other", err: errors.New("connection reset")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUniqueViolation(tt.err); got != tt.want {
				t.Fatalf("isUniqueViolation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigDatabaseSettings(t *testing.T) {
	t.Setenv("LANQIN_DB_DRIVER", "POSTGRESQL")
	t.Setenv("LANQIN_DB_DSN", "postgres://fallback")
	t.Setenv("LANQIN_DATABASE_URL", "postgres://preferred")
	t.Setenv("LANQIN_DB_MAX_OPEN_CONNS", "42")
	t.Setenv("LANQIN_DB_MAX_IDLE_CONNS", "12")
	t.Setenv("LANQIN_DB_CONN_MAX_LIFETIME_SECONDS", "900")
	t.Setenv("LANQIN_DB_CONN_MAX_IDLE_TIME_SECONDS", "120")
	t.Setenv("LANQIN_DB_CONNECT_TIMEOUT_SECONDS", "7")

	cfg := LoadConfig()
	if cfg.DBDriver != "postgresql" {
		t.Fatalf("DBDriver = %q, want postgres environment value", cfg.DBDriver)
	}
	if cfg.DBDSN != "postgres://preferred" {
		t.Fatalf("DBDSN = %q, want LANQIN_DATABASE_URL precedence", cfg.DBDSN)
	}
	if cfg.DBMaxOpenConns != 42 || cfg.DBMaxIdleConns != 12 {
		t.Fatalf("pool sizes = %d/%d, want 42/12", cfg.DBMaxOpenConns, cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetimeSeconds != 900 || cfg.DBConnMaxIdleTimeSeconds != 120 || cfg.DBConnectTimeoutSeconds != 7 {
		t.Fatalf("database durations = %d/%d/%d, want 900/120/7",
			cfg.DBConnMaxLifetimeSeconds, cfg.DBConnMaxIdleTimeSeconds, cfg.DBConnectTimeoutSeconds)
	}
}

func TestNormalizeDatabaseConfig(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		want   string
	}{
		{name: "legacy empty is sqlite", driver: "", want: databaseDriverSQLite},
		{name: "sqlite alias", driver: "sqlite3", want: databaseDriverSQLite},
		{name: "mysql", driver: "MYSQL", want: databaseDriverMySQL},
		{name: "postgres alias", driver: "pgsql", want: databaseDriverPostgres},
		{name: "postgresql alias", driver: "PostgreSQL", want: databaseDriverPostgres},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeDatabaseConfig(Config{DBDriver: tt.driver, DBMaxOpenConns: 3, DBMaxIdleConns: 8})
			if cfg.DBDriver != tt.want {
				t.Fatalf("driver = %q, want %q", cfg.DBDriver, tt.want)
			}
			if cfg.DBMaxIdleConns != 3 {
				t.Fatalf("max idle = %d, want clamped to max open 3", cfg.DBMaxIdleConns)
			}
		})
	}
}

func TestOpenDatabaseSQLiteDefaults(t *testing.T) {
	db, err := openDatabase(context.Background(), Config{DBPath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE smoke_test (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("sqlite smoke test: %v", err)
	}
}

func TestOpenDatabaseRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "unknown driver", cfg: Config{DBDriver: "oracle"}},
		{name: "mysql missing DSN", cfg: Config{DBDriver: "mysql"}},
		{name: "postgres missing DSN", cfg: Config{DBDriver: "postgres"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if db, err := openDatabase(context.Background(), tt.cfg); err == nil {
				_ = db.Close()
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestRebindPostgres(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "ordinary placeholders",
			query: "SELECT * FROM messages WHERE mailbox_id = ? AND created_at < ?",
			want:  "SELECT * FROM messages WHERE mailbox_id = $1 AND created_at < $2",
		},
		{
			name:  "quoted values and identifiers",
			query: `SELECT '?', "column?", 'it''s ?' FROM messages WHERE id = ?`,
			want:  `SELECT '?', "column?", 'it''s ?' FROM messages WHERE id = $1`,
		},
		{
			name:  "comments",
			query: "SELECT ? -- leave ? alone\n/* outer ? /* nested ? */ done */ WHERE id = ?",
			want:  "SELECT $1 -- leave ? alone\n/* outer ? /* nested ? */ done */ WHERE id = $2",
		},
		{
			name:  "dollar quoted body",
			query: "SELECT $$body ?$$, $tag$also ?$tag$, ?",
			want:  "SELECT $$body ?$$, $tag$also ?$tag$, $1",
		},
		{
			name:  "json operators and escaped literal",
			query: "SELECT payload ?| array['a'], payload ?& array['b'], payload ?? 'c' WHERE id = ?",
			want:  "SELECT payload ?| array['a'], payload ?& array['b'], payload ? 'c' WHERE id = $1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rebindPostgres(tt.query); got != tt.want {
				t.Fatalf("rebindPostgres() =\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRebindingConnectorCoversDirectPreparedAndTransactionQueries(t *testing.T) {
	recorder := &recordingConn{}
	db := openRecordingDB(recorder)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "UPDATE direct_query SET value = ?", "direct"); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, "UPDATE prepared_query SET value = ?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx, "prepared"); err != nil {
		_ = stmt.Close()
		t.Fatal(err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE transaction_query SET value = ?", "transaction"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"UPDATE direct_query SET value = $1",
		"UPDATE prepared_query SET value = $1",
		"UPDATE transaction_query SET value = $1",
	}
	if len(recorder.queries) != len(want) {
		t.Fatalf("recorded queries = %q, want %q", recorder.queries, want)
	}
	for i := range want {
		if recorder.queries[i] != want[i] {
			t.Fatalf("query %d = %q, want %q", i, recorder.queries[i], want[i])
		}
	}
}

func openRecordingDB(conn *recordingConn) *sql.DB {
	return sql.OpenDB(rebindingConnector{inner: recordingConnector{conn: conn}})
}

type recordingConnector struct {
	conn *recordingConn
}

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return c.conn, nil
}

func (c recordingConnector) Driver() driver.Driver {
	return recordingDriver{conn: c.conn}
}

type recordingDriver struct {
	conn *recordingConn
}

func (d recordingDriver) Open(string) (driver.Conn, error) {
	return d.conn, nil
}

type recordingConn struct {
	queries []string
}

func (c *recordingConn) Prepare(query string) (driver.Stmt, error) {
	c.queries = append(c.queries, query)
	return recordingStmt{}, nil
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) Begin() (driver.Tx, error) { return recordingTx{}, nil }

func (c *recordingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return recordingTx{}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.queries = append(c.queries, query)
	return driver.RowsAffected(1), nil
}

type recordingStmt struct{}

func (recordingStmt) Close() error  { return nil }
func (recordingStmt) NumInput() int { return -1 }
func (recordingStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (recordingStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("recording statement query is not supported")
}

type recordingTx struct{}

func (recordingTx) Commit() error   { return nil }
func (recordingTx) Rollback() error { return nil }
