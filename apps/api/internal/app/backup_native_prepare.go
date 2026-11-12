package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

const nativeRestoreActiveKey = "backup.native.restore.active.v1"

type nativeRestoreMarker struct {
	Version        int                     `json:"version"`
	Driver         string                  `json:"driver"`
	DatabaseSHA256 string                  `json:"databaseSHA256"`
	Operation      string                  `json:"operation"`
	CreatedAt      time.Time               `json:"createdAt"`
	Phase          string                  `json:"phase,omitempty"`
	Plan           *NativeInstallationPlan `json:"plan,omitempty"`
	PlanSHA256     string                  `json:"planSHA256,omitempty"`
	Journal        string                  `json:"journal,omitempty"`
	Transaction    string                  `json:"transaction,omitempty"`
}

type NativeInstallationPlan struct {
	Version                   int             `json:"version"`
	Driver                    string          `json:"driver"`
	Operation                 string          `json:"operation"`
	DatabaseSHA256            string          `json:"databaseSHA256"`
	ReportSHA256              string          `json:"reportSHA256"`
	RuntimeDataRoot           string          `json:"runtimeDataRoot"`
	RuntimeMailRoot           string          `json:"runtimeMailRoot"`
	Targets                   install.Targets `json:"targets"`
	SendQueue                 map[string]int  `json:"sendQueueCanceledByOriginalStatus"`
	Scheduled                 map[string]int  `json:"scheduledSendsCancelledByOriginalStatus"`
	LocalDelivery             map[string]int  `json:"localDeliveryPausedByOriginalStatus"`
	SessionsRevoked           int             `json:"sessionsRevoked"`
	LoginChallengesRevoked    int             `json:"loginChallengesRevoked"`
	APITokensDisabled         int             `json:"apiTokensDisabled"`
	ExternalAccountsDisabled  int             `json:"externalAccountsDisabled"`
	WebhookNotificationsHeld  int             `json:"webhookNotificationsHeld"`
	TelegramNotificationsHeld int             `json:"telegramNotificationsHeld"`
	BackupRuntimeRemoved      int             `json:"backupRuntimeRemoved"`
	Unindexed                 int             `json:"unindexed"`
	Notice                    string          `json:"notice"`
}

func readNativeRestoreMarker(ctx context.Context, query backupSnapshotQuery, driver, keyName string, forUpdate bool) (nativeRestoreMarker, string, error) {
	var marker nativeRestoreMarker
	var raw string
	column, _ := nativeIdentifier(driver, "key")
	sql := "SELECT value FROM system_settings WHERE " + column + "=?"
	if forUpdate {
		sql += " FOR UPDATE"
	}
	if err := query.QueryRowContext(ctx, sql, keyName).Scan(&raw); err != nil {
		return marker, "", err
	}
	if len(raw) > 60<<10 {
		return marker, "", errNativeDatabaseBackup
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&marker) != nil || decoder.Decode(new(any)) != io.EOF || marker.Version != 1 || marker.Driver != driver || len(marker.Operation) != 32 || marker.CreatedAt.IsZero() {
		return marker, "", errNativeDatabaseBackup
	}
	operation, err := hex.DecodeString(marker.Operation)
	if err != nil || len(operation) != 16 {
		return marker, "", errNativeDatabaseBackup
	}
	digest, err := hex.DecodeString(marker.DatabaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return marker, "", errNativeDatabaseBackup
	}
	if marker.Phase == "" {
		marker.Phase = "imported"
	}
	return marker, raw, nil
}

func nativeRuntimeRoot(value string) bool {
	return value != "/" && strings.HasPrefix(value, "/") && path.Clean(value) == value && utf8.ValidString(value) && !strings.ContainsAny(value, "\\:\x00") && strings.IndexFunc(value, unicode.IsControl) < 0 && len(value) <= 4096
}

func nativeSourceRelative(root, name string) (string, error) {
	separator := "/"
	if strings.Contains(root, "\\") {
		separator = "\\"
	}
	if !strings.HasPrefix(name, root+separator) {
		return "", errNativeDatabaseBackup
	}
	rel := strings.TrimPrefix(name, root+separator)
	if separator == "\\" {
		rel = strings.ReplaceAll(rel, "\\", "/")
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) || path.Clean(rel) != rel || strings.ContainsAny(rel, "\\\x00") || strings.IndexFunc(rel, unicode.IsControl) >= 0 {
		return "", errNativeDatabaseBackup
	}
	return rel, nil
}

