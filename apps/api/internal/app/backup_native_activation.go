package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

func nativeFileBytes(name string, limit int64) ([]byte, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errNativeDatabaseBackup
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errNativeDatabaseBackup
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errNativeDatabaseBackup
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != before.Size() {
		return nil, errNativeDatabaseBackup
	}
	return data, nil
}

// VerifyNativeRuntime checks the deployment namespace's actual database and
// installed data plan without App.New, migrations, workers or a listening
// service. A coordinator runs this in the final container environment before
// committing service activation; a host-side DSN is not proof of that binding.
func VerifyNativeRuntime(ctx context.Context, cfg Config, expectedPlanSHA256 string) error {
	cfg = normalizeDatabaseConfig(cfg)
	want, err := hex.DecodeString(expectedPlanSHA256)
	if err != nil || len(want) != sha256.Size || !nativeRuntimeRoot(cfg.DataDir) || !nativeRuntimeRoot(cfg.MaildirRoot) || cfg.DBDriver != databaseDriverMySQL && cfg.DBDriver != databaseDriverPostgres {
		return errNativeDatabaseBackup
	}
	planBytes, err := nativeFileBytes(filepath.Join(cfg.DataDir, "restore-install-plan.json"), 32<<10)
	if err != nil {
		return errNativeDatabaseBackup
	}
	actual := sha256.Sum256(planBytes)
	if hex.EncodeToString(actual[:]) != hex.EncodeToString(want) {
		return errNativeDatabaseBackup
	}
	var plan NativeInstallationPlan
	if json.Unmarshal(planBytes, &plan) != nil || plan.Version != 1 || plan.Driver != cfg.DBDriver || plan.RuntimeDataRoot != cfg.DataDir || plan.RuntimeMailRoot != cfg.MaildirRoot {
		return errNativeDatabaseBackup
	}
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return errNativeDatabaseBackup
	}
	defer db.Close()
	if _, err := nativeSchemaHeader(ctx, db, cfg.DBDriver); err != nil {
		return errNativeDatabaseBackup
	}
	keys, err := readNativeForeignKeys(ctx, db, cfg.DBDriver)
	if err != nil || validateNativeForeignKeys(ctx, db, cfg.DBDriver, keys) != nil {
		return errNativeDatabaseBackup
	}
	if checkNativeRestoreActivation(ctx, db, cfg.DBDriver, cfg.DataDir, cfg.MaildirRoot) != nil {
		return errNativeDatabaseBackup
	}
	active, _, err := readNativeRestoreMarker(ctx, db, cfg.DBDriver, nativeRestoreActiveKey, false)
	if err != nil || active.Phase != "active" || active.Operation != plan.Operation || active.PlanSHA256 != hex.EncodeToString(want) {
		return errNativeDatabaseBackup
	}
	return ctx.Err()
}

func sameNativePlan(marker nativeRestoreMarker, targets install.Targets) bool {
	plan := marker.Plan
	if plan == nil || plan.Version != 1 || plan.Driver != marker.Driver || plan.Operation != marker.Operation || plan.DatabaseSHA256 != marker.DatabaseSHA256 || plan.Targets != targets || !nativeRuntimeRoot(plan.RuntimeDataRoot) || !nativeRuntimeRoot(plan.RuntimeMailRoot) {
		return false
	}
	for _, digest := range []string{marker.PlanSHA256, plan.ReportSHA256, marker.DatabaseSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return false
		}
	}
	return true
}

func ActivateNativeInstallation(ctx context.Context, cfg Config, journal string, targets install.Targets, confirmed bool) (install.JournalInfo, error) {
	if runtime.GOOS != "linux" {
		return install.JournalInfo{}, install.ErrSwitchPlatform
	}
	if !confirmed {
		return install.JournalInfo{}, install.ErrMaintenanceRequired
	}
	return activateNativeInstallation(ctx, cfg, journal, targets)
}

