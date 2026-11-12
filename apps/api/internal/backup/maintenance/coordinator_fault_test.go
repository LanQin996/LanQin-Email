package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/install"
)

func faultConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"project", "state", "workspace", "staged", "data", "mail", "dkim"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pin := func(name, content string) PathDigest {
		file := filepath.Join(root, "project", name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(content))
		return PathDigest{Path: file, SHA256: hex.EncodeToString(digest[:])}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Version: 1, Mode: "all-in-one", Driver: "sqlite", DockerPath: executable, ProjectDirectory: filepath.Join(root, "project"), ProjectName: "synthetic-maintenance-fault", ComposeFiles: []PathDigest{pin("compose.yml", "services:\n  lanqin-email:\n    image: synthetic.example.invalid/mail:fixture\n")}, EnvironmentFile: pin("fixture.env", "LANQIN_DB_DRIVER=sqlite\nSYNTHETIC_SECRET=never-echo-coordinator-secret\n"), StateDir: filepath.Join(root, "state"), Targets: install.Targets{Data: filepath.Join(root, "data"), Mail: filepath.Join(root, "mail"), DKIM: filepath.Join(root, "dkim")}, Owners: install.Owners{Data: install.Owner{UID: 0, GID: 0}, Mail: install.Owner{UID: 0, GID: 0}, DKIM: install.Owner{UID: 0, GID: 0}}, RuntimeDataRoot: "/data", RuntimeMailRoot: "/var/mail/vhosts", RuntimeDKIMRoot: "/var/lib/rspamd/dkim", Workspace: filepath.Join(root, "workspace"), Staged: filepath.Join(root, "staged"), ExpectedReportSHA256: strings.Repeat("a", 64), StepTimeoutSeconds: 1, HealthTimeoutSeconds: 1}
}

type faultController struct {
	t        *testing.T
	calls    []string
	fail     string
	panicAt  string
	cancel   context.CancelFunc
	cancelAt string
}

func (c *faultController) action(ctx context.Context, stage string) error {
	c.calls = append(c.calls, stage)
	if stage == c.panicAt {
		panic("synthetic coordinator crash")
	}
	if stage == c.cancelAt && c.cancel != nil {
		c.cancel()
	}
	if c.fail == stage {
		return errors.New("never-echo-coordinator-secret postgres://private-password@host/database /private/provider-response")
	}
	return ctx.Err()
}
func (c *faultController) Preflight(ctx context.Context, _ Config) error {
	return c.action(ctx, "preflight")
}
func (c *faultController) Stop(ctx context.Context, _ Config) error { return c.action(ctx, "stop") }
func (c *faultController) VerifyStopped(ctx context.Context, _ Config) error {
	return c.action(ctx, "verify-stopped")
}
func (c *faultController) Recreate(ctx context.Context, _ Config) error {
	return c.action(ctx, "recreate")
}
func (c *faultController) Healthy(ctx context.Context, _ Config) error {
	return c.action(ctx, "healthy")
}
func (c *faultController) count(name string) int {
	n := 0
	for _, call := range c.calls {
		if call == name {
			n++
		}
	}
	return n
}

func assertFaultPrivate(t *testing.T, state State, err error) {
	t.Helper()
	encoded, _ := json.Marshal(state)
	text := string(encoded)
	if err != nil {
		text += err.Error()
	}
	for _, private := range []string{"never-echo-coordinator-secret", "private-password", "/private/provider-response"} {
		if strings.Contains(text, private) {
			t.Fatal("coordinator leaked controller/provider error")
		}
	}
}

