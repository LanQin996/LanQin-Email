package maintenance

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/preservation"
)

type composePreservationFixture struct {
	cfg        Config
	controller *ComposeController
	docker     *fakeComposeDocker
	identity   *age.X25519Identity
	postfixID  string
	archive    []byte
	asset      []byte
}

func newComposePreservationFixture(t *testing.T, quarantine bool) *composePreservationFixture {
	t.Helper()
	cfg := faultConfig(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	asset := []byte("synthetic-tls-key-material-do-not-echo")
	assetPath := filepath.Join(cfg.ProjectDirectory, "synthetic-tls.key")
	if err := os.WriteFile(assetPath, asset, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(asset)
	cfg.Preservation = &PreservationConfig{Recipient: identity.Recipient().String(), IdentityEnv: "SYNTHETIC_MAINTENANCE_PRESERVATION_IDENTITY", Assets: []PathDigest{{Path: assetPath, SHA256: hex.EncodeToString(digest[:])}}, QuarantineQueues: quarantine}
	t.Setenv(cfg.Preservation.IdentityEnv, identity.String())
	docker := &fakeComposeDocker{t: t, cfg: cfg}
	docker.setDeployment("sqlite", composeDatabaseIdentity{})
	controller := docker.controller()
	var postfixID string
	for _, container := range docker.containers {
		if container.Config.Labels["com.docker.compose.service"] == postfixService(cfg) {
			postfixID = container.ID
		}
	}
	archive := queuedPreservationTar(t, "synthetic-queue-body-A-do-not-echo")
	docker.spoolArchives = map[string][]byte{postfixID: archive}
	return &composePreservationFixture{cfg, controller, docker, identity, postfixID, archive, asset}
}

func queuedPreservationTar(t *testing.T, payload string) []byte {
	t.Helper()
	data := postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/deferred/synthetic-message", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(payload))}})
	needle := bytes.Repeat([]byte{'x'}, len(payload))
	index := bytes.Index(data, needle)
	if index < 0 {
		t.Fatal("fixture queue payload absent")
	}
	copy(data[index:index+len(payload)], payload)
	return data
}

func preservationReceipts(t *testing.T, cfg Config) map[string]preservationReceipt {
	t.Helper()
	entries, err := os.ReadDir(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]preservationReceipt{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "preservation-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cfg.StateDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var receipt preservationReceipt
		if json.Unmarshal(data, &receipt) != nil {
			t.Fatal("invalid receipt fixture")
		}
		result[entry.Name()] = receipt
	}
	return result
}

func preservationStartedSince(cfg Config, calls [][]string) bool {
	for _, call := range calls {
		if len(call) > len(composePrefix(cfg)) && call[0] == "compose" && call[len(composePrefix(cfg))] == "up" {
			return true
		}
	}
	return false
}

func prepareStoppedPreservation(t *testing.T, fixture *composePreservationFixture) {
	t.Helper()
	if err := fixture.controller.Preflight(t.Context(), fixture.cfg); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controller.Stop(t.Context(), fixture.cfg); err != nil {
		t.Fatal(err)
	}
}

func TestComposePreservationVerifiesOpaqueNonemptyQueueAndAssetsBeforeReplacement(t *testing.T) {
	f := newComposePreservationFixture(t, true)
	prepareStoppedPreservation(t, f)
	if err := f.controller.EnsureEmptyPostfixQueues(t.Context(), f.cfg); err == nil {
		t.Fatal("optional preservation weakened legacy empty-queue gate")
	}
	if err := f.controller.EnsureSafePostfixQueues(t.Context(), f.cfg); err != nil {
		t.Fatal("explicit queue quarantine could not preserve evidence", err)
	}
	items, want, err := f.controller.preservationSnapshot(t.Context(), f.cfg, mustPreservationState(t, f.cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer clearPreservationItems(items)
	receipts := preservationReceipts(t, f.cfg)
	if len(receipts) != 1 {
		t.Fatal("verified preservation receipt not durably published")
	}
	var receiptName string
	var receipt preservationReceipt
	for name, item := range receipts {
		receiptName, receipt = name, item
	}
	cipherName := strings.TrimSuffix(receiptName, ".json") + ".age"
	cipher, err := os.ReadFile(filepath.Join(f.cfg.StateDir, cipherName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{f.identity.String(), "synthetic-queue-body-A-do-not-echo", string(f.asset), "never-echo-coordinator-secret"} {
		if bytes.Contains(cipher, []byte(secret)) {
			t.Fatal("preservation ciphertext exposed source or identity")
		}
	}
	if err := preservation.Verify(t.Context(), bytes.NewReader(cipher), f.identity.String(), receipt.CipherSHA256, want, preservation.DefaultLimits()); err != nil {
		t.Fatal("published file did not match actual captured source manifest", err)
	}
	output := filepath.Join(t.TempDir(), "isolated-evidence")
	manifest, err := preservation.Restore(t.Context(), bytes.NewReader(cipher), output, f.identity.String(), receipt.CipherSHA256, preservation.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	foundQueue, foundAsset := false, false
	for index, entry := range manifest.Entries {
		data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(preservation.PayloadName(index))))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(entry.Name, "spools/") {
			foundQueue = true
			if !bytes.Equal(data, f.archive) {
				t.Fatal("opaque queued-message tar changed")
			}
		}
		if entry.Source == f.cfg.Preservation.Assets[0].Path {
			foundAsset = true
			if !bytes.Equal(data, f.asset) {
				t.Fatal("explicit asset changed")
			}
		}
	}
	if !foundQueue || !foundAsset {
		t.Fatal("bundle omitted queue or explicit deployment asset")
	}
	if _, err := os.Lstat(filepath.Join(output, "postfix")); !os.IsNotExist(err) {
		t.Fatal("isolated restoration expanded the Postfix tar")
	}
	baseRunner := f.docker.run
	upChecked := false
	f.controller.run = func(ctx context.Context, cfg Config, args []string) ([]byte, error) {
		if len(args) > len(composePrefix(cfg)) && args[0] == "compose" && args[len(composePrefix(cfg))] == "up" {
			upChecked = true
			receipts := preservationReceipts(t, cfg)
			if len(receipts) != 1 {
				t.Fatal("replacement preceded verified receipt")
			}
			file, err := os.Open(filepath.Join(cfg.StateDir, cipherName))
			if err != nil {
				t.Fatal(err)
			}
			verifyErr := preservation.Verify(ctx, file, f.identity.String(), receipt.CipherSHA256, want, preservation.DefaultLimits())
			file.Close()
			if verifyErr != nil {
				t.Fatal("replacement saw unverified actual bundle", verifyErr)
			}
		}
		return baseRunner(ctx, cfg, args)
	}
	if err := f.controller.Recreate(t.Context(), f.cfg); err != nil || !upChecked {
		t.Fatal("verified preservation never reached controlled replacement", err)
	}
}

func mustPreservationState(t *testing.T, cfg Config) State {
	t.Helper()
	state, err := ReadState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestComposePreservationRequiresExplicitQuarantineConsentAndMatchingIdentity(t *testing.T) {
	for _, scenario := range []string{"no-consent", "missing-key", "wrong-key", "invalid-key"} {
		t.Run(scenario, func(t *testing.T) {
			f := newComposePreservationFixture(t, scenario != "no-consent")
			prepareStoppedPreservation(t, f)
			switch scenario {
			case "missing-key":
				t.Setenv(f.cfg.Preservation.IdentityEnv, "")
			case "invalid-key":
				t.Setenv(f.cfg.Preservation.IdentityEnv, "private-invalid-value-do-not-echo")
			case "wrong-key":
				other, err := age.GenerateX25519Identity()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(f.cfg.Preservation.IdentityEnv, other.String())
			}
			before := len(f.docker.calls)
			err := f.controller.Recreate(t.Context(), f.cfg)
			if err == nil || strings.Contains(err.Error(), "private-invalid") {
				t.Fatal("unconsented/unrecoverable queued data authorized replacement")
			}
			if preservationStartedSince(f.cfg, f.docker.calls[before:]) {
				t.Fatal("invalid preservation reached container replacement")
			}
			if err := f.controller.StartOriginal(t.Context(), f.cfg); err != nil {
				t.Fatal("refusal lost original queued-mail container recovery", err)
			}
		})
	}
}

func TestComposePreservationColdReuseAndInterruptedPublicationNeverTrustExistence(t *testing.T) {
	for _, damage := range []string{"", "cipher-tampered", "receipt-tampered", "cipher-missing", "receipt-missing"} {
		t.Run(damage, func(t *testing.T) {
			f := newComposePreservationFixture(t, true)
			prepareStoppedPreservation(t, f)
			if err := f.controller.EnsureSafePostfixQueues(t.Context(), f.cfg); err != nil {
				t.Fatal(err)
			}
			receipts := preservationReceipts(t, f.cfg)
			var receiptName string
			for name := range receipts {
				receiptName = name
			}
			cipherName := strings.TrimSuffix(receiptName, ".json") + ".age"
			cipherPath := filepath.Join(f.cfg.StateDir, cipherName)
			receiptPath := filepath.Join(f.cfg.StateDir, receiptName)
			original, err := os.ReadFile(cipherPath)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "cipher-tampered":
				data := append([]byte{}, original...)
				data[len(data)-1] ^= 1
				if err := os.WriteFile(cipherPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "receipt-tampered":
				if err := os.WriteFile(receiptPath, []byte(`{"version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "cipher-missing":
				if err := os.Rename(cipherPath, cipherPath+".retained"); err != nil {
					t.Fatal(err)
				}
			case "receipt-missing":
				if err := os.Rename(receiptPath, receiptPath+".retained"); err != nil {
					t.Fatal(err)
				}
			}
			cold := f.docker.controller()
			if err := cold.PreflightRecovery(t.Context(), f.cfg); err != nil {
				t.Fatal(err)
			}
			err = cold.EnsureSafePostfixQueues(t.Context(), f.cfg)
			allow := damage == "" || damage == "receipt-missing"
			if (err == nil) != allow {
				t.Fatal("cold/interrupted preservation validation decision incorrect", damage, err)
			}
			if allow {
				data, err := os.ReadFile(cipherPath)
				if err != nil || !bytes.Equal(data, original) || len(preservationReceipts(t, f.cfg)) != 1 {
					t.Fatal("verified cold reuse rewrote or lost immutable evidence", err)
				}
			} else {
				before := len(f.docker.calls)
				if err := cold.Recreate(t.Context(), f.cfg); err == nil || preservationStartedSince(f.cfg, f.docker.calls[before:]) {
					t.Fatal("damaged preservation authorized destruction")
				}
			}
		})
	}
}

func TestComposePreservationChangedQueueProducesNewVerifiedBundle(t *testing.T) {
	f := newComposePreservationFixture(t, true)
	prepareStoppedPreservation(t, f)
	if err := f.controller.EnsureSafePostfixQueues(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	old := preservationReceipts(t, f.cfg)
	f.docker.spoolArchives[f.postfixID] = queuedPreservationTar(t, "synthetic-queue-body-B-do-not-echo")
	if err := f.controller.EnsureSafePostfixQueues(t.Context(), f.cfg); err != nil {
		t.Fatal("changed stopped queue not freshly captured", err)
	}
	newReceipts := preservationReceipts(t, f.cfg)
	if len(old) != 1 || len(newReceipts) != 2 {
		t.Fatal("changed queue reused stale snapshot receipt")
	}
	for name, receipt := range old {
		if newReceipts[name] != receipt {
			t.Fatal("new queue replaced earlier preservation receipt")
		}
	}
	// A later committed deployment uses new container IDs even when image and
	// queue bytes match. That new writable layer needs a new bound snapshot.
	for i, container := range f.docker.containers {
		if container.ID == f.postfixID {
			next := strings.Repeat("e", 64)
			f.docker.containers[i].ID = next
			f.docker.spoolArchives[next] = f.docker.spoolArchives[f.postfixID]
		}
	}
	if err := f.controller.EnsureSafePostfixQueues(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if len(preservationReceipts(t, f.cfg)) != 3 {
		t.Fatal("changed container set reused old writable-layer authority")
	}
}

func TestComposePreservationChangedAssetAndCapacityFailBeforeReplacement(t *testing.T) {
	for _, scenario := range []string{"asset-changed", "changed-during-verification", "capacity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newComposePreservationFixture(t, true)
			prepareStoppedPreservation(t, f)
			switch scenario {
			case "asset-changed":
				if err := os.WriteFile(f.cfg.Preservation.Assets[0].Path, []byte("private-changed-asset-do-not-echo"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-during-verification":
				base := f.docker.run
				copies := 0
				f.controller.run = func(ctx context.Context, cfg Config, args []string) ([]byte, error) {
					if len(args) > 0 && args[0] == "cp" {
						copies++
						if copies == 2 {
							if err := os.WriteFile(cfg.Preservation.Assets[0].Path, []byte("private-changed-asset-do-not-echo"), 0o600); err != nil {
								t.Fatal(err)
							}
						}
					}
					return base(ctx, cfg, args)
				}
			case "capacity":
				for i := 0; i < 16; i++ {
					name := filepath.Join(f.cfg.StateDir, fmt.Sprintf("preservation-capacity-%02d.age", i))
					if err := os.WriteFile(name, []byte("reserved-private-slot"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := len(f.docker.calls)
			err := f.controller.Recreate(t.Context(), f.cfg)
			if err == nil || strings.Contains(err.Error(), "private-changed") || preservationStartedSince(f.cfg, f.docker.calls[before:]) {
				t.Fatal("changed/unbounded preservation authorized replacement or leaked input")
			}
		})
	}
}

var preservationEnvHelper = flag.Bool("lanqin-preservation-env-helper", false, "private test subprocess only")

func TestComposePreservationDockerEnvironmentHelper(t *testing.T) {
	if !*preservationEnvHelper {
		t.Skip("private subprocess helper")
	}
	if os.Getenv("HOME") != "" {
		t.Fatal("preservation identity inherited by Docker process")
	}
	fmt.Println("PRESERVATION_IDENTITY_NOT_INHERITED")
}

func TestComposePreservationIdentityWhitelistAliasNotPassedToDocker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production runner is Linux only")
	}
	cfg := faultConfig(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Preservation = &PreservationConfig{Recipient: identity.Recipient().String(), IdentityEnv: "HOME"}
	t.Setenv("HOME", identity.String())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := runComposeDocker(ctx, cfg, []string{"-test.run=^TestComposePreservationDockerEnvironmentHelper$", "-lanqin-preservation-env-helper=true"})
	if err != nil || !bytes.Contains(output, []byte("PRESERVATION_IDENTITY_NOT_INHERITED")) || bytes.Contains(output, []byte(identity.String())) {
		t.Fatal("explicit identity env alias was passed to Docker or helper failed", err)
	}
}