func PrepareNativeInstallation(ctx context.Context, cfg Config, staged, workspace, runtimeData, runtimeMail string, targets install.Targets, expectedReportSHA256 string, limits backup.Limits) (*install.Prepared, error) {
	cfg = normalizeDatabaseConfig(cfg)
	if !nativeRuntimeRoot(runtimeData) || !nativeRuntimeRoot(runtimeMail) || runtimeData == runtimeMail || strings.HasPrefix(runtimeData, runtimeMail+"/") || strings.HasPrefix(runtimeMail, runtimeData+"/") {
		return nil, errNativeDatabaseBackup
	}
	if _, err := install.CheckTargets(targets); err != nil {
		return nil, err
	}
	return install.PrepareNativeFiles(ctx, staged, workspace, cfg.DBDriver, expectedReportSHA256, limits, func(ctx context.Context, snapshot string, reportBytes []byte) (json.RawMessage, error) {
		return prepareNativeDatabaseInstallation(ctx, cfg, snapshot, reportBytes, runtimeData, runtimeMail, targets, limits)
	})
}

func prepareNativeDatabaseInstallation(ctx context.Context, cfg Config, snapshot string, reportBytes []byte, runtimeData, runtimeMail string, targets install.Targets, limits backup.Limits) (json.RawMessage, error) {
	var report OnlineBackupReport
	if json.Unmarshal(reportBytes, &report) != nil || report.Version != 2 || report.Scope != cfg.DBDriver+"-reference-closure" || !report.Complete {
		return nil, errNativeDatabaseBackup
	}
	reportDigest := sha256.Sum256(reportBytes)
	reportSHA := hex.EncodeToString(reportDigest[:])
	databaseSHA := ""
	for _, item := range report.Files {
		if item.Kind == "database" && item.Destination == "data/lanqin-native.jsonl" {
			databaseSHA = item.SHA256
		}
	}
	file, err := os.Open(filepath.Join(snapshot, "data", "lanqin-native.jsonl"))
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, io.LimitReader(file, limits.MaxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || hex.EncodeToString(digest.Sum(nil)) != databaseSHA {
		return nil, errNativeDatabaseBackup
	}
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer conn.Close()
	unlock, err := lockExternalSchema(ctx, conn, cfg.DBDriver)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer unlock()
	if _, err := nativeSchemaHeader(ctx, conn, cfg.DBDriver); err != nil {
		return nil, err
	}
	keys, err := readNativeForeignKeys(ctx, conn, cfg.DBDriver)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer tx.Rollback()
	marker, raw, err := readNativeRestoreMarker(ctx, tx, cfg.DBDriver, nativeRestorePendingKey, true)
	if err != nil || marker.DatabaseSHA256 != databaseSHA || marker.Phase != "imported" && marker.Phase != "prepared" {
		return nil, errNativeDatabaseBackup
	}
	plan := NativeInstallationPlan{Version: 1, Driver: cfg.DBDriver, Operation: marker.Operation, DatabaseSHA256: databaseSHA, ReportSHA256: reportSHA, RuntimeDataRoot: runtimeData, RuntimeMailRoot: runtimeMail, Targets: targets}
	prepared := marker.Phase == "prepared"
	if prepared {
		previous := marker.Plan
		if previous == nil || previous.ReportSHA256 != reportSHA || previous.RuntimeDataRoot != runtimeData || previous.RuntimeMailRoot != runtimeMail || previous.Targets != targets {
			return nil, errNativeDatabaseBackup
		}
		plan = *previous
	}
	if err := mapNativeReferences(ctx, tx, report, runtimeData, runtimeMail, limits, prepared, &plan); err != nil {
		return nil, err
	}
	if !prepared {
		plan.SendQueue, err = pauseNativeWork(ctx, tx, "send_queue", []string{"queued", "sending", "failed", "pending"}, []string{"delivered", "canceled"}, "canceled", limits.MaxEntries)
		if err != nil {
			return nil, err
		}
		plan.Scheduled, err = pauseNativeWork(ctx, tx, "scheduled_sends", []string{"pending", "sending"}, []string{"sent", "failed", "cancelled"}, "cancelled", limits.MaxEntries)
		if err != nil {
			return nil, err
		}
		plan.LocalDelivery, err = pauseNativeWork(ctx, tx, "local_delivery_jobs", []string{"queued", "sending"}, []string{"delivered", "failed", "restore_paused"}, "restore_paused", limits.MaxEntries)
		if err != nil {
			return nil, err
		}
		key, _ := nativeIdentifier(cfg.DBDriver, "key")
		for _, change := range []struct {
			table, predicate, statement string
			count                       *int
		}{
			{"sessions", "", "DELETE FROM sessions", &plan.SessionsRevoked},
			{"login_challenges", "", "DELETE FROM login_challenges", &plan.LoginChallengesRevoked},
			{"api_tokens", " WHERE disabled<>1", "UPDATE api_tokens SET disabled=1 WHERE disabled<>1", &plan.APITokensDisabled},
			{"external_imap_accounts", " WHERE enabled<>0", "UPDATE external_imap_accounts SET enabled=0 WHERE enabled<>0", &plan.ExternalAccountsDisabled},
			{"status_webhook_outbox", " WHERE delivered_at IS NULL", "UPDATE status_webhook_outbox SET next_attempt_at='9999-12-31T23:59:59Z' WHERE delivered_at IS NULL", &plan.WebhookNotificationsHeld},
			{"telegram_notification_outbox", " WHERE delivered_at IS NULL", "UPDATE telegram_notification_outbox SET next_attempt_at='9999-12-31T23:59:59Z' WHERE delivered_at IS NULL", &plan.TelegramNotificationsHeld},
			{"system_settings", " WHERE " + key + " IN ('backup.queue.v1','backup.schedules.v1') OR " + key + " LIKE 'backup.job.v1.%' OR " + key + " LIKE 'backup.transfer.v1.%' OR " + key + " LIKE 'backup.discovery.v1.%' OR " + key + " LIKE 'backup.drive.oauth.v1.%'", "DELETE FROM system_settings WHERE " + key + " IN ('backup.queue.v1','backup.schedules.v1') OR " + key + " LIKE 'backup.job.v1.%' OR " + key + " LIKE 'backup.transfer.v1.%' OR " + key + " LIKE 'backup.discovery.v1.%' OR " + key + " LIKE 'backup.drive.oauth.v1.%'", &plan.BackupRuntimeRemoved},
		} {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+change.table+change.predicate).Scan(change.count); err != nil || *change.count < 0 || *change.count > limits.MaxEntries {
				return nil, errNativeDatabaseBackup
			}
			if _, err := tx.ExecContext(ctx, change.statement); err != nil {
				return nil, errNativeDatabaseBackup
			}
		}
	}
	if validateNativeForeignKeys(ctx, tx, cfg.DBDriver, keys) != nil {
		return nil, errNativeDatabaseBackup
	}
	if err := checkNativePreparedWork(ctx, tx); err != nil {
		return nil, err
	}
	plan.Notice = "Native restore prepared but NOT activated. Old sending canceled, local delivery paused, sessions/challenges revoked, API tokens disabled, external accounts disabled and pending notifications held. Original database and archive remain unchanged. Explicit maintenance directory switching and journal-bound activation are required; services are never started by this tool."
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil || len(data) > 32<<10 {
		return nil, errNativeDatabaseBackup
	}
	data = append(data, '\n')
	planDigest := sha256.Sum256(data)
	marker.Phase, marker.Plan, marker.PlanSHA256 = "prepared", &plan, hex.EncodeToString(planDigest[:])
	value, err := json.Marshal(marker)
	if err != nil || len(value) > 60<<10 {
		return nil, errNativeDatabaseBackup
	}
	key, _ := nativeIdentifier(cfg.DBDriver, "key")
	result, err := tx.ExecContext(ctx, "UPDATE system_settings SET value=?,updated_at=? WHERE "+key+"=? AND value=?", string(value), time.Now().UTC().Format(time.RFC3339Nano), nativeRestorePendingKey, raw)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return nil, errNativeDatabaseBackup
	}
	if tx.Commit() != nil {
		return nil, errNativeDatabaseBackup
	}
	return data, nil
}

