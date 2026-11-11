package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/install"
)

func TestMaintenanceConfigExplicitOwnersAndRecoveryInputs(t *testing.T) {
	cfg := faultConfig(t)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"Data"`)) || bytes.Contains(data, []byte(`"UID"`)) || !bytes.Contains(data, []byte(`"uid":0`)) {
		t.Fatal("config did not emit lower-camel explicit owner/target fields")
	}
	if _, err := DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatal("valid pinned config rejected", err)
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		t.Fatal("fixture JSON")
	}
	owners := value["owners"].(map[string]any)
	mail := owners["mail"].(map[string]any)
	delete(mail, "gid")
	missing, _ := json.Marshal(value)
	if _, err := DecodeConfig(bytes.NewReader(missing)); err == nil {
		t.Fatal("omitted service identity became root")
	}
	if _, err := DecodeConfig(bytes.NewReader(append(data, []byte("{}")...))); err == nil {
		t.Fatal("trailing config document accepted")
	}
	if err := os.Remove(cfg.Staged); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeConfig(bytes.NewReader(data)); err == nil {
		t.Fatal("new Run accepted missing source")
	}
	if _, err := DecodeRecoveryConfig(bytes.NewReader(data)); err != nil {
		t.Fatal("recovery re-required consumed source input", err)
	}
	if err := os.Rename(cfg.Targets.Data, cfg.Targets.Data+".saved-for-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRecoveryConfig(bytes.NewReader(data)); err != nil {
		t.Fatal("recovery rejected interrupted target rename", err)
	}
	if err := os.WriteFile(cfg.ComposeFiles[0].Path, []byte("changed config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRecoveryConfig(bytes.NewReader(data)); err == nil {
		t.Fatal("recovery accepted changed Compose pin")
	}
}

func TestMaintenanceStatusReadsExactJournalAfterReferencedInputsDisappear(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			cfg := faultConfig(t)
			if driver == "postgres" {
				cfg.Driver = driver
				cfg.TargetDSNEnv = "SYNTHETIC_NEW_DSN"
				next := filepath.Join(cfg.ProjectDirectory, "next.env")
				content := []byte("LANQIN_DB_DRIVER=postgres\nSYNTHETIC_SECRET=synthetic-next-environment\n")
				if err := os.WriteFile(next, content, 0o600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(content)
				cfg.NextEnvironmentFile = &PathDigest{Path: next, SHA256: hex.EncodeToString(digest[:])}
				cfg.RuntimeDatabase = &RuntimeDatabaseBinding{Host: "postgres", Port: 5432, Database: "synthetic_new", User: "synthetic_user"}
			}
			txn := strings.Repeat("0", 32)
			info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { return info, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
			before, err := Run(t.Context(), cfg, &faultController{t: t}, actions)
			if err != nil {
				t.Fatal("fixture lifecycle", err)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			missing := []string{cfg.Staged, cfg.EnvironmentFile.Path, cfg.ComposeFiles[0].Path, cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM}
			if cfg.NextEnvironmentFile != nil {
				missing = append(missing, cfg.NextEnvironmentFile.Path)
			}
			for _, name := range missing {
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			}
			decoded, err := DecodeStatusConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatal("status required missing deployment/source inputs", err)
			}
			state, err := ReadState(decoded)
			if err != nil || state.ConfigSHA256 != before.ConfigSHA256 || state.Sequence != before.Sequence || state.Phase != before.Phase {
				t.Fatal("read-only status lost bound journal", err)
			}
			if _, err := DecodeRecoveryConfig(bytes.NewReader(data)); err == nil {
				t.Fatal("status-only missing-input tolerance leaked to mutating recovery")
			}
			cfg.ProjectName = "changed-status-plan"
			changed, _ := json.Marshal(cfg)
			decoded, err = DecodeStatusConfig(bytes.NewReader(changed))
			if err != nil {
				t.Fatal("structurally valid status config was not decodable", err)
			}
			if _, err := ReadState(decoded); err == nil {
				t.Fatal("changed JSON fields bypassed exact journal config hash")
			}
		})
	}
}

func TestMaintenanceProjectLockSerializesDifferentOperationDirectories(t *testing.T) {
	cfg := faultConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	txn := strings.Repeat("6", 32)
	info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
	actions := Actions{Install: func(ctx context.Context) (install.JournalInfo, error) {
		close(entered)
		select {
		case <-release:
			return info, nil
		case <-ctx.Done():
			return info, ctx.Err()
		}
	}, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
	go func() { _, err := Run(t.Context(), cfg, &faultController{t: t}, actions); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first operation never acquired lock")
	}
	other := cfg
	other.StateDir = t.TempDir()
	if err := os.Chmod(other.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	second := &faultController{t: t}
	_, err := Run(t.Context(), other, second, actions)
	close(release)
	if !errors.Is(err, ErrLocked) || len(second.calls) != 0 {
		t.Fatal("separate StateDir bypassed shared project lock", err)
	}
	if err := <-result; err != nil {
		t.Fatal("first maintenance failed", err)
	}
	if _, err := Run(t.Context(), cfg, second, actions); err == nil {
		t.Fatal("completed operation journal replayed")
	}
}

func TestMaintenanceStateRejectsPartialAndModifiedJournalAndForeignReceipt(t *testing.T) {
	for _, damage := range []string{"truncated", "changed-config-hash", "foreign-receipt"} {
		t.Run(damage, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t, panicAt: "recreate"}
			txn := strings.Repeat("7", 32)
			info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { return info, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error {
				t.Fatal("foreign receipt reached data rollback")
				return nil
			}}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("fixture did not crash")
					}
				}()
				Run(t.Context(), cfg, controller, actions)
			}()
			journal := filepath.Join(cfg.StateDir, stateJournalName)
			data, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			if damage == "foreign-receipt" {
				foreign := info
				foreign.Path = filepath.Join(t.TempDir(), filepath.Base(info.Path))
				before := len(controller.calls)
				if _, err := Recover(t.Context(), cfg, controller, actions, "rollback", true, foreign); err == nil || len(controller.calls) != before {
					t.Fatal("foreign install journal accepted")
				}
				return
			}
			if damage == "truncated" {
				data = data[:len(data)-1]
			} else {
				data = bytes.Replace(data, []byte(`"configSHA256":"`), []byte(`"configSHA256":"bad`), 1)
			}
			if err := os.WriteFile(journal, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(cfg); err == nil {
				t.Fatal("damaged state advertised as recoverable")
			}
			before := len(controller.calls)
			if _, err := Recover(t.Context(), cfg, controller, actions, "resume-start", true); err == nil || len(controller.calls) != before {
				t.Fatal("damaged state triggered an action")
			}
		})
	}
}

func TestMaintenanceRecoveryUsesCurrentValidationOnlyAfterDurableCommit(t *testing.T) {
	for _, phase := range []string{"committed", "precommit", "current-validation-failed", "fallback-strict"} {
		t.Run(phase, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t}
			txn := strings.Repeat("8", 32)
			info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
			strict, current, installs := 0, 0, 0
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				installs++
				if phase == "precommit" {
					panic("synthetic precommit interruption")
				}
				return info, nil
			}, Validate: func(context.Context, install.JournalInfo) error { strict++; return nil }, ValidateCommitted: func(context.Context, install.JournalInfo) error { current++; return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
			if phase == "precommit" {
				func() {
					defer func() {
						if recover() == nil {
							t.Fatal("fixture did not stop precommit")
						}
					}()
					Run(t.Context(), cfg, controller, actions)
				}()
			} else if _, err := Run(t.Context(), cfg, controller, actions); err != nil {
				t.Fatal(err)
			}
			if current != 0 || phase != "precommit" && strict != 1 {
				t.Fatal("new installation bypassed strict snapshot validation")
			}
			actions.Install = func(context.Context) (install.JournalInfo, error) {
				t.Fatal("recovery reran installation")
				return install.JournalInfo{}, nil
			}
			actions.Validate = func(context.Context, install.JournalInfo) error {
				strict++
				return errors.New("snapshot has changed after serving new mail")
			}
			if phase == "fallback-strict" {
				actions.ValidateCommitted = nil
			}
			if phase == "current-validation-failed" {
				actions.ValidateCommitted = func(context.Context, install.JournalInfo) error {
					current++
					return errors.New("current runtime pair mismatch")
				}
			}
			var state State
			var err error
			if phase == "precommit" {
				state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true, info)
			} else {
				state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
			}
			if phase == "committed" {
				if err != nil || current != 1 || strict != 1 || state.Phase != "healthy" {
					t.Fatal("legitimate committed data changes blocked current validation", err)
				}
			} else if err == nil || !state.NeedsRecovery {
				t.Fatal("failed recovery validation was ignored")
			}
			if phase == "precommit" && (current != 0 || strict != 1) {
				t.Fatal("precommit recovery used relaxed current validator")
			}
			if phase == "fallback-strict" && (current != 0 || strict != 2) {
				t.Fatal("missing current validator did not preserve strict fallback")
			}
			if installs != 1 {
				t.Fatal("recovery install count changed")
			}
		})
	}
}

func TestMaintenanceUnchangedPreparationCanResumeOriginalAfterFailedHealth(t *testing.T) {
	for _, knownUnchanged := range []bool{true, false} {
		t.Run(strconv.FormatBool(knownUnchanged), func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t, fail: "healthy"}
			installs, rollbacks := 0, 0
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				installs++
				if knownUnchanged {
					return install.JournalInfo{}, ErrInstallUnchanged
				}
				return install.JournalInfo{}, errors.New("unknown installation outcome")
			}, Validate: func(context.Context, install.JournalInfo) error {
				t.Fatal("original-only recovery validated a nonexistent install")
				return nil
			}, Rollback: func(context.Context, install.JournalInfo) error {
				rollbacks++
				t.Fatal("original-only recovery rolled back a nonexistent install")
				return nil
			}}
			state, err := Run(t.Context(), cfg, controller, actions)
			if err == nil || !state.NeedsRecovery || !state.ServicesStopped || state.OriginalDataUnchanged != knownUnchanged || state.Transaction.Path != "" {
				t.Fatal("unchanged/unknown outcome not persisted distinctly", state, err)
			}
			durable, err := ReadState(cfg)
			if err != nil || durable.OriginalDataUnchanged != knownUnchanged {
				t.Fatal("original data identity was not durable", err)
			}
			controller.fail = ""
			beforeStarts := controller.count("recreate")
			state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
			if knownUnchanged {
				if err != nil || state.NeedsRecovery || !state.OriginalServicesRestored || controller.count("recreate") != beforeStarts+1 {
					t.Fatal("known untouched original services could not resume", state, err)
				}
			} else if err == nil || !state.NeedsRecovery || controller.count("recreate") != beforeStarts {
				t.Fatal("unknown install outcome was mistaken for unchanged data")
			}
			if installs != 1 || rollbacks != 0 {
				t.Fatal("resume repeated install or invented rollback", installs, rollbacks)
			}
		})
	}
}

type corePreservingControllerFixture struct{ *faultController }

func (controller *corePreservingControllerFixture) StartOriginal(ctx context.Context, _ Config) error {
	return controller.action(ctx, "start-original")
}
func (controller *corePreservingControllerFixture) PreflightRecovery(ctx context.Context, _ Config) error {
	return controller.action(ctx, "recovery-preflight")
}

func TestMaintenancePreservesOriginalContainersOnlyBeforeCommit(t *testing.T) {
	for _, stage := range []string{"prepare-unchanged", "validation-rollback", "postcommit-rollback"} {
		t.Run(stage, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &corePreservingControllerFixture{&faultController{t: t}}
			txn := strings.Repeat("9", 32)
			info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
			installs, rollbacks := 0, 0
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				installs++
				if stage == "prepare-unchanged" {
					return install.JournalInfo{}, ErrInstallUnchanged
				}
				return info, nil
			}, Validate: func(context.Context, install.JournalInfo) error {
				if stage == "validation-rollback" {
					return errors.New("synthetic validation failure")
				}
				return nil
			}, Rollback: func(context.Context, install.JournalInfo) error { rollbacks++; return nil }}
			state, err := Run(t.Context(), cfg, controller, actions)
			if controller.count("preflight") != 1 || controller.count("recovery-preflight") != 0 {
				t.Fatal("Run relaxed its strict preflight")
			}
			if stage != "postcommit-rollback" {
				if err == nil || state.Committed || controller.count("start-original") != 1 || controller.count("recreate") != 0 || !state.OriginalServicesRestored {
					t.Fatal("precommit recovery replaced original writable container layers", state, err)
				}
				wantRollback := 0
				if stage == "validation-rollback" {
					wantRollback = 1
				}
				if rollbacks != wantRollback {
					t.Fatal("wrong data rollback count")
				}
				return
			}
			if err != nil || controller.count("recreate") != 1 || controller.count("start-original") != 0 {
				t.Fatal("commit did not use new deployment recreation", err)
			}
			controller.fail = "preflight" // Running env may differ after host-env repair.
			state, err = Recover(t.Context(), cfg, controller, actions, "rollback", true)
			if err != nil || state.NeedsRecovery || !state.RollbackCompleted || controller.count("recovery-preflight") != 1 || controller.count("preflight") != 1 || controller.count("recreate") != 2 || controller.count("start-original") != 0 {
				t.Fatal("postcommit explicit rollback reused old container assumption or strict active env preflight", state, err)
			}
			if installs != 1 || rollbacks != 1 {
				t.Fatal("explicit recovery replayed installation")
			}
		})
	}
}