func TestCoordinatorFaultsRespectStopInstallAndFirstWriterBoundaries(t *testing.T) {
	for _, scenario := range []string{"success", "preflight-failed", "partial-stop-failed", "verify-stopped-failed", "install-before-journal-failed", "install-unchanged", "partial-install-failed", "validation-failed", "rollback-failed", "start-failed", "health-failed"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t}
			failure := errors.New("never-echo-coordinator-secret postgres://private-password@host/database")
			switch scenario {
			case "preflight-failed":
				controller.fail = "preflight"
			case "partial-stop-failed":
				controller.fail = "stop"
			case "verify-stopped-failed":
				controller.fail = "verify-stopped"
			case "start-failed":
				controller.fail = "recreate"
			case "health-failed":
				controller.fail = "healthy"
			}
			installs, validations, rollbacks := 0, 0, 0
			transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("1", 32)+".journal"), TransactionID: strings.Repeat("1", 32), RollbackTargets: cfg.Targets}
			actions := Actions{
				OnProgress: func(observed State) {
					persisted, readErr := ReadState(cfg)
					if readErr != nil || persisted != observed {
						t.Fatal("progress preceded its durable journal state", readErr)
					}
				},
				Install: func(ctx context.Context) (install.JournalInfo, error) {
					installs++
					if controller.count("stop") < 1 || controller.count("verify-stopped") < 1 {
						t.Fatal("install preceded verified complete stop")
					}
					if scenario == "install-before-journal-failed" {
						return install.JournalInfo{}, failure
					}
					if scenario == "install-unchanged" {
						return install.JournalInfo{}, ErrInstallUnchanged
					}
					if scenario == "partial-install-failed" {
						return transaction, failure
					}
					return transaction, ctx.Err()
				},
				Validate: func(ctx context.Context, info install.JournalInfo) error {
					validations++
					if installs != 1 || info != transaction {
						t.Fatal("validation lost actual install transaction")
					}
					if scenario == "validation-failed" || scenario == "rollback-failed" {
						return failure
					}
					return ctx.Err()
				},
				Rollback: func(ctx context.Context, info install.JournalInfo) error {
					rollbacks++
					if info != transaction || controller.count("recreate") != 0 {
						t.Fatal("automatic rollback crossed first-writer boundary or used wrong journal")
					}
					if scenario == "rollback-failed" {
						return failure
					}
					return ctx.Err()
				},
			}
			state, err := Run(t.Context(), cfg, controller, actions)
			assertFaultPrivate(t, state, err)
			if (err == nil) != (scenario == "success") {
				t.Fatal("wrong coordinator fault outcome", scenario, err)
			}
			if scenario == "preflight-failed" || scenario == "partial-stop-failed" || scenario == "verify-stopped-failed" {
				if installs != 0 || validations != 0 || rollbacks != 0 || controller.count("recreate") != 0 {
					t.Fatal("failed stop/preflight touched installation or started writers")
				}
			} else if scenario == "success" {
				if installs != 1 || validations != 1 || rollbacks != 0 || !state.Committed || state.NeedsRecovery || controller.count("recreate") != 1 || controller.count("healthy") != 1 {
					t.Fatal("success missing verified installation/commit/health")
				}
			} else if scenario == "start-failed" || scenario == "health-failed" {
				if !state.Committed || !state.NeedsRecovery || rollbacks != 0 || controller.count("stop") < 2 {
					t.Fatal("post-start failure automatically rolled back or left writers uncontrolled")
				}
			} else {
				wantRollbacks := 1
				if scenario == "install-before-journal-failed" || scenario == "install-unchanged" {
					wantRollbacks = 0
				}
				if state.Committed || rollbacks != wantRollbacks {
					t.Fatal("precommit failure rollback/start boundary incorrect")
				}
				if scenario == "rollback-failed" && controller.count("recreate") != 0 {
					t.Fatal("unverified failed rollback restarted writers")
				}
				if (scenario == "partial-install-failed" || scenario == "validation-failed") && (controller.count("recreate") != 1 || controller.count("healthy") != 1) {
					t.Fatal("verified original deployment not re-created after successful rollback")
				}
				if scenario == "install-unchanged" && (controller.count("recreate") != 1 || !state.OriginalServicesRestored) {
					t.Fatal("explicit unchanged sentinel did not restore original services")
				}
				if scenario == "rollback-failed" && !state.NeedsRecovery {
					t.Fatal("failed rollback omitted recovery requirement")
				}
			}
		})
	}
}

