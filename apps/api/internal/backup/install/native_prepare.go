package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/nativedb"
	"lanqin-email-api/internal/backup/online"
)

// NativeDatabasePreparation is called only after the copied payload and report
// have been reverified. It may prepare an explicitly selected guarded NEW
// database; it must never activate it or modify the original live database.
type NativeDatabasePreparation func(context.Context, string, []byte) (json.RawMessage, error)

func PrepareNativeFiles(ctx context.Context, container, workspace, driver, reportSHA256 string, limits backup.Limits, prepareDatabase NativeDatabasePreparation) (_ *Prepared, result error) {
	if prepareDatabase == nil {
		return nil, errInvalid
	}
	if _, err := VerifyNativeStaged(ctx, container, driver, reportSHA256, limits); err != nil {
		return nil, err
	}
	sourcePath, err := materializeDirectory(container)
	if err != nil {
		return nil, errInvalid
	}
	work, err := materializeDirectory(workspace)
	if err != nil || within(sourcePath, work) || within(work, sourcePath) {
		return nil, errInvalid
	}
	info, err := os.Stat(work)
	if err != nil || !materializePrivateInfo(info) || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errInvalid
	}
	directory, err := os.MkdirTemp(work, ".lanqin-native-install-")
	if err != nil {
		return nil, errInvalid
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, errInvalid
	}
	owned, statErr := handle.Stat()
	closeErr := handle.Close()
	if statErr != nil || closeErr != nil {
		return nil, errInvalid
	}
	cleanup := func() error {
		current, err := os.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(owned, current) {
			return errInvalid
		}
		return os.RemoveAll(directory)
	}
	defer func() {
		if result != nil {
			if err := cleanup(); err != nil {
				result = errors.New("cannot clean failed native private installation preparation")
			}
		}
	}()
	copyContainer := filepath.Join(directory, "recovered")
	copySnapshot := filepath.Join(copyContainer, "snapshot")
	if os.Mkdir(copyContainer, 0o700) != nil || os.Mkdir(copySnapshot, 0o700) != nil {
		return nil, errInvalid
	}
	source, err := os.OpenRoot(filepath.Join(sourcePath, "snapshot"))
	if err != nil {
		return nil, errInvalid
	}
	defer source.Close()
	output, err := os.OpenRoot(copySnapshot)
	if err != nil {
		return nil, errInvalid
	}
	defer output.Close()
	files, dirs, err := inventory(ctx, source, limits)
	if err != nil {
		return nil, err
	}
	if len(files)+len(dirs) > limits.MaxEntries-4 {
		return nil, errInvalid
	}
	for _, name := range dirs {
		if err := output.MkdirAll(name, 0o700); err != nil {
			return nil, errInvalid
		}
	}
	mappings := make([]online.FileMapping, 0, len(files))
	for name, info := range files {
		mappings = append(mappings, online.FileMapping{Source: name, Destination: name, ExpectedInfo: info})
	}
	if _, err := online.CopyFiles(ctx, source, output, mappings, online.FileLimits{MaxBytes: limits.MaxBytes, MaxEntries: limits.MaxEntries}); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(copyContainer, "READY"), []byte("Copied authenticated native recovery; NOT activated.\n"), 0o600); err != nil {
		return nil, errInvalid
	}
	if _, err := VerifyNativeStaged(ctx, copyContainer, driver, reportSHA256, limits); err != nil {
		return nil, err
	}
	content, err := output.ReadFile(reportName)
	if err != nil || len(content) > 64<<20 {
		return nil, errInvalid
	}
	digest := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), reportSHA256) {
		return nil, errInvalid
	}
	var report collectionReport
	if json.Unmarshal(content, &report) != nil {
		return nil, errInvalid
	}
	for _, item := range report.Files {
		if item.InternalDate != nil {
			if err := output.Chtimes(item.Destination, *item.InternalDate, *item.InternalDate); err != nil {
				return nil, errInvalid
			}
		}
	}
	plan, err := prepareDatabase(ctx, copySnapshot, content)
	if err != nil {
		return nil, err
	}
	if len(plan) < 2 || len(plan) > 32<<10 || !json.Valid(plan) {
		return nil, errInvalid
	}
	// The typed dump is a recovery input, not a live database or necessary copy
	// of old sessions/queues to ship with the installed application data tree.
	if err := output.Remove(nativedb.Filename); err != nil {
		return nil, errInvalid
	}
	if err := writePreparedExclusive(output, sourceReportName, nil); err != nil {
		return nil, err
	}
	if err := output.Rename(reportName, sourceReportName); err != nil {
		return nil, errInvalid
	}
	if err := writePreparedExclusive(output, preparedMarkerName, []byte(preparedMarkerContent)); err != nil {
		return nil, err
	}
	if err := writePreparedExclusive(output, "data/restore-install-plan.json", plan); err != nil {
		return nil, err
	}
	final, finalDirs, err := inventory(ctx, output, limits)
	if err != nil || len(final)+len(finalDirs) >= limits.MaxEntries {
		return nil, errInvalid
	}
	remaining := limits.MaxBytes - int64(len(plan))
	if remaining < 0 {
		return nil, errInvalid
	}
	for _, file := range final {
		if file.Size() > remaining {
			return nil, errInvalid
		}
		remaining -= file.Size()
	}
	if err := writePreparedExclusive(output, "INSTALL-PLAN.json", plan); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &Prepared{Path: copySnapshot, close: cleanup}, nil
}