func mapNativeReferences(ctx context.Context, tx *sql.Tx, report OnlineBackupReport, dataRoot, mailRoot string, limits backup.Limits, prepared bool, plan *NativeInstallationPlan) error {
	messages, attachments := map[string]OnlineBackupFile{}, map[string]OnlineBackupFile{}
	for _, item := range report.Files {
		switch item.Kind {
		case "message", "reconstructed-draft":
			if _, ok := messages[item.MessageID]; ok {
				return errNativeDatabaseBackup
			}
			messages[item.MessageID] = item
		case "attachment":
			if _, ok := attachments[item.AttachmentID]; ok {
				return errNativeDatabaseBackup
			}
			attachments[item.AttachmentID] = item
		case "unindexed-candidate":
			if !prepared {
				plan.Unindexed++
			}
		}
	}
	type mapping struct{ id, target string }
	changes := []mapping{}
	rows, err := tx.QueryContext(ctx, "SELECT id,message_id,storage_path,size_bytes FROM attachments")
	if err != nil {
		return errNativeDatabaseBackup
	}
	for rows.Next() {
		var id, message, old string
		var size int64
		if rows.Scan(&id, &message, &old, &size) != nil || len(changes) >= limits.MaxEntries {
			rows.Close()
			return errNativeDatabaseBackup
		}
		item, ok := attachments[id]
		target := dataRoot + "/" + strings.TrimPrefix(item.Destination, "data/")
		valid := prepared && old == target
		if !prepared {
			relative, err := nativeSourceRelative(report.Sources.Data, old)
			valid = err == nil && item.Destination == "data/"+relative
		}
		if !ok || !valid || item.MessageID != message || item.Bytes != size {
			rows.Close()
			return errNativeDatabaseBackup
		}
		changes = append(changes, mapping{id, target})
		delete(attachments, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(attachments) != 0 {
		return errNativeDatabaseBackup
	}
	for _, change := range changes {
		if _, err := tx.ExecContext(ctx, "UPDATE attachments SET storage_path=? WHERE id=?", change.target, change.id); err != nil {
			return errNativeDatabaseBackup
		}
	}
	changes = nil
	rows, err = tx.QueryContext(ctx, `SELECT m.id,m.raw_path,CASE WHEN EXISTS(SELECT 1 FROM external_imap_messages e WHERE e.local_message_id=m.id) OR EXISTS(SELECT 1 FROM external_imap_folder_states f WHERE f.local_folder_id=m.folder_id) THEN 1 ELSE 0 END FROM messages m`)
	if err != nil {
		return errNativeDatabaseBackup
	}
	count := 0
	for rows.Next() {
		var id, old string
		var external int
		count++
		if rows.Scan(&id, &old, &external) != nil || count > limits.MaxEntries {
			rows.Close()
			return errNativeDatabaseBackup
		}
		item, ok := messages[id]
		if old == "" && !ok && external != 0 {
			continue
		}
		target := mailRoot + "/" + strings.TrimPrefix(item.Destination, "mail/")
		valid := prepared && old == target
		if !prepared && old == "" {
			valid = item.Kind == "reconstructed-draft" && external == 0
		} else if !prepared {
			relative, err := nativeSourceRelative(report.Sources.Mail, old)
			valid = err == nil && item.Destination == "mail/"+relative
		}
		if !ok || !valid {
			rows.Close()
			return errNativeDatabaseBackup
		}
		changes = append(changes, mapping{id, target})
		delete(messages, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(messages) != 0 {
		return errNativeDatabaseBackup
	}
	for _, change := range changes {
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET raw_path=? WHERE id=?", change.target, change.id); err != nil {
			return errNativeDatabaseBackup
		}
	}
	return nil
}

func pauseNativeWork(ctx context.Context, tx *sql.Tx, table string, active, terminal []string, paused string, limit int) (map[string]int, error) {
	counts := map[string]int{}
	states := map[string]bool{}
	for _, state := range active {
		states[state] = true
	}
	for _, state := range terminal {
		states[state] = false
	}
	rows, err := tx.QueryContext(ctx, "SELECT status,COUNT(*) FROM "+table+" GROUP BY status")
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	total := 0
	for rows.Next() {
		var state string
		var count int
		if rows.Scan(&state, &count) != nil || count < 0 {
			rows.Close()
			return nil, errNativeDatabaseBackup
		}
		enabled, ok := states[state]
		total += count
		if !ok || total > limit {
			rows.Close()
			return nil, errNativeDatabaseBackup
		}
		if enabled {
			counts[state] = count
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	for state := range counts {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET status=? WHERE status=?", paused, state); err != nil {
			return nil, errNativeDatabaseBackup
		}
	}
	return counts, nil
}

func checkNativePreparedWork(ctx context.Context, db backupSnapshotQuery) error {
	for _, query := range []string{
		"SELECT COUNT(*) FROM sessions", "SELECT COUNT(*) FROM login_challenges", "SELECT COUNT(*) FROM api_tokens WHERE disabled<>1", "SELECT COUNT(*) FROM external_imap_accounts WHERE enabled<>0",
		"SELECT COUNT(*) FROM send_queue WHERE status NOT IN ('delivered','canceled')", "SELECT COUNT(*) FROM scheduled_sends WHERE status NOT IN ('sent','failed','cancelled')", "SELECT COUNT(*) FROM local_delivery_jobs WHERE status NOT IN ('delivered','failed','restore_paused')",
		"SELECT COUNT(*) FROM status_webhook_outbox WHERE delivered_at IS NULL AND next_attempt_at<>'9999-12-31T23:59:59Z'", "SELECT COUNT(*) FROM telegram_notification_outbox WHERE delivered_at IS NULL AND next_attempt_at<>'9999-12-31T23:59:59Z'",
	} {
		var count int
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 0 {
			return errNativeDatabaseBackup
		}
	}
	return nil
}
