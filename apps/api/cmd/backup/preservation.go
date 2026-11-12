package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/maintenance"
	"lanqin-email-api/internal/backup/preservation"
)

type preservationReceipt struct {
	Version         int    `json:"version"`
	Scope           string `json:"scope"`
	ConfigSHA256    string `json:"configSHA256"`
	OperationID     string `json:"operationId"`
	Entries         int    `json:"entries"`
	Bytes           int64  `json:"bytes"`
	Assets          int    `json:"assets"`
	Spools          int    `json:"spools"`
	Verified        bool   `json:"verified"`
	Restored        bool   `json:"restored"`
	ServicesChanged bool   `json:"servicesChanged"`
}

// These commands only authenticate preservation evidence or publish a NEW
// private evidence directory. Source labels never become filesystem targets,
// deployment instructions, container commands or Postfix queue replay inputs.
func runPreservation(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	if mode != "preservation-restore" && mode != "preservation-verify" {
		return errors.New("unknown preservation command")
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("in", "", "encrypted preservation evidence")
	identityFile := flags.String("identity-file", "", "private age X25519 recovery identity file; never a key value")
	expected := flags.String("expected-sha256", "", "independently trusted ciphertext SHA-256 receipt")
	output := ""
	if mode == "preservation-restore" {
		flags.StringVar(&output, "out", "", "NEW isolated evidence directory; deployment files and queues are never applied")
	}
	limits := preservation.DefaultLimits()
	flags.Int64Var(&limits.MaxBytes, "max-bytes", limits.MaxBytes, "maximum authenticated evidence payload bytes")
	flags.IntVar(&limits.MaxEntries, "max-entries", limits.MaxEntries, "maximum authenticated evidence entries")
	defaults := preservation.DefaultLimits()
	if flags.Parse(args) != nil || flags.NArg() != 0 || *input == "" || *identityFile == "" || mode == "preservation-restore" && output == "" || limits.MaxBytes < 1 || limits.MaxBytes > defaults.MaxBytes || limits.MaxEntries < 1 || limits.MaxEntries > defaults.MaxEntries {
		return errors.New("preservation requires --in, --identity-file, --expected-sha256 and bounded limits; restore also requires a NEW --out; use only documented flags")
	}
	digest, err := hex.DecodeString(*expected)
	if err != nil || len(digest) != 32 {
		return errors.New("preservation requires an independently trusted ciphertext SHA-256 receipt")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := readPreservationIdentity(*identityFile)
	if err != nil {
		return err
	}
	defer clear(key)
	identity := strings.TrimSpace(string(key))
	if _, err := age.ParseX25519Identity(identity); err != nil {
		return errors.New("invalid preservation recovery identity file")
	}
	before, err := os.Lstat(*input)
	// The bundle format's payload budget is separate from bounded tar/age
	// framing overhead. No unbounded ciphertext stream is accepted by the CLI.
	maxCiphertext := limits.MaxBytes + (8 << 20)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > maxCiphertext {
		return errors.New("preservation input must be a bounded regular ciphertext file")
	}
	archive, err := os.Open(*input)
	if err != nil {
		return errors.New("cannot open preservation evidence")
	}
	opened, err := archive.Stat()
	if err != nil || !os.SameFile(before, opened) {
		archive.Close()
		return errors.New("preservation input identity changed")
	}
	reader := io.LimitReader(archive, maxCiphertext+1)
	var manifest preservation.Manifest
	if mode == "preservation-restore" {
		manifest, err = preservation.Restore(ctx, reader, output, identity, hex.EncodeToString(digest), limits)
	} else {
		manifest, err = preservation.Inspect(ctx, reader, identity, hex.EncodeToString(digest), limits)
	}
	closeErr := archive.Close()
	if err != nil || closeErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("preservation evidence authentication, limits or isolated output validation failed; services and deployment files were not changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt := preservationReceipt{Version: manifest.Version, Scope: manifest.Scope, ConfigSHA256: manifest.Binding.ConfigSHA256, OperationID: manifest.Binding.OperationID, Entries: len(manifest.Entries), Verified: true, Restored: mode == "preservation-restore"}
	for _, entry := range manifest.Entries {
		receipt.Bytes += entry.Bytes
		if strings.HasPrefix(entry.Name, "assets/") {
			receipt.Assets++
		}
		if strings.HasPrefix(entry.Name, "spools/") {
			receipt.Spools++
		}
	}
	if strings.Contains(receipt.OperationID, "AGE-SECRET-KEY-") {
		return errors.New("preservation metadata cannot be reported safely; retain any authenticated isolated output")
	}
	if err := json.NewEncoder(stdout).Encode(receipt); err != nil {
		return errors.New("cannot write preservation verification receipt; authenticated isolated output may already exist")
	}
	return nil
}

func readPreservationIdentity(name string) ([]byte, error) {
	invalid := errors.New("cannot read private preservation recovery identity file")
	absolute, err := filepath.Abs(name)
	if err != nil || maintenance.ValidatePlanFile(absolute) != nil {
		return nil, invalid
	}
	before, err := os.Lstat(absolute)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 4096 {
		return nil, invalid
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, invalid
	}
	opened, statErr := file.Stat()
	key, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if statErr != nil || readErr != nil || closeErr != nil || !os.SameFile(before, opened) || len(key) < 1 || len(key) > 4096 {
		clear(key)
		return nil, invalid
	}
	return key, nil
}
