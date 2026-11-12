package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"lanqin-email-api/internal/backup/install"
)

type Controller interface {
	Preflight(context.Context, Config) error
	Stop(context.Context, Config) error
	VerifyStopped(context.Context, Config) error
	Recreate(context.Context, Config) error
	Healthy(context.Context, Config) error
}

// Optional finite operations preserve original container writable layers before
// commit, and inspect recovery metadata even when running container envs differ
// from an explicitly restored host environment file.
type originalServiceStarter interface {
	StartOriginal(context.Context, Config) error
}
type recoveryPreflighter interface {
	PreflightRecovery(context.Context, Config) error
}

type Actions struct {
	// OnProgress observes a successfully persisted state. Reporting must not
	// control execution or authorize replay of a maintenance action.
	OnProgress        func(State)
	Install           func(context.Context) (install.JournalInfo, error)
	Validate          func(context.Context, install.JournalInfo) error
	ValidateCommitted func(context.Context, install.JournalInfo) error
	Rollback          func(context.Context, install.JournalInfo) error
}

type coordinator struct {
	cfg        Config
	controller Controller
	actions    Actions
	journal    *stateJournal
	state      State
}

// Run starts exactly one operation in a NEW private StateDir journal. Existing
// state, even a successful previous run, is never silently replayed.
func Run(ctx context.Context, cfg Config, controller Controller, actions Actions) (State, error) {
	var empty State
	if ValidateConfig(cfg) != nil || controller == nil || actions.Install == nil || actions.Validate == nil || actions.Rollback == nil {
		return empty, ErrConfig
	}
	if cfg.NextEnvironmentFile != nil {
		status, err := EnvironmentStatus(cfg)
		if err != nil || status != "original" {
			return empty, ErrConfig
		}
	}
	unlock, err := lockProject(cfg)
	if err != nil {
		return empty, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	journal, err := createStateJournal(cfg)
	if err != nil {
		return empty, err
	}
	defer journal.file.Close()
	hash, _ := ConfigHash(cfg)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return empty, ErrState
	}
	c := coordinator{cfg: cfg, controller: controller, actions: actions, journal: journal, state: State{Version: 1, OperationID: hex.EncodeToString(nonce[:]), ConfigSHA256: hash, Phase: "preflight"}}
	if err := c.save("preflight", "preflight"); err != nil {
		return c.state, err
	}
	if err := c.call(ctx, false, func(ctx context.Context) error { return controller.Preflight(ctx, cfg) }); err != nil {
		return c.failBeforeCommit("preflight_failed", false, false)
	}
	if err := c.save("stopping", "stop"); err != nil {
		return c.state, err
	}
	if err := c.stop(ctx); err != nil {
		return c.failBeforeCommit("stop_failed", true, false)
	}
	if err := c.save("stopped", ""); err != nil {
		return c.state, err
	}
	if err := c.save("installing", "install"); err != nil {
		return c.state, err
	}
	var info install.JournalInfo
	err = c.call(ctx, false, func(ctx context.Context) error { var err error; info, err = actions.Install(ctx); return err })
	if info.Path != "" {
		c.state.Transaction = info
	}
	if err != nil || !safeTransaction(cfg, info) {
		unchanged := errors.Is(err, ErrInstallUnchanged) && info.Path == ""
		code := "install_failed"
		if errors.Is(err, ErrPostfixQueueUnsafe) {
			code = "postfix_queue_not_verified"
		}
		return c.failBeforeCommit(code, true, !unchanged)
	}
	if err := c.save("installed", ""); err != nil {
		return c.state, err
	}
	if err := c.validate(ctx, false); err != nil {
		return c.failBeforeCommit("validate_failed", true, true)
	}
	if err := c.save("validated", ""); err != nil {
		return c.state, err
	}
	// This durable boundary precedes the FIRST service start. Once services
	// may have received new mail, failure never authorizes automatic rollback.
	c.state.Committed = true
	if err := c.save("committed", ""); err != nil {
		return c.state, err
	}
	return c.startCommitted(ctx)
}

