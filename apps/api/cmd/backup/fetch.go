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
	"strings"
	"unicode"

	"lanqin-email-api/internal/backup/storage"
)

type backupDownloader func(context.Context, storage.Destination, string, string, string, storage.Options, storage.Credentials) (storage.DownloadReceipt, error)

func runFetch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runFetchWithDownloader(ctx, args, stdout, stderr, storage.Download)
}

// The downloader owns transport, bounded staging, hash verification and atomic
// no-replace publication. This CLI never reads recovery keys or runs services.
func runFetchWithDownloader(ctx context.Context, args []string, stdout, stderr io.Writer, download backupDownloader) error {
	flags := flag.NewFlagSet("fetch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", "", "JSON destination config containing credential environment references")
	destinationID := flags.String("destination", "", "one configured destination ID")
	objectKey := flags.String("object-key", "", "object key or Drive file ID from the upload report")
	receipt := flags.String("expected-sha256", "", "ciphertext SHA-256 from a separately trusted receipt")
	output := flags.String("out", "", "new encrypted output file (parent directory must exist)")
	opts := storage.DefaultOptions()
	flags.Int64Var(&opts.MaxBytes, "max-bytes", opts.MaxBytes, "maximum ciphertext bytes (up to 5 GiB)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *configFile == "" || *destinationID == "" || *objectKey == "" || *receipt == "" || *output == "" {
		return errors.New("fetch requires --config, --destination, --object-key, --expected-sha256 and --out; no positional arguments")
	}
	want, err := hex.DecodeString(*receipt)
	if err != nil || len(want) != sha256.Size {
		return errors.New("invalid trusted ciphertext SHA-256 receipt")
	}
	if opts.MaxBytes < 1 || opts.MaxBytes > storage.MaxUploadBytes {
		return errors.New("invalid fetch size limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(*output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("fetch output already exists or cannot be inspected")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(*output))
	if err != nil {
		return errors.New("fetch output parent must exist")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return errors.New("fetch output parent must be a directory")
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return errors.New("cannot resolve fetch output")
	}
	canonicalOutput := filepath.Join(parent, filepath.Base(*output))
	file, err := os.Open(*configFile)
	if err != nil {
		return errors.New("cannot open destination config")
	}
	cfg, configErr := storage.DecodeConfig(file)
	closeErr := file.Close()
	if configErr != nil {
		return configErr
	}
	if closeErr != nil {
		return errors.New("cannot close destination config")
	}
	var selected *storage.Destination
	for i := range cfg.Destinations {
		if cfg.Destinations[i].ID == *destinationID {
			selected = &cfg.Destinations[i]
			break
		}
	}
	if selected == nil {
		return errors.New("fetch destination is not configured")
	}
	credentials, err := fetchCredentials(*selected)
	if err != nil {
		return err
	}
	if download == nil {
		return errors.New("fetch transport unavailable")
	}
	// A selected disabled destination may be read for recovery without enabling
	// automatic backups or changing its persisted state. Only this ID is used.
	verified, err := download(ctx, *selected, *objectKey, hex.EncodeToString(want), canonicalOutput, opts, credentials)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Provider responses, credential material and resumable session URLs are
		// not part of CLI diagnostics, even if an adapter ever returns them.
		return errors.New("cannot fetch and verify encrypted backup; no verified download was reported")
	}
	if !strings.EqualFold(verified.SHA256, hex.EncodeToString(want)) || verified.Bytes < 1 || verified.Bytes > opts.MaxBytes {
		return errors.New("fetch transport returned an invalid verification receipt")
	}
	if _, err := fmt.Fprintf(stdout, "Encrypted file verified: bytes=%d sha256=%s\nUse restore with an independently held recovery key to a NEW staging directory; this is NOT a production overwrite.\n", verified.Bytes, strings.ToLower(verified.SHA256)); err != nil {
		return errors.New("verified encrypted file was published, but its receipt could not be written")
	}
	return nil
}

func fetchCredentials(d storage.Destination) (storage.Credentials, error) {
	var result storage.Credentials
	get := func(reference string) (string, error) {
		value, ok := os.LookupEnv(reference)
		if !ok || value == "" || len(value) > 65536 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", errors.New("selected destination credentials are unavailable or invalid")
		}
		return value, nil
	}
	var err error
	switch d.Type {
	case "s3":
		result.AccessKeyID, err = get(d.S3.AccessKeyIDEnv)
		if err == nil {
			result.SecretAccessKey, err = get(d.S3.SecretAccessKeyEnv)
		}
		if err == nil && d.S3.SessionTokenEnv != "" {
			result.SessionToken, err = get(d.S3.SessionTokenEnv)
		}
	case "webdav":
		result.Username, err = get(d.WebDAV.UsernameEnv)
		if err == nil && strings.Contains(result.Username, ":") {
			err = errors.New("selected destination credentials are unavailable or invalid")
		}
		if err == nil {
			result.Password, err = get(d.WebDAV.PasswordEnv)
		}
	case "drive":
		result.DriveAccessToken, err = get(d.Drive.AccessTokenEnv)
	default:
		err = errors.New("unsupported fetch destination")
	}
	return result, err
}
