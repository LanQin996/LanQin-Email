package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/install"
)

func preservationAssetFixture(t *testing.T, name string, data []byte) PathDigest {
	t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return PathDigest{Path: name, SHA256: hex.EncodeToString(digest[:])}
}

func TestMaintenancePreservationConfigPinsExplicitAssetsWithoutReadingIdentityEnvironment(t *testing.T) {
	cfg := faultConfig(t)
	withoutPreservation, _ := json.Marshal(cfg)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_PRESERVATION_IDENTITY", "not-a-private-key-and-must-not-be-read")
	asset := preservationAssetFixture(t, filepath.Join(cfg.ProjectDirectory, "synthetic-tls.key"), []byte("-----BEGIN PRIVATE KEY-----\nsynthetic deployment TLS bytes\n-----END PRIVATE KEY-----\n"))
	cfg.Preservation = &PreservationConfig{Recipient: identity.Recipient().String(), IdentityEnv: "SYNTHETIC_PRESERVATION_IDENTITY", Assets: []PathDigest{asset}}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal("explicit TLS asset rejected or decoder read identity env", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(identity.String())) || bytes.Contains(data, []byte("not-a-private-key")) {
		t.Fatal("config serialized recovery credential")
	}
	if _, err := DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatal("preservation config roundtrip", err)
	}
	original, _ := ConfigHash(cfg)
	changed := cfg
	copy := *cfg.Preservation
	changed.Preservation = &copy
	changed.Preservation.IdentityEnv = "ANOTHER_EXPLICIT_IDENTITY"
	updated, _ := ConfigHash(changed)
	if original == updated {
		t.Fatal("preservation settings not bound to operation config hash")
	}
	quarantined := cfg
	quarantine := *cfg.Preservation
	quarantine.QuarantineQueues = true
	quarantined.Preservation = &quarantine
	quarantineHash, _ := ConfigHash(quarantined)
	if quarantineHash == original {
		t.Fatal("destructive queue quarantine consent was not bound to config hash")
	}
	unchanged := cfg
	unchanged.Preservation = nil
	nilAgain, _ := json.Marshal(unchanged)
	if !bytes.Equal(withoutPreservation, nilAgain) {
		t.Fatal("nil preservation changed legacy config encoding/hash")
	}
	if err := os.Remove(asset.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStatusConfig(bytes.NewReader(data)); err != nil {
		t.Fatal("read-only status required missing asset", err)
	}
	if _, err := DecodeRecoveryConfig(bytes.NewReader(data)); err == nil {
		t.Fatal("mutating recovery ignored missing pinned asset")
	}
	preservationAssetFixture(t, asset.Path, []byte("modified deployment asset"))
	if err := VerifyPinnedFiles(cfg); err == nil {
		t.Fatal("asset tampering ignored after initial config load")
	}
}

