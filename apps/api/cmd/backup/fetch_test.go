package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/storage"
)

type fetchFixture struct {
	config, output, receipt, object string
	data                            []byte
	cfg                             storage.Config
}

func newFetchFixture(t *testing.T) fetchFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := storage.Config{Version: 1, Destinations: []storage.Destination{
		{ID: "s3_a", Name: "Synthetic S3 A", Type: "s3", S3: &storage.S3Config{Endpoint: "https://s3.example.invalid", Region: "auto", Bucket: "backup-bucket", Prefix: "first", AccessKeyIDEnv: "BACKUP_FETCH_A_KEY", SecretAccessKeyEnv: "BACKUP_FETCH_A_SECRET", SessionTokenEnv: "BACKUP_FETCH_A_SESSION"}},
		{ID: "dav_b", Name: "Synthetic DAV B", Type: "webdav", WebDAV: &storage.WebDAVConfig{URL: "https://dav.example.invalid/backups", UsernameEnv: "BACKUP_FETCH_B_USER", PasswordEnv: "BACKUP_FETCH_B_PASSWORD"}},
		{ID: "drive_c", Name: "Synthetic Drive C", Type: "drive", Disabled: true, Drive: &storage.DriveConfig{FolderID: "synthetic_folder_id", AccessTokenEnv: "BACKUP_FETCH_C_TOKEN"}},
	}}
	values := map[string]string{"BACKUP_FETCH_A_KEY": "a-synthetic-key", "BACKUP_FETCH_A_SECRET": "a-synthetic-secret", "BACKUP_FETCH_A_SESSION": "a-synthetic-session", "BACKUP_FETCH_B_USER": "b-synthetic-user", "BACKUP_FETCH_B_PASSWORD": "b-synthetic-password", "BACKUP_FETCH_C_TOKEN": "c-synthetic-token"}
	for name, value := range values {
		t.Setenv(name, value)
	}
	config := filepath.Join(dir, "destinations.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	content := []byte("age-encryption.org/v1\nsynthetic test ciphertext bytes")
	hash := sha256.Sum256(content)
	return fetchFixture{config: config, output: filepath.Join(dir, "fetched.age"), receipt: hex.EncodeToString(hash[:]), object: "lanqin-" + strings.Repeat("a", 32) + ".age", data: content, cfg: cfg}
}

func (f fetchFixture) args(destination string) []string {
	return []string{"--config", f.config, "--destination", destination, "--object-key", f.object, "--expected-sha256", f.receipt, "--out", f.output}
}

func TestFetchCLIOnlyResolvesChosenCredentialsAndPrintsSafeReceipt(t *testing.T) {
	for _, selected := range []string{"s3_a", "dav_b", "drive_c"} {
		t.Run(selected, func(t *testing.T) {
			f := newFetchFixture(t)
			for _, name := range []string{"BACKUP_FETCH_A_KEY", "BACKUP_FETCH_A_SECRET", "BACKUP_FETCH_A_SESSION", "BACKUP_FETCH_B_USER", "BACKUP_FETCH_B_PASSWORD", "BACKUP_FETCH_C_TOKEN"} {
				if selected != "s3_a" && strings.HasPrefix(name, "BACKUP_FETCH_A_") || selected != "dav_b" && strings.HasPrefix(name, "BACKUP_FETCH_B_") || selected != "drive_c" && strings.HasPrefix(name, "BACKUP_FETCH_C_") {
					t.Setenv(name, "") // Unselected credentials are deliberately unavailable.
				}
			}
			var stdout, stderr bytes.Buffer
			calls := 0
			fetch := func(ctx context.Context, d storage.Destination, object, receipt, output string, opts storage.Options, c storage.Credentials) (storage.DownloadReceipt, error) {
				calls++
				if ctx.Err() != nil || d.ID != selected || object != f.object || receipt != f.receipt || opts.MaxBytes != 1<<30 {
					t.Error("wrong target, receipt or default limit")
				}
				switch selected {
				case "s3_a":
					if c.AccessKeyID != "a-synthetic-key" || c.SecretAccessKey != "a-synthetic-secret" || c.SessionToken != "a-synthetic-session" || c.Password != "" || c.DriveAccessToken != "" {
						t.Error("S3 credential isolation failed")
					}
				case "dav_b":
					if c.Username != "b-synthetic-user" || c.Password != "b-synthetic-password" || c.SecretAccessKey != "" || c.DriveAccessToken != "" {
						t.Error("WebDAV credential isolation failed")
					}
				case "drive_c":
					if c.DriveAccessToken != "c-synthetic-token" || c.SecretAccessKey != "" || c.Password != "" || !d.Disabled {
						t.Error("Drive recovery did not preserve disabled target and isolated token")
					}
				}
				if err := os.WriteFile(output, f.data, 0o600); err != nil {
					return storage.DownloadReceipt{}, err
				}
				return storage.DownloadReceipt{SHA256: receipt, Bytes: int64(len(f.data))}, nil
			}
			args := f.args(selected)
			for i := range args {
				if args[i] == f.receipt {
					args[i] = strings.ToUpper(f.receipt) // normalized before passing to transport.
				}
			}
			if err := runFetchWithDownloader(t.Context(), args, &stdout, &stderr, fetch); err != nil || calls != 1 {
				t.Fatal("fetch failed", err)
			}
			if !strings.Contains(stdout.String(), "sha256="+f.receipt) || !strings.Contains(stdout.String(), "NEW staging directory") || !strings.Contains(stdout.String(), "NOT a production overwrite") {
				t.Fatal("safe receipt/recovery boundary missing")
			}
			for _, secret := range []string{"synthetic-key", "synthetic-secret", "synthetic-session", "synthetic-user", "synthetic-password", "synthetic-token", f.config, f.output, "s3.example.invalid", "dav.example.invalid"} {
				if strings.Contains(stdout.String()+stderr.String(), secret) {
					t.Fatal("fetch diagnostics leaked private configuration")
				}
			}
		})
	}
}

