package maintenance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeOriginalStartPreservesDurableIDsAndHostInodes(t *testing.T) {
	for _, damage := range []string{"", "container-replaced", "root-replaced", "baseline-missing", "baseline-modified", "image-retagged"} {
		t.Run(damage, func(t *testing.T) {
			cfg, controller, fake := newFakeComposeDocker(t, "stack")
			if err := controller.Preflight(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := controller.Stop(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "container-replaced":
				for i, container := range fake.containers {
					if container.Config.Labels["com.docker.compose.service"] == "postfix" {
						fake.containers[i].ID = strings.Repeat("f", 64)
					}
				}
			case "root-replaced":
				fake.directoryIDs[cfg.Targets.Data] = "different-kernel-directory-identity"
			case "baseline-missing":
				if err := os.Rename(filepath.Join(cfg.StateDir, composeOriginalName), filepath.Join(cfg.StateDir, "retained-original-baseline")); err != nil {
					t.Fatal(err)
				}
			case "baseline-modified":
				if err := os.WriteFile(filepath.Join(cfg.StateDir, composeOriginalName), []byte(`{"version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "image-retagged":
				fake.imageChanged = true
			}
			// A new controller proves recovery does not rely on process memory.
			recovered := fake.controller()
			before := len(fake.calls)
			err := recovered.StartOriginal(t.Context(), cfg)
			if (err == nil) != (damage == "") {
				t.Fatal("original-start identity proof incorrect", damage, err)
			}
			started := false
			for _, call := range fake.calls[before:] {
				if call[0] == "compose" {
					operation := call[len(composePrefix(cfg))]
					if operation == "up" {
						t.Fatal("original start replaced writable layers")
					}
					if operation == "start" {
						started = true
					}
				}
			}
			if started != (damage == "") {
				t.Fatal("unproven original-start changed services")
			}
		})
	}
}

func TestComposeRecoveryPreflightIgnoresOnlyStoppedRuntimeEnvironmentDrift(t *testing.T) {
	cfg := environmentConfig(t)
	fake := &fakeComposeDocker{t: t, cfg: cfg}
	fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
	controller := fake.controller()
	if err := controller.Preflight(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := controller.Stop(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	for i, container := range fake.containers {
		if appContainer(cfg, container) {
			fake.containers[i].Config.Env = append(fake.containers[i].Config.Env, "SYNTHETIC_RECOVERY_ENV_DRIFT=changed")
		}
	}
	// Metadata-only recovery preflight permits controlled Stop. It cannot
	// establish a new baseline or authorize original-container Start.
	if err := controller.PreflightRecovery(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	for i, container := range fake.containers {
		if appContainer(cfg, container) {
			fake.containers[i].Config.Env = []string{"LANQIN_DB_DRIVER=postgres", "LANQIN_DATABASE_URL=postgres://wrong:synthetic@database:5432/wrong"}
		}
	}
	if err := controller.PreflightRecovery(t.Context(), cfg); err != nil {
		t.Fatal("repaired env could not reach controlled stop metadata proof", err)
	}
	if err := controller.StartOriginal(t.Context(), cfg); err == nil {
		t.Fatal("metadata-only preflight authorized mismatched original environment")
	}
	if err := controller.VerifyStopped(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	baseline, err := readComposeOriginal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(baseline)
	if strings.Contains(string(encoded), "synthetic-old-secret") {
		t.Fatal("original baseline stored DSN/password")
	}
}
