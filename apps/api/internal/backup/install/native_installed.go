package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/nativedb"
	"lanqin-email-api/internal/backup/online"
)

func VerifyNativeInstalled(ctx context.Context, journal string, targets Targets, driver, reportSHA256, planSHA256 string, limits backup.Limits) (JournalInfo, error) {
	if driver != "mysql" && driver != "postgres" || limits.MaxBytes < 1 || limits.MaxEntries < 1 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	return verifyInstalledPayload(ctx, journal, targets, driver, reportSHA256, planSHA256, "", limits)
}

func verifyInstalledPayload(ctx context.Context, journal string, targets Targets, driver, reportSHA256, planSHA256, databaseSHA256 string, limits backup.Limits) (JournalInfo, error) {
	info, err := VerifyCommitted(ctx, journal, targets)
	if err != nil {
		return JournalInfo{}, err
	}
	if _, err := VerifyPlanTransaction(ctx, journal, targets, planSHA256); err != nil {
		return JournalInfo{}, err
	}
	dataRoot, err := os.OpenRoot(targets.Data)
	if err != nil {
		return JournalInfo{}, err
	}
	defer dataRoot.Close()
	stat, err := dataRoot.Lstat("restore-source-report.json")
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64<<20 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	content, err := dataRoot.ReadFile("restore-source-report.json")
	if err != nil {
		return JournalInfo{}, err
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != reportSHA256 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	var report collectionReport
	version := 2
	databaseName := nativedb.Filename
	if driver == "sqlite" {
		version = 1
		databaseName = "data/lanqin.db"
	}
	if json.Unmarshal(content, &report) != nil || report.Version != version || report.Scope != driver+"-reference-closure" || !report.Complete {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	expected := map[string]online.CollectedFile{}
	dates := map[string]time.Time{}
	databases := 0
	for _, item := range report.Files {
		if item.Destination == databaseName {
			if item.Kind != "database" {
				return JournalInfo{}, ErrSwitchUnsafe
			}
			databases++
			continue
		}
		if !allowed(item.Destination) || item.Bytes < 0 || item.Destination == sourceReportName || item.Destination == "data/restore-install-plan.json" {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		if _, exists := expected[item.Destination]; exists {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		expected[item.Destination] = item.CollectedFile
		if item.InternalDate != nil {
			dates[item.Destination] = *item.InternalDate
		}
	}
	if databases != 1 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	if driver == "sqlite" {
		decoded, err := hex.DecodeString(databaseSHA256)
		if err != nil || len(decoded) != sha256.Size {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		stat, err := dataRoot.Lstat("lanqin.db")
		if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > limits.MaxBytes {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		expected[databaseName] = online.CollectedFile{Destination: databaseName, Bytes: stat.Size(), SHA256: databaseSHA256}
	}
	expected[sourceReportName] = online.CollectedFile{Destination: sourceReportName, Bytes: int64(len(content)), SHA256: reportSHA256}
	plan, err := dataRoot.ReadFile("restore-install-plan.json")
	if err != nil || len(plan) > 32<<10 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	expected["data/restore-install-plan.json"] = online.CollectedFile{Destination: "data/restore-install-plan.json", Bytes: int64(len(plan)), SHA256: planSHA256}
	entries := 0
	remaining := limits.MaxBytes
	for i, directory := range []string{targets.Data, targets.Mail, targets.DKIM} {
		root, err := os.OpenRoot(directory)
		if err != nil {
			return JournalInfo{}, err
		}
		err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			entries++
			if entries > limits.MaxEntries {
				return ErrSwitchUnsafe
			}
			if name == "." {
				return nil
			}
			stat, err := root.Lstat(name)
			if err != nil || stat.Mode()&os.ModeSymlink != 0 {
				return ErrSwitchUnsafe
			}
			full := []string{"data", "mail", "dkim"}[i] + "/" + filepath.ToSlash(name)
			if !allowed(full) {
				return ErrSwitchUnsafe
			}
			if stat.IsDir() {
				return nil
			}
			if !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > remaining {
				return ErrSwitchUnsafe
			}
			remaining -= stat.Size()
			if name == ".lanqin-restore-"+info.TransactionID {
				return nil
			}
			want, ok := expected[full]
			if !ok || want.Bytes != stat.Size() {
				return ErrSwitchUnsafe
			}
			if date, ok := dates[full]; ok && !stat.ModTime().Equal(date) {
				return ErrSwitchUnsafe
			}
			want.Destination = filepath.ToSlash(name)
			if err := verifyFile(ctx, root, want, stat); err != nil {
				return err
			}
			delete(expected, full)
			return nil
		})
		root.Close()
		if err != nil {
			return JournalInfo{}, err
		}
	}
	if len(expected) != 0 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	// Recheck transaction identity after the potentially long payload readback.
	if _, err := VerifyCommitted(ctx, journal, targets); err != nil {
		return JournalInfo{}, err
	}
	return info, nil
}