// Recover performs an explicit operation after inspection, never another Install.
// An optional operator-supplied install receipt can resolve an interrupted
// installing phase only when it belongs to this StateDir and these exact roots.
func Recover(ctx context.Context, cfg Config, controller Controller, actions Actions, operation string, confirmed bool, transaction ...install.JournalInfo) (State, error) {
	var empty State
	if !confirmed {
		return empty, ErrConfirmed
	}
	if operation != "resume-start" && operation != "rollback" || len(transaction) > 1 || controller == nil || actions.Validate == nil || actions.Rollback == nil || ValidateRecoveryConfig(cfg) != nil {
		return empty, ErrConfig
	}
	unlock, err := lockProject(cfg)
	if err != nil {
		return empty, err
	}
	defer unlock()
	journal, state, err := openStateJournal(cfg)
	if err != nil {
		return state, err
	}
	defer journal.file.Close()
	c := coordinator{cfg: cfg, controller: controller, actions: actions, journal: journal, state: state}
	if len(transaction) == 1 {
		if state.OriginalDataUnchanged && state.Transaction.Path == "" || !safeTransaction(cfg, transaction[0]) || state.Transaction.Path != "" && state.Transaction != transaction[0] {
			return state, ErrConfig
		}
		c.state.Transaction = transaction[0]
	}
	if cfg.NextEnvironmentFile != nil {
		status, statusErr := EnvironmentStatus(cfg)
		if statusErr != nil || status == "incomplete" {
			paths, pathErr := checkedEnvironmentPaths(cfg)
			if operation != "rollback" || pathErr != nil {
				return c.recordFailure("environment_recovery_required")
			}
			if _, err := readEnvironmentJournal(paths, cfg); err != nil {
				return c.recordFailure("environment_recovery_required")
			}
			// Restoring the fixed host env file does not mutate running container
			// environments or start writers. It lets pinned Compose preflight
			// work after a crash between saving old and publishing next.
			if err := c.save("failed", "rollback-environment"); err != nil {
				return c.state, err
			}
			repair, cancel := context.WithTimeout(context.Background(), stepTimeout(cfg))
			err := RollbackEnvironment(repair, cfg)
			cancel()
			if err != nil {
				return c.recordFailure("environment_recovery_failed")
			}
			if VerifyPinnedFiles(cfg) != nil {
				return c.recordFailure("environment_recovery_failed")
			}
		}
	}
	if err := c.save("preflight", "preflight"); err != nil {
		return c.state, err
	}
	preflight := controller.Preflight
	if recovery, ok := controller.(recoveryPreflighter); ok {
		preflight = recovery.PreflightRecovery
	}
	if err := c.call(ctx, false, func(ctx context.Context) error { return preflight(ctx, cfg) }); err != nil {
		return c.recordFailure("recovery_preflight_failed")
	}
	if err := c.save("stopping", "stop"); err != nil {
		return c.state, err
	}
	if err := c.stop(ctx); err != nil {
		return c.recordFailure("recovery_stop_failed")
	}
	if err := c.save("stopped", ""); err != nil {
		return c.state, err
	}
	if operation == "rollback" {
		if !safeTransaction(cfg, c.state.Transaction) {
			return c.recordFailure("install_outcome_unknown")
		}
		if err := c.save("failed", "rollback"); err != nil {
			return c.state, err
		}
		if err := c.call(ctx, false, func(ctx context.Context) error { return actions.Rollback(ctx, c.state.Transaction) }); err != nil {
			return c.recordFailure("rollback_failed")
		}
		c.state.RollbackCompleted = true
		if err := c.save("failed", ""); err != nil {
			return c.state, err
		}
		return c.startOriginal(ctx)
	}
	if !c.state.RollbackCompleted && !c.state.OriginalServicesRestored && !c.state.OriginalDataUnchanged {
		if !safeTransaction(cfg, c.state.Transaction) {
			return c.recordFailure("install_outcome_unknown")
		}
		if err := c.validate(ctx, c.state.Committed); err != nil {
			return c.recordFailure("recovery_validate_failed")
		}
		if err := c.save("validated", ""); err != nil {
			return c.state, err
		}
		c.state.Committed = true
		if err := c.save("committed", ""); err != nil {
			return c.state, err
		}
		return c.startCommitted(ctx)
	}
	return c.startOriginal(ctx)
}

