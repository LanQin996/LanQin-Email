package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const (
	databaseDriverSQLite   = "sqlite"
	databaseDriverMySQL    = "mysql"
	databaseDriverPostgres = "postgres"
)

func normalizeDatabaseConfig(cfg Config) Config {
	switch strings.ToLower(strings.TrimSpace(cfg.DBDriver)) {
	case "", "sqlite", "sqlite3":
		cfg.DBDriver = databaseDriverSQLite
	case "mysql":
		cfg.DBDriver = databaseDriverMySQL
	case "pg", "pgsql", "postgres", "postgresql":
		cfg.DBDriver = databaseDriverPostgres
	default:
		cfg.DBDriver = strings.ToLower(strings.TrimSpace(cfg.DBDriver))
	}

	if cfg.DBMaxOpenConns <= 0 {
		cfg.DBMaxOpenConns = 20
	}
	if cfg.DBMaxIdleConns <= 0 {
		cfg.DBMaxIdleConns = 10
	}
	if cfg.DBMaxIdleConns > cfg.DBMaxOpenConns {
		cfg.DBMaxIdleConns = cfg.DBMaxOpenConns
	}
	if cfg.DBConnMaxLifetimeSeconds <= 0 {
		cfg.DBConnMaxLifetimeSeconds = 1800
	}
	if cfg.DBConnMaxIdleTimeSeconds <= 0 {
		cfg.DBConnMaxIdleTimeSeconds = 300
	}
	if cfg.DBConnectTimeoutSeconds <= 0 {
		cfg.DBConnectTimeoutSeconds = 10
	}
	return cfg
}

func openDatabase(ctx context.Context, cfg Config) (*sql.DB, error) {
	cfg = normalizeDatabaseConfig(cfg)

	var (
		db  *sql.DB
		err error
	)
	switch cfg.DBDriver {
	case databaseDriverSQLite:
		if strings.TrimSpace(cfg.DBPath) == "" {
			return nil, errors.New("sqlite database path is required")
		}
		db, err = sql.Open("sqlite", cfg.DBPath)
		if err == nil {
			// SQLite serializes writes. A single shared connection also ensures that
			// connection-scoped PRAGMAs apply consistently.
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
		}
	case databaseDriverMySQL:
		if strings.TrimSpace(cfg.DBDSN) == "" {
			return nil, errors.New("mysql database DSN is required")
		}
		mysqlConfig, parseErr := mysql.ParseDSN(cfg.DBDSN)
		if parseErr != nil {
			return nil, errors.New("invalid mysql database DSN")
		}
		connector, connectorErr := mysql.NewConnector(mysqlConfig)
		if connectorErr != nil {
			return nil, errors.New("invalid mysql database configuration")
		}
		db = sql.OpenDB(connector)
	case databaseDriverPostgres:
		if strings.TrimSpace(cfg.DBDSN) == "" {
			return nil, errors.New("postgres database DSN is required")
		}
		pgxConfig, parseErr := pgx.ParseConfig(cfg.DBDSN)
		if parseErr != nil {
			return nil, errors.New("invalid postgres database DSN")
		}
		connector := stdlib.GetConnector(*pgxConfig)
		db = sql.OpenDB(rebindingConnector{inner: connector})
	default:
		return nil, fmt.Errorf("unsupported database driver %q (supported: sqlite, mysql, postgres)", cfg.DBDriver)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", cfg.DBDriver, err)
	}

	if cfg.DBDriver != databaseDriverSQLite {
		db.SetMaxOpenConns(cfg.DBMaxOpenConns)
		db.SetMaxIdleConns(cfg.DBMaxIdleConns)
		db.SetConnMaxLifetime(time.Duration(cfg.DBConnMaxLifetimeSeconds) * time.Second)
		db.SetConnMaxIdleTime(time.Duration(cfg.DBConnMaxIdleTimeSeconds) * time.Second)
	}

	pingCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DBConnectTimeoutSeconds)*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to %s database: %w", cfg.DBDriver, err)
	}
	return db, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "constraint failed") ||
		strings.Contains(message, "duplicate entry")
}