func TestCoordinatorExplicitRecoveryAfterFirstWriterNeverReinstalls(t *testing.T) {
	for _, operation := range []string{"resume-start", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t, fail: "healthy"}
			transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("5", 32)+".journal"), TransactionID: strings.Repeat("5", 32), RollbackTargets: cfg.Targets}
			installs, rollbacks, initialValidations, committedValidations := 0, 0, 0, 0
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { installs++; return transaction, nil }, Validate: func(context.Context, install.JournalInfo) error {
				initialValidations++
				if initialValidations > 1 {
					t.Fatal("committed deployment was compared with old archive instead of current runtime")
				}
				return nil
			}, ValidateCommitted: func(context.Context, install.JournalInfo) error { committedValidations++; return nil }, Rollback: func(_ context.Context, info install.JournalInfo) error {
				rollbacks++
				if info != transaction || controller.count("verify-stopped") < 2 {
					t.Fatal("explicit rollback not bound to stopped original transaction")
				}
				return nil
			}}
			state, err := Run(t.Context(), cfg, controller, actions)
			if err == nil || !state.Committed || !state.NeedsRecovery || rollbacks != 0 {
				t.Fatal("health failure lost no-automatic-rollback boundary")
			}
			controller.fail = ""
			before := len(controller.calls)
			if _, err := Recover(t.Context(), cfg, controller, actions, operation, false); err == nil || len(controller.calls) != before {
				t.Fatal("unconfirmed post-writer recovery changed services")
			}
			state, err = Recover(t.Context(), cfg, controller, actions, operation, true)
			if err != nil || state.NeedsRecovery || installs != 1 {
				t.Fatal("explicit recovery replayed install or failed", err)
			}
			if operation == "rollback" && (rollbacks != 1 || !state.RollbackCompleted || !state.OriginalServicesRestored) {
				t.Fatal("explicit rollback failed to restore old services")
			}
			if operation == "resume-start" && (rollbacks != 0 || !state.Committed || state.Phase != "healthy") {
				t.Fatal("explicit resume crossed rollback boundary")
			}
			if initialValidations != 1 || committedValidations != boolFaultInt(operation == "resume-start") {
				t.Fatal("pre/post-first-writer validation boundaries mixed")
			}
		})
	}
}

func boolFaultInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type faultPreservingController struct {
	*faultController
	recovering     bool
	stopFirstFails bool
	beforeOriginal func()
}

func (c *faultPreservingController) Preflight(ctx context.Context, cfg Config) error {
	if c.recovering {
		c.t.Fatal("Recover used strict running-env preflight instead of finite recovery metadata preflight")
	}
	return c.faultController.Preflight(ctx, cfg)
}
func (c *faultPreservingController) PreflightRecovery(ctx context.Context, _ Config) error {
	return c.action(ctx, "preflight-recovery")
}
func (c *faultPreservingController) StartOriginal(ctx context.Context, _ Config) error {
	if c.beforeOriginal != nil {
		c.beforeOriginal()
	}
	return c.action(ctx, "start-original")
}
func (c *faultPreservingController) Stop(ctx context.Context, cfg Config) error {
	if c.stopFirstFails && c.count("stop") == 0 {
		c.calls = append(c.calls, "stop")
		return errors.New("synthetic partial service stop")
	}
	return c.faultController.Stop(ctx, cfg)
}

