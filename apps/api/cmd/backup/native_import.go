package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
)

func runImportNative(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import-native", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	driver := flags.String("driver", "", "target database driver, exactly mysql or postgres")
	dsnEnv := flags.String("target-dsn-env", "", "explicit environment variable with an EMPTY target database DSN")
	input := flags.String("in", "", "encrypted native backup archive")
	identityFile := flags.String("identity-file", "", "offline age recovery identity file, never key value")
	passwordFile := flags.String("password-file", "", "private backup password file instead of --identity-file")
	expected := flags.String("expected-sha256", "", "separately trusted ciphertext SHA-256")
	workspace := flags.String("workspace", "", "existing private temporary plaintext workspace")
	webJob := flags.String("web-job", "", "completed authenticated native Web recovery task, instead of archive/key/hash")
	webStateDSNEnv := flags.String("web-state-dsn-env", "", "explicit environment reference for original Web task database")
	webRoot := flags.String("web-restore-root", "", "original private Web recovery workspace root")
	webRecordedRoot := flags.String("web-recorded-root", "", "optional original container root recorded by Web task, when --web-restore-root is an explicit host mapping")
	confirmed := flags.Bool("confirm-empty-target", false, "acknowledge this selected target database is empty and not live; import will NOT activate it")
	limits := backup.DefaultLimits()
	flags.Int64Var(&limits.MaxBytes, "max-bytes", limits.MaxBytes, "maximum plaintext bytes")
	flags.IntVar(&limits.MaxEntries, "max-entries", limits.MaxEntries, "maximum staged file entries")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*driver != "mysql" && *driver != "postgres") || !nativeDSNEnvPattern.MatchString(*dsnEnv) || *workspace == "" || !*confirmed || limits.MaxBytes < 4096 || limits.MaxBytes > 1<<40 || limits.MaxEntries < 4 || limits.MaxEntries > 100000 {
		return errors.New("import-native requires --driver, --target-dsn-env, --in, exactly one --identity-file or --password-file, --expected-sha256, --workspace and --confirm-empty-target; use a separate EMPTY non-live database")
	}
	web := *webJob != "" || *webStateDSNEnv != "" || *webRoot != "" || *webRecordedRoot != ""
	if web && (*webJob == "" || !nativeDSNEnvPattern.MatchString(*webStateDSNEnv) || *webStateDSNEnv == *dsnEnv || *webRoot == "" || *input != "" || *identityFile != "" || *passwordFile != "" || *expected != "") || !web && (*input == "" || (*identityFile == "") == (*passwordFile == "") || *expected == "") {
		return errors.New("native import requires either archive/hash and exactly one private identity or password file, or --web-job, --web-state-dsn-env and --web-restore-root; Web state and empty target DSN references must differ")
	}
	value, ok := os.LookupEnv(*dsnEnv)
	if !ok || value == "" || len(value) > 16384 {
		return errors.New("native TARGET DSN environment variable unavailable")
	}
	if web {
		stateDSN, ok := os.LookupEnv(*webStateDSNEnv)
		if !ok || stateDSN == "" || len(stateDSN) > 16384 || stateDSN == value {
			return errors.New("native Web task DSN unavailable or not separate from the empty target")
		}
		result, err := app.ImportWebNativeBackupAt(ctx, app.Config{DBDriver: *driver, DBDSN: value}, stateDSN, *webRoot, *webRecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), *webJob, *workspace, limits)
		return writeNativeImportReceipt(stdout, result, err)
	}
	if *passwordFile != "" {
		password, err := readBackupPasswordFile(*passwordFile)
		if err != nil {
			return err
		}
		defer clear(password)
		result, err := app.ImportNativeBackupPassword(ctx, app.Config{DBDriver: *driver, DBDSN: value}, *input, string(password), *expected, *workspace, limits)
		return writeNativeImportReceipt(stdout, result, err)
	}
	info, err := os.Lstat(*identityFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return errors.New("cannot read recovery identity file")
	}
	file, err := os.Open(*identityFile)
	if err != nil {
		return errors.New("cannot open recovery identity file")
	}
	key, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	defer clear(key)
	if readErr != nil || closeErr != nil || len(key) > 4096 {
		return errors.New("cannot read recovery identity file")
	}
	result, err := app.ImportNativeBackup(ctx, app.Config{DBDriver: *driver, DBDSN: value}, *input, strings.TrimSpace(string(key)), *expected, *workspace, limits)
	return writeNativeImportReceipt(stdout, result, err)
}

func writeNativeImportReceipt(stdout io.Writer, result app.NativeImportResult, err error) error {
	if result.Rows > 0 {
		if outputErr := json.NewEncoder(stdout).Encode(result); outputErr != nil {
			return errors.New("cannot write native import receipt; inspect the selected target before retrying")
		}
	}
	return err
}
