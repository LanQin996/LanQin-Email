package app

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"lanqin-email-api/internal/backup"
)

// ResolveWebRestore reads one authenticated completed Web task without App.New,
// migrations, workers, environment discovery or service control. The operator
// explicitly supplies the original metadata DB, configured recovery root, master
// key and job ID. The installer MUST still verify and prepare the actual files.
func ResolveWebRestore(ctx context.Context, database, recoveryRoot, masterKey, id string) (string, string, error) {
	return ResolveWebRestoreAt(ctx, database, recoveryRoot, "", masterKey, id)
}

// ResolveWebRestoreAt permits an explicit operator mapping from a recorded
// Linux container root to a private directory in the installer's namespace.
// The recorded root is only compared with the authenticated task; never opened.
func ResolveWebRestoreAt(ctx context.Context, database, recoveryRoot, recordedRoot, masterKey, id string) (string, string, error) {
	invalid := errors.New("verified web recovery task unavailable or configuration does not match")
	if !backupJobIDPattern.MatchString(id) || database == "" || recoveryRoot == "" {
		return "", "", invalid
	}
	database, err := filepath.Abs(database)
	if err != nil {
		return "", "", invalid
	}
	info, err := os.Lstat(database)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", invalid
	}
	name := filepath.ToSlash(database)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=ro&_pragma=trusted_schema(OFF)&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", "", invalid
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a := &App{db: db, cfg: Config{DBDriver: databaseDriverSQLite, DBPath: database, BackupRestoreDir: recoveryRoot, BackupSecretKey: masterKey}, backupRestoreRequests: make(chan backupRestoreExecution, 1)}
	root, err := a.backupRestoreRoot()
	if err != nil {
		return "", "", invalid
	}
	expectedRoot, err := recordedWebRestoreRoot(root, recordedRoot)
	if err != nil {
		return "", "", invalid
	}
	job, _, err := a.backupReadJob(ctx, id)
	if err != nil || job.Kind != "restore" || job.Status != "succeeded" || job.CancelRequested || job.Restore == nil || !job.Restore.Ready || job.Restore.Manifest == nil || job.Restore.Manifest.Scope != "online-data" || job.Restore.Root != expectedRoot || filepath.Base(job.Restore.Directory) != job.Restore.Directory || !strings.HasPrefix(job.Restore.Directory, id+"-") {
		return "", "", invalid
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return "", "", invalid
	}
	defer parent.Close()
	child, err := parent.Lstat(job.Restore.Directory)
	if err != nil || !child.IsDir() || child.Mode()&os.ModeSymlink != 0 {
		return "", "", invalid
	}
	container := filepath.Join(root, job.Restore.Directory, "recovered")
	resolved, err := filepath.EvalSymlinks(container)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(container) {
		return "", "", invalid
	}
	// The plaintext collector report is bound to the previously authenticated
	// recovery, rather than trusted merely because a READY filename exists.
	if err := backup.VerifyStagedOnlineReport(ctx, container, job.Restore.ReportSHA256); err != nil {
		return "", "", invalid
	}
	return container, job.Restore.ReportSHA256, nil
}

// ResolveWebNativeRestore authenticates a completed native Web recovery task
// from its original database without starting the application or migrations.
// The caller must still pass the returned container through native staged
// verification and installation preparation before switching any live roots.
func ResolveWebNativeRestore(ctx context.Context, driver, dsn, recoveryRoot, masterKey, id string) (string, string, error) {
	return ResolveWebNativeRestoreAt(ctx, driver, dsn, recoveryRoot, "", masterKey, id)
}

func ResolveWebNativeRestoreAt(ctx context.Context, driver, dsn, recoveryRoot, recordedRoot, masterKey, id string) (string, string, error) {
	recovery, err := resolveWebNativeRestore(ctx, driver, dsn, recoveryRoot, recordedRoot, masterKey, id)
	return recovery.container, recovery.reportSHA256, err
}

type nativeWebRecovery struct {
	container, reportSHA256, sourceSHA256 string
}

func resolveWebNativeRestore(ctx context.Context, driver, dsn, recoveryRoot, recordedRoot, masterKey, id string) (nativeWebRecovery, error) {
	invalid := errors.New("verified native Web recovery task unavailable or configuration does not match")
	if driver != databaseDriverMySQL && driver != databaseDriverPostgres || dsn == "" || recoveryRoot == "" || !backupJobIDPattern.MatchString(id) {
		return nativeWebRecovery{}, invalid
	}
	db, err := openNativeBackupDatabase(ctx, Config{DBDriver: driver, DBDSN: dsn})
	if err != nil {
		return nativeWebRecovery{}, invalid
	}
	defer db.Close()
	a := &App{db: db, cfg: Config{DBDriver: driver, BackupRestoreDir: recoveryRoot, BackupSecretKey: masterKey}, backupRestoreRequests: make(chan backupRestoreExecution, 1)}
	root, err := a.backupRestoreRoot()
	if err != nil {
		return nativeWebRecovery{}, invalid
	}
	expectedRoot, err := recordedWebRestoreRoot(root, recordedRoot)
	if err != nil {
		return nativeWebRecovery{}, invalid
	}
	job, _, err := a.backupReadJob(ctx, id)
	if err != nil || job.Kind != "restore" || job.Status != "succeeded" || job.CancelRequested || job.Restore == nil || !job.Restore.Ready || job.Restore.Manifest == nil || job.Restore.Manifest.Scope != "online-native-data" || job.Restore.Manifest.Driver != driver || job.Restore.Root != expectedRoot || filepath.Base(job.Restore.Directory) != job.Restore.Directory || !strings.HasPrefix(job.Restore.Directory, id+"-") {
		return nativeWebRecovery{}, invalid
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return nativeWebRecovery{}, invalid
	}
	defer parent.Close()
	child, err := parent.Lstat(job.Restore.Directory)
	if err != nil || !child.IsDir() || child.Mode()&os.ModeSymlink != 0 {
		return nativeWebRecovery{}, invalid
	}
	container := filepath.Join(root, job.Restore.Directory, "recovered")
	resolved, err := filepath.EvalSymlinks(container)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(container) {
		return nativeWebRecovery{}, invalid
	}
	if err := backup.VerifyStagedOnlineReport(ctx, container, job.Restore.ReportSHA256, driver); err != nil {
		return nativeWebRecovery{}, invalid
	}
	return nativeWebRecovery{container: container, reportSHA256: job.Restore.ReportSHA256, sourceSHA256: job.Artifact.SHA256}, nil
}

func recordedWebRestoreRoot(actual, recorded string) (string, error) {
	if recorded == "" {
		return actual, nil
	}
	if !nativeRuntimeRoot(recorded) {
		return "", errors.New("invalid recorded Web recovery root")
	}
	return recorded, nil
}