func TestCoordinatorPrecommitRestorationPreservesOriginalContainersAndSpool(t *testing.T) {
	for _, scenario := range []string{"unchanged-preparation", "partial-switch-rolled-back", "partial-stop-then-success", "original-start-health-failed"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultPreservingController{faultController: &faultController{t: t}, stopFirstFails: scenario == "partial-stop-then-success"}
			if scenario == "original-start-health-failed" {
				controller.fail = "healthy"
			}
			transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("8", 32)+".journal"), TransactionID: strings.Repeat("8", 32), RollbackTargets: cfg.Targets}
			rolledBack := false
			controller.beforeOriginal = func() {
				state, err := ReadState(cfg)
				if err != nil || state.Committed || !state.RollbackCompleted && !state.OriginalDataUnchanged {
					t.Fatal("original container reuse lacked unchanged/verified rollback provenance", err)
				}
				if scenario == "partial-switch-rolled-back" && !rolledBack {
					t.Fatal("old containers started before original bind identities restored")
				}
			}
			installs := 0
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				installs++
				if scenario == "partial-switch-rolled-back" {
					return transaction, errors.New("synthetic partial install")
				}
				return install.JournalInfo{}, ErrInstallUnchanged
			}, Validate: func(context.Context, install.JournalInfo) error {
				t.Fatal("failed/unchanged install validated as new")
				return nil
			}, Rollback: func(context.Context, install.JournalInfo) error { rolledBack = true; return nil }}
			state, err := Run(t.Context(), cfg, controller, actions)
			if err == nil || state.Committed || controller.count("preflight") != 1 || controller.count("preflight-recovery") != 0 || controller.count("start-original") != 1 || controller.count("recreate") != 0 {
				t.Fatal("precommit recovery replaced original writable layer/spool", scenario, err)
			}
			if (scenario == "partial-stop-then-success") != (installs == 0) {
				t.Fatal("partial Stop changed data before verified stop")
			}
			if scenario == "original-start-health-failed" {
				if !state.NeedsRecovery || controller.count("stop") < 3 {
					t.Fatal("original container health failure did not halt writers")
				}
			} else if !state.OriginalServicesRestored || state.NeedsRecovery || controller.count("healthy") != 1 {
				t.Fatal("restored original services skipped strict health proof")
			}
		})
	}
}

func TestCoordinatorRecoveryMetadataPreflightCannotWeakenRunOrCommittedRollback(t *testing.T) {
	cfg := faultConfig(t)
	controller := &faultPreservingController{faultController: &faultController{t: t, fail: "healthy"}}
	transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("9", 32)+".journal"), TransactionID: strings.Repeat("9", 32), RollbackTargets: cfg.Targets}
	installCalls, rollbackCalls := 0, 0
	queueEmpty := false
	actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { installCalls++; return transaction, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, ValidateCommitted: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error {
		rollbackCalls++
		if !queueEmpty {
			return errors.New("synthetic stopped Postfix queue is not empty")
		}
		return nil
	}}
	state, err := Run(t.Context(), cfg, controller, actions)
	if err == nil || !state.Committed || controller.count("preflight") != 1 || controller.count("preflight-recovery") != 0 || controller.count("recreate") != 1 {
		t.Fatal("Run weakened initial strict preflight")
	}
	controller.recovering = true
	controller.fail = ""
	controller.beforeOriginal = func() {
		t.Fatal("committed rollback reused original containers instead of binding rolled-back data deliberately")
	}
	state, err = Recover(t.Context(), cfg, controller, actions, "rollback", true)
	if err == nil || !state.NeedsRecovery || !state.ServicesStopped || rollbackCalls != 1 || controller.count("preflight-recovery") != 1 || controller.count("recreate") != 1 || controller.count("start-original") != 0 {
		t.Fatal("nonempty/unknown postcommit spool authorized destructive rollback/recreation")
	}
	queueEmpty = true
	state, err = Recover(t.Context(), cfg, controller, actions, "rollback", true)
	if err != nil || state.NeedsRecovery || !state.OriginalServicesRestored || rollbackCalls != 2 || installCalls != 1 || controller.count("preflight") != 1 || controller.count("preflight-recovery") != 2 || controller.count("start-original") != 0 || controller.count("recreate") != 2 || controller.count("healthy") != 2 {
		t.Fatal("explicit postcommit recovery bypassed metadata/strict health or replayed install", err)
	}
}