func (c *coordinator) call(ctx context.Context, health bool, action func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if VerifyPinnedFiles(c.cfg) != nil {
		return ErrConfig
	}
	limit := stepTimeout(c.cfg)
	if health {
		limit = healthTimeout(c.cfg)
	}
	step, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := action(step)
	if err != nil {
		return err
	}
	return step.Err()
}

func (c *coordinator) save(phase, pending string) error {
	c.state.Phase, c.state.PendingAction = phase, pending
	if c.journal.save(&c.state) != nil {
		c.state.NeedsRecovery = true
		c.state.LastErrorCode = "state_persistence_failed"
		return ErrState
	}
	if c.actions.OnProgress != nil {
		c.actions.OnProgress(c.state)
	}
	return nil
}

func (c *coordinator) stop(ctx context.Context) error {
	c.state.ServicesStopped = false
	if err := c.call(ctx, false, func(ctx context.Context) error { return c.controller.Stop(ctx, c.cfg) }); err != nil {
		return err
	}
	if err := c.call(ctx, false, func(ctx context.Context) error { return c.controller.VerifyStopped(ctx, c.cfg) }); err != nil {
		return err
	}
	c.state.ServicesStopped = true
	return nil
}

func (c *coordinator) validate(ctx context.Context, committed bool) error {
	if err := c.call(ctx, false, func(ctx context.Context) error { return c.controller.VerifyStopped(ctx, c.cfg) }); err != nil {
		return err
	}
	c.state.ServicesStopped = true
	pending := "validate"
	validation := c.actions.Validate
	if committed && c.actions.ValidateCommitted != nil {
		pending = "validate-committed"
		validation = c.actions.ValidateCommitted
	}
	if err := c.save("installed", pending); err != nil {
		return err
	}
	if err := c.call(ctx, false, func(ctx context.Context) error { return validation(ctx, c.state.Transaction) }); err != nil {
		return err
	}
	return c.call(ctx, false, func(ctx context.Context) error { return c.controller.VerifyStopped(ctx, c.cfg) })
}

func (c *coordinator) startCommitted(ctx context.Context) (State, error) {
	if err := c.save("starting", "recreate"); err != nil {
		return c.state, err
	}
	c.state.ServicesStopped = false
	if err := c.call(ctx, false, func(ctx context.Context) error { return c.controller.Recreate(ctx, c.cfg) }); err != nil {
		return c.failAfterCommit("recreate_failed")
	}
	if err := c.save("starting", "health"); err != nil {
		return c.state, err
	}
	if err := c.call(ctx, true, func(ctx context.Context) error { return c.controller.Healthy(ctx, c.cfg) }); err != nil {
		return c.failAfterCommit("health_failed")
	}
	c.state.NeedsRecovery = false
	c.state.LastErrorCode = ""
	if err := c.save("healthy", ""); err != nil {
		return c.state, err
	}
	return c.state, nil
}

