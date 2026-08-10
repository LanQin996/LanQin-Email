package app

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalDatabaseContract(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		dsnEnv string
	}{
		{name: "postgres", driver: databaseDriverPostgres, dsnEnv: "LANQIN_TEST_POSTGRES_DSN"},
		{name: "mysql", driver: databaseDriverMySQL, dsnEnv: "LANQIN_TEST_MYSQL_DSN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := os.Getenv(tt.dsnEnv)
			if dsn == "" {
				t.Skipf("%s is not configured", tt.dsnEnv)
			}
			dataDir := t.TempDir()
			cfg := Config{
				DBDriver:      tt.driver,
				DBDSN:         dsn,
				DataDir:       dataDir,
				AdminEmail:    "admin@contract.test",
				AdminPassword: "ContractPassword123!",
			}

			a, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err != nil {
				t.Fatalf("initialize %s: %v", tt.driver, err)
			}
			if a.messageSearchFTS {
				t.Fatal("external database must use the portable search fallback")
			}
			assertExternalDatabaseContract(t, a)
			if err := a.Close(); err != nil {
				t.Fatalf("close first app: %v", err)
			}

			// Reopening validates migration idempotency and persisted seed data.
			cfg.DataDir = filepath.Join(dataDir, "reopen")
			reopened, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err != nil {
				t.Fatalf("reopen %s: %v", tt.driver, err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			var count int
			if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, cfg.AdminEmail).Scan(&count); err != nil || count != 1 {
				t.Fatalf("persisted administrator count=%d err=%v", count, err)
			}
		})
	}
}

func assertExternalDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var adminID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email=?`, a.cfg.AdminEmail).Scan(&adminID); err != nil {
		t.Fatalf("load seeded administrator: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, newID("usr"), a.cfg.AdminEmail, "Duplicate", "user", "unused", 0, now, now)
	if !isUniqueViolation(err) {
		t.Fatalf("duplicate email error=%v, want unique violation", err)
	}

	groupID := newID("grp")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO permission_groups(id,name,description,permissions_json,limits_json,system,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, groupID, groupID, "", "[]", "{}", 0, now, now); err != nil {
		t.Fatalf("insert permission group: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO user_permission_groups(user_id,group_id,created_at) VALUES(?,?,?)`, adminID, groupID, now); err != nil {
		t.Fatalf("insert permission membership: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM permission_groups WHERE id=?`, groupID); err != nil {
		t.Fatalf("delete permission group: %v", err)
	}
	var membershipCount int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_permission_groups WHERE group_id=?`, groupID).Scan(&membershipCount); err != nil {
		t.Fatalf("check foreign-key cascade: %v", err)
	}
	if membershipCount != 0 {
		t.Fatalf("foreign-key cascade left %d memberships", membershipCount)
	}

	var migrationCount int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, externalSchemaVersion).Scan(&migrationCount); err != nil {
		t.Fatalf("load schema version: %v", err)
	}
	if migrationCount != 1 {
		t.Fatalf("schema version rows=%d, want 1", migrationCount)
	}
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM users WHERE id=?`, "missing").Scan(new(string)); err != sql.ErrNoRows {
		t.Fatalf("missing row error=%v, want sql.ErrNoRows", err)
	}
}
