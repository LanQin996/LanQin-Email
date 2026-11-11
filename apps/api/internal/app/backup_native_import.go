package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

type NativeImportResult struct {
	Driver       string `json:"driver"`
	Rows         int64  `json:"rows"`
	SourceSHA256 string `json:"sourceSHA256"`
	ReportSHA256 string `json:"reportSHA256"`
	Activated    bool   `json:"activated"`
}

// ImportNativeBackup decrypts and validates a trusted archive into a private
// temporary tree, then imports rows into an explicitly selected EMPTY target.
// It leaves an activation guard in that database. It never starts services or
// changes a deployment DSN, Maildir, attachment roots or any existing database.
func ImportNativeBackup(ctx context.Context, cfg Config, input, identity, expectedSHA256, workspace string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	return importNativeBackup(ctx, cfg, input, identity, expectedSHA256, workspace, limits, false)
}

// ImportNativeBackupPassword retains the same private staging, same-engine
// verification, empty-target and activation guards as private-key recovery.
func ImportNativeBackupPassword(ctx context.Context, cfg Config, input, password, expectedSHA256, workspace string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	return importNativeBackup(ctx, cfg, input, password, expectedSHA256, workspace, limits, true)
}

func importNativeBackup(ctx context.Context, cfg Config, input, secret, expectedSHA256, workspace string, limits backup.Limits, passwordMode bool) (result NativeImportResult, resultErr error) {
	cfg = normalizeDatabaseConfig(cfg)
	invalid := errors.New("native import requires a verified same-engine archive, private workspace and empty target database")
	want, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(want) != sha256.Size || cfg.DBDriver != "mysql" && cfg.DBDriver != "postgres" || limits.MaxBytes < 4096 || limits.MaxBytes > 1<<40 || limits.MaxEntries < 4 || limits.MaxEntries > 100000 {
		return result, invalid
	}
	work, err := resolvedBackupDirectory(workspace)
	if err != nil {
		return result, invalid
	}
	info, err := os.Stat(work)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return result, invalid
	}
	inputInfo, err := os.Lstat(input)
	if err != nil || !inputInfo.Mode().IsRegular() || inputInfo.Size() > 2*limits.MaxBytes {
		return result, invalid
	}
	file, err := os.Open(input)
	if err != nil {
		return result, invalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(inputInfo, opened) {
		return result, invalid
	}
	directory, err := os.MkdirTemp(work, ".lanqin-native-import-")
	if err != nil {
		return result, invalid
	}
	ownedDirectory, err := os.Open(directory)
	if err != nil {
		return result, invalid
	}
	owned, err := ownedDirectory.Stat()
	closeDirectoryErr := ownedDirectory.Close()
	if err != nil || closeDirectoryErr != nil {
		return result, invalid
	}
	defer func() {
		current, err := os.Lstat(directory)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(owned, current) || os.RemoveAll(directory) != nil {
			resultErr = errors.New("native import private staging cleanup failed; inspect the selected empty target before retrying")
		}
	}()
	container := filepath.Join(directory, "recovered")
	var manifest backup.Manifest
	if passwordMode {
		manifest, err = backup.RestorePassword(ctx, file, container, secret, expectedSHA256, limits)
	} else {
		manifest, err = backup.Restore(ctx, file, container, strings.TrimSpace(secret), expectedSHA256, limits)
	}
	if err != nil || manifest.Version != 2 || manifest.Scope != "online-native-data" || manifest.Driver != cfg.DBDriver {
		return result, invalid
	}
	reportSHA, err := backup.StagedOnlineReportSHA256(ctx, container, cfg.DBDriver)
	if err != nil {
		return result, invalid
	}
	if _, err := install.VerifyNativeStaged(ctx, container, cfg.DBDriver, reportSHA, limits); err != nil {
		return result, invalid
	}
	return importNativeSnapshot(ctx, cfg, filepath.Join(container, "snapshot"), reportSHA, strings.ToLower(expectedSHA256), limits)
}

// ImportWebNativeBackup uses the original authenticated Web task as authority,
// verifies a private independent copy before touching the new empty target,
// and leaves the database guarded. No private recovery key is needed again.
func ImportWebNativeBackup(ctx context.Context, cfg Config, stateDSN, recoveryRoot, masterKey, id, workspace string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	return ImportWebNativeBackupAt(ctx, cfg, stateDSN, recoveryRoot, "", masterKey, id, workspace, limits)
}

func ImportWebNativeBackupAt(ctx context.Context, cfg Config, stateDSN, recoveryRoot, recordedRoot, masterKey, id, workspace string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	cfg = normalizeDatabaseConfig(cfg)
	invalid := errors.New("native Web import requires an authenticated task, separate empty target and private workspace")
	if stateDSN == "" || cfg.DBDSN == "" || stateDSN == cfg.DBDSN || limits.MaxBytes < 4096 || limits.MaxBytes > 1<<40 || limits.MaxEntries < 4 || limits.MaxEntries > 100000 {
		return result, invalid
	}
	recovery, err := resolveWebNativeRestore(ctx, cfg.DBDriver, stateDSN, recoveryRoot, recordedRoot, masterKey, id)
	if err != nil {
		return result, invalid
	}
	prepared, err := install.PrepareNativeFiles(ctx, recovery.container, workspace, cfg.DBDriver, recovery.reportSHA256, limits, func(ctx context.Context, snapshot string, _ []byte) (json.RawMessage, error) {
		var err error
		result, err = importNativeSnapshot(ctx, cfg, snapshot, recovery.reportSHA256, recovery.sourceSHA256, limits)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"purpose":"guarded-native-import"}`), nil
	})
	if err != nil {
		return result, invalid
	}
	if err := prepared.Close(); err != nil {
		return result, errors.New("native Web import private copy cleanup failed; inspect selected target before retry")
	}
	return result, nil
}

func importNativeSnapshot(ctx context.Context, cfg Config, snapshot, reportSHA, sourceSHA string, limits backup.Limits) (result NativeImportResult, resultErr error) {
	invalid := errors.New("native import snapshot validation failed; inspect selected target before retry")
	content, err := nativeFileBytes(filepath.Join(snapshot, "data", "backup-online.json"), 64<<20)
	if err != nil {
		return result, invalid
	}
	actualReport := sha256.Sum256(content)
	if hex.EncodeToString(actualReport[:]) != reportSHA {
		return result, invalid
	}
	var report OnlineBackupReport
	if json.Unmarshal(content, &report) != nil {
		return result, invalid
	}
	databaseSHA := ""
	for _, file := range report.Files {
		if file.Kind == "database" && file.Destination == "data/lanqin-native.jsonl" {
			databaseSHA = file.SHA256
		}
	}
	if len(databaseSHA) != 64 {
		return result, invalid
	}
	_, rows, err := restoreNativeDatabaseRows(ctx, cfg, filepath.Join(snapshot, "data", "lanqin-native.jsonl"), limits.MaxBytes, databaseSHA)
	if err != nil {
		return result, invalid
	}
	return NativeImportResult{Driver: cfg.DBDriver, Rows: rows, SourceSHA256: sourceSHA, ReportSHA256: reportSHA, Activated: false}, nil
}
