package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"lanqin-email-api/internal/backup/storage"
)

func runUpload(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("upload", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configFile := flags.String("config", "", "JSON destination config containing environment variable references, never secrets")
	input := flags.String("in", "", "existing encrypted binary age backup")
	receipt := flags.String("expected-sha256", "", "ciphertext SHA-256 from a separately trusted receipt")
	selected := flags.String("destinations", "", "comma-separated destination IDs (default: all enabled destinations)")
	networkPolicyEnv := flags.String("network-policy-env", "", "explicit environment reference for a deployment host/port/private-IP allowlist; default public-only")
	indexKeyEnv := flags.String("index-key-env", "", "optional explicit environment reference for publishing authenticated cloud recovery indexes")
	resumeDir := flags.String("resume-dir", "", "existing private directory for encrypted upload checkpoints")
	resumeID := flags.String("resume-id", "", "stable upload operation identifier; reuse only for the exact same ciphertext and targets")
	opts := storage.DefaultOptions()
	flags.IntVar(&opts.Concurrency, "concurrency", opts.Concurrency, "maximum simultaneous destinations (1..8)")
	flags.IntVar(&opts.Attempts, "attempts", opts.Attempts, "maximum attempts per destination (1..3)")
	flags.DurationVar(&opts.AttemptTimeout, "timeout", opts.AttemptTimeout, "per-attempt timeout (up to 1h)")
	flags.Int64Var(&opts.MaxBytes, "max-bytes", opts.MaxBytes, "maximum ciphertext size (up to 5 GiB)")
	flags.StringVar(&opts.SpoolDir, "spool-dir", "", "existing private spool directory (default: OS temporary directory)")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid upload arguments; credentials and checkpoint encryption keys belong in the environment")
	}
	if flags.NArg() != 0 || *configFile == "" || *input == "" || *receipt == "" {
		return errors.New("upload requires --config, --in and --expected-sha256; no positional arguments")
	}
	if (*resumeDir == "") != (*resumeID == "") {
		return errors.New("upload checkpoints require both --resume-dir and --resume-id")
	}
	if *indexKeyEnv != "" {
		key, err := backupIndexKeyFromEnv(*indexKeyEnv)
		if err != nil {
			return err
		}
		defer clear(key)
		opts.RecoveryIndexKey = key
	}
	networkPolicy, err := backupNetworkPolicyFromEnv(*networkPolicyEnv)
	if err != nil {
		return err
	}
	if *resumeDir != "" {
		key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("LANQIN_BACKUP_SECRET_KEY")))
		if err != nil || len(key) != 32 {
			return errors.New("upload checkpoints require LANQIN_BACKUP_SECRET_KEY as a Base64-encoded 32-byte key")
		}
		store, err := storage.OpenFileCheckpointStore(*resumeDir, *resumeID, key)
		clear(key)
		if err != nil {
			return errors.New("cannot exclusively open the private encrypted upload checkpoint store")
		}
		defer store.Close()
		opts.Checkpoints = store
	}
	file, err := os.Open(*configFile)
	if err != nil {
		return errors.New("cannot open destination config")
	}
	cfg, err := storage.DecodeConfigWithNetworkPolicy(file, networkPolicy)
	file.Close()
	if err != nil {
		return err
	}
	if *selected != "" {
		cfg, err = selectDestinations(cfg, *selected)
		if err != nil {
			return err
		}
	}
	report, uploadErr := storage.Upload(ctx, *input, *receipt, cfg, opts)
	if len(report.Destinations) > 0 {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return errors.New("cannot write upload report; remote uploads may have succeeded")
		}
	}
	return uploadErr
}

func selectDestinations(cfg storage.Config, value string) (storage.Config, error) {
	selected := make(map[string]bool)
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		if id == "" || selected[id] {
			return storage.Config{}, errors.New("destination selection must contain unique nonempty IDs")
		}
		selected[id] = true
	}
	result := storage.Config{Version: cfg.Version}
	for _, d := range cfg.Destinations {
		if selected[d.ID] {
			result.Destinations = append(result.Destinations, d)
			delete(selected, d.ID)
		}
	}
	if len(selected) > 0 {
		return storage.Config{}, errors.New("destination selection contains unknown IDs")
	}
	return result, nil
}