func activateNativeInstallation(ctx context.Context, cfg Config, journal string, targets install.Targets) (install.JournalInfo, error) {
	cfg = normalizeDatabaseConfig(cfg)
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return install.JournalInfo{}, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	defer conn.Close()
	unlock, err := lockExternalSchema(ctx, conn, cfg.DBDriver)
	if err != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	defer unlock()
	marker, raw, err := readNativeRestoreMarker(ctx, conn, cfg.DBDriver, nativeRestorePendingKey, false)
	alreadyActive := errors.Is(err, sql.ErrNoRows)
	if alreadyActive {
		marker, raw, err = readNativeRestoreMarker(ctx, conn, cfg.DBDriver, nativeRestoreActiveKey, false)
	}
	if err != nil || !sameNativePlan(marker, targets) || marker.Phase != "prepared" && !alreadyActive || alreadyActive && marker.Phase != "active" {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	info, err := install.VerifyNativeInstalled(ctx, journal, targets, cfg.DBDriver, marker.Plan.ReportSHA256, marker.PlanSHA256, backup.DefaultLimits())
	if err != nil {
		return install.JournalInfo{}, err
	}
	if marker.Transaction != "" && (marker.Transaction != info.TransactionID || marker.Journal != info.Path) {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	if _, err := nativeSchemaHeader(ctx, conn, cfg.DBDriver); err != nil {
		return install.JournalInfo{}, err
	}
	keys, err := readNativeForeignKeys(ctx, conn, cfg.DBDriver)
	if err != nil {
		return install.JournalInfo{}, err
	}
	reportBytes, err := nativeFileBytes(filepath.Join(targets.Data, "restore-source-report.json"), 64<<20)
	if err != nil {
		return install.JournalInfo{}, err
	}
	digest := sha256.Sum256(reportBytes)
	if hex.EncodeToString(digest[:]) != marker.Plan.ReportSHA256 {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	var report OnlineBackupReport
	if json.Unmarshal(reportBytes, &report) != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	defer tx.Rollback()
	keyName := nativeRestorePendingKey
	if alreadyActive {
		keyName = nativeRestoreActiveKey
	}
	current, currentRaw, err := readNativeRestoreMarker(ctx, tx, cfg.DBDriver, keyName, true)
	if err != nil || currentRaw != raw || current.Operation != marker.Operation {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	plan := *marker.Plan
	if mapNativeReferences(ctx, tx, report, plan.RuntimeDataRoot, plan.RuntimeMailRoot, backup.DefaultLimits(), true, &plan) != nil || checkNativePreparedWork(ctx, tx) != nil || validateNativeForeignKeys(ctx, tx, cfg.DBDriver, keys) != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	if _, err := install.VerifyCommitted(ctx, journal, targets); err != nil {
		return install.JournalInfo{}, err
	}
	if !alreadyActive {
		marker.Phase, marker.Transaction, marker.Journal = "active", info.TransactionID, info.Path
		encoded, err := json.Marshal(marker)
		if err != nil || len(encoded) > 60<<10 {
			return install.JournalInfo{}, errNativeDatabaseBackup
		}
		column, _ := nativeIdentifier(cfg.DBDriver, "key")
		query := insertIgnoreSQL(cfg.DBDriver, "INSERT INTO system_settings("+column+",value,updated_at) VALUES(?,?,?)", "("+column+")")
		result, err := tx.ExecContext(ctx, query, nativeRestoreActiveKey, string(encoded), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return install.JournalInfo{}, errNativeDatabaseBackup
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return install.JournalInfo{}, errNativeDatabaseBackup
		}
		result, err = tx.ExecContext(ctx, "DELETE FROM system_settings WHERE "+column+"=? AND value=?", nativeRestorePendingKey, raw)
		if err != nil {
			return install.JournalInfo{}, errNativeDatabaseBackup
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return install.JournalInfo{}, errNativeDatabaseBackup
		}
	}
	if tx.Commit() != nil {
		return install.JournalInfo{}, errNativeDatabaseBackup
	}
	return info, nil
}

func RollbackNativeInstallation(ctx context.Context, cfg Config, journal string, targets install.Targets, confirmed bool) error {
	if runtime.GOOS != "linux" {
		return install.ErrSwitchPlatform
	}
	if !confirmed {
		return install.ErrMaintenanceRequired
	}
	return rollbackNativeInstallation(ctx, cfg, journal, targets, install.Rollback)
}

func rollbackNativeInstallation(ctx context.Context, cfg Config, journal string, targets install.Targets, rollbackFiles func(context.Context, string, install.Targets, bool) error) error {
	cfg = normalizeDatabaseConfig(cfg)
	db, err := openNativeBackupDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return errNativeDatabaseBackup
	}
	defer conn.Close()
	unlock, err := lockExternalSchema(ctx, conn, cfg.DBDriver)
	if err != nil {
		return errNativeDatabaseBackup
	}
	defer unlock()
	marker, _, err := readNativeRestoreMarker(ctx, conn, cfg.DBDriver, nativeRestorePendingKey, false)
	if errors.Is(err, sql.ErrNoRows) {
		marker, _, err = readNativeRestoreMarker(ctx, conn, cfg.DBDriver, nativeRestoreActiveKey, false)
	}
	if err != nil || !sameNativePlan(marker, targets) || marker.Phase != "prepared" && marker.Phase != "active" && marker.Phase != "rollback" && marker.Phase != "rolled-back" {
		return errNativeDatabaseBackup
	}
	info, err := install.VerifyPlanTransaction(ctx, journal, targets, marker.PlanSHA256)
	if err != nil {
		return err
	}
	if marker.Transaction != "" && (marker.Transaction != info.TransactionID || marker.Journal != info.Path) {
		return errNativeDatabaseBackup
	}
	marker.Phase, marker.Transaction, marker.Journal = "rollback", info.TransactionID, info.Path
	if err := guardNativeRollback(ctx, conn, cfg.DBDriver, marker); err != nil {
		return err
	}
	if err := rollbackFiles(ctx, journal, targets, true); err != nil {
		return err
	}
	marker.Phase = "rolled-back"
	return guardNativeRollback(ctx, conn, cfg.DBDriver, marker)
}

func guardNativeRollback(ctx context.Context, conn *sql.Conn, driver string, marker nativeRestoreMarker) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errNativeDatabaseBackup
	}
	defer tx.Rollback()
	current, raw, err := readNativeRestoreMarker(ctx, tx, driver, nativeRestorePendingKey, true)
	column, _ := nativeIdentifier(driver, "key")
	value, marshalErr := json.Marshal(marker)
	if marshalErr != nil || len(value) > 60<<10 {
		return errNativeDatabaseBackup
	}
	if errors.Is(err, sql.ErrNoRows) {
		current, _, err = readNativeRestoreMarker(ctx, tx, driver, nativeRestoreActiveKey, true)
		if err != nil || current.Operation != marker.Operation {
			return errNativeDatabaseBackup
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO system_settings("+column+",value,updated_at) VALUES(?,?,?)", nativeRestorePendingKey, string(value), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return errNativeDatabaseBackup
		}
	} else {
		if err != nil || current.Operation != marker.Operation {
			return errNativeDatabaseBackup
		}
		result, err := tx.ExecContext(ctx, "UPDATE system_settings SET value=?,updated_at=? WHERE "+column+"=? AND value=?", string(value), time.Now().UTC().Format(time.RFC3339Nano), nativeRestorePendingKey, raw)
		if err != nil {
			return errNativeDatabaseBackup
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return errNativeDatabaseBackup
		}
	}
	if tx.Commit() != nil {
		return errNativeDatabaseBackup
	}
	return nil
}
