package app

import (
	"context"
	"database/sql"
	"errors"
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
			t.Run("concurrent mailbox daily quota", func(t *testing.T) {
				if tt.driver != databaseDriverPostgres {
					t.Skip("PostgreSQL statement-snapshot regression")
				}
				t.Run("commit consumes the slot", func(t *testing.T) {
					assertPostgresConcurrentMailboxDailyQuota(t, a, true)
				})
				t.Run("rollback preserves the slot", func(t *testing.T) {
					assertPostgresConcurrentMailboxDailyQuota(t, a, false)
				})
			})
			if err := a.Close(); err != nil {
				t.Fatalf("close first app: %v", err)
			}

			// Exercise a real V8 upgrade, not just a fresh-schema reopen.
			prepareExternalSchemaV8Upgrade(t, cfg)
			cfg.DataDir = filepath.Join(dataDir, "upgrade")
			upgraded, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err != nil {
				t.Fatalf("upgrade %s from V8: %v", tt.driver, err)
			}
			t.Run("upgraded invite defaults", func(t *testing.T) {
				defer func() {
					if err := upgraded.Close(); err != nil {
						t.Errorf("close upgraded app: %v", err)
					}
				}()
				assertUpgradedInviteDefaults(t, upgraded)
			})

			// Reopening validates upgrade idempotency and persisted seed data.
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

// The contract DSNs must point at dedicated test databases. Downgrade only the
// V9-V11 additions after closing the app so its workers cannot race the fixture.
func prepareExternalSchemaV8Upgrade(t *testing.T, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	indexSuffix := ""
	if cfg.DBDriver == databaseDriverMySQL {
		indexSuffix = " ON sessions"
		// V8 had an implicit FK index. MySQL discarded it when the newer
		// explicit index was added, so restore a supporting index first.
		if _, err := db.ExecContext(ctx, "CREATE INDEX idx_sessions_user_v8_fixture ON sessions(user_id)"); err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		"DROP TABLE mailbox_creation_events",
		"ALTER TABLE registration_invites DROP COLUMN permission_group_ids_json",
		"DROP INDEX idx_sessions_user" + indexSuffix,
		"DROP INDEX idx_sessions_expires" + indexSuffix,
		"ALTER TABLE users DROP COLUMN two_factor_last_counter",
		"ALTER TABLE smtp_send_events DROP COLUMN recipients",
		"DELETE FROM schema_migrations WHERE version>=9",
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare V8: %s: %v", statement, err)
		}
	}
}

func assertUpgradedInviteDefaults(t *testing.T, a *App) {
	t.Helper()
	ctx := t.Context()
	var groups string
	var maxUses, used int
	if err := a.db.QueryRowContext(ctx, "SELECT permission_group_ids_json,max_uses,used_count FROM registration_invites WHERE code=?", "CONTRACT-CODE").Scan(&groups, &maxUses, &used); err != nil {
		t.Fatal(err)
	}
	if groups != "[]" || maxUses != 2 || used != 0 {
		t.Fatalf("upgraded invite groups=%q maxUses=%d used=%d", groups, maxUses, used)
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	id := newID("inv")
	if _, err := a.db.ExecContext(ctx, "INSERT INTO registration_invites(id,code,max_uses,created_at,updated_at) VALUES(?,?,1,?,?)", id, id, now, now); err != nil {
		t.Fatalf("insert using upgraded default: %v", err)
	}
	defer a.db.ExecContext(ctx, "DELETE FROM registration_invites WHERE id=?", id)
	if err := a.db.QueryRowContext(ctx, "SELECT permission_group_ids_json FROM registration_invites WHERE id=?", id).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != "[]" {
		t.Fatalf("new invite default=%q, want []", groups)
	}
}

func assertPostgresConcurrentMailboxDailyQuota(t *testing.T, a *App, commitFirst bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	userID := newID("usr")
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, "INSERT INTO users(id,email,display_name,role,password_hash,created_at,updated_at) VALUES(?,?,?,'user','unused',?,?)", userID, userID+"@contract.test", "Quota Contract", now, now); err != nil {
		t.Fatal(err)
	}
	defer a.db.ExecContext(ctx, "DELETE FROM users WHERE id=?", userID)
	var domainID string
	if err := a.db.QueryRowContext(ctx, "SELECT id FROM domains WHERE name=?", "contract.test").Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	user := &User{ID: userID, Limits: PermissionLimits{MaxMailboxes: 10, MaxMailboxesPerDay: 1}}
	first, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if _, err := a.createMailboxWithPasswordHashTx(ctx, first, userID, domainID, userID+"-a", "First", "unused", 1024, "active", user); err != nil {
		t.Fatal(err)
	}
	second, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	var pid int
	if err := second.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.createMailboxWithPasswordHashTx(ctx, second, userID, domainID, userID+"-b", "Second", "unused", 1024, "active", user)
		if err == nil {
			err = second.Commit()
		} else {
			_ = second.Rollback()
		}
		done <- err
	}()
	// Observe a real lock wait instead of sleeping and hoping both requests overlap.
	for {
		var waiting bool
		if err := a.db.QueryRowContext(ctx, "SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=?", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second creation completed before first committed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if commitFirst {
		err = first.Commit()
	} else {
		err = first.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if commitFirst && !errors.Is(err, errMailboxDailyLimitReached) {
			t.Errorf("second creation error=%v, want daily quota rejection", err)
		} else if !commitFirst && err != nil {
			t.Errorf("second creation after rollback: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var total, events, mailboxes int
	if err := a.db.QueryRowContext(ctx, "SELECT mailboxes_created_total FROM users WHERE id=?", userID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailbox_creation_events WHERE user_id=?", userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailboxes WHERE user_id=?", userID).Scan(&mailboxes); err != nil {
		t.Fatal(err)
	}
	if total != 1 || events != 1 || mailboxes != 1 {
		t.Errorf("daily limit=1: total=%d events=%d mailboxes=%d", total, events, mailboxes)
	}
}
