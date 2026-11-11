package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/nativedb"
	"lanqin-email-api/internal/backup/storage"
)

func TestNativeDatabaseBackupRestoreContract(t *testing.T) {
	for _, driver := range []string{databaseDriverMySQL, databaseDriverPostgres} {
		t.Run(driver, func(t *testing.T) {
			prefix := "LANQIN_TEST_NATIVE_" + strings.ToUpper(driver)
			sourceDSN, targetDSN := os.Getenv(prefix+"_SOURCE_DSN"), os.Getenv(prefix+"_TARGET_DSN")
			if sourceDSN == "" || targetDSN == "" {
				t.Skip("isolated native source and EMPTY target DSNs not configured")
			}
			if sourceDSN == targetDSN {
				t.Fatal("native recovery must use a separate empty target database")
			}
			root := t.TempDir()
			cfg := Config{DBDriver: driver, DBDSN: sourceDSN, DataDir: filepath.Join(root, "source-data"), MaildirRoot: filepath.Join(root, "source-mail"), AdminEmail: "admin@native.test", AdminPassword: "SyntheticNative123!", PublicHostname: "native.test", PublicBaseURL: "http://localhost:18034", AllowInsecureHTTP: true}
			cfg.CookieName, cfg.SessionTTLHours = "lanqin_test", 24
			if err := os.MkdirAll(cfg.MaildirRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			// Stop this synthetic app's workers for deterministic fixture setup. The
			// dedicated exporter itself never calls New or borrows a.db.
			a.workerCancel()
			a.workerWG.Wait()
			client := backupLoggedInAdmin(t, a)
			var mailboxID string
			if err := a.db.QueryRowContext(t.Context(), "SELECT id FROM mailboxes WHERE address=?", cfg.AdminEmail).Scan(&mailboxID); err != nil {
				t.Fatal(err)
			}
			var draft MailMessage
			attachmentBytes := []byte{0, 255, 1, 2, 3, 128, 65, 66}
			if status := client.do("POST", "/api/mail/drafts", map[string]any{"mailboxId": mailboxID, "subject": "Native 合成 draft", "to": []string{"recipient@native.test"}, "text": "typed database snapshot", "attachments": []AttachmentInput{{Filename: "synthetic.bin", ContentType: "application/octet-stream", ContentBase64: base64.StdEncoding.EncodeToString(attachmentBytes)}}}, &draft); status != 201 {
				t.Fatal("draft fixture", status)
			}
			if _, err := a.backfillSQLiteMessagesToMaildir(t.Context()); err != nil {
				t.Fatal(err)
			}
			seedNativeInstallationWork(t, a, mailboxID, draft.ID)
			column, _ := nativeIdentifier(driver, "key")
			if _, err := a.db.ExecContext(t.Context(), "INSERT INTO system_settings("+column+",value,updated_at) VALUES(?,?,?)", backupTransferPrefix+"synthetic.session", "synthetic-sensitive-session", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.ExecContext(t.Context(), "INSERT INTO system_settings("+column+",value,updated_at) VALUES(?,?,?)", "backup.discovery.v1.synthetic.0", "synthetic-sensitive-discovery", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			cutPath := filepath.Join(root, "database.jsonl")
			cut, err := prepareNativeDatabaseCut(t.Context(), a.cfg, cutPath, 1<<30)
			if err != nil {
				t.Fatal("native consistent export", err)
			}
			defer cut.Close()
			key, _ := nativeIdentifier(driver, "key")
			// A separate writer must commit while the read-only snapshot remains
			// open, and subsequent source-reference queries retain their old cut.
			now := a.now().UTC().Format(time.RFC3339Nano)
			query := insertIgnoreSQL(driver, "INSERT INTO system_settings("+key+",value,updated_at) VALUES(?,?,?)", "("+key+")")
			if _, err := a.db.ExecContext(t.Context(), query, "native.synthetic.after-cut", "new-live-value", now); err != nil {
				t.Fatal("backup blocked a normal write", err)
			}
			var count int
			if err := cut.Tx.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM system_settings WHERE "+key+"='native.synthetic.after-cut'").Scan(&count); err != nil || count != 0 {
				t.Fatal("frozen reference queries advanced after export", count, err)
			}
			if _, err := cut.Tx.ExecContext(t.Context(), "INSERT INTO system_settings("+key+",value,updated_at) VALUES(?,?,?)", "native.synthetic.must-not-write", "forbidden", now); err == nil {
				t.Fatal("native source snapshot transaction was writable")
			}
			cut.Close()
			file, err := os.Open(cutPath)
			if err != nil {
				t.Fatal(err)
			}
			_, _, readErr := nativedb.Read(t.Context(), file, nativedb.DefaultLimits(), nil, func(table string, cells []nativedb.Cell) error {
				for _, cell := range cells {
					if cell.Kind == "utf8" {
						value, _ := cell.Value(64 << 20)
						if text, ok := value.(string); ok && (strings.Contains(text, "synthetic-sensitive-session") || strings.Contains(text, "synthetic-sensitive-discovery")) {
							t.Fatal("runtime upload session or discovery page leaked into native dump")
						}
					}
				}
				return nil
			})
			file.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			var originalDiscovery string
			if err := a.db.QueryRowContext(t.Context(), "SELECT value FROM system_settings WHERE "+column+"=?", "backup.discovery.v1.synthetic.0").Scan(&originalDiscovery); err != nil || originalDiscovery != "synthetic-sensitive-discovery" {
				t.Fatal("native export changed source discovery runtime", err)
			}
			prepared, err := PrepareOnlineBackup(t.Context(), a.cfg, t.TempDir(), DefaultOnlineBackupOptions())
			if err != nil {
				t.Fatal("native full reference collection", err)
			}
			defer prepared.Close()
			if prepared.Report.Scope != driver+"-reference-closure" || prepared.Report.Version != 2 || prepared.Report.Summary.Messages < 1 || prepared.Report.Summary.Drafts != 1 || prepared.Report.Summary.Attachments != 1 || prepared.Report.Summary.DKIMKeys < 1 {
				t.Fatal("native collection scope/counts", prepared.Report.Summary)
			}
			identity, _ := age.GenerateX25519Identity()
			var encrypted bytes.Buffer
			manifest, err := backup.WriteNative(t.Context(), prepared.Path, &encrypted, identity.Recipient().String(), driver, backup.DefaultLimits())
			if err != nil {
				t.Fatal("native archive", err)
			}
			digest := sha256.Sum256(encrypted.Bytes())
			ciphertextPath := filepath.Join(root, "native.age")
			if err := os.WriteFile(ciphertextPath, encrypted.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(root, "recovered")
			got, err := backup.Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), restored, identity.String(), hex.EncodeToString(digest[:]), backup.DefaultLimits())
			if err != nil || got.Driver != driver || got.Scope != "online-native-data" || got.Version != 2 || manifest.Version != 2 {
				t.Fatal("native encrypted recovery", err)
			}
			targetCfg := a.cfg
			targetCfg.DBDSN = targetDSN
			targetCfg.DataDir = filepath.Join(root, "target-data")
			targetCfg.MaildirRoot = filepath.Join(root, "target-mail")
			importWorkspace := t.TempDir()
			wrong, _ := age.GenerateX25519Identity()
			if _, err := ImportNativeBackup(t.Context(), targetCfg, ciphertextPath, wrong.String(), hex.EncodeToString(digest[:]), importWorkspace, backup.DefaultLimits()); err == nil {
				t.Fatal("wrong native recovery key accepted")
			}
			probe, err := openNativeBackupDatabase(t.Context(), targetCfg)
			if err != nil {
				t.Fatal(err)
			}
			where := "table_schema=DATABASE()"
			if driver == databaseDriverPostgres {
				where = "table_schema=current_schema()"
			}
			var tables int
			if err := probe.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.tables WHERE "+where+" AND table_type='BASE TABLE'").Scan(&tables); err != nil || tables != 0 {
				probe.Close()
				t.Fatal("wrong-key import touched target database", err)
			}
			probe.Close()
			result, err := ImportNativeBackup(t.Context(), targetCfg, ciphertextPath, identity.String(), hex.EncodeToString(digest[:]), importWorkspace, backup.DefaultLimits())
			if err != nil || result.Driver != driver || result.Rows < 1 || result.Activated {
				t.Fatal("native row recovery", err)
			}
			if files, err := os.ReadDir(importWorkspace); err != nil || len(files) != 0 {
				t.Fatal("native import leaked temporary plaintext", err)
			}
			db, err := openNativeBackupDatabase(t.Context(), targetCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var subject string
			if err := db.QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", draft.ID).Scan(&subject); err != nil || subject != "Native 合成 draft" {
				t.Fatal("native draft text changed", subject, err)
			}
			if err := checkNativeRestoreActivation(t.Context(), db, driver); err == nil {
				t.Fatal("raw imported DB could start business workers")
			}
			if _, _, err := restoreNativeDatabaseRows(t.Context(), targetCfg, cutPath, 1<<30); err == nil {
				t.Fatal("native import overwrote nonempty target")
			}
			if _, err := New(targetCfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				t.Fatal("App.New activated uninstalled native database")
			}
			var restoredAttachmentPath string
			if err := db.QueryRowContext(t.Context(), "SELECT storage_path FROM attachments WHERE message_id=?", draft.ID).Scan(&restoredAttachmentPath); err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(cfg.DataDir, restoredAttachmentPath)
			if err != nil || !filepath.IsLocal(rel) {
				t.Fatal("unexpected original attachment root")
			}
			payload, err := os.ReadFile(filepath.Join(restored, "snapshot", "data", rel))
			if err != nil || !bytes.Equal(payload, attachmentBytes) {
				t.Fatal("native attachment payload changed", err)
			}
			reportSHA, err := backup.StagedOnlineReportSHA256(t.Context(), restored, driver)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(restored, "snapshot", "data", rel), bytes.Repeat([]byte{0x42}, len(attachmentBytes)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := install.VerifyNativeStaged(t.Context(), restored, driver, reportSHA, backup.DefaultLimits()); err == nil {
				t.Fatal("native staged payload tampering bypassed collection digest")
			}
			if badDSN := os.Getenv(prefix + "_BAD_DSN"); badDSN != "" {
				badPath := filepath.Join(root, "bad-foreign-key.jsonl")
				source, err := os.Open(cutPath)
				if err != nil {
					t.Fatal(err)
				}
				output, err := os.Create(badPath)
				if err != nil {
					source.Close()
					t.Fatal(err)
				}
				var writer *nativedb.Writer
				messageColumn := -1
				_, _, err = nativedb.Read(t.Context(), source, nativedb.DefaultLimits(), func(header nativedb.Header) error {
					for _, table := range header.Tables {
						if table.Name == "attachments" {
							base := 0
							for _, column := range table.Columns {
								if !column.Generated {
									if column.Name == "message_id" {
										messageColumn = base
									}
									base++
								}
							}
						}
					}
					var err error
					writer, err = nativedb.NewWriter(output, header, nativedb.DefaultLimits())
					return err
				}, func(table string, cells []nativedb.Cell) error {
					if table == "attachments" {
						if messageColumn < 0 {
							t.Fatal("attachment message column not found")
						}
						cells[messageColumn] = nativedb.Cell{Kind: "utf8", Data: base64.StdEncoding.EncodeToString([]byte("synthetic-missing-parent"))}
					}
					return writer.Row(table, cells)
				})
				source.Close()
				if err == nil {
					err = writer.Close()
				}
				output.Close()
				if err != nil {
					t.Fatal(err)
				}
				badCfg := targetCfg
				badCfg.DBDSN = badDSN
				if _, _, err := restoreNativeDatabaseRows(t.Context(), badCfg, badPath, 1<<30); err == nil {
					t.Fatal("invalid native foreign-key graph committed")
				}
				bad, err := openNativeBackupDatabase(t.Context(), badCfg)
				if err != nil {
					t.Fatal(err)
				}
				var count int
				if err := bad.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 0 {
					bad.Close()
					t.Fatal("failed native import left partially restored rows", count, err)
				}
				bad.Close()
			}
			assertNativeInstallationPreparation(t, a, targetCfg, root, encrypted.Bytes(), identity.String(), hex.EncodeToString(digest[:]), draft.ID)
			if executable, cliTarget := os.Getenv("LANQIN_TEST_BACKUP_CLI"), os.Getenv(prefix+"_CLI_DSN"); executable != "" && cliTarget != "" {
				archive := filepath.Join(root, "public-cli.age")
				keyFile := filepath.Join(root, "public-identity.txt")
				if err := os.WriteFile(keyFile, []byte(identity.String()), 0o600); err != nil {
					t.Fatal(err)
				}
				workspace := t.TempDir()
				environment := []string{}
				for _, item := range os.Environ() {
					if !strings.HasPrefix(item, "LANQIN_") {
						environment = append(environment, item)
					}
				}
				environment = append(environment, "NATIVE_SYNTHETIC_SOURCE_DSN="+sourceDSN, "NATIVE_SYNTHETIC_TARGET_DSN="+cliTarget)
				run := func(args ...string) []byte {
					command := exec.CommandContext(t.Context(), executable, args...)
					command.Env = environment
					data, err := command.CombinedOutput()
					if err != nil {
						t.Fatal("public native backup CLI failed", args[0], err, string(data))
					}
					if bytes.Contains(data, []byte(sourceDSN)) || bytes.Contains(data, []byte(cliTarget)) || bytes.Contains(data, []byte(identity.String())) {
						t.Fatal("public CLI leaked private recovery or connection data")
					}
					return data
				}
				created := run("create-online", "--driver", driver, "--dsn-env", "NATIVE_SYNTHETIC_SOURCE_DSN", "--data-dir", cfg.DataDir, "--maildir-root", cfg.MaildirRoot, "--workspace", workspace, "--recipient", identity.Recipient().String(), "--out", archive)
				position := bytes.Index(created, []byte("sha256="))
				if position < 0 {
					t.Fatal("CLI did not return trustworthy ciphertext receipt")
				}
				sha := strings.TrimSpace(string(created[position+7:]))
				if len(sha) != 64 {
					t.Fatal("invalid CLI receipt")
				}
				imported := run("import-native", "--driver", driver, "--target-dsn-env", "NATIVE_SYNTHETIC_TARGET_DSN", "--in", archive, "--identity-file", keyFile, "--expected-sha256", sha, "--workspace", workspace, "--confirm-empty-target")
				var result NativeImportResult
				if json.Unmarshal(imported, &result) != nil || result.Activated || result.Rows < 1 || result.Driver != driver {
					t.Fatal("public CLI import receipt invalid")
				}
				if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
					t.Fatal("public CLI retained plaintext staging", err)
				}
				if runtime.GOOS == "linux" {
					assertNativeMaintenanceCLI(t, run, driver, archive, keyFile, sha, result.ReportSHA256, cfg, cliTarget)
				}
			}
			assertNativeWebBackup(t, a, identity)
		})
	}
}

