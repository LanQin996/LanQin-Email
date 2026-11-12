package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

const discoveryTestKeyEnv = "BACKUP_SYNTHETIC_DISCOVERY_KEY"

func discoveryTestKey() []byte { return bytes.Repeat([]byte{0x5c}, 32) }

func discoveryTestEntry(t *testing.T, f fetchFixture, selected string) (storage.Destination, storage.RecoveryIndexEntry, json.RawMessage) {
	t.Helper()
	var d storage.Destination
	for _, candidate := range f.cfg.Destinations {
		if candidate.ID == selected {
			d = candidate
			break
		}
	}
	if d.ID == "" {
		t.Fatal("unknown discovery fixture destination")
	}
	object := f.object
	if d.S3 != nil {
		object = d.S3.Prefix + "/" + object
	}
	if d.Drive != nil {
		object = "synthetic-original-drive-file"
	}
	original := d
	original.ID = "original_" + d.ID
	document, err := storage.SealRecoveryIndex(original, object, f.receipt, int64(len(f.data)), time.Date(2026, 11, 3, 12, 34, 56, 123, time.UTC), discoveryTestKey())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := storage.OpenRecoveryIndex(d, document, discoveryTestKey())
	if err != nil {
		t.Fatal(err)
	}
	return d, entry, json.RawMessage(document)
}

func discoveryTestBundle(t *testing.T, path string, documents ...json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(backupDiscoveryBundle{Version: 1, Indexes: documents})
	if err != nil || os.WriteFile(path, data, 0o600) != nil {
		t.Fatal("cannot save discovery bundle fixture", err)
	}
}

func discoveryTestArgs(f fetchFixture, id, output string) []string {
	return []string{"--config", f.config, "--destination", id, "--index-key-env", discoveryTestKeyEnv, "--out", output}
}

func indexedFetchTestArgs(f fetchFixture, id, object, bundle string) []string {
	return []string{"--config", f.config, "--destination", id, "--object-key", object, "--recovery-index-file", bundle, "--index-key-env", discoveryTestKeyEnv, "--out", f.output}
}

func TestDiscoverCLIAuthenticatesReconfiguredDestinationsAndIsolatesCredentials(t *testing.T) {
	for _, id := range []string{"s3_a", "dav_b", "drive_c"} {
		t.Run(id, func(t *testing.T) {
			f := newFetchFixture(t)
			d, entry, _ := discoveryTestEntry(t, f, id)
			t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
			for _, name := range []string{"BACKUP_FETCH_A_KEY", "BACKUP_FETCH_A_SECRET", "BACKUP_FETCH_A_SESSION", "BACKUP_FETCH_B_USER", "BACKUP_FETCH_B_PASSWORD", "BACKUP_FETCH_C_TOKEN"} {
				if id != "s3_a" && strings.HasPrefix(name, "BACKUP_FETCH_A_") || id != "dav_b" && strings.HasPrefix(name, "BACKUP_FETCH_B_") || id != "drive_c" && strings.HasPrefix(name, "BACKUP_FETCH_C_") {
					t.Setenv(name, "")
				}
			}
			output := filepath.Join(t.TempDir(), "discovery.json")
			calls := 0
			lister := func(ctx context.Context, selected storage.Destination, key []byte, credentials storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				calls++
				if ctx.Err() != nil || selected.ID != id || !bytes.Equal(key, discoveryTestKey()) {
					t.Fatal("discovery authority changed")
				}
				switch id {
				case "s3_a":
					if credentials.AccessKeyID != "a-synthetic-key" || credentials.SecretAccessKey != "a-synthetic-secret" || credentials.SessionToken != "a-synthetic-session" || credentials.Password != "" || credentials.DriveAccessToken != "" {
						t.Fatal("S3 credential isolation failed")
					}
				case "dav_b":
					if credentials.Username != "b-synthetic-user" || credentials.Password != "b-synthetic-password" || credentials.SecretAccessKey != "" || credentials.DriveAccessToken != "" {
						t.Fatal("DAV credential isolation failed")
					}
				case "drive_c":
					if credentials.DriveAccessToken != "c-synthetic-token" || credentials.SecretAccessKey != "" || credentials.Password != "" || !selected.Disabled {
						t.Fatal("Drive credential isolation/disabled recovery failed")
					}
				}
				return []storage.RecoveryIndexEntry{entry}, nil
			}
			var stdout bytes.Buffer
			if err := runDiscoverWithLister(t.Context(), discoveryTestArgs(f, id, output), &stdout, lister); err != nil || calls != 1 {
				t.Fatal("discovery failed", err)
			}
			selected, err := readDiscoverySelection(output, d, entry.ObjectKey, discoveryTestKey())
			if err != nil || selected != entry {
				t.Fatal("discovery bundle did not preserve original authenticated identity", err)
			}
			if selected.DestinationID == d.ID {
				t.Fatal("new instance ID replaced signed original identity")
			}
			var inventory struct {
				Items  []storage.RecoveryIndexEntry `json:"items"`
				Notice string                       `json:"notice"`
			}
			if json.Unmarshal(stdout.Bytes(), &inventory) != nil || len(inventory.Items) != 1 || inventory.Items[0] != entry || !strings.Contains(inventory.Notice, "cannot prove") {
				t.Fatal("discovery overclaimed completeness or lost inventory")
			}
			for _, secret := range []string{"a-synthetic-secret", "a-synthetic-key", "a-synthetic-session", "b-synthetic-user", "b-synthetic-password", "c-synthetic-token", base64.StdEncoding.EncodeToString(discoveryTestKey()), f.config, output} {
				if strings.Contains(stdout.String(), secret) {
					t.Fatal("discovery inventory exposed private configuration")
				}
			}
			before, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if err := runDiscoverWithLister(t.Context(), discoveryTestArgs(f, id, output), &bytes.Buffer{}, lister); err == nil || calls != 1 {
				t.Fatal("discovery overwrote bundle or contacted provider before absence check")
			}
			after, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("existing discovery bundle changed")
			}
		})
	}
}