// rebindingConnector keeps the application on database/sql's standard DB and
// Tx types while applying PostgreSQL placeholders on every connection path.
// This covers direct queries, prepared statements, and queries inside a Tx.
type rebindingConnector struct {
	inner driver.Connector
}

func (c rebindingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &rebindingConn{inner: conn}, nil
}

func (c rebindingConnector) Driver() driver.Driver {
	return c.inner.Driver()
}

type rebindingConn struct {
	inner driver.Conn
}

func (c *rebindingConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(rebindPostgres(query))
}

func (c *rebindingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.inner.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, rebindPostgres(query))
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.Prepare(query)
	}
}

func (c *rebindingConn) Close() error {
	return c.inner.Close()
}

func (c *rebindingConn) Begin() (driver.Tx, error) {
	return c.inner.Begin()
}

func (c *rebindingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.inner.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func (c *rebindingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if conn, ok := c.inner.(driver.ExecerContext); ok {
		return conn.ExecContext(ctx, rebindPostgres(query), args)
	}
	return nil, driver.ErrSkip
}

func (c *rebindingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if conn, ok := c.inner.(driver.QueryerContext); ok {
		return conn.QueryContext(ctx, rebindPostgres(query), args)
	}
	return nil, driver.ErrSkip
}

func (c *rebindingConn) CheckNamedValue(value *driver.NamedValue) error {
	if conn, ok := c.inner.(driver.NamedValueChecker); ok {
		return conn.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *rebindingConn) Ping(ctx context.Context) error {
	if conn, ok := c.inner.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return nil
}

func (c *rebindingConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.inner.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *rebindingConn) IsValid() bool {
	if conn, ok := c.inner.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

// rebindPostgres converts neutral '?' placeholders to PostgreSQL's $n form.
// Question marks inside quoted text, identifiers, comments, dollar-quoted
// bodies, and the ?| / ?& JSON operators are preserved. Use ?? for a literal
// question-mark operator when it would otherwise be ambiguous.
func rebindPostgres(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 8)
	placeholder := 1

	for i := 0; i < len(query); {
		switch query[i] {
		case '\'':
			start := i
			i++
			for i < len(query) {
				if query[i] == '\\' && i+1 < len(query) {
					i += 2
					continue
				}
				if query[i] == '\'' {
					i++
					if i < len(query) && query[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(query[start:i])
		case '"':
			start := i
			i++
			for i < len(query) {
				if query[i] == '"' {
					i++
					if i < len(query) && query[i] == '"' {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(query[start:i])
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				start := i
				i += 2
				for i < len(query) && query[i] != '\n' {
					i++
				}
				out.WriteString(query[start:i])
				continue
			}
			out.WriteByte(query[i])
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				start := i
				i += 2
				depth := 1
				for i < len(query) && depth > 0 {
					if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
						depth++
						i += 2
					} else if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
						depth--
						i += 2
					} else {
						i++
					}
				}
				out.WriteString(query[start:i])
				continue
			}
			out.WriteByte(query[i])
			i++
		case '$':
			if delimiter, ok := postgresDollarDelimiter(query[i:]); ok {
				start := i
				i += len(delimiter)
				if end := strings.Index(query[i:], delimiter); end >= 0 {
					i += end + len(delimiter)
				} else {
					i = len(query)
				}
				out.WriteString(query[start:i])
				continue
			}
			out.WriteByte(query[i])
			i++
		case '?':
			if i+1 < len(query) && query[i+1] == '?' {
				out.WriteByte('?')
				i += 2
				continue
			}
			if i+1 < len(query) && (query[i+1] == '|' || query[i+1] == '&') {
				out.WriteByte('?')
				i++
				continue
			}
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(placeholder))
			placeholder++
			i++
		default:
			out.WriteByte(query[i])
			i++
		}
	}
	return out.String()
}

func postgresDollarDelimiter(query string) (string, bool) {
	if len(query) < 2 || query[0] != '$' {
		return "", false
	}
	for i := 1; i < len(query); i++ {
		if query[i] == '$' {
			return query[:i+1], true
		}
		if !((query[i] >= 'a' && query[i] <= 'z') ||
			(query[i] >= 'A' && query[i] <= 'Z') ||
			(query[i] >= '0' && query[i] <= '9') || query[i] == '_') {
			return "", false
		}
	}
	return "", false
}
