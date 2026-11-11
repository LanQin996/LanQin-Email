package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
)

// runCreateOnline invokes only the read-only collectors. In particular, it must
// never call app.New/LoadConfig, start workers, or expose service-control hooks.
func runCreateOnline(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("create-online", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	driver := flags.String("driver", "sqlite", "database driver: sqlite, postgres or mysql (native format is experimental)")
	database := flags.String("database", "", "live SQLite database file (must already use WAL)")
	dsnEnv := flags.String("dsn-env", "", "explicit environment variable containing a PostgreSQL/MySQL SOURCE DSN; never the DSN itself")
	dataDir := flags.String("data-dir", "", "configured live application data root")
	maildir := flags.String("maildir-root", "", "configured live Maildir root")
	workspace := flags.String("workspace", "", "existing private collection workspace outside the live roots")
	recipient := flags.String("recipient", "", "age X25519 public recipient")
	passwordFile := flags.String("password-file", "", "private backup password file instead of --recipient")
	output := flags.String("out", "", "new encrypted output file outside the collection roots")
	defaults := app.DefaultOnlineBackupOptions()
	maxBytes := flags.Int64("max-bytes", defaults.MaxBytes, "maximum total collected data size")
	maxEntries := flags.Int("max-entries", defaults.MaxEntries, "maximum collected entry count")
	maxMIMEBytes := flags.Int64("max-mime-bytes", defaults.MaxMIMEBytes, "maximum reconstructed draft MIME size")
	attempts := flags.Int("attempts", defaults.Attempts, "bounded collection attempts on concurrent source changes")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid create-online arguments; database credentials belong only in the explicitly selected environment variable")
	}
	databaseDriver := strings.ToLower(strings.TrimSpace(*driver))
	if flags.NArg() != 0 || *dataDir == "" || *maildir == "" || *workspace == "" || (*recipient == "") == (*passwordFile == "") || *output == "" {
		return errors.New("create-online requires --database, --data-dir, --maildir-root, --workspace, exactly one --recipient or --password-file, and --out")
	}
	if databaseDriver != "sqlite" && databaseDriver != "postgres" && databaseDriver != "mysql" {
		return errors.New("unsupported create-online database driver")
	}
	if databaseDriver == "sqlite" && (*database == "" || *dsnEnv != "") || databaseDriver != "sqlite" && (*database != "" || !nativeDSNEnvPattern.MatchString(*dsnEnv)) {
		return errors.New("SQLite requires --database without --dsn-env; PostgreSQL/MySQL require --dsn-env without --database")
	}
	var password []byte
	if *passwordFile != "" {
		var err error
		password, err = readBackupPasswordFile(*passwordFile)
		if err != nil {
			return err
		}
		defer clear(password)
		if !validNewBackupPassword(password) {
			return errors.New("new backup passwords must contain 8 to 128 Unicode characters, without control characters or only whitespace")
		}
	} else {
		if _, err := age.ParseX25519Recipient(*recipient); err != nil {
			return errors.New("invalid X25519 recipient")
		}
	}
	if *maxBytes < 4096 || *maxBytes > 1<<40 || *maxEntries < 4 || *maxEntries > 100000 || *maxMIMEBytes < 8192 || *maxMIMEBytes > 256<<20 || *attempts < 1 || *attempts > 3 {
		return errors.New("invalid create-online limits")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	canonicalOutput, err := onlineOutputPath(*output, *dataDir, *maildir)
	if err != nil {
		return err
	}
	opts := defaults
	opts.MaxBytes, opts.MaxEntries, opts.MaxMIMEBytes, opts.Attempts = *maxBytes, *maxEntries, *maxMIMEBytes, *attempts
	cfg := app.Config{DBDriver: databaseDriver, DBPath: *database, DataDir: *dataDir, MaildirRoot: *maildir}
	if databaseDriver != "sqlite" {
		value, ok := os.LookupEnv(*dsnEnv)
		if !ok || value == "" || len(value) > 16384 {
			return errors.New("native SOURCE DSN environment variable unavailable")
		}
		cfg.DBDSN = value
	}
	prepared, err := app.PrepareOnlineBackup(ctx, cfg, *workspace, opts)
	if err != nil {
		return err
	}
	defer prepared.Close()
	// The randomly-created collection root was not known at initial preflight.
	// Recheck it before publish creates even a temporary ciphertext output.
	canonicalOutput, err = onlineOutputPath(canonicalOutput, prepared.Path)
	if err != nil {
		return err
	}
	digest := sha256.New()
	if err := publish(canonicalOutput, func(file *os.File) error {
		var err error
		limits := backup.Limits{MaxBytes: *maxBytes, MaxEntries: *maxEntries}
		if *passwordFile != "" && databaseDriver == "sqlite" {
			_, err = backup.WriteOnlinePassword(ctx, prepared.Path, io.MultiWriter(file, digest), string(password), limits)
		} else if *passwordFile != "" {
			_, err = backup.WriteNativePassword(ctx, prepared.Path, io.MultiWriter(file, digest), string(password), databaseDriver, limits)
		} else if databaseDriver == "sqlite" {
			_, err = backup.WriteOnline(ctx, prepared.Path, io.MultiWriter(file, digest), *recipient, limits)
		} else {
			_, err = backup.WriteNative(ctx, prepared.Path, io.MultiWriter(file, digest), *recipient, databaseDriver, limits)
		}
		if err != nil {
			return err
		}
		// Plaintext cleanup is part of successful preparation, not an error
		// reported after publish has already linked the final ciphertext.
		if err := prepared.Close(); err != nil {
			return errors.New("cannot clean private plaintext backup workspace; encrypted output was not published")
		}
		return nil
	}); err != nil {
		return err
	}
	summary := prepared.Report.Summary
	_, err = fmt.Fprintf(stdout, "Encrypted online DATA snapshot created; no services were stopped.\nCaptured messages=%d drafts=%d attachments=%d unindexed=%d dkim_keys=%d attempts=%d external_cached_only=%d\nPostfix queue, deployment secrets and remote-only IMAP content are NOT included.\nExternal cached-only records include database/attachment data, not original upstream MIME.\nUnindexed delivery candidates remain quarantined for restore reconciliation.\nKeep this receipt separately trusted: sha256=%s\n", summary.Messages, summary.Drafts, summary.Attachments, summary.Unindexed, summary.DKIMKeys, summary.Attempts, summary.ExternalCachedMessagesOnly, hex.EncodeToString(digest.Sum(nil)))
	return err
}

var nativeDSNEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func onlineOutputPath(output string, sourceRoots ...string) (string, error) {
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("online backup output must be absent")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return "", errors.New("online backup output parent must exist")
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return "", errors.New("cannot resolve online backup output")
	}
	for _, sourceRoot := range sourceRoots {
		source, err := filepath.EvalSymlinks(sourceRoot)
		if err != nil {
			return "", errors.New("cannot resolve online backup source root")
		}
		source, err = filepath.Abs(source)
		if err != nil {
			return "", errors.New("cannot resolve online backup source root")
		}
		if relative, err := filepath.Rel(source, parent); err == nil && filepath.IsLocal(relative) {
			return "", errors.New("online backup output must be outside the collected source roots")
		}
	}
	return filepath.Join(parent, filepath.Base(output)), nil
}
