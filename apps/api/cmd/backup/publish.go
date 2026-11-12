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

	"lanqin-email-api/internal/backup/catalog"
)

func runPublish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("catalog", "", "existing private artifact directory shared with the API")
	input := flags.String("in", "", "existing encrypted age artifact")
	digest := flags.String("expected-sha256", "", "ciphertext SHA-256 from a trusted receipt")
	maxBytes := flags.Int64("max-bytes", 1<<30, "maximum ciphertext size (API currently accepts up to 1 GiB)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *dir == "" || *input == "" || *digest == "" {
		return errors.New("publish requires --catalog, --in and --expected-sha256")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("LANQIN_BACKUP_SECRET_KEY")))
	if err != nil || len(key) != 32 {
		return errors.New("LANQIN_BACKUP_SECRET_KEY must be a Base64-encoded 32-byte key")
	}
	entry, err := catalog.Publish(ctx, *dir, *input, *digest, key, *maxBytes)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(entry)
}
