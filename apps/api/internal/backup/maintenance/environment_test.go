package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/install"
)

const originalEnvironmentFixture = "LANQIN_DB_DRIVER=postgres\nLANQIN_DATABASE_URL=postgres://old:synthetic-old-secret@database/old\nLANQIN_DB_NAME=old\n"
const nextEnvironmentFixture = "LANQIN_DB_DRIVER=postgres\nLANQIN_DATABASE_URL=postgres://new:synthetic-next-secret@database/new\nLANQIN_DB_NAME=new\n"

func environmentConfig(t *testing.T) Config {
	t.Helper()
	cfg := faultConfig(t)
	if err := os.WriteFile(cfg.EnvironmentFile.Path, []byte(originalEnvironmentFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	original := sha256.Sum256([]byte(originalEnvironmentFixture))
	cfg.EnvironmentFile.SHA256 = hex.EncodeToString(original[:])
	nextPath := filepath.Join(t.TempDir(), "private-next.env")
	if err := os.WriteFile(nextPath, []byte(nextEnvironmentFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	next := sha256.Sum256([]byte(nextEnvironmentFixture))
	cfg.NextEnvironmentFile = &PathDigest{Path: nextPath, SHA256: hex.EncodeToString(next[:])}
	cfg.Driver, cfg.TargetDSNEnv = "postgres", "SYNTHETIC_NEW_DATABASE_DSN"
	cfg.RuntimeDatabase = &RuntimeDatabaseBinding{Host: "database", Port: 5432, Database: "new", User: "new"}
	return cfg
}

// Windows exercises real file copies and renames in its private fixture, while
// explicitly simulating Linux directory fsync and numeric owner checks. The
// production API retains its Linux-only gate and uses RENAME_NOREPLACE.
func testEnvironmentOps() environmentOps {
	return environmentOps{rename: func(source, target string) error {
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("synthetic no-replace target exists")
		}
		return os.Rename(source, target)
	}, syncDirectory: func(string) error { return nil }}
}

func assertEnvironmentBytes(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != want {
		t.Fatal("environment content not preserved", name, err)
	}
}

func TestEnvironmentTransactionKeepsSameParentOriginalAndIdempotentRollback(t *testing.T) {
	cfg := environmentConfig(t)
	ops := testEnvironmentOps()
	paths, err := checkedEnvironmentPaths(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(paths.old) != filepath.Dir(cfg.EnvironmentFile.Path) || filepath.Dir(paths.staged) != filepath.Dir(cfg.EnvironmentFile.Path) || filepath.Dir(paths.nextSaved) != filepath.Dir(cfg.EnvironmentFile.Path) {
		t.Fatal("environment rename would cross filesystem parents")
	}
	if status, err := EnvironmentStatus(cfg); err != nil || status != "original" {
		t.Fatal("fresh environment not recognized", status, err)
	}
	if err := applyEnvironment(t.Context(), cfg, ops); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentBytes(t, paths.live, nextEnvironmentFixture)
	assertEnvironmentBytes(t, paths.old, originalEnvironmentFixture)
	assertEnvironmentBytes(t, cfg.NextEnvironmentFile.Path, nextEnvironmentFixture)
	if status, err := EnvironmentStatus(cfg); err != nil || status != "next" {
		t.Fatal("committed environment not recognized", status, err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyEnvironment(t.Context(), cfg, ops); err != nil {
		t.Fatal("completed apply not idempotent", err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repeated apply changed durable journal")
	}
	for _, secret := range []string{"synthetic-old-secret", "synthetic-next-secret", "postgres://"} {
		if bytes.Contains(after, []byte(secret)) {
			t.Fatal("environment contents/credentials entered journal")
		}
	}
	if err := rollbackEnvironment(t.Context(), cfg, ops); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentBytes(t, paths.live, originalEnvironmentFixture)
	assertEnvironmentBytes(t, paths.nextSaved, nextEnvironmentFixture)
	if status, err := EnvironmentStatus(cfg); err != nil || status != "original" {
		t.Fatal("rollback environment not original", status, err)
	}
	before, err = os.ReadFile(paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackEnvironment(t.Context(), cfg, ops); err != nil {
		t.Fatal("repeat environment rollback failed", err)
	}
	after, err = os.ReadFile(paths.journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repeat rollback changed journal")
	}
}

func TestEnvironmentTransactionPreservesValidEmptyOriginalEnvFile(t *testing.T) {
	cfg := environmentConfig(t)
	if err := os.WriteFile(cfg.EnvironmentFile.Path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	cfg.EnvironmentFile.SHA256 = hex.EncodeToString(digest[:])
	if err := applyEnvironment(t.Context(), cfg, testEnvironmentOps()); err != nil {
		t.Fatal("valid empty original env file rejected", err)
	}
	if err := rollbackEnvironment(t.Context(), cfg, testEnvironmentOps()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.EnvironmentFile.Path)
	if err != nil || len(data) != 0 {
		t.Fatal("empty original env file was not preserved", err)
	}
}

func TestEnvironmentTransactionFaultsPreserveOriginalAndPermitExplicitRollback(t *testing.T) {
	for _, failure := range []string{"cancel-before", "save-old", "publish-new", "sync-before-save", "sync-after-save", "sync-after-publish", "cancel-after-save"} {
		t.Run(failure, func(t *testing.T) {
			cfg := environmentConfig(t)
			ops := testEnvironmentOps()
			originalOps := ops
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			renames, syncs := 0, 0
			ops.rename = func(source, target string) error {
				renames++
				if failure == "save-old" && renames == 1 || failure == "publish-new" && renames == 2 {
					return errors.New("synthetic-next-secret provider failure")
				}
				if err := originalOps.rename(source, target); err != nil {
					return err
				}
				if failure == "cancel-after-save" && renames == 1 {
					cancel()
				}
				return nil
			}
			ops.syncDirectory = func(string) error {
				syncs++
				if failure == "sync-before-save" && syncs == 2 || failure == "sync-after-save" && syncs == 3 || failure == "sync-after-publish" && syncs == 4 {
					return errors.New("synthetic-old-secret sync error")
				}
				return nil
			}
			if failure == "cancel-before" {
				cancel()
			}
			err := applyEnvironment(ctx, cfg, ops)
			if err == nil || strings.Contains(err.Error(), "synthetic-") {
				t.Fatal("fault apply succeeded or leaked environment error")
			}
			paths, pathErr := checkedEnvironmentPaths(cfg)
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			if failure == "cancel-before" {
				if !environmentAbsent(paths.journal) || renames != 0 {
					t.Fatal("canceled environment request touched state")
				}
			}
			if failure == "sync-after-save" || failure == "publish-new" || failure == "cancel-after-save" || failure == "sync-after-publish" {
				if status, err := EnvironmentStatus(cfg); status != "incomplete" || err == nil {
					t.Fatal("uncertain switch claimed complete", status)
				}
			}
			if err := rollbackEnvironment(t.Context(), cfg, originalOps); err != nil {
				t.Fatal("explicit rollback could not recover known files", err)
			}
			assertEnvironmentBytes(t, paths.live, originalEnvironmentFixture)
			if status, err := EnvironmentStatus(cfg); err != nil || status != "original" {
				t.Fatal("recovered original environment unrecognized", status, err)
			}
		})
	}
}

func TestEnvironmentTransactionRejectsOverwritesSymlinksTamperingAndChangedConfig(t *testing.T) {
	for _, scenario := range []string{"existing-new", "existing-old", "changed-original", "changed-next-source", "changed-backup", "changed-live-after-apply", "changed-journal", "changed-config"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := environmentConfig(t)
			ops := testEnvironmentOps()
			paths, err := checkedEnvironmentPaths(cfg)
			if err != nil {
				t.Fatal(err)
			}
			preapply := scenario == "existing-new" || scenario == "existing-old" || scenario == "changed-original" || scenario == "changed-next-source"
			if !preapply {
				if err := applyEnvironment(t.Context(), cfg, ops); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "existing-new":
				if err := os.WriteFile(paths.staged, []byte("unrelated sibling"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "existing-old":
				if err := os.WriteFile(paths.old, []byte("unrelated original sibling"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-original":
				if err := os.WriteFile(paths.live, []byte("unrecognized old environment"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-next-source":
				if err := os.WriteFile(cfg.NextEnvironmentFile.Path, []byte("unrecognized next source"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-backup":
				if err := os.WriteFile(paths.old, []byte("unrecognized backup"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-live-after-apply":
				if err := os.WriteFile(paths.live, []byte("unrecognized new live"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-journal":
				if err := os.WriteFile(paths.journal, []byte("modified journal"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-config":
				cfg.ProjectName += "-changed"
			}
			liveBefore, _ := os.ReadFile(paths.live)
			if preapply {
				if err := applyEnvironment(t.Context(), cfg, ops); err == nil {
					t.Fatal("untrusted/occupied environment path accepted")
				}
			} else {
				if err := rollbackEnvironment(t.Context(), cfg, ops); err == nil {
					t.Fatal("unknown environment state overwritten during rollback")
				}
			}
			liveAfter, _ := os.ReadFile(paths.live)
			if !bytes.Equal(liveBefore, liveAfter) {
				t.Fatal("refused transaction altered live environment")
			}
		})
	}
	if runtime.GOOS != "linux" {
		cfg := environmentConfig(t)
		if !errors.Is(ApplyEnvironment(t.Context(), cfg), ErrUnsupported) || !errors.Is(RollbackEnvironment(t.Context(), cfg), ErrUnsupported) {
			t.Fatal("non-Linux production environment switching allowed")
		}
	}
}

func TestEnvironmentTransactionRejectsSymlinkInputsAndBackupAndUnsafeLinuxModes(t *testing.T) {
	for _, which := range []string{"live", "next-source", "saved-original"} {
		t.Run(which, func(t *testing.T) {
			cfg := environmentConfig(t)
			ops := testEnvironmentOps()
			paths, err := checkedEnvironmentPaths(cfg)
			if err != nil {
				t.Fatal(err)
			}
			name := paths.live
			if which == "next-source" {
				name = cfg.NextEnvironmentFile.Path
			}
			if which == "saved-original" {
				if err := applyEnvironment(t.Context(), cfg, ops); err != nil {
					t.Fatal(err)
				}
				name = paths.old
			}
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(t.TempDir(), "owned-fixture-original.env")
			if err := os.Rename(name, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, name); err != nil {
				t.Skip("test OS cannot create symlink: " + err.Error())
			}
			if which == "saved-original" {
				if err := rollbackEnvironment(t.Context(), cfg, ops); err == nil {
					t.Fatal("rollback followed symlink backup")
				}
			} else {
				if err := applyEnvironment(t.Context(), cfg, ops); err == nil {
					t.Fatal("environment apply followed symlink input")
				}
			}
			data, err := os.ReadFile(moved)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatal("refused symlink operation changed original file", err)
			}
		})
	}
	if runtime.GOOS == "linux" {
		for _, which := range []string{"live", "next-source", "parent", "state"} {
			t.Run("unsafe-linux-mode-"+which, func(t *testing.T) {
				cfg := environmentConfig(t)
				name := cfg.EnvironmentFile.Path
				mode := os.FileMode(0o644)
				switch which {
				case "next-source":
					name = cfg.NextEnvironmentFile.Path
				case "parent":
					name = filepath.Dir(cfg.EnvironmentFile.Path)
					mode = 0o777
				case "state":
					name = cfg.StateDir
					mode = 0o755
				}
				if err := os.Chmod(name, mode); err != nil {
					t.Fatal(err)
				}
				if err := applyEnvironment(t.Context(), cfg, testEnvironmentOps()); err == nil {
					t.Fatal("unsafe environment mode accepted")
				}
			})
		}
	}
}

func TestEnvironmentIncompleteConfigIsRecoveryOnlyAndNeverRepairedOutsideProjectLock(t *testing.T) {
	cfg := environmentConfig(t)
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal("valid native environment fixture rejected", err)
	}
	transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("6", 32)+".journal"), TransactionID: strings.Repeat("6", 32), RollbackTargets: cfg.Targets}
	stateJournal, err := createStateJournal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configSHA, _ := ConfigHash(cfg)
	state := State{Version: 1, OperationID: strings.Repeat("7", 32), ConfigSHA256: configSHA, Phase: "installing", PendingAction: "install", Transaction: transaction, ServicesStopped: true}
	if err := stateJournal.save(&state); err != nil {
		t.Fatal(err)
	}
	stateJournal.file.Close()
	ops := testEnvironmentOps()
	originalOps := ops
	count := 0
	ops.rename = func(source, target string) error {
		count++
		if count == 2 {
			return errors.New("synthetic crash before next publish")
		}
		return originalOps.rename(source, target)
	}
	if err := applyEnvironment(t.Context(), cfg, ops); err == nil {
		t.Fatal("fixture did not stop between renames")
	}
	if !environmentAbsent(cfg.EnvironmentFile.Path) {
		t.Fatal("fixture should have no live env after saving original")
	}
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("Run accepted incomplete environment")
	}
	if err := ValidateRecoveryConfig(cfg); err != nil {
		t.Fatal("known incomplete private environment blocked explicit recovery", err)
	}
	changed := cfg
	next := *cfg.NextEnvironmentFile
	next.SHA256 = strings.Repeat("0", 64)
	changed.NextEnvironmentFile = &next
	if err := ValidateRecoveryConfig(changed); err == nil {
		t.Fatal("changed next source/digest bypassed recovery config binding")
	}
	controller := &faultController{t: t}
	actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
		t.Fatal("recovery reinstalled data")
		return install.JournalInfo{}, nil
	}, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
	unlock, err := lockProject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, recoverErr := Recover(t.Context(), cfg, controller, actions, "rollback", true)
	unlock()
	if recoverErr == nil || !environmentAbsent(cfg.EnvironmentFile.Path) || len(controller.calls) != 0 {
		t.Fatal("locked recovery repaired environment or invoked services")
	}
	if _, err := Recover(t.Context(), cfg, controller, actions, "resume-start", true); err == nil || !environmentAbsent(cfg.EnvironmentFile.Path) || len(controller.calls) != 0 {
		t.Fatal("incomplete environment resumed services instead of requiring rollback")
	}
	if runtime.GOOS == "linux" {
		state, err := Recover(t.Context(), cfg, controller, actions, "rollback", true)
		if err != nil || !state.RollbackCompleted || !state.OriginalServicesRestored {
			t.Fatal("explicit Linux recovery did not restore env/data before old services", err)
		}
	} else {
		// Public mutation is intentionally Linux-only; this fixture validates the
		// same filesystem restoration through explicitly injected test operations.
		if err := rollbackEnvironment(t.Context(), cfg, originalOps); err != nil {
			t.Fatal(err)
		}
	}
	assertEnvironmentBytes(t, cfg.EnvironmentFile.Path, originalEnvironmentFixture)
	if err := ValidateRecoveryConfig(cfg); err != nil {
		t.Fatal("restored original environment no longer matches pins", err)
	}
}
