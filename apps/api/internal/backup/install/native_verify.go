package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/nativedb"
	"lanqin-email-api/internal/backup/online"
)

// VerifyNativeStaged verifies every actual staged file against the previously
// authenticated report. It is not a database import, path remap or activation.
func VerifyNativeStaged(ctx context.Context, container, driver, expectedReportSHA256 string, limits backup.Limits) (nativedb.Header, error) {
	var empty nativedb.Header
	if driver != "mysql" && driver != "postgres" || limits.MaxBytes < 1 || limits.MaxEntries < 1 {
		return empty, errInvalid
	}
	path, err := materializeDirectory(container)
	if err != nil {
		return empty, errInvalid
	}
	info, err := os.Stat(path)
	if err != nil || !materializePrivateInfo(info) || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return empty, errInvalid
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return empty, errInvalid
	}
	defer root.Close()
	ready, err := root.Lstat("READY")
	if err != nil || !ready.Mode().IsRegular() || ready.Size() < 1 || ready.Size() > 4096 {
		return empty, errInvalid
	}
	if backup.VerifyStagedOnlineReport(ctx, path, expectedReportSHA256, driver) != nil {
		return empty, errInvalid
	}
	snapshotInfo, err := root.Lstat("snapshot")
	if err != nil || !snapshotInfo.IsDir() || snapshotInfo.Mode()&os.ModeSymlink != 0 {
		return empty, errInvalid
	}
	source, err := root.OpenRoot("snapshot")
	if err != nil {
		return empty, errInvalid
	}
	defer source.Close()
	files, _, err := inventory(ctx, source, limits)
	if err != nil {
		return empty, err
	}
	if _, ok := files["data/lanqin.db"]; ok {
		return empty, errInvalid
	}
	reportInfo := files[reportName]
	if reportInfo == nil || reportInfo.Size() > 64<<20 {
		return empty, errInvalid
	}
	content, err := source.ReadFile(reportName)
	if err != nil || len(content) > 64<<20 {
		return empty, errInvalid
	}
	digest := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expectedReportSHA256) {
		return empty, errInvalid
	}
	copied := make([]online.CollectedFile, 0, len(files))
	for name, observed := range files {
		file, err := source.Open(name)
		if err != nil {
			return empty, errInvalid
		}
		opened, err := file.Stat()
		if err != nil || !os.SameFile(observed, opened) {
			file.Close()
			return empty, errInvalid
		}
		hash := sha256.New()
		n, copyErr := io.Copy(hash, contextReader{ctx, io.LimitReader(file, observed.Size()+1)})
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || n != observed.Size() {
			return empty, errInvalid
		}
		item := online.CollectedFile{Destination: name, Bytes: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
		if verifyFile(ctx, source, item, observed) != nil {
			return empty, errInvalid
		}
		copied = append(copied, item)
	}
	if _, _, err := validateReport(content, copied, driver); err != nil {
		return empty, err
	}
	dbFile, err := source.Open(filepath.FromSlash(nativedb.Filename))
	if err != nil {
		return empty, errInvalid
	}
	defer dbFile.Close()
	nativeLimits := nativedb.DefaultLimits()
	nativeLimits.MaxBytes = limits.MaxBytes
	header, _, err := nativedb.Read(ctx, dbFile, nativeLimits, nil, nil)
	if err != nil || header.Driver != driver {
		return empty, errInvalid
	}
	final, _, err := inventory(ctx, source, limits)
	if err != nil || len(final) != len(files) {
		return empty, errInvalid
	}
	for name, before := range files {
		after := final[name]
		if after == nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return empty, errInvalid
		}
	}
	return header, nil
}