func TestCoordinatorRecoveryMetadataFailureStopsBeforeAnyWriterAction(t *testing.T) {
	cfg := faultConfig(t)
	controller := &faultPreservingController{faultController: &faultController{t: t, fail: "start-original"}}
	actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { return install.JournalInfo{}, ErrInstallUnchanged }, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error {
		t.Fatal("unchanged recovery tried data rollback")
		return nil
	}}
	state, err := Run(t.Context(), cfg, controller, actions)
	if err == nil || state.Committed || !state.NeedsRecovery || !state.OriginalDataUnchanged {
		t.Fatal("cannot seed preserved-original interrupted start")
	}
	controller.recovering = true
	controller.fail = "preflight-recovery"
	beforeStops, beforeStarts := controller.count("stop"), controller.count("start-original")
	state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
	if err == nil || !state.NeedsRecovery || controller.count("stop") != beforeStops || controller.count("start-original") != beforeStarts || controller.count("recreate") != 0 {
		t.Fatal("failed recovery metadata preflight touched writer state")
	}
	controller.fail = ""
	state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
	if err != nil || state.Committed || !state.OriginalServicesRestored || controller.count("start-original") != beforeStarts+1 || controller.count("healthy") != 1 {
		t.Fatal("explicit preserved original recovery did not resume same containers with health check", err)
	}
}

type queueGuardFaultController struct {
	*faultPreservingController
	queueEmpty                 bool
	acceptedDuringFailedHealth bool
	destructiveRecreates       int
}

func (c *queueGuardFaultController) Recreate(ctx context.Context, cfg Config) error {
	if !c.queueEmpty {
		return ErrPostfixQueueUnsafe
	}
	c.destructiveRecreates++
	return c.faultController.Recreate(ctx, cfg)
}
func (c *queueGuardFaultController) Healthy(ctx context.Context, cfg Config) error {
	if c.acceptedDuringFailedHealth {
		c.queueEmpty = false
		c.acceptedDuringFailedHealth = false
		_ = c.faultController.Healthy(ctx, cfg)
		return errors.New("synthetic unhealthy after SMTP accepted queued mail")
	}
	return c.faultController.Healthy(ctx, cfg)
}

func TestCoordinatorCommittedResumeStartCannotBypassRecreationQueueGuard(t *testing.T) {
	cfg := faultConfig(t)
	controller := &queueGuardFaultController{faultPreservingController: &faultPreservingController{faultController: &faultController{t: t}}, queueEmpty: true, acceptedDuringFailedHealth: true}
	transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("a", 32)+".journal"), TransactionID: strings.Repeat("a", 32), RollbackTargets: cfg.Targets}
	installs, rollbacks := 0, 0
	actions := Actions{Install: func(context.Context) (install.JournalInfo, error) { installs++; return transaction, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, ValidateCommitted: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { rollbacks++; return nil }}
	state, err := Run(t.Context(), cfg, controller, actions)
	if err == nil || !state.Committed || !state.NeedsRecovery || controller.queueEmpty || controller.destructiveRecreates != 1 {
		t.Fatal("cannot seed committed failure with retained new Postfix queue")
	}
	controller.recovering = true
	state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
	if err == nil || !state.NeedsRecovery || !state.ServicesStopped || controller.destructiveRecreates != 1 || installs != 1 || rollbacks != 0 || controller.count("start-original") != 0 {
		t.Fatal("resume-start evaded adapter queue guard through another action")
	}
	controller.queueEmpty = true
	state, err = Recover(t.Context(), cfg, controller, actions, "resume-start", true)
	if err != nil || state.NeedsRecovery || state.Phase != "healthy" || controller.destructiveRecreates != 2 || installs != 1 || rollbacks != 0 {
		t.Fatal("explicit safe resume failed after queue handled", err)
	}
}

func TestCoordinatorPersistsCommitBeforeAnyServiceRecreation(t *testing.T) {
	cfg := faultConfig(t)
	controller := &faultController{t: t}
	transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("2", 32)+".journal"), TransactionID: strings.Repeat("2", 32), RollbackTargets: cfg.Targets}
	wrapped := &commitInspectingController{faultController: controller, cfg: cfg, transaction: transaction}
	state, err := Run(t.Context(), cfg, wrapped, Actions{Install: func(context.Context) (install.JournalInfo, error) { return transaction, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { t.Fatal("healthy lifecycle rolled back"); return nil }})
	if err != nil || !state.Committed {
		t.Fatal("valid maintenance lifecycle failed", err)
	}
}

