package install

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"lanqin-email-api/internal/backup"
)

// VerifySQLiteInstalled is an offline, read-only post-switch check. The caller
// must have stopped every deployment writer and retain the independently
// recorded plan digest; this function never starts App.New or migrates data.
func VerifySQLiteInstalled(ctx context.Context, journal string, targets Targets, planSHA256, sourceReportSHA256, databaseSHA256 string) error {
	if err := verifySQLiteCurrent(ctx, journal, targets, planSHA256, false); err != nil {
		return err
	}
	_, err := verifyInstalledPayload(ctx, journal, targets, "sqlite", sourceReportSHA256, planSHA256, databaseSHA256, backup.DefaultLimits())
	return err
}

// VerifySQLiteRuntime is for an already committed deployment that may have
// accepted new mail. It verifies current integrity and immutable transaction/
// plan identity, not equality with the old archive or replay of its rows.
func VerifySQLiteRuntime(ctx context.Context, journal string, targets Targets, planSHA256 string) error {
	return verifySQLiteCurrent(ctx, journal, targets, planSHA256, true)
}

func verifySQLiteCurrent(ctx context.Context, journal string, targets Targets, planSHA256 string, committed bool) error {
	if _, err := VerifyCommitted(ctx, journal, targets); err != nil {
		return err
	}
	if _, err := VerifyPlanTransaction(ctx, journal, targets, planSHA256); err != nil {
		return err
	}
	database := filepath.Join(targets.Data, "lanqin.db")
	before, err := pinnedInfo(database, false)
	if err != nil || !before.Mode().IsRegular() {
		return ErrSwitchUnsafe
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if !committed && requireAbsent(database+suffix) != nil {
			return ErrSwitchUnsafe
		}
		if committed {
			if stat, err := os.Lstat(database + suffix); err == nil && !stat.Mode().IsRegular() || err != nil && !os.IsNotExist(err) {
				return ErrSwitchUnsafe
			}
		}
	}
	name := filepath.ToSlash(database)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=ro&_pragma=trusted_schema(OFF)&_pragma=busy_timeout(100)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return ErrSwitchUnsafe
	}
	db.SetMaxOpenConns(1)
	checkErr := checkDatabase(ctx, db)
	if checkErr == nil {
		checkErr = checkTriggers(ctx, db)
	}
	closeErr := db.Close()
	if checkErr != nil || closeErr != nil {
		return ErrSwitchUnsafe
	}
	after, err := pinnedInfo(database, false)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ErrSwitchUnsafe
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if !committed && requireAbsent(database+suffix) != nil {
			return ErrSwitchUnsafe
		}
	}
	if _, err := VerifyCommitted(ctx, journal, targets); err != nil {
		return err
	}
	_, err = VerifyPlanTransaction(ctx, journal, targets, planSHA256)
	return err
}
