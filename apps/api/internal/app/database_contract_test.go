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
	template, err := a.mailTemplate(ctx, smtpTestTemplateKey)
	if err != nil || template.Key != smtpTestTemplateKey {
		t.Fatalf("load default mail template: template=%+v err=%v", template, err)
	}

	groupID := newID("grp")
	systemColumn := permissionGroupSystemColumnSQL(a.cfg.DBDriver)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO permission_groups(id,name,description,permissions_json,limits_json,`+systemColumn+`,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, groupID, groupID, "", "[]", "{}", 0, now, now); err != nil {
		t.Fatalf("insert permission group: %v", err)
	}
	group, err := a.permissionGroupByID(ctx, groupID)
	if err != nil || group.ID != groupID || group.System {
		t.Fatalf("load permission group: group=%+v err=%v", group, err)
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

	assertExternalDeliveryCascade(t, ctx, a, adminID, now)
	assertOAuthIdentityContract(t, ctx, a)
	assertRegistrationInviteContract(t, ctx, a, adminID)

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

func TestSQLiteOAuthIdentityContract(t *testing.T) {
	a := newTestApp(t)
	assertOAuthIdentityContract(t, context.Background(), a)
}

func TestSQLiteRegistrationInviteContract(t *testing.T) {
	a := newTestApp(t)
	admin, _ := defaultAdminUserAndMailbox(t, a)
	assertRegistrationInviteContract(t, context.Background(), a, admin.ID)
}

func assertRegistrationInviteContract(t *testing.T, ctx context.Context, a *App, adminID string) {
	t.Helper()
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO registration_invites(id,code,max_uses,used_count,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("inv"), "CONTRACT-CODE", 2, 0, adminID, now, now); err != nil {
		t.Fatalf("insert registration invite: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO registration_invites(id,code,max_uses,used_count,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("inv"), "CONTRACT-CODE", 2, 0, adminID, now, now); !isUniqueViolation(err) {
		t.Fatalf("duplicate registration invite error=%v, want unique violation", err)
	}
}

func assertOAuthIdentityContract(t *testing.T, ctx context.Context, a *App) {
	t.Helper()
	now := a.now().UTC().Format(time.RFC3339Nano)
	userID := newID("usr")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, userID, userID+"@oauth.test", "OAuth Contract", "user", "unused", 0, now, now); err != nil {
		t.Fatalf("insert OAuth contract user: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO oauth_identities(provider,subject,user_id,username,created_at,updated_at) VALUES(?,?,?,?,?,?)`, linuxDoProvider, "contract-subject", userID, "contract", now, now); err != nil {
		t.Fatalf("insert OAuth identity: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO oauth_identities(provider,subject,user_id,username,created_at,updated_at) VALUES(?,?,?,?,?,?)`, linuxDoProvider, "other-subject", userID, "other", now, now); !isUniqueViolation(err) {
		t.Fatalf("duplicate user/provider error=%v, want unique violation", err)
	}
	stateHash := hashToken("contract-state")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO oauth_login_states(token_hash,purpose,user_id,expires_at,created_at) VALUES(?,?,?,?,?)`, stateHash, "link", userID, a.now().UTC().Add(time.Minute).Format(time.RFC3339Nano), now); err != nil {
		t.Fatalf("insert OAuth state: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID); err != nil {
		t.Fatalf("delete OAuth contract user: %v", err)
	}
	for table, target := range map[string][2]string{
		"oauth_identities":   {"subject", "contract-subject"},
		"oauth_login_states": {"token_hash", stateHash},
	} {
		column, value := target[0], target[1]
		var count int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+column+`=?`, value).Scan(&count); err != nil || count != 0 {
			t.Fatalf("OAuth cascade %s count=%d err=%v", table, count, err)
		}
	}
}

func assertExternalDeliveryCascade(t *testing.T, ctx context.Context, a *App, adminID, now string) {
	t.Helper()
	var domainID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM domains WHERE name=?`, "contract.test").Scan(&domainID); err != nil {
		t.Fatalf("load seeded domain: %v", err)
	}
	mailboxID := newID("mbx")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,domain_id,local_part,address,display_name,password_hash,quota_mb,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, mailboxID, adminID, domainID, mailboxID, mailboxID+"@contract.test", "Contract", "unused", 1, "active", now, now); err != nil {
		t.Fatalf("insert cascade mailbox: %v", err)
	}
	queueID := newID("queue")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO send_queue(id,user_id,mailbox_id,source,mail_from,header_from,recipients_json,mime_base64,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, queueID, adminID, mailboxID, "contract", mailboxID+"@contract.test", mailboxID+"@contract.test", "[]", "", "delivered", now, now, now); err != nil {
		t.Fatalf("insert cascade send queue: %v", err)
	}
	deliveryID := newID("delivery")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO delivery_events(id,external_id,provider,queue_id,recipient,status,occurred_at,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, deliveryID, deliveryID, "contract", queueID, "recipient@example.test", "delivered", now, now); err != nil {
		t.Fatalf("insert cascade delivery event: %v", err)
	}
	outboxID := newID("outbox")
	if _, err := a.db.ExecContext(ctx, `INSERT INTO status_webhook_outbox(id,event_key,event_type,mailbox_id,payload_json,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, outboxID, outboxID, "contract", mailboxID, "{}", now, now, now); err != nil {
		t.Fatalf("insert cascade webhook: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE id=?`, mailboxID); err != nil {
		t.Fatalf("delete cascade mailbox: %v", err)
	}
	for table, id := range map[string]string{
		"delivery_events":       deliveryID,
		"send_queue":            queueID,
		"status_webhook_outbox": outboxID,
	} {
		var count int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id=?`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade cleanup %s count=%d err=%v", table, count, err)
		}
	}
}