func TestFetchCLIPreflightRejectsBeforeDownload(t *testing.T) {
	for _, scenario := range []string{"missing-flags", "positional", "unknown-destination", "missing-receipt", "invalid-receipt", "zero-limit", "oversized-limit", "existing-output", "missing-parent", "missing-credentials", "default-aws-forbidden", "invalid-config", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFetchFixture(t)
			args := f.args("s3_a")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			replace := func(flag, value string) {
				for i := range args {
					if args[i] == flag {
						args[i+1] = value
					}
				}
			}
			switch scenario {
			case "missing-flags":
				args = nil
			case "positional":
				args = append(args, "unexpected")
			case "unknown-destination":
				replace("--destination", "missing")
			case "missing-receipt":
				replace("--expected-sha256", "")
			case "invalid-receipt":
				replace("--expected-sha256", "not-sha256")
			case "zero-limit":
				args = append(args, "--max-bytes", "0")
			case "oversized-limit":
				args = append(args, "--max-bytes", fmt.Sprint(storage.MaxUploadBytes+1))
			case "existing-output":
				if err := os.WriteFile(f.output, []byte("owned existing output"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				replace("--out", filepath.Join(filepath.Dir(f.output), "missing", "fetched.age"))
			case "missing-credentials", "default-aws-forbidden":
				t.Setenv("BACKUP_FETCH_A_KEY", "")
				t.Setenv("AWS_ACCESS_KEY_ID", "must-not-be-used")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-be-used")
			case "invalid-config":
				if err := os.WriteFile(f.config, []byte(`{"version":1,"password":"NEVER-ECHO-CONFIG-SECRET"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			var stdout, stderr bytes.Buffer
			err := runFetchWithDownloader(ctx, args, &stdout, &stderr, func(context.Context, storage.Destination, string, string, string, storage.Options, storage.Credentials) (storage.DownloadReceipt, error) {
				t.Error("invalid request constructed a download")
				return storage.DownloadReceipt{}, nil
			})
			if err == nil || stdout.Len() != 0 || strings.Contains(err.Error()+stderr.String(), "NEVER-ECHO-CONFIG-SECRET") {
				t.Fatal("invalid input accepted or secret leaked", err)
			}
			if scenario == "existing-output" {
				data, _ := os.ReadFile(f.output)
				if string(data) != "owned existing output" {
					t.Fatal("preflight overwrote existing data")
				}
			}
		})
	}
}

func TestFetchCLIRedactsTransportFailureAndRejectsInvalidReceipt(t *testing.T) {
	f := newFetchFixture(t)
	for _, invalidReceipt := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		err := runFetchWithDownloader(t.Context(), f.args("drive_c"), &stdout, &stderr, func(context.Context, storage.Destination, string, string, string, storage.Options, storage.Credentials) (storage.DownloadReceipt, error) {
			if !invalidReceipt {
				return storage.DownloadReceipt{}, errors.New("provider echoed c-synthetic-token and upload_id=NEVER-ECHO-SESSION")
			}
			return storage.DownloadReceipt{SHA256: strings.Repeat("0", 64), Bytes: 20}, nil
		})
		if err == nil || stdout.Len() != 0 {
			t.Fatal("transport failure emitted success")
		}
		if strings.Contains(err.Error()+stdout.String()+stderr.String(), "synthetic-token") || strings.Contains(err.Error(), "NEVER-ECHO-SESSION") {
			t.Fatal("transport details leaked")
		}
	}
}

func TestFetchCLIDispatchRequiresExplicitRecoverySource(t *testing.T) {
	if err := run(t.Context(), []string{"fetch"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "fetch requires") {
		t.Fatal("fetch dispatch did not reach its explicit preflight", err)
	}
}
