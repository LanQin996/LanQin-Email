// backup is an offline snapshot utility. It intentionally has no production
// service-control or in-place overwrite capabilities. Cloud uploads are opt-in
// and read per-destination credential references from operator configuration.
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
		return errors.New("usage: backup keygen|create|restore|upload (see docs/backup.md)")
	}
	if args[0] == "upload" {
		return runUpload(ctx, args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyFile := flags.String("identity-file", "", "private recovery key file (never a command-line key value)")
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
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	limits := backup.Limits{MaxBytes: *maxBytes, MaxEntries: *maxEntries}
	switch args[0] {
	case "keygen":
		if *keyFile == "" {
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
		if !*offline || *snapshot == "" || *output == "" || *recipient == "" {
			return errors.New("create requires --snapshot, --recipient, --out and --confirm-offline-snapshot")
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
			_, err := backup.Write(ctx, source, io.MultiWriter(file, digest), *recipient, limits)
			return err
		}); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Encrypted offline DATA snapshot created; deployment secrets and Postfix queue are NOT included.\nKeep this receipt separately trusted: sha256="+hex.EncodeToString(digest.Sum(nil)))
		return err
	case "restore":
		if *keyFile == "" || *input == "" || *output == "" || *expectedHash == "" {
			return errors.New("restore requires --identity-file, --in, --out and --expected-sha256 from a trusted receipt")
		}
		want, err := hex.DecodeString(*expectedHash)
		if err != nil || len(want) != sha256.Size {
			return errors.New("invalid expected SHA-256")
		}
		identityFile, err := os.Open(*keyFile)
		if err != nil {
			return errors.New("cannot open recovery key file")
		}
		key, readErr := io.ReadAll(io.LimitReader(identityFile, 4097))
		closeErr := identityFile.Close()
		if readErr != nil || closeErr != nil || len(key) > 4096 {
			return errors.New("cannot read recovery key file")
		}
		archive, err := os.Open(*input)
		if err != nil {
			return errors.New("cannot open backup file")
		}
		defer archive.Close()
		if _, err := backup.Restore(ctx, archive, *output, strings.TrimSpace(string(key)), *expectedHash, limits); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Verified data staged under out/snapshot; READY is present. No live services or ownership were changed.")
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