func (c *coordinator) startOriginal(ctx context.Context) (State, error) {
	if err := c.call(ctx, false, func(ctx context.Context) error { return c.controller.VerifyStopped(ctx, c.cfg) }); err != nil {
		return c.recordFailure("original_writers_not_stopped")
	}
	start := c.controller.Recreate
	pending := "recreate-original"
	if !c.state.Committed && (c.state.OriginalDataUnchanged || c.state.RollbackCompleted) {
		if original, ok := c.controller.(originalServiceStarter); ok {
			start = original.StartOriginal
			pending = "start-original"
		}
	}
	if err := c.save("failed", pending); err != nil {
		return c.state, err
	}
	c.state.ServicesStopped = false
	if err := c.call(ctx, false, func(ctx context.Context) error { return start(ctx, c.cfg) }); err != nil {
		return c.halt("original_recreate_failed")
	}
	if err := c.save("failed", "health-original"); err != nil {
		return c.state, err
	}
	if err := c.call(ctx, true, func(ctx context.Context) error { return c.controller.Healthy(ctx, c.cfg) }); err != nil {
		return c.halt("original_health_failed")
	}
	c.state.OriginalServicesRestored = true
	c.state.NeedsRecovery = false
	if err := c.save("failed", ""); err != nil {
		return c.state, err
	}
	return c.state, nil
}

func (c *coordinator) failBeforeCommit(code string, stopAttempted, installAttempted bool) (State, error) {
	c.state.LastErrorCode = code
	if !stopAttempted {
		c.state.NeedsRecovery = false
		if err := c.save("failed", ""); err != nil {
			return c.state, err
		}
		return c.state, errors.New("maintenance preflight failed; services were not changed")
	}
	// Stop/verification failures before Install, and the explicit unchanged
	// preparation sentinel, prove that the original data never switched. Keep
	// that fact durable even if restoring the old services fails afterwards.
	if !installAttempted {
		c.state.OriginalDataUnchanged = true
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.save("failed", "stop"); err != nil {
		return c.state, err
	}
	if err := c.stop(cleanup); err != nil {
		return c.recordFailure("writers_not_stopped")
	}
	if installAttempted {
		if !safeTransaction(c.cfg, c.state.Transaction) {
			return c.recordFailure("install_outcome_unknown")
		}
		if err := c.save("failed", "rollback"); err != nil {
			return c.state, err
		}
		if err := c.call(cleanup, false, func(ctx context.Context) error { return c.actions.Rollback(ctx, c.state.Transaction) }); err != nil {
			return c.recordFailure("rollback_failed")
		}
		c.state.RollbackCompleted = true
		if err := c.save("failed", ""); err != nil {
			return c.state, err
		}
	}
	state, err := c.startOriginal(cleanup)
	if err != nil {
		return state, err
	}
	return state, errors.New("maintenance failed before commit; original services restored after verified rollback")
}

func (c *coordinator) failAfterCommit(code string) (State, error) { return c.halt(code) }
func (c *coordinator) halt(code string) (State, error) {
	c.state.LastErrorCode = code
	c.state.NeedsRecovery = true
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.save("failed", "stop"); err != nil {
		return c.state, err
	}
	if err := c.stop(cleanup); err != nil {
		c.state.LastErrorCode = "recovery_stop_failed"
	}
	if err := c.save("failed", ""); err != nil {
		return c.state, err
	}
	return c.state, ErrRecovery
}
func (c *coordinator) recordFailure(code string) (State, error) {
	c.state.LastErrorCode = code
	c.state.NeedsRecovery = true
	if err := c.save("failed", ""); err != nil {
		return c.state, err
	}
	return c.state, ErrRecovery
}

func safeTransaction(cfg Config, info install.JournalInfo) bool {
	return info.Path != "" && filepath.IsAbs(info.Path) && filepath.Dir(info.Path) == cfg.StateDir && validOperationID(info.TransactionID) && filepath.Base(info.Path) == "restore-"+info.TransactionID+".journal" && info.RollbackTargets == cfg.Targets
}

func lockProject(cfg Config) (func(), error) {
	root, err := os.OpenRoot(cfg.ProjectDirectory)
	if err != nil {
		return nil, ErrConfig
	}
	defer root.Close()
	const name = ".lanqin-maintenance.lock"
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ErrLocked
	}
	before, err := root.Lstat(name)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || !privateInfo(before) || !os.SameFile(before, opened) {
		file.Close()
		return nil, ErrLocked
	}
	if err := lockStateFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return func() { unlockStateFile(file); file.Close() }, nil
}