func TestDiscoverCLIPreflightAndProviderFailuresDoNotLeakOrPublish(t *testing.T) {
	for _, scenario := range []string{"missing-config", "missing-destination", "missing-key-env", "missing-out", "unknown-flag", "positional", "raw-key", "missing-key", "invalid-key", "short-key", "oversized-key", "missing-credentials", "unknown-destination", "cancelled", "provider", "duplicate", "too-many", "invalid-result", "wrong-location"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFetchFixture(t)
			_, entry, _ := discoveryTestEntry(t, f, "dav_b")
			output := filepath.Join(t.TempDir(), "discovery.json")
			args := discoveryTestArgs(f, "dav_b", output)
			t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			replace := func(flag, value string) {
				for i := 0; i < len(args)-1; i++ {
					if args[i] == flag {
						args[i+1] = value
					}
				}
			}
			switch scenario {
			case "missing-config":
				replace("--config", "")
			case "missing-destination":
				replace("--destination", "")
			case "missing-key-env":
				replace("--index-key-env", "")
			case "missing-out":
				replace("--out", "")
			case "unknown-flag":
				args = append(args, "--private=never-echo-provider-secret")
			case "positional":
				args = append(args, "never-echo-provider-secret")
			case "raw-key":
				replace("--index-key-env", base64.StdEncoding.EncodeToString(discoveryTestKey()))
			case "missing-key":
				t.Setenv(discoveryTestKeyEnv, "")
			case "invalid-key":
				t.Setenv(discoveryTestKeyEnv, "never-echo-provider-secret")
			case "short-key":
				t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(make([]byte, 31)))
			case "oversized-key":
				t.Setenv(discoveryTestKeyEnv, strings.Repeat("s", 257))
			case "missing-credentials":
				t.Setenv("BACKUP_FETCH_B_PASSWORD", "")
			case "unknown-destination":
				replace("--destination", "unknown")
			case "cancelled":
				cancel()
			}
			calls := 0
			lister := func(context.Context, storage.Destination, []byte, storage.Credentials) ([]storage.RecoveryIndexEntry, error) {
				calls++
				switch scenario {
				case "provider":
					return nil, errors.New("never-echo-provider-secret https://nas/private/session")
				case "duplicate":
					return []storage.RecoveryIndexEntry{entry, entry}, nil
				case "too-many":
					return make([]storage.RecoveryIndexEntry, 129), nil
				case "invalid-result":
					entry.SHA256 = "never-echo-provider-secret"
					return []storage.RecoveryIndexEntry{entry}, nil
				case "wrong-location":
					entry.Location = strings.Repeat("0", 64)
					return []storage.RecoveryIndexEntry{entry}, nil
				default:
					t.Fatal("preflight failure called provider")
					return nil, nil
				}
			}
			var stdout bytes.Buffer
			err := runDiscoverWithLister(ctx, args, &stdout, lister)
			providerReached := scenario == "provider" || scenario == "duplicate" || scenario == "too-many" || scenario == "invalid-result" || scenario == "wrong-location"
			if err == nil || stdout.Len() != 0 || calls != boolTestInt(providerReached) {
				t.Fatal("incorrect discovery failure boundary", scenario, err)
			}
			for _, secret := range []string{"never-echo-provider-secret", base64.StdEncoding.EncodeToString(discoveryTestKey()), "https://nas/private/session"} {
				if strings.Contains(err.Error()+stdout.String(), secret) {
					t.Fatal("discovery error leaked private provider/key data")
				}
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed discovery published inventory")
			}
		})
	}
}

func boolTestInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestDiscoverySelectionAndIndexedFetchVerifyReceiptAndOriginalDriveID(t *testing.T) {
	for _, id := range []string{"s3_a", "dav_b", "drive_c"} {
		t.Run(id, func(t *testing.T) {
			f := newFetchFixture(t)
			d, entry, document := discoveryTestEntry(t, f, id)
			bundle := filepath.Join(t.TempDir(), "signed-discovery.json")
			discoveryTestBundle(t, bundle, document)
			t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
			got, err := readDiscoverySelection(bundle, d, entry.ObjectKey, discoveryTestKey())
			if err != nil || got != entry {
				t.Fatal("signed selection failed", err)
			}
			calls := 0
			download := func(_ context.Context, selected storage.Destination, object, sha, output string, opts storage.Options, credentials storage.Credentials) (storage.DownloadReceipt, error) {
				calls++
				if selected.ID != entry.DestinationID || object != entry.ObjectKey || sha != entry.SHA256 || opts.MaxBytes != entry.Bytes {
					t.Fatal("indexed fetch ignored signed object/size/hash/original ID")
				}
				if id == "drive_c" && (selected.Drive.FolderID != d.Drive.FolderID || credentials.DriveAccessToken != "c-synthetic-token") {
					t.Fatal("Drive indexed fetch lost folder/current explicit credentials")
				}
				if err := os.WriteFile(output, f.data, 0o600); err != nil {
					return storage.DownloadReceipt{}, err
				}
				return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(f.data))}, nil
			}
			var stdout, stderr bytes.Buffer
			if err := runFetchWithDownloader(t.Context(), indexedFetchTestArgs(f, id, entry.ObjectKey, bundle), &stdout, &stderr, download); err != nil || calls != 1 {
				t.Fatal("indexed fetch failed", err)
			}
			if stderr.Len() != 0 || !strings.Contains(stdout.String(), "sha256="+entry.SHA256) {
				t.Fatal("indexed fetch receipt missing")
			}
		})
	}
}