func seedNativeInstallationWork(t *testing.T, a *App, mailboxID, draftID string) {
	t.Helper()
	var userID string
	if err := a.db.QueryRowContext(t.Context(), "SELECT user_id FROM mailboxes WHERE id=?", mailboxID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO send_queue(id,user_id,mailbox_id,source,mail_from,header_from,recipients_json,mime_base64,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", []any{"native-send", userID, mailboxID, "native_test", a.cfg.AdminEmail, a.cfg.AdminEmail, `["recipient@native.test"]`, "c3ludGhldGljLXBheWxvYWQ=", now, now, now}},
		{"INSERT INTO scheduled_sends(id,user_id,mailbox_id,draft_id,payload_json,send_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", []any{"native-schedule", userID, mailboxID, draftID, `{"synthetic":"preserve-me"}`, now, now, now}},
		{"INSERT INTO api_tokens(id,user_id,name,token_hash,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", []any{"native-token", userID, "synthetic token", "synthetic-token-hash", now, now, now}},
		{"INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", []any{"native-external", userID, mailboxID, "synthetic account", "imap.example.invalid", 993, "tls", a.cfg.AdminEmail, "synthetic-not-a-real-ciphertext", now, now}},
		{"INSERT INTO status_webhook_outbox(id,event_key,event_type,mailbox_id,payload_json,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", []any{"native-notification", "native-event-key", "synthetic", mailboxID, `{"synthetic":"preserve-notification"}`, now, now, now}},
	} {
		if _, err := a.db.ExecContext(t.Context(), query.sql, query.args...); err != nil {
			t.Fatal("native work fixture", err)
		}
	}
}

func assertNativeInstallationPreparation(t *testing.T, source *App, cfg Config, root string, encrypted []byte, identity, receipt, draftID string) {
	t.Helper()
	container := filepath.Join(root, "native-install-recovered")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(encrypted), container, identity, receipt, backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	reportSHA, err := backup.StagedOnlineReportSHA256(t.Context(), container, cfg.DBDriver)
	if err != nil {
		t.Fatal(err)
	}
	targets := install.Targets{Data: filepath.Join(root, "native-live-data"), Mail: filepath.Join(root, "native-live-mail"), DKIM: filepath.Join(root, "native-live-dkim")}
	for _, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "old"), []byte("synthetic-old-tree"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtimeData, runtimeMail := "/runtime/data", "/runtime/mail"
	if runtime.GOOS == "linux" {
		runtimeData, runtimeMail = targets.Data, targets.Mail
	}
	work := t.TempDir()
	p, err := PrepareNativeInstallation(t.Context(), cfg, container, work, runtimeData, runtimeMail, targets, reportSHA, backup.DefaultLimits())
	if err != nil {
		t.Fatal("native installation preparation", err)
	}
	defer p.Close()
	content, err := os.ReadFile(filepath.Join(p.Path, "data", "restore-install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan NativeInstallationPlan
	if json.Unmarshal(content, &plan) != nil || plan.SendQueue["queued"] != 1 || plan.Scheduled["pending"] != 1 || plan.SessionsRevoked < 1 || plan.APITokensDisabled != 1 || plan.ExternalAccountsDisabled != 1 || plan.WebhookNotificationsHeld != 1 {
		t.Fatal("native preparation omitted work suspension", plan)
	}
	db, err := openNativeBackupDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if checkNativePreparedWork(t.Context(), db) != nil {
		t.Fatal("prepared database could replay old tasks")
	}
	var raw, mime, status string
	if err := db.QueryRowContext(t.Context(), "SELECT raw_path FROM messages WHERE id=?", draftID).Scan(&raw); err != nil || !strings.HasPrefix(raw, runtimeMail+"/") {
		t.Fatal("native message path not remapped", err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT status,mime_base64 FROM send_queue WHERE id='native-send'").Scan(&status, &mime); err != nil || status != "canceled" || mime != "c3ludGhldGljLXBheWxvYWQ=" {
		t.Fatal("native send payload lost", err)
	}
	if err := source.db.QueryRowContext(t.Context(), "SELECT status FROM send_queue WHERE id='native-send'").Scan(&status); err != nil || status != "queued" {
		t.Fatal("preparation changed original database", err)
	}
	if _, err := os.Lstat(filepath.Join(p.Path, "data", "lanqin-native.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old runtime database dump was shipped as live data")
	}
	if _, err := ActivateNativeInstallation(t.Context(), cfg, "nonexistent-journal", targets, true); err == nil {
		t.Fatal("native DB activated without committed directory transaction")
	}
	repeated, err := PrepareNativeInstallation(t.Context(), cfg, container, work, runtimeData, runtimeMail, targets, reportSHA, backup.DefaultLimits())
	if err != nil {
		t.Fatal("guarded preparation could not be safely repeated", err)
	}
	repeated.Close()
	if _, err := PrepareNativeInstallation(t.Context(), cfg, container, work, "/different/data", runtimeMail, targets, reportSHA, backup.DefaultLimits()); err == nil {
		t.Fatal("prepared DB silently rebound to different roots")
	}
	journal := t.TempDir()
	activate := ActivateNativeInstallation
	rollback := RollbackNativeInstallation
	var info install.JournalInfo
	if runtime.GOOS == "linux" {
		owner := install.Owner{UID: os.Getuid(), GID: os.Getgid()}
		info, err = install.Apply(t.Context(), p.Path, targets, install.Owners{Data: owner, Mail: owner, DKIM: owner}, journal, true)
		if err != nil {
			t.Fatal("real native directory switch", err)
		}
	} else {
		info = simulateNativeCommittedTrees(t, p.Path, targets, journal)
		activate = func(ctx context.Context, cfg Config, journal string, targets install.Targets, confirmed bool) (install.JournalInfo, error) {
			return activateNativeInstallation(ctx, cfg, journal, targets)
		}
		rollback = func(ctx context.Context, cfg Config, journal string, targets install.Targets, confirmed bool) error {
			return rollbackNativeInstallation(ctx, cfg, journal, targets, func(ctx context.Context, journal string, targets install.Targets, _ bool) error {
				marker, _, err := readNativeRestoreMarker(ctx, db, cfg.DBDriver, nativeRestorePendingKey, false)
				if err != nil || marker.Phase != "rollback" {
					t.Fatal("database was not guarded BEFORE directory rollback", err)
				}
				for _, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
					failed := directory + ".failed-" + info.TransactionID
					if _, err := os.Lstat(failed); os.IsNotExist(err) {
						if err := os.Rename(directory, failed); err != nil {
							return err
						}
						if err := os.Rename(directory+".rollback-"+info.TransactionID, directory); err != nil {
							return err
						}
					}
				}
				file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				_, err = file.WriteString("\"rolled-back\"\n")
				file.Close()
				return err
			})
		}
		t.Log("Windows tests exercise real database activation with simulated directory publication; public Linux-only gates remain enforced")
	}
	// A guarded database remains unusable if a legacy task or mapped path was
	// modified between preparation and activation. Repair only the synthetic
	// fixture after each independent probe.
	for _, mutation := range []struct {
		change, repair string
		args           []any
	}{
		{"UPDATE send_queue SET status='queued' WHERE id='native-send'", "UPDATE send_queue SET status='canceled' WHERE id='native-send'", nil},
		{"UPDATE messages SET raw_path='/unexpected/message' WHERE id=?", "", []any{draftID}},
	} {
		if _, err := db.ExecContext(t.Context(), mutation.change, mutation.args...); err != nil {
			t.Fatal(err)
		}
		if _, err := activate(t.Context(), cfg, info.Path, targets, true); err == nil {
			t.Fatal("changed prepared state activated")
		}
		if mutation.repair != "" {
			if _, err := db.ExecContext(t.Context(), mutation.repair); err != nil {
				t.Fatal(err)
			}
		} else if _, err := db.ExecContext(t.Context(), "UPDATE messages SET raw_path=? WHERE id=?", raw, draftID); err != nil {
			t.Fatal(err)
		}
		if checkNativeRestoreActivation(t.Context(), db, cfg.DBDriver) == nil {
			t.Fatal("failed activation removed guard")
		}
	}
	planDigest := sha256.Sum256(content)
	if _, err := install.VerifyNativeInstalled(t.Context(), info.Path, targets, cfg.DBDriver, reportSHA, hex.EncodeToString(planDigest[:]), backup.DefaultLimits()); err != nil {
		t.Fatal("native installed files not verifiable", err)
	}
	if _, err := activate(t.Context(), cfg, info.Path, targets, true); err != nil {
		t.Fatal("journal-bound native activation", err)
	}
	if _, err := activate(t.Context(), cfg, info.Path, targets, true); err != nil {
		t.Fatal("repeat activation before startup", err)
	}
	if err := checkNativeRestoreActivation(t.Context(), db, cfg.DBDriver, runtimeData, runtimeMail); err != nil && runtime.GOOS == "linux" {
		t.Fatal("correct installed DB/tree pair blocked", err)
	}
	wrong := targets
	wrong.Mail = targets.DKIM
	if _, err := activate(t.Context(), cfg, info.Path, wrong, true); err == nil {
		t.Fatal("wrong journal targets authorized activation")
	}
	// A restored live instance must be able to produce a new portable backup:
	// old installation identity is runtime state, not part of the next import.
	recaptured := filepath.Join(root, "activated-recapture.jsonl")
	cut, err := prepareNativeDatabaseCut(t.Context(), cfg, recaptured, 1<<30)
	if err != nil {
		t.Fatal("activated database could not be backed up", err)
	}
	cut.Close()
	recapturedBytes, err := os.ReadFile(recaptured)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(recapturedBytes, []byte(base64.StdEncoding.EncodeToString([]byte(nativeRestoreActiveKey)))) {
		t.Fatal("old activation identity leaked into subsequent backup")
	}
	failure := errors.New("synthetic interrupted rollback")
	if err := rollbackNativeInstallation(t.Context(), cfg, info.Path, targets, func(context.Context, string, install.Targets, bool) error { return failure }); !errors.Is(err, failure) {
		t.Fatal("rollback failure not propagated", err)
	}
	if checkNativeRestoreActivation(t.Context(), db, cfg.DBDriver) == nil {
		t.Fatal("interrupted rollback left new database active")
	}
	if err := rollback(t.Context(), cfg, info.Path, targets, true); err != nil {
		t.Fatal("native rollback", err)
	}
	if err := rollback(t.Context(), cfg, info.Path, targets, true); err != nil {
		t.Fatal("native repeat rollback", err)
	}
	if checkNativeRestoreActivation(t.Context(), db, cfg.DBDriver, runtimeData, runtimeMail) == nil {
		t.Fatal("rolled-back new DB was left activated")
	}
	for _, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		if data, err := os.ReadFile(filepath.Join(directory, "old")); err != nil || string(data) != "synthetic-old-tree" {
			t.Fatal("native rollback lost old files", err)
		}
	}
}

func assertNativeMaintenanceCLI(t *testing.T, run func(...string) []byte, driver, archive, identityFile, ciphertextSHA, reportSHA string, sourceCfg Config, newDSN string) {
	t.Helper()
	root := t.TempDir()
	staged := filepath.Join(root, "restored")
	restored := run("restore", "--in", archive, "--identity-file", identityFile, "--expected-sha256", ciphertextSHA, "--out", staged)
	if !bytes.Contains(restored, []byte("report-sha256="+reportSHA)) {
		t.Fatal("restore and import report receipts differ")
	}
	assertNativeMaintenanceCLILifecycle(t, run, sourceCfg, newDSN, []string{"--staged", staged, "--expected-report-sha256", reportSHA})
}

func assertNativeMaintenanceCLILifecycle(t *testing.T, run func(...string) []byte, sourceCfg Config, newDSN string, sourceArgs []string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Fatal("public native maintenance lifecycle requires real Linux filesystem operations")
	}
	driver := sourceCfg.DBDriver
	root := t.TempDir()
	targets := install.Targets{Data: filepath.Join(root, "live-data"), Mail: filepath.Join(root, "live-mail"), DKIM: filepath.Join(root, "live-dkim")}
	for _, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "old"), []byte("synthetic-old-tree"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	work, journalDir := t.TempDir(), t.TempDir()
	base := []string{"--driver", driver, "--target-dsn-env", "NATIVE_SYNTHETIC_TARGET_DSN", "--data-target", targets.Data, "--mail-target", targets.Mail, "--dkim-target", targets.DKIM, "--confirm-maintenance"}
	args := append([]string{"install-native"}, base...)
	args = append(args, sourceArgs...)
	args = append(args, "--workspace", work, "--runtime-data-root", targets.Data, "--runtime-mail-root", targets.Mail, "--journal-dir", journalDir)
	for _, name := range []string{"data-uid", "mail-uid", "dkim-uid"} {
		args = append(args, "--"+name, strconv.Itoa(os.Getuid()))
	}
	for _, name := range []string{"data-gid", "mail-gid", "dkim-gid"} {
		args = append(args, "--"+name, strconv.Itoa(os.Getgid()))
	}
	var receipt struct {
		Journal, DatabaseState string
		ServicesStarted        bool
	}
	if data := run(args...); json.Unmarshal(data, &receipt) != nil || receipt.DatabaseState != "activated" || receipt.ServicesStarted || receipt.Journal == "" {
		t.Fatal("public native installation receipt invalid", string(data))
	}
	cfg := sourceCfg
	cfg.DBDSN, cfg.DataDir, cfg.MaildirRoot = newDSN, targets.Data, targets.Mail
	db, err := openNativeBackupDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := checkNativeRestoreActivation(t.Context(), db, driver, targets.Data, targets.Mail); err != nil {
		t.Fatal("CLI installation did not authorize correct DB/tree", err)
	}
	planBytes, err := os.ReadFile(filepath.Join(targets.Data, "restore-install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	planSum := sha256.Sum256(planBytes)
	planSHA := hex.EncodeToString(planSum[:])
	run("verify-native", "--driver", driver, "--dsn-env", "NATIVE_SYNTHETIC_TARGET_DSN", "--data-root", targets.Data, "--mail-root", targets.Mail, "--expected-plan-sha256", planSHA)
	if err := VerifyNativeRuntime(t.Context(), cfg, strings.Repeat("0", 64)); err == nil {
		t.Fatal("runtime verifier accepted wrong independent plan digest")
	}
	// Once a deployment has committed, current new work is valid. The runtime
	// verifier must not replay or require the old installation's paused queues.
	if _, err := db.ExecContext(t.Context(), "UPDATE send_queue SET status='queued' WHERE id='native-send'"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeRuntime(t.Context(), cfg, planSHA); err != nil {
		t.Fatal("runtime verifier rejected committed new work", err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE send_queue SET status='canceled' WHERE id='native-send'"); err != nil {
		t.Fatal(err)
	}
	if err := checkNativeRestoreActivation(t.Context(), db, driver, targets.Data, targets.DKIM); err == nil {
		t.Fatal("CLI installation authorized mismatched runtime roots")
	}
	oldDB, err := openNativeBackupDatabase(t.Context(), sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNativeRestoreActivation(t.Context(), oldDB, driver, targets.Data, targets.Mail); err == nil {
		oldDB.Close()
		t.Fatal("original database authorized newly installed directory tree")
	}
	oldDB.Close()
	recoverArgs := append(append([]string{}, base...), "--journal", receipt.Journal)
	for _, mode := range []string{"activate-native", "rollback-native", "rollback-native"} {
		var recovered struct {
			Journal, DatabaseState string
			ServicesStarted        bool
			RollbackCompleted      bool
		}
		data := run(append([]string{mode}, recoverArgs...)...)
		if json.Unmarshal(data, &recovered) != nil || recovered.Journal != receipt.Journal || recovered.ServicesStarted {
			t.Fatal("public native recovery receipt invalid", mode)
		}
		if mode == "activate-native" && (recovered.DatabaseState != "activated" || recovered.RollbackCompleted) || mode == "rollback-native" && (recovered.DatabaseState != "guarded" || !recovered.RollbackCompleted) {
			t.Fatal("public native recovery receipt misstated outcome", mode)
		}
	}
	if checkNativeRestoreActivation(t.Context(), db, driver, targets.Data, targets.Mail) == nil {
		t.Fatal("CLI rollback left new DB active")
	}
	for _, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		if content, err := os.ReadFile(filepath.Join(directory, "old")); err != nil || string(content) != "synthetic-old-tree" {
			t.Fatal("CLI rollback lost old tree", err)
		}
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Fatal("CLI installation leaked private preparation", err)
	}
}

func simulateNativeCommittedTrees(t *testing.T, prepared string, targets install.Targets, journal string) install.JournalInfo {
	t.Helper()
	txn := "0123456789abcdef0123456789abcdef"
	roots := []map[string]string{}
	for i, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		name := []string{"data", "mail", "dkim"}[i]
		old, new := strings.Repeat("a", 32), strings.Repeat("b", 32)
		if err := os.WriteFile(filepath.Join(directory, ".lanqin-restore-"+txn), []byte("old\n"+old+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(directory, directory+".rollback-"+txn); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(directory, os.DirFS(filepath.Join(prepared, name))); err != nil {
			t.Fatal(err)
		}
		// CopyFS does not preserve INTERNALDATE; emulate the installer's copied
		// file timestamps as well as bytes, without bypassing production gates.
		if err := fs.WalkDir(os.DirFS(filepath.Join(prepared, name)), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Chtimes(filepath.Join(directory, path), info.ModTime(), info.ModTime())
		}); err != nil {
			t.Fatal(err)
		}
		if name == "data" {
			if err := os.Remove(filepath.Join(directory, ".lanqin-restore-not-activated")); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(directory, ".lanqin-restore-"+txn), []byte("new\n"+new+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, map[string]string{"Name": name, "Target": directory, "OldToken": old, "NewToken": new})
	}
	data, _ := json.Marshal(map[string]any{"Version": 1, "Txn": txn, "Roots": roots})
	file := filepath.Join(journal, "restore-"+txn+".journal")
	if err := os.WriteFile(file, append(append(data, '\n'), []byte("\"committed\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	return install.JournalInfo{Path: file, TransactionID: txn, RollbackTargets: targets}
}

func assertNativeWebBackup(t *testing.T, a *App, identity *age.X25519Identity) {
	t.Helper()
	_, records, _ := backupJobFixture(t, a)
	a.cfg.BackupRecipient, a.cfg.BackupWorkspace = identity.Recipient().String(), t.TempDir()
	setBackupTestPassword(t, a, "synthetic native Web password")
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client := backupLoggedInAdmin(t, a)
	var capabilities map[string]any
	if status := client.do("GET", "/api/admin/backups/capabilities", nil, &capabilities); status != 200 || capabilities["onlineBackupAvailable"] != true {
		t.Fatal("native Web backup capability unavailable", status, capabilities)
	}
	var created backupJobView
	if status := client.do("POST", "/api/admin/backups/snapshots", map[string]any{"destinationIds": []string{records[0].Config.ID}, "requestId": "native-web-snapshot-0001", "destinationsRevision": backupRevision(raw), "expectedRecipient": backupTestRecipient(t, a)}, &created); status != 202 {
		t.Fatal("native Web snapshot admission failed", status)
	}
	lease, err := a.claimBackupUpload(t.Context())
	if err != nil || lease == nil || lease.ID != created.ID {
		t.Fatal("native Web snapshot not claimed", err)
	}
	var ciphertext []byte
	a.backupUploadFile = func(ctx context.Context, file *os.File, sha string, cfg storage.Config, _ storage.Options, _ func(string) (storage.Credentials, error)) (storage.Report, error) {
		var err error
		ciphertext, err = io.ReadAll(file)
		if err != nil {
			return storage.Report{}, err
		}
		now := time.Now().UTC()
		return storage.Report{SHA256: sha, Bytes: int64(len(ciphertext)), Destinations: []storage.Result{{DestinationID: cfg.Destinations[0].ID, State: "succeeded", ObjectKey: "synthetic/native.age", CompletedAt: &now, Attempts: 1}}}, nil
	}
	a.runBackupUpload(t.Context(), *lease)
	finished, _, err := a.backupReadJob(t.Context(), created.ID)
	if err != nil || finished.Status != "succeeded" || finished.Artifact.ID == "" {
		t.Fatal("native Web snapshot failed", finished.ErrorCode, err)
	}
	recovered := filepath.Join(t.TempDir(), "recovered")
	manifest, err := backup.RestorePassword(t.Context(), bytes.NewReader(ciphertext), recovered, backupTestEncryption(t, a).Password, finished.Artifact.SHA256, backup.DefaultLimits())
	if err != nil || manifest.Driver != a.cfg.DBDriver || manifest.Scope != "online-native-data" {
		t.Fatal("Web worker produced incorrectly labeled native archive", err)
	}
	reportSHA, err := backup.StagedOnlineReportSHA256(t.Context(), recovered, a.cfg.DBDriver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.VerifyNativeStaged(t.Context(), recovered, a.cfg.DBDriver, reportSHA, backup.DefaultLimits()); err != nil {
		t.Fatal("native Web archive payload invalid", err)
	}
	if entries, err := os.ReadDir(a.cfg.BackupWorkspace); err != nil || len(entries) != 0 {
		t.Fatal("native Web snapshot retained plaintext", err)
	}
	a.cfg.BackupRestoreDir = t.TempDir()
	a.backupDownloadFile = func(_ context.Context, _ storage.Destination, _, sha, output string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
		if err := os.WriteFile(output, ciphertext, 0o600); err != nil {
			return storage.DownloadReceipt{}, err
		}
		return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(ciphertext))}, nil
	}
	_, raw, err = a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var restoreJob backupJobView
	req := backupRestoreRequest{SourceJobID: created.ID, DestinationID: records[0].Config.ID, ExpectedSHA256: finished.Artifact.SHA256, DestinationsRevision: backupRevision(raw), RequestID: "native-web-restore-0001", EncryptionMode: "password", BackupPassword: backupTestEncryption(t, a).Password, Password: a.cfg.AdminPassword, Confirm: true}
	if status := client.do("POST", "/api/admin/backups/restores", req, &restoreJob); status != 202 {
		t.Fatal("native Web recovery admission failed", status)
	}
	select {
	case execution := <-a.backupRestoreRequests:
		a.runBackupRestore(t.Context(), execution)
	default:
		t.Fatal("native Web recovery not dispatched")
	}
	stored, _, err := a.backupReadJob(t.Context(), restoreJob.ID)
	if err != nil || stored.Status != "succeeded" || !stored.Restore.Ready || stored.Restore.Manifest.Driver != a.cfg.DBDriver {
		t.Fatal("native Web recovery failed", stored.ErrorCode, err)
	}
	container, receipt, err := ResolveWebNativeRestore(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, restoreJob.ID)
	if err != nil || receipt != reportSHA || container != filepath.Join(a.cfg.BackupRestoreDir, stored.Restore.Directory, "recovered") {
		t.Fatal("native Web handoff failed", err)
	}
	for _, inputs := range []struct{ root, key, id string }{
		{t.TempDir(), a.cfg.BackupSecretKey, restoreJob.ID},
		{a.cfg.BackupRestoreDir, backupTestKey + "x", restoreJob.ID},
		{a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, created.ID},
	} {
		if _, _, err := ResolveWebNativeRestore(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, inputs.root, inputs.key, inputs.id); err == nil {
			t.Fatal("native handoff accepted wrong root/key/task")
		}
	}
	reportPath := filepath.Join(container, "snapshot", "data", "backup-online.json")
	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWebNativeRestore(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, restoreJob.ID); err == nil {
		t.Fatal("native handoff trusted modified plaintext report")
	}
	if err := os.WriteFile(reportPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// Emulate a container-created authenticated task mounted at a different host
	// path. The recorded Linux root is an explicit binding, never opened here.
	stored, storedRaw, err := a.backupReadJob(t.Context(), restoreJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Restore.Root = "/backups/restore"
	if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+stored.ID, storedRaw, stored); err != nil {
		t.Fatal(err)
	}
	mappedRoot := filepath.Join(t.TempDir(), "host-recovery")
	if err := os.Rename(a.cfg.BackupRestoreDir, mappedRoot); err != nil {
		t.Fatal(err)
	}
	a.cfg.BackupRestoreDir = mappedRoot
	if _, _, err := ResolveWebNativeRestore(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, mappedRoot, a.cfg.BackupSecretKey, stored.ID); err == nil {
		t.Fatal("implicit native root remapping accepted")
	}
	for _, recorded := range []string{"/wrong/recovery", "/backups/../restore", "/", "relative"} {
		if _, _, err := ResolveWebNativeRestoreAt(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, mappedRoot, recorded, a.cfg.BackupSecretKey, stored.ID); err == nil {
			t.Fatal("wrong recorded native root accepted")
		}
	}
	container, receipt, err = ResolveWebNativeRestoreAt(t.Context(), a.cfg.DBDriver, a.cfg.DBDSN, mappedRoot, "/backups/restore", a.cfg.BackupSecretKey, stored.ID)
	if err != nil || container != filepath.Join(mappedRoot, stored.Restore.Directory, "recovered") || receipt != reportSHA {
		t.Fatal("explicit native root mapping failed", err)
	}
	assertNativeWebImportCLI(t, a, restoreJob.ID, container, receipt, finished.Artifact.SHA256, "/backups/restore")
	assertNativeMaintenanceSourceImport(t, a, restoreJob.ID, "/backups/restore")
	t.Log("Native Web HTTP snapshot/recovery and authenticated handoff passed with real database/archive; cloud upload/download injected")
}

func assertNativeWebImportCLI(t *testing.T, a *App, jobID, container, reportSHA, ciphertextSHA, recordedRoot string) {
	t.Helper()
	targetDSN := os.Getenv("LANQIN_TEST_NATIVE_" + strings.ToUpper(a.cfg.DBDriver) + "_WEB_DSN")
	executable := os.Getenv("LANQIN_TEST_BACKUP_CLI")
	if targetDSN == "" || executable == "" {
		t.Log("isolated WEB target DSN/CLI absent; Web import subprocess not exercised")
		return
	}
	if targetDSN == a.cfg.DBDSN {
		t.Fatal("Web import test requires separate EMPTY database")
	}
	workspace := t.TempDir()
	cfg := Config{DBDriver: a.cfg.DBDriver, DBDSN: targetDSN}
	// A report can remain authenticated while a later payload changes. Both
	// original and copied-byte validation must run before creating target tables.
	dumpPath := filepath.Join(container, "snapshot", "data", "lanqin-native.jsonl")
	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dumpPath, append(append([]byte{}, dump...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportWebNativeBackupAt(t.Context(), cfg, a.cfg.DBDSN, a.cfg.BackupRestoreDir, recordedRoot, a.cfg.BackupSecretKey, jobID, workspace, backup.DefaultLimits()); err == nil {
		t.Fatal("Web import accepted changed payload")
	}
	probe, err := openNativeBackupDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	where := "table_schema=DATABASE()"
	if a.cfg.DBDriver == databaseDriverPostgres {
		where = "table_schema=current_schema()"
	}
	var count int
	if err := probe.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.tables WHERE "+where+" AND table_type='BASE TABLE'").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid Web payload touched empty target", err)
	}
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "LANQIN_") && !strings.HasPrefix(item, "NATIVE_SYNTHETIC_") {
			environment = append(environment, item)
		}
	}
	environment = append(environment, "NATIVE_SYNTHETIC_TARGET_DSN="+targetDSN, "NATIVE_SYNTHETIC_STATE_DSN="+a.cfg.DBDSN, "LANQIN_BACKUP_SECRET_KEY="+a.cfg.BackupSecretKey)
	run := func(args ...string) []byte {
		command := exec.CommandContext(t.Context(), executable, args...)
		command.Env = environment
		output, err := command.CombinedOutput()
		for _, secret := range []string{targetDSN, a.cfg.DBDSN, a.cfg.BackupSecretKey} {
			if bytes.Contains(output, []byte(secret)) {
				t.Fatal("Web native subprocess leaked connection or master key")
			}
		}
		if err != nil {
			t.Fatal("Web native CLI failed", args[0], err, string(output))
		}
		return output
	}
	webArgs := []string{"--web-job", jobID, "--web-state-dsn-env", "NATIVE_SYNTHETIC_STATE_DSN", "--web-restore-root", a.cfg.BackupRestoreDir, "--web-recorded-root", recordedRoot}
	importArgs := []string{"import-native", "--driver", a.cfg.DBDriver, "--target-dsn-env", "NATIVE_SYNTHETIC_TARGET_DSN", "--workspace", workspace, "--confirm-empty-target"}
	output := run(append(importArgs, webArgs...)...)
	var result NativeImportResult
	if json.Unmarshal(output, &result) != nil || result.Activated || result.Rows < 1 || result.Driver != cfg.DBDriver || result.ReportSHA256 != reportSHA || result.SourceSHA256 != ciphertextSHA {
		t.Fatal("Web import receipt not bound to authenticated task")
	}
	if checkNativeRestoreActivation(t.Context(), probe, cfg.DBDriver) == nil {
		t.Fatal("Web import activated target")
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatal("Web import retained private copy", err)
	}
	if data, err := os.ReadFile(dumpPath); err != nil || !bytes.Equal(data, dump) {
		t.Fatal("Web import mutated original recovery", err)
	}
	if _, err := ImportWebNativeBackupAt(t.Context(), cfg, a.cfg.DBDSN, a.cfg.BackupRestoreDir, recordedRoot, a.cfg.BackupSecretKey, jobID, workspace, backup.DefaultLimits()); err == nil {
		t.Fatal("Web import overwrote nonempty target")
	}
	if runtime.GOOS == "linux" {
		assertNativeMaintenanceCLILifecycle(t, run, a.cfg, targetDSN, webArgs)
		if data, err := os.ReadFile(dumpPath); err != nil || !bytes.Equal(data, dump) {
			t.Fatal("Web maintenance installation mutated authenticated source recovery", err)
		}
		t.Log("Public native Web handoff import/install/activation/repeated rollback passed with real Linux directories and explicit process UID/GID")
	} else {
		t.Log("Public Web import exercised; Linux-only Web maintenance lifecycle not executed on this host")
	}
}
