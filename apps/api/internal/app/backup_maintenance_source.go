package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

type WebMaintenanceSource struct {
	Directory    string
	ReportSHA256 string
	SourceSHA256 string
	Driver       string
}

var errWebMaintenanceSource = errors.New("authenticated Web maintenance source or current administrator unavailable")

// ResolveWebMaintenanceSource uses only explicit database configuration and a
// fresh read-only connection/transaction. It never migrates, starts workers or
// reads default application environment. Cross-UID copying is a separate step.
func ResolveWebMaintenanceSource(ctx context.Context, cfg Config, recoveryRoot, recordedRoot, masterKey, jobID, actorID, expectedReportSHA string) (WebMaintenanceSource, error) {
	empty := WebMaintenanceSource{}
	cfg = normalizeDatabaseConfig(cfg)
	if !backupJobIDPattern.MatchString(jobID) || actorID == "" || len(actorID) > 128 || !maintenanceSourceDigest(expectedReportSHA) || recoveryRoot == "" {
		return empty, errWebMaintenanceSource
	}
	db, err := openMaintenanceSourceDatabase(ctx, cfg)
	if err != nil {
		return empty, errWebMaintenanceSource
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, errWebMaintenanceSource
	}
	defer tx.Rollback()
	var role string
	var disabled int
	if tx.QueryRowContext(ctx, `SELECT role,disabled FROM users WHERE id=?`, actorID).Scan(&role, &disabled) != nil || role != "admin" || disabled != 0 {
		return empty, errWebMaintenanceSource
	}
	var raw string
	key := backupJobPrefix + jobID
	if tx.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE `+keyColumnSQL(cfg.DBDriver)+`=?`, key).Scan(&raw) != nil {
		return empty, errWebMaintenanceSource
	}
	a := &App{cfg: Config{DBDriver: cfg.DBDriver, DBPath: cfg.DBPath, BackupRestoreDir: recoveryRoot, BackupSecretKey: masterKey}, backupRestoreRequests: make(chan backupRestoreExecution, 1)}
	var job backupJob
	if a.backupOpen(key, raw, &job) != nil || job.ID != jobID || !validBackupJob(job) || job.Kind != "restore" || job.Status != "succeeded" || job.CancelRequested || job.Restore == nil || !job.Restore.Ready || job.Restore.Manifest == nil || job.Restore.ReportSHA256 != expectedReportSHA || !maintenanceSourceDigest(job.Artifact.SHA256) {
		return empty, errWebMaintenanceSource
	}
	m := job.Restore.Manifest
	if cfg.DBDriver == "sqlite" && (m.Version != 1 || m.Scope != "online-data" || m.Driver != "sqlite") || cfg.DBDriver != "sqlite" && (m.Version != 2 || m.Scope != "online-native-data" || m.Driver != cfg.DBDriver) {
		return empty, errWebMaintenanceSource
	}
	if tx.Rollback() != nil {
		return empty, errWebMaintenanceSource
	}
	root, err := a.backupRestoreRoot()
	if err != nil {
		return empty, errWebMaintenanceSource
	}
	expectedRoot, err := recordedWebRestoreRoot(root, recordedRoot)
	if err != nil || job.Restore.Root != expectedRoot || filepath.Base(job.Restore.Directory) != job.Restore.Directory || !strings.HasPrefix(job.Restore.Directory, jobID+"-") {
		return empty, errWebMaintenanceSource
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return empty, errWebMaintenanceSource
	}
	defer parent.Close()
	child, err := parent.Lstat(job.Restore.Directory)
	if err != nil || !child.IsDir() || child.Mode()&os.ModeSymlink != 0 {
		return empty, errWebMaintenanceSource
	}
	container := filepath.Join(root, job.Restore.Directory, "recovered")
	resolved, err := filepath.EvalSymlinks(container)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(container) || backup.VerifyStagedOnlineReport(ctx, container, expectedReportSHA, cfg.DBDriver) != nil {
		return empty, errWebMaintenanceSource
	}
	return WebMaintenanceSource{Directory: container, ReportSHA256: expectedReportSHA, SourceSHA256: job.Artifact.SHA256, Driver: cfg.DBDriver}, nil
}

func maintenanceSourceDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func openMaintenanceSourceDatabase(ctx context.Context, cfg Config) (*sql.DB, error) {
	if cfg.DBDriver != "sqlite" {
		return openNativeBackupDatabase(ctx, cfg)
	}
	name, err := filepath.Abs(cfg.DBPath)
	if err != nil || cfg.DBPath == "" {
		return nil, errWebMaintenanceSource
	}
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errWebMaintenanceSource
	}
	path := filepath.ToSlash(name)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=trusted_schema(OFF)&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errWebMaintenanceSource
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errWebMaintenanceSource
	}
	return db, nil
}

// ImportVerifiedNativeStage independently copies and verifies installer-owned
// evidence, then imports only into an explicitly selected empty same-engine DB.
// The existing importer leaves an activation guard and never starts services.
func ImportVerifiedNativeStage(ctx context.Context, cfg Config, staged, reportSHA, sourceSHA, workspace string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	cfg = normalizeDatabaseConfig(cfg)
	invalid := errors.New("verified private native stage and independent empty target required")
	if cfg.DBDriver != "mysql" && cfg.DBDriver != "postgres" || cfg.DBDSN == "" || !maintenanceSourceDigest(reportSHA) || !maintenanceSourceDigest(sourceSHA) || limits.MaxBytes < 4096 || limits.MaxBytes > 1<<30 || limits.MaxEntries < 4 || limits.MaxEntries > 100000 {
		return result, invalid
	}
	if _, err := install.VerifyNativeStaged(ctx, staged, cfg.DBDriver, reportSHA, limits); err != nil {
		return result, invalid
	}
	prepared, err := install.PrepareNativeFiles(ctx, staged, workspace, cfg.DBDriver, reportSHA, limits, func(ctx context.Context, snapshot string, _ []byte) (json.RawMessage, error) {
		var err error
		result, err = importNativeSnapshot(ctx, cfg, snapshot, reportSHA, sourceSHA, limits)
		if err != nil {
			return nil, invalid
		}
		return json.RawMessage(`{"purpose":"guarded-native-import"}`), nil
	})
	if err != nil {
		return result, invalid
	}
	if prepared.Close() != nil {
		return result, errors.New("native import private staging cleanup failed; inspect selected target before retry")
	}
	return result, nil
}
