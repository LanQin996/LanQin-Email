package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeRenderPinRejectsColdResolvedSpecDriftButAllowsSafeStop(t *testing.T) {
	cfg, controller, fake := newFakeComposeDocker(t, "stack")
	if err := controller.Preflight(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	before, err := readComposeRenderPin(cfg, cfg.EnvironmentFile.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	service := fake.deployment.Services["web"]
	service.Environment["SYNTHETIC_UNPINNED_INCLUDE"] = composeString("changed-spec")
	fake.deployment.Services["web"] = service
	for i, container := range fake.containers {
		if container.Config.Labels["com.docker.compose.service"] == "web" {
			fake.containers[i].Config.Env = append(fake.containers[i].Config.Env, "SYNTHETIC_UNPINNED_INCLUDE=changed-spec")
		}
	}
	cold := fake.controller()
	if err := cold.Preflight(t.Context(), cfg); err == nil {
		t.Fatal("cold controller accepted changed resolved spec with identical pinned top-level files")
	}
	if err := cold.PreflightRecovery(t.Context(), cfg); err != nil {
		t.Fatal("safe recovery metadata could not reach Stop", err)
	}
	if err := cold.Stop(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := cold.VerifyStopped(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	startCalls := len(fake.calls)
	if err := cold.StartOriginal(t.Context(), cfg); err == nil {
		t.Fatal("changed render authorized original start")
	}
	if err := cold.Recreate(t.Context(), cfg); err == nil {
		t.Fatal("changed render authorized replacement")
	}
	for _, call := range fake.calls[startCalls:] {
		if call[0] == "compose" && (call[len(composePrefix(cfg))] == "up" || call[len(composePrefix(cfg))] == "start") {
			t.Fatal("untrusted cold spec changed writers")
		}
	}
	after, err := readComposeRenderPin(cfg, cfg.EnvironmentFile.SHA256)
	if err != nil || after != before {
		t.Fatal("recovery overwrote immutable original render pin", err)
	}
	// Restore the original observed spec; recovery can now preserve/start its
	// original containers without inventing a replacement render baseline.
	delete(service.Environment, "SYNTHETIC_UNPINNED_INCLUDE")
	fake.deployment.Services["web"] = service
	for i, container := range fake.containers {
		if container.Config.Labels["com.docker.compose.service"] == "web" {
			environment := []string{}
			for _, value := range container.Config.Env {
				if !strings.HasPrefix(value, "SYNTHETIC_UNPINNED_INCLUDE=") {
					environment = append(environment, value)
				}
			}
			fake.containers[i].Config.Env = environment
		}
	}
	if err := cold.StartOriginal(t.Context(), cfg); err != nil {
		t.Fatal("restored pinned original render could not start original containers", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, composeRenderPinName(cfg.EnvironmentFile.SHA256)))
	if err != nil || strings.Contains(string(data), "changed-spec") || strings.Contains(string(data), "synthetic-") {
		t.Fatal("render pin persisted private environment instead of hashes", err)
	}
}

func TestComposeColdRecoveryDoesNotReplaceMissingOriginalRenderPin(t *testing.T) {
	cfg, controller, fake := newFakeComposeDocker(t, "all-in-one")
	if err := controller.Preflight(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := controller.Stop(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(cfg.StateDir, composeRenderPinName(cfg.EnvironmentFile.SHA256))
	if err := os.Rename(name, name+".retained"); err != nil {
		t.Fatal(err)
	}
	cold := fake.controller()
	if err := cold.PreflightRecovery(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := cold.VerifyStopped(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := cold.EnsureEmptyPostfixQueues(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := cold.StartOriginal(t.Context(), cfg); err == nil {
		t.Fatal("missing old render pin was silently recreated")
	}
	if err := cold.Recreate(t.Context(), cfg); err == nil {
		t.Fatal("missing old render pin authorized replacement")
	}
	if _, err := os.Lstat(name); !os.IsNotExist(err) {
		t.Fatal("cold recovery wrote a new trusted old slot", err)
	}
}

func TestComposeNextRenderPinOnlyCreatedByInitialPrecommitOperation(t *testing.T) {
	for _, mode := range []string{"initial-operation", "cold-recovery", "same-controller-recovery", "wrong-initial-phase"} {
		t.Run(mode, func(t *testing.T) {
			cfg := environmentConfig(t)
			fake := &fakeComposeDocker{t: t, cfg: cfg}
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
			initial := fake.controller()
			if err := initial.Preflight(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := initial.Stop(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.EnvironmentFile.Path, []byte(nextEnvironmentFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			oldContainers := append([]composeContainer{}, fake.containers...)
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "new", "new", "synthetic-next-secret"})
			fake.containers = oldContainers
			fakeComposePhase(t, cfg, "installed", "validate")
			controller := initial
			if mode == "cold-recovery" {
				controller = fake.controller()
			}
			if mode == "cold-recovery" || mode == "same-controller-recovery" {
				if err := controller.PreflightRecovery(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong-initial-phase" {
				fakeComposePhase(t, cfg, "stopping", "stop")
			}
			err := controller.ValidateNativeRuntime(t.Context(), cfg, strings.Repeat("b", 64))
			if (err == nil) != (mode == "initial-operation") {
				t.Fatal("next slot creation authorization incorrect", mode, err)
			}
			name := filepath.Join(cfg.StateDir, composeRenderPinName(cfg.NextEnvironmentFile.SHA256))
			if mode == "initial-operation" {
				if _, err := readComposeRenderPin(cfg, cfg.NextEnvironmentFile.SHA256); err != nil {
					t.Fatal("initial installed operation did not pin next render", err)
				}
				cold := fake.controller()
				if err := cold.PreflightRecovery(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				if err := cold.ValidateNativeRuntime(t.Context(), cfg, strings.Repeat("b", 64)); err != nil {
					t.Fatal("cold controller could not use previously pinned next render", err)
				}
			} else {
				if _, err := os.Lstat(name); !os.IsNotExist(err) {
					t.Fatal("cold/wrong-phase controller created unknown next slot", err)
				}
				if err := controller.Stop(t.Context(), cfg); err != nil {
					t.Fatal("missing next pin blocked controlled stop", err)
				}
				if err := controller.EnsureEmptyPostfixQueues(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				if err := controller.Recreate(t.Context(), cfg); err == nil {
					t.Fatal("missing next pin authorized new writers")
				}
				// Explicit rollback can return the trusted old env/spec, then start
				// its original containers. No unknown next pin is required or written.
				if err := os.WriteFile(cfg.EnvironmentFile.Path, []byte(originalEnvironmentFixture), 0o600); err != nil {
					t.Fatal(err)
				}
				fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
				fake.containers = oldContainers
				if err := controller.StartOriginal(t.Context(), cfg); err != nil {
					t.Fatal("rollback to original pinned env could not preserve old containers", err)
				}
			}
		})
	}
}