func TestMaintenancePreservationRejectsUntrustedAndPrivateRecoveryAssets(t *testing.T) {
	for _, scenario := range []string{"bad-recipient", "private-recipient", "bad-env-reference", "too-many", "duplicate", "hardlink", "symlink", "compose-duplicate", "env-duplicate", "live-data", "live-mail", "live-dkim", "workspace", "state", "staged", "web-root", "directory", "bad-sha", "oversize", "recovery-key", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := faultConfig(t)
			identity, _ := age.GenerateX25519Identity()
			asset := preservationAssetFixture(t, filepath.Join(cfg.ProjectDirectory, "explicit-asset.bin"), []byte("synthetic deployment asset"))
			cfg.Preservation = &PreservationConfig{Recipient: identity.Recipient().String(), IdentityEnv: "SYNTHETIC_PRESERVATION_IDENTITY", Assets: []PathDigest{asset}}
			switch scenario {
			case "bad-recipient":
				cfg.Preservation.Recipient = "invalid-public-key"
			case "private-recipient":
				cfg.Preservation.Recipient = identity.String()
			case "bad-env-reference":
				cfg.Preservation.IdentityEnv = "AGE-SECRET-KEY-do-not-echo"
			case "too-many":
				cfg.Preservation.Assets = make([]PathDigest, 25)
			case "duplicate":
				cfg.Preservation.Assets = append(cfg.Preservation.Assets, asset)
			case "hardlink":
				alias := filepath.Join(cfg.ProjectDirectory, "hardlink-asset.bin")
				if err := os.Link(asset.Path, alias); err != nil {
					t.Skip("filesystem hardlinks unavailable")
				}
				copy := asset
				copy.Path = alias
				cfg.Preservation.Assets = append(cfg.Preservation.Assets, copy)
			case "compose-duplicate":
				cfg.Preservation.Assets[0] = cfg.ComposeFiles[0]
			case "symlink":
				alias := filepath.Join(cfg.ProjectDirectory, "symlink-asset.bin")
				if err := os.Symlink(asset.Path, alias); err != nil {
					t.Skip("filesystem symlinks unavailable")
				}
				cfg.Preservation.Assets[0].Path = alias
			case "env-duplicate":
				cfg.Preservation.Assets[0] = cfg.EnvironmentFile
			case "live-data", "live-mail", "live-dkim", "workspace", "state":
				root := map[string]string{"live-data": cfg.Targets.Data, "live-mail": cfg.Targets.Mail, "live-dkim": cfg.Targets.DKIM, "workspace": cfg.Workspace, "state": cfg.StateDir}[scenario]
				cfg.Preservation.Assets[0] = preservationAssetFixture(t, filepath.Join(root, "asset.bin"), []byte("excluded runtime material"))
			case "staged":
				cfg.Preservation.Assets[0] = preservationAssetFixture(t, filepath.Join(cfg.Staged, "staged-input.bin"), []byte("must not preserve restore inputs as deployment assets"))
			case "web-root":
				cfg.Staged, cfg.ExpectedReportSHA256 = "", ""
				cfg.WebJob = "bkj_" + strings.Repeat("a", 32)
				cfg.WebRestoreRoot = filepath.Join(filepath.Dir(cfg.StateDir), "web-recovery")
				if err := os.Mkdir(cfg.WebRestoreRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				metadata := preservationAssetFixture(t, filepath.Join(cfg.Targets.Data, "synthetic-state.db"), []byte("synthetic metadata fixture; never opened as SQL"))
				cfg.WebStateDB = metadata.Path
				cfg.Preservation.Assets[0] = preservationAssetFixture(t, filepath.Join(cfg.WebRestoreRoot, "web-input.bin"), []byte("must not preserve authenticated web restore source as asset"))
			case "directory":
				cfg.Preservation.Assets[0].Path = cfg.ProjectDirectory
			case "bad-sha":
				cfg.Preservation.Assets[0].SHA256 = strings.Repeat("0", 64)
			case "oversize":
				cfg.Preservation.Assets[0] = preservationAssetFixture(t, asset.Path, bytes.Repeat([]byte{'x'}, (8<<20)+1))
			case "recovery-key":
				cfg.Preservation.Assets[0] = preservationAssetFixture(t, asset.Path, []byte(identity.String()))
			case "missing":
				if err := os.Remove(asset.Path); err != nil {
					t.Fatal(err)
				}
			}
			data, _ := json.Marshal(cfg)
			if _, err := DecodeConfig(bytes.NewReader(data)); err == nil || strings.Contains(err.Error(), identity.String()) || strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("unsafe preservation config accepted or leaked")
			}
		})
	}
}

func TestMaintenancePreservationRejectsAgeIdentityInAutomaticPinsBeforeController(t *testing.T) {
	for _, source := range []string{"compose", "current-env", "next-env", "cross-64k-boundary"} {
		t.Run(source, func(t *testing.T) {
			cfg := faultConfig(t)
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			content := []byte("SYNTHETIC_MISPLACED_IDENTITY=" + identity.String() + "\n")
			switch source {
			case "compose":
				cfg.ComposeFiles[0] = preservationAssetFixture(t, cfg.ComposeFiles[0].Path, content)
			case "current-env":
				cfg.EnvironmentFile = preservationAssetFixture(t, cfg.EnvironmentFile.Path, content)
			case "cross-64k-boundary":
				content = append(bytes.Repeat([]byte{'x'}, (64<<10)-5), []byte(identity.String())...)
				cfg.EnvironmentFile = preservationAssetFixture(t, cfg.EnvironmentFile.Path, content)
			case "next-env":
				cfg.Driver = "postgres"
				cfg.TargetDSNEnv = "SYNTHETIC_TARGET_DSN"
				cfg.RuntimeDatabase = &RuntimeDatabaseBinding{Host: "postgres", Port: 5432, Database: "synthetic_next", User: "synthetic_user"}
				next := preservationAssetFixture(t, filepath.Join(cfg.ProjectDirectory, "next.env"), content)
				cfg.NextEnvironmentFile = &next
			}
			// Nil policy retains legacy streaming/hash validation; the new rule is
			// enforced only when opting into this separate preservation mechanism.
			if err := VerifyPinnedFiles(cfg); err != nil {
				t.Fatal("nil preservation changed legacy pin behavior", err)
			}
			cfg.Preservation = &PreservationConfig{Recipient: identity.Recipient().String(), IdentityEnv: "SYNTHETIC_IDENTITY_NOT_LOOKED_UP"}
			before, _ := ConfigHash(cfg)
			if err := VerifyPinnedFiles(cfg); err == nil || strings.Contains(err.Error(), identity.String()) {
				t.Fatal("matching pin allowed embedded recovery identity or leaked it")
			}
			after, _ := ConfigHash(cfg)
			if before != after {
				t.Fatal("sensitive pin rejection modified configuration binding")
			}
			controller := &faultController{t: t}
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				t.Fatal("embedded recovery identity reached install")
				return install.JournalInfo{}, nil
			}, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
			if _, err := Run(t.Context(), cfg, controller, actions); err == nil || len(controller.calls) != 0 {
				t.Fatal("embedded identity reached controller preflight before being rejected")
			}
		})
	}
}
