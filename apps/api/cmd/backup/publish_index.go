package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

type backupIndexPublisher func(context.Context, storage.Destination, string, string, int64, time.Time, []byte, storage.Credentials) error

func runPublishIndex(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runPublishIndexWithPublisher(ctx, args, stdout, storage.PublishRecoveryIndex)
}

func runPublishIndexWithPublisher(ctx context.Context, args []string, stdout io.Writer, publishIndex backupIndexPublisher) error {
	flags := flag.NewFlagSet("publish-index", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "destination JSON with credential environment references")
	id := flags.String("destination", "", "one original upload destination ID")
	object := flags.String("object-key", "", "original uploaded backup object key or Drive file ID")
	expected := flags.String("expected-sha256", "", "independently retained trusted ciphertext SHA-256")
	size := flags.Int64("bytes", 0, "ciphertext size from independently retained upload receipt")
	keyEnv := flags.String("index-key-env", "", "explicit reference for the independently retained original backup master/index key")
	policyEnv := flags.String("network-policy-env", "", "explicit deployment network policy environment reference")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *config == "" || *id == "" || *object == "" || *size < 1 || *size > storage.MaxUploadBytes || *keyEnv == "" {
		return errors.New("publish-index requires --config, --destination, --object-key, --expected-sha256, --bytes and --index-key-env")
	}
	digest, err := hex.DecodeString(*expected)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("publish-index requires an independently trusted ciphertext SHA-256")
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
	if selected == nil || publishIndex == nil {
		return errors.New("recovery index destination unavailable")
	}
	credentials, err := fetchCredentials(*selected)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := publishIndex(ctx, *selected, *object, hex.EncodeToString(digest), *size, time.Now().UTC(), key, credentials); err != nil {
		return errors.New("cannot publish and verify recovery index; preserve the ciphertext receipt and inspect remote state before retry")
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		DestinationID string `json:"destinationId"`
		ObjectKey     string `json:"objectKey"`
		IndexState    string `json:"indexState"`
	}{selected.ID, *object, "succeeded"}); err != nil {
		return errors.New("recovery index verified but cannot write publication receipt")
	}
	return nil
}