func TestIndexedFetchRejectsInvalidBundlesBeforeDownload(t *testing.T) {
	for _, scenario := range []string{"tamper", "wrong-key", "duplicate", "wrong-location", "wrong-object", "mixed-expected-hash", "missing-index-key", "missing-index-file", "unknown-bundle-field", "trailing-json", "null-indexes", "too-many", "oversized-file", "over-fetch-limit"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFetchFixture(t)
			d, entry, document := discoveryTestEntry(t, f, "dav_b")
			bundle := filepath.Join(t.TempDir(), "discovery.json")
			discoveryTestBundle(t, bundle, document)
			t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
			args := indexedFetchTestArgs(f, d.ID, entry.ObjectKey, bundle)
			switch scenario {
			case "tamper":
				discoveryTestBundle(t, bundle, bytes.Replace(document, []byte(entry.SHA256), []byte(strings.Repeat("0", 64)), 1))
			case "wrong-key":
				t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32)))
			case "duplicate":
				discoveryTestBundle(t, bundle, document, document)
			case "wrong-location":
				other := d
				settings := *d.WebDAV
				settings.URL += "/other"
				other.WebDAV = &settings
				signed, err := storage.SealRecoveryIndex(other, entry.ObjectKey, entry.SHA256, entry.Bytes, entry.CompletedAt, discoveryTestKey())
				if err != nil {
					t.Fatal(err)
				}
				discoveryTestBundle(t, bundle, signed)
			case "wrong-object":
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--object-key" {
						args[i+1] = "lanqin-" + strings.Repeat("b", 32) + ".age"
					}
				}
			case "mixed-expected-hash":
				args = append(args, "--expected-sha256", entry.SHA256)
			case "missing-index-key":
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--index-key-env" {
						args[i+1] = ""
					}
				}
			case "missing-index-file":
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--recovery-index-file" {
						args[i+1] = ""
					}
				}
			case "unknown-bundle-field":
				data, _ := os.ReadFile(bundle)
				data = bytes.Replace(data, []byte(`"version":1`), []byte(`"unknown":"never-echo-bundle-secret","version":1`), 1)
				if err := os.WriteFile(bundle, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "trailing-json":
				data, _ := os.ReadFile(bundle)
				if err := os.WriteFile(bundle, append(data, []byte(`{}`)...), 0o600); err != nil {
					t.Fatal(err)
				}
			case "null-indexes":
				if err := os.WriteFile(bundle, []byte(`{"version":1,"indexes":null}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "too-many":
				documents := make([]json.RawMessage, 129)
				for i := range documents {
					documents[i] = document
				}
				discoveryTestBundle(t, bundle, documents...)
			case "oversized-file":
				if err := os.WriteFile(bundle, bytes.Repeat([]byte{' '}, maxDiscoveryFileBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			case "over-fetch-limit":
				args = append(args, "--max-bytes", fmt.Sprint(entry.Bytes-1))
			}
			var stdout, stderr bytes.Buffer
			err := runFetchWithDownloader(t.Context(), args, &stdout, &stderr, func(context.Context, storage.Destination, string, string, string, storage.Options, storage.Credentials) (storage.DownloadReceipt, error) {
				t.Fatal("invalid bundle reached download")
				return storage.DownloadReceipt{}, nil
			})
			if err == nil || stdout.Len() != 0 || stderr.Len() != 0 || strings.Contains(err.Error(), "never-echo-bundle-secret") {
				t.Fatal("invalid indexed fetch accepted or echoed", scenario, err)
			}
			if _, err := os.Stat(f.output); !os.IsNotExist(err) {
				t.Fatal("invalid indexed fetch created output")
			}
		})
	}
}

func TestIndexedFetchRejectsMismatchedActualLengthAndProviderErrors(t *testing.T) {
	for _, scenario := range []string{"short", "wrong-hash", "provider"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFetchFixture(t)
			d, entry, document := discoveryTestEntry(t, f, "drive_c")
			bundle := filepath.Join(t.TempDir(), "discovery.json")
			discoveryTestBundle(t, bundle, document)
			t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
			var stdout, stderr bytes.Buffer
			err := runFetchWithDownloader(t.Context(), indexedFetchTestArgs(f, d.ID, entry.ObjectKey, bundle), &stdout, &stderr, func(context.Context, storage.Destination, string, string, string, storage.Options, storage.Credentials) (storage.DownloadReceipt, error) {
				if scenario == "provider" {
					return storage.DownloadReceipt{}, errors.New("never-echo-provider-secret c-synthetic-token")
				}
				if scenario == "wrong-hash" {
					return storage.DownloadReceipt{SHA256: strings.Repeat("0", 64), Bytes: entry.Bytes}, nil
				}
				return storage.DownloadReceipt{SHA256: entry.SHA256, Bytes: entry.Bytes - 1}, nil
			})
			if err == nil || stdout.Len() != 0 || stderr.Len() != 0 || strings.Contains(err.Error(), "never-echo-provider-secret") || strings.Contains(err.Error(), "c-synthetic-token") {
				t.Fatal("invalid transport result accepted or leaked")
			}
		})
	}
}
