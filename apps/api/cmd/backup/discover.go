package main

import (
	"bytes"
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

const maxDiscoveryFileBytes = 3 << 20

type backupDiscoveryBundle struct {
	Version int               `json:"version"`
	Indexes []json.RawMessage `json:"indexes"`
}

type backupIndexDiscoverer func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error)

func backupIndexKeyFromEnv(reference string) ([]byte, error) {
	invalid := errors.New("recovery index requires an explicit environment reference containing the original Base64-encoded 32-byte backup index key")
	if !nativeDSNEnvPattern.MatchString(reference) {
		return nil, invalid
	}
	value, ok := os.LookupEnv(reference)
	if !ok || len(value) > 256 {
		return nil, invalid
	}
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, invalid
	}
	return key, nil
}

func runDiscover(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runDiscoverWithLister(ctx, args, stdout, storage.DiscoverRecoveryIndexes)
}

func runDiscoverWithLister(ctx context.Context, args []string, stdout io.Writer, discover backupIndexDiscoverer) error {
	flags := flag.NewFlagSet("discover", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "destination JSON with credential environment references")
	id := flags.String("destination", "", "one explicitly selected destination, including its original Drive folder")
	keyEnv := flags.String("index-key-env", "", "environment reference for independently retained original backup master/index key")
	policyEnv := flags.String("network-policy-env", "", "explicit deployment network policy environment reference")
	output := flags.String("out", "", "new private authenticated recovery index bundle")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *config == "" || *id == "" || *keyEnv == "" || *output == "" {
		return errors.New("discover requires --config, --destination, --index-key-env and --out; no key values or positional arguments")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	key, err := backupIndexKeyFromEnv(*keyEnv)
	if err != nil {
		return err
	}
	defer clear(key)
	policy, err := backupNetworkPolicyFromEnv(*policyEnv)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(*output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("discovery output must be absent")
	}
	file, err := os.Open(*config)
	if err != nil {
		return errors.New("cannot open recovery destination config")
	}
	cfg, err := storage.DecodeConfigWithNetworkPolicy(file, policy)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("invalid recovery destination config")
	}
	var selected *storage.Destination
	for i := range cfg.Destinations {
		if cfg.Destinations[i].ID == *id {
			selected = &cfg.Destinations[i]
		}
	}
	if selected == nil || discover == nil {
		return errors.New("recovery discovery destination unavailable")
	}
	credentials, err := fetchCredentials(*selected)
	if err != nil {
		return err
	}
	entries, err := discover(ctx, *selected, key, credentials)
	if err != nil {
		return errors.New("cannot authenticate remote recovery indexes; no recovery inventory published")
	}
	if len(entries) > 128 {
		return errors.New("recovery index discovery limit exceeded")
	}
	bundle := backupDiscoveryBundle{Version: 1, Indexes: make([]json.RawMessage, 0, len(entries))}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.ObjectKey] {
			return errors.New("ambiguous duplicate recovery index")
		}
		seen[entry.ObjectKey] = true
		original := *selected
		original.ID = entry.DestinationID
		document, err := storage.SealRecoveryIndex(original, entry.ObjectKey, entry.SHA256, entry.Bytes, entry.CompletedAt, key)
		if err != nil {
			return errors.New("invalid authenticated recovery index result")
		}
		verified, err := storage.OpenRecoveryIndex(*selected, document, key)
		if err != nil || verified != entry {
			return errors.New("recovery index discovery returned inconsistent source binding")
		}
		bundle.Indexes = append(bundle.Indexes, json.RawMessage(document))
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > maxDiscoveryFileBytes {
		return errors.New("recovery discovery bundle exceeds limit")
	}
	if err := publish(*output, func(file *os.File) error { _, err := file.Write(append(data, '\n')); return err }); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Items  []storage.RecoveryIndexEntry `json:"items"`
		Notice string                       `json:"notice"`
	}{entries, "Authenticated observed indexes only; remote omission or deletion cannot prove this inventory is complete or newest. Use fetch with this bundle, then restore with the independently held recovery key."}); err != nil {
		return errors.New("authenticated discovery bundle saved but cannot write its inventory")
	}
	return nil
}

func readDiscoverySelection(path string, d storage.Destination, object string, key []byte) (storage.RecoveryIndexEntry, error) {
	invalid := errors.New("recovery index bundle is invalid, ambiguous or does not bind the selected destination/object")
	var empty storage.RecoveryIndexEntry
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxDiscoveryFileBytes {
		return empty, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return empty, invalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return empty, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDiscoveryFileBytes+1))
	if err != nil || int64(len(data)) != info.Size() {
		return empty, invalid
	}
	var bundle backupDiscoveryBundle
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&bundle) != nil || decoder.Decode(new(any)) != io.EOF || bundle.Version != 1 || bundle.Indexes == nil || len(bundle.Indexes) > 128 {
		return empty, invalid
	}
	seen := map[string]bool{}
	var selected *storage.RecoveryIndexEntry
	for _, document := range bundle.Indexes {
		entry, err := storage.OpenRecoveryIndex(d, document, key)
		if err != nil || seen[entry.ObjectKey] {
			return empty, invalid
		}
		seen[entry.ObjectKey] = true
		if entry.ObjectKey == object {
			selected = &entry
		}
	}
	if selected == nil {
		return empty, invalid
	}
	return *selected, nil
}
