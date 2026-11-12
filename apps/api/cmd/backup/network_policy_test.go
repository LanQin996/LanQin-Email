package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/storage"
)

const cliNetworkPolicyFixture = `{"version":1,"targets":[{"host":"192.168.1.10","port":8443,"ips":["192.168.1.10"]}]}`

func TestBackupCLINetworkPolicyRequiresExplicitValidEnvironmentReference(t *testing.T) {
	t.Setenv("LANQIN_BACKUP_NETWORK_ALLOWLIST_JSON", cliNetworkPolicyFixture)
	if policy, err := backupNetworkPolicyFromEnv(""); err != nil || policy != nil {
		t.Fatal("CLI implicitly consumed deployment allowlist")
	}
	for _, reference := range []string{"https://do-not-echo@host", "UNSET_SYNTHETIC_NETWORK_POLICY", "SYNTHETIC_NETWORK_POLICY"} {
		t.Setenv("UNSET_SYNTHETIC_NETWORK_POLICY", "")
		t.Setenv("SYNTHETIC_NETWORK_POLICY", `{"do-not-echo":"synthetic-secret"}`)
		if _, err := backupNetworkPolicyFromEnv(reference); err == nil || strings.Contains(err.Error(), "do-not-echo") || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("bad policy accepted or echoed")
		}
	}
	t.Setenv("SYNTHETIC_NETWORK_POLICY", cliNetworkPolicyFixture)
	if policy, err := backupNetworkPolicyFromEnv("SYNTHETIC_NETWORK_POLICY"); err != nil || policy == nil {
		t.Fatal("explicit valid policy unavailable", err)
	}
}

func TestFetchCLIPropagatesExplicitNASPolicyAndDefaultsPublicOnly(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-public", true: "explicit-nas"}[allow], func(t *testing.T) {
			f := newFetchFixture(t)
			f.cfg.Destinations[1].WebDAV.URL = "https://192.168.1.10:8443/backups"
			data, err := json.Marshal(f.cfg)
			if err != nil || os.WriteFile(f.config, data, 0o600) != nil {
				t.Fatal("cannot write NAS fixture", err)
			}
			t.Setenv("SYNTHETIC_NETWORK_POLICY", cliNetworkPolicyFixture)
			args := f.args("dav_b")
			if allow {
				args = append(args, "--network-policy-env", "SYNTHETIC_NETWORK_POLICY")
			}
			called := false
			download := func(_ context.Context, d storage.Destination, _, sha, output string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
				called = true
				if !allow || d.NetworkPolicy == nil {
					t.Fatal("selected downloader lost explicit policy")
				}
				if err := os.WriteFile(output, f.data, 0o600); err != nil {
					t.Fatal(err)
				}
				return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(f.data))}, nil
			}
			var out, diagnostic bytes.Buffer
			err = runFetchWithDownloader(t.Context(), args, &out, &diagnostic, download)
			if (err == nil) != allow || called != allow {
				t.Fatal("CLI network policy did not gate transfer", err)
			}
		})
	}
}