type commitInspectingController struct {
	*faultController
	cfg         Config
	transaction install.JournalInfo
}

func (c *commitInspectingController) Recreate(ctx context.Context, cfg Config) error {
	state, err := ReadState(c.cfg)
	if err != nil || !state.Committed || state.Transaction != c.transaction {
		c.t.Fatal("service recreation began before durable commit receipt", err)
	}
	return c.faultController.Recreate(ctx, cfg)
}

func TestCoordinatorCancellationUsesIndependentBoundedRollbackAndStopsAfterStart(t *testing.T) {
	for _, phase := range []string{"before-run", "during-install", "during-recreate"} {
		t.Run(phase, func(t *testing.T) {
			cfg := faultConfig(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			controller := &faultController{t: t, cancel: cancel}
			if phase == "before-run" {
				cancel()
			}
			if phase == "during-recreate" {
				controller.cancelAt = "recreate"
			}
			transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("3", 32)+".journal"), TransactionID: strings.Repeat("3", 32), RollbackTargets: cfg.Targets}
			installs, rollbacks := 0, 0
			state, err := Run(ctx, cfg, controller, Actions{
				Install: func(context.Context) (install.JournalInfo, error) {
					installs++
					if phase == "during-install" {
						cancel()
						return transaction, ctx.Err()
					}
					return transaction, nil
				},
				Validate: func(ctx context.Context, _ install.JournalInfo) error { return ctx.Err() },
				Rollback: func(cleanup context.Context, _ install.JournalInfo) error {
					rollbacks++
					if cleanup.Err() != nil {
						t.Fatal("rollback inherited canceled request")
					}
					if _, ok := cleanup.Deadline(); !ok {
						t.Fatal("rollback cleanup had no bound")
					}
					return nil
				},
			})
			if err == nil {
				t.Fatal("cancellation reported success")
			}
			assertFaultPrivate(t, state, err)
			if phase == "before-run" && installs != 0 {
				t.Fatal("canceled request touched install")
			}
			if phase == "during-install" && (rollbacks != 1 || state.Committed || controller.count("recreate") != 1) {
				t.Fatal("canceled partial switch not rolled back before first writer")
			}
			if phase == "during-recreate" && (rollbacks != 0 || !state.Committed || !state.NeedsRecovery || controller.count("stop") < 2) {
				t.Fatal("canceled recreation crossed rollback boundary")
			}
		})
	}
}

func TestCoordinatorCrashStatesAndChangedConfigRejectBlindReplay(t *testing.T) {
	for _, phase := range []string{"install", "recreate"} {
		t.Run(phase, func(t *testing.T) {
			cfg := faultConfig(t)
			controller := &faultController{t: t}
			transaction := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+strings.Repeat("4", 32)+".journal"), TransactionID: strings.Repeat("4", 32), RollbackTargets: cfg.Targets}
			if phase == "recreate" {
				controller.panicAt = "recreate"
			}
			actions := Actions{Install: func(context.Context) (install.JournalInfo, error) {
				if phase == "install" {
					panic("synthetic install process death")
				}
				return transaction, nil
			}, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("fixture crash did not occur")
					}
				}()
				Run(t.Context(), cfg, controller, actions)
			}()
			before := len(controller.calls)
			state, err := ReadState(cfg)
			if err != nil || state.Phase == "healthy" {
				t.Fatal("crashed action lost durable uncertain state", err)
			}
			if _, err := Run(t.Context(), cfg, controller, actions); err == nil || len(controller.calls) != before {
				t.Fatal("existing crash journal blindly replayed actions")
			}
			if _, err := Recover(t.Context(), cfg, controller, actions, "rollback", false); err == nil || len(controller.calls) != before {
				t.Fatal("recovery ran without explicit confirmation")
			}
			if err := os.WriteFile(cfg.ComposeFiles[0].Path, []byte("services: {} # never-echo-coordinator-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Recover(t.Context(), cfg, controller, actions, "resume-start", true); err == nil || len(controller.calls) != before {
				t.Fatal("changed deployment config resumed crash state")
			}
		})
	}
}
