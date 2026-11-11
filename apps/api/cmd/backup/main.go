// backup exports encrypted data snapshots and isolated recovery evidence.
// Service coordination requires an explicit pinned maintenance plan. Cloud
// uploads are opt-in and use per-destination operator credential references.
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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "backup:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: backup keygen|create|create-online|restore|import-native|install-native|activate-native|rollback-native|verify-native|maintenance-host|maintenance-host-resolve|maintain-plan|maintain|maintain-status|maintain-recover|preservation-verify|preservation-restore|install|rollback|upload|fetch|discover|publish-index|publish (see docs/backup.md)")
	}
	if args[0] == "maintenance-host" {
		return runMaintenanceHost(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "maintenance-host-resolve" {
		return runMaintenanceHostMode(ctx, args[0], args[1:], stdout, stderr)
	}
	if args[0] == "maintain" || args[0] == "maintain-plan" || args[0] == "maintain-status" || args[0] == "maintain-recover" {
		return runMaintenance(ctx, args[0], args[1:], stdout, stderr)
	}
	if args[0] == "preservation-verify" || args[0] == "preservation-restore" {
		return runPreservation(ctx, args[0], args[1:], stdout, stderr)
	}
	if args[0] == "install" {
		return runInstall(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "import-native" {
		return runImportNative(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "verify-native" {
		return runVerifyNative(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "install-native" || args[0] == "activate-native" || args[0] == "rollback-native" {
		return runNativeMaintenance(ctx, args[0], args[1:], stdout, stderr)
	}
	if args[0] == "rollback" {
		return runRollback(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "upload" {
		return runUpload(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "fetch" {
		return runFetch(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "discover" {
		return runDiscover(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "publish-index" {
		return runPublishIndex(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "publish" {
		return runPublish(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "create-online" {
		return runCreateOnline(ctx, args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	keyFile := flags.String("identity-file", "", "private recovery key file (never a command-line key value)")
	passwordFile := flags.String("password-file", "", "private backup password file, instead of a recipient or recovery identity")
	snapshot := flags.String("snapshot", "", "prepared offline SQLite snapshot containing data, mail, dkim")
	recipient := flags.String("recipient", "", "age X25519 public recipient")
	output := flags.String("out", "", "new output file or new restore container directory")
	input := flags.String("in", "", "encrypted backup file")
	expectedHash := flags.String("expected-sha256", "", "ciphertext SHA-256 from a separately trusted receipt")
	offline := flags.Bool("confirm-offline-snapshot", false, "acknowledge ALL writers were stopped before preparing the snapshot")
	defaults := backup.DefaultLimits()
	maxBytes := flags.Int64("max-bytes", defaults.MaxBytes, "maximum total payload size")
	maxEntries := flags.Int("max-entries", defaults.MaxEntries, "maximum payload entry count")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New("invalid backup arguments; use documented flags and private credential files, never password values")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	limits := backup.Limits{MaxBytes: *maxBytes, MaxEntries: *maxEntries}
	switch args[0] {
	case "keygen":
		if *keyFile == "" || *passwordFile != "" {
			return errors.New("--identity-file is required")
		}
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			return errors.New("cannot generate recovery key")
		}
		if err := publish(*keyFile, func(file *os.File) error {
			_, err := fmt.Fprintln(file, identity.String())
			return err
		}); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, identity.Recipient().String())
		return err
	case "create":
		if !*offline || *snapshot == "" || *output == "" || (*recipient == "") == (*passwordFile == "") || *keyFile != "" {
			return errors.New("create requires --snapshot, exactly one --recipient or --password-file, --out and --confirm-offline-snapshot")
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
		}
		// Do not create a temporary output inside a source that WalkDir will read.
		source, err := filepath.EvalSymlinks(*snapshot)
		if err != nil {
			return errors.New("cannot resolve snapshot")
		}
		source, err = filepath.Abs(source)
		if err != nil {
			return err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(*output))
		if err != nil {
			return errors.New("output parent must exist")
		}
		parent, err = filepath.Abs(parent)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, parent)
		if err == nil && filepath.IsLocal(relative) {
			return errors.New("backup output must be outside the source snapshot")
		}
		digest := sha256.New()
		if err := publish(*output, func(file *os.File) error {
			var err error
			if *passwordFile != "" {
				_, err = backup.WritePassword(ctx, source, io.MultiWriter(file, digest), string(password), limits)
			} else {
				_, err = backup.Write(ctx, source, io.MultiWriter(file, digest), *recipient, limits)
			}
			return err
		}); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Encrypted offline DATA snapshot created; deployment secrets and Postfix queue are NOT included.\nKeep this receipt separately trusted: sha256="+hex.EncodeToString(digest.Sum(nil)))
		return err
	case "restore":
		if (*keyFile == "") == (*passwordFile == "") || *recipient != "" || *input == "" || *output == "" || *expectedHash == "" {
			return errors.New("restore requires exactly one --identity-file or --password-file, --in, --out and --expected-sha256 from a trusted receipt")
		}
		want, err := hex.DecodeString(*expectedHash)
		if err != nil || len(want) != sha256.Size {
			return errors.New("invalid expected SHA-256")
		}
		var key []byte
		if *passwordFile != "" {
			key, err = readBackupPasswordFile(*passwordFile)
			if err != nil {
				return err
			}
		} else {
			identityFile, err := os.Open(*keyFile)
			if err != nil {
				return errors.New("cannot open recovery key file")
			}
			var readErr error
			key, readErr = io.ReadAll(io.LimitReader(identityFile, 4097))
			closeErr := identityFile.Close()
			if readErr != nil || closeErr != nil || len(key) > 4096 {
				clear(key)
				return errors.New("cannot read recovery key file")
			}
		}
		defer clear(key)
		archive, err := os.Open(*input)
		if err != nil {
			return errors.New("cannot open backup file")
		}
		defer archive.Close()
		var manifest backup.Manifest
		if *passwordFile != "" {
			manifest, err = backup.RestorePassword(ctx, archive, *output, string(key), *expectedHash, limits)
		} else {
			manifest, err = backup.Restore(ctx, archive, *output, strings.TrimSpace(string(key)), *expectedHash, limits)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Verified data staged under out/snapshot; READY is present. No live services or ownership were changed.")
		if err == nil && (manifest.Scope == "online-native-data" || manifest.Scope == "online-data") {
			reportSHA, reportErr := backup.StagedOnlineReportSHA256(ctx, *output, manifest.Driver)
			if reportErr != nil {
				return errors.New("cannot obtain authenticated installation report receipt")
			}
			_, err = fmt.Fprintln(stdout, "Keep this installation report receipt separately trusted: report-sha256="+reportSHA)
		}
		return err
	default:
		return errors.New("unknown backup command")
	}
}

// publish uses a same-filesystem hard link as an atomic no-replace publication.
// Unlike Rename, Link cannot overwrite an existing file on Unix. Unsupported
// filesystems fail rather than silently falling back to an unsafe replacement.
func publish(destination string, write func(*os.File) error) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output already exists or cannot be inspected")
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".lanqin-backup-*")
	if err != nil {
		return errors.New("cannot create private output file")
	}
	name := file.Name()
	defer os.Remove(name) // Only the newly-created temporary file; never a directory.
	defer file.Close()
	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(name, destination); err != nil {
		return errors.New("cannot publish output without replacement; destination must be absent and filesystem must support hard links")
	}
	return nil
}
