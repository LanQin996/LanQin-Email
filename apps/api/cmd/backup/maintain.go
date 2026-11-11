package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/maintenance"
)

type maintenanceCLIRequest struct {
	Operation   string
	Transaction *install.JournalInfo
}

type maintenanceCLIExecutor func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error)

func runMaintenance(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	return runMaintenanceWithExecutor(ctx, mode, args, stdout, func() error {
		if runtime.GOOS != "linux" {
			return install.ErrSwitchPlatform
		}
		return nil
	}, executeMaintenance)
}

func runMaintenanceWithExecutor(ctx context.Context, mode string, args []string, stdout io.Writer, platform func() error, execute maintenanceCLIExecutor) error {
	if mode != "maintain" && mode != "maintain-plan" && mode != "maintain-status" && mode != "maintain-recover" {
		return errors.New("unknown maintenance mode")
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "private deployment-owned maintenance plan with pinned configuration files")
	confirmed := flags.Bool("confirm-maintenance", false, "explicitly authorize bounded deployment stop, verified installation and service recreation")
	operation := flags.String("operation", "", "recovery only: resume-start or rollback; a rollback preserves the new tree but may quarantine new mail")
	journal := flags.String("journal", "", "optional explicitly inspected install receipt for an interrupted installing phase")
	transaction := flags.String("transaction", "", "transaction ID paired with --journal")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *configPath == "" || execute == nil || platform == nil {
		return errors.New("maintenance requires --config and only documented flags, never credentials or shell hooks")
	}
	if mode == "maintain" && !*confirmed || mode == "maintain-recover" && !*confirmed {
		return install.ErrMaintenanceRequired
	}
	if mode != "maintain-recover" && (*operation != "" || *journal != "" || *transaction != "") || mode == "maintain-recover" && (*operation != "resume-start" && *operation != "rollback" || (*journal == "") != (*transaction == "")) {
		return errors.New("invalid maintenance recovery operation or receipt; never replay an uncertain install")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Plan/status do not control services; mutation gates precede all filesystem,
	// environment or Docker access. There is no user-facing platform bypass.
	if mode == "maintain" || mode == "maintain-recover" {
		if err := platform(); err != nil {
			return err
		}
	}
	if maintenance.ValidatePlanFile(*configPath) != nil {
		return errors.New("maintenance plan must be private, deployment-owned and unchanged")
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return errors.New("cannot open private maintenance plan")
	}
	before, statErr := os.Lstat(*configPath)
	opened, openErr := file.Stat()
	if statErr != nil || openErr != nil || !before.Mode().IsRegular() || !os.SameFile(before, opened) || before.Size() > 64<<10 || runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
		file.Close()
		return errors.New("maintenance plan must be a private regular file")
	}
	var cfg maintenance.Config
	var decodeErr error
	if mode == "maintain-status" {
		cfg, decodeErr = maintenance.DecodeStatusConfig(file)
	} else if mode == "maintain-recover" {
		cfg, decodeErr = maintenance.DecodeRecoveryConfig(file)
	} else {
		cfg, decodeErr = maintenance.DecodeConfig(file)
	}
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return errors.New("maintenance plan is invalid, unsafe or its pinned files changed")
	}
	request := maintenanceCLIRequest{Operation: *operation}
	if *journal != "" {
		id, err := hex.DecodeString(*transaction)
		if err != nil || len(id) != 16 || filepath.Dir(*journal) != cfg.StateDir || filepath.Base(*journal) != "restore-"+*transaction+".journal" {
			return errors.New("recovery receipt must name the inspected install journal in this plan's private state directory")
		}
		request.Transaction = &install.JournalInfo{Path: *journal, TransactionID: *transaction, RollbackTargets: cfg.Targets}
	}
	if mode == "maintain-plan" {
		digest, err := maintenance.ConfigHash(cfg)
		if err != nil {
			return errors.New("cannot calculate maintenance plan binding")
		}
		services := []string{"lanqin-email"}
		if cfg.Mode == "stack" {
			services = []string{"api", "postfix", "dovecot", "rspamd", "web", "nginx"}
		}
		return writeMaintenanceJSON(stdout, struct {
			ConfigSHA256          string          `json:"configSHA256"`
			Driver                string          `json:"driver"`
			Services              []string        `json:"services"`
			Targets               install.Targets `json:"targets"`
			EnvironmentTransition bool            `json:"environmentTransition"`
			ServicesStarted       bool            `json:"servicesStarted"`
		}{digest, cfg.Driver, services, cfg.Targets, cfg.NextEnvironmentFile != nil, false})
	}
	state, actionErr := execute(ctx, mode, cfg, request)
	if state.OperationID != "" || state.Phase != "" {
		if err := writeMaintenanceJSON(stdout, state); err != nil {
			return err
		}
	}
	if actionErr != nil {
		return errors.New("maintenance did not complete; retain the state directory, environment journal and all old/new trees; inspect the receipt before explicit recovery")
	}
	return nil
}

func writeMaintenanceJSON(output io.Writer, value any) error {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return errors.New("cannot write maintenance receipt; deployment state may already have changed, retain the private state directory")
	}
	return nil
}

func executeMaintenance(ctx context.Context, mode string, cfg maintenance.Config, request maintenanceCLIRequest) (maintenance.State, error) {
	if mode == "maintain-status" {
		return maintenance.ReadState(cfg)
	}
	if mode == "maintain" {
		seconds := cfg.StepTimeoutSeconds
		if seconds == 0 {
			seconds = 60
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		staged, reportSHA, err := resolveMaintenanceSource(checkCtx, cfg)
		if err == nil {
			err = backup.VerifyStagedOnlineReport(checkCtx, staged, reportSHA, cfg.Driver)
		}
		if err == nil && cfg.Driver != "sqlite" {
			_, err = install.VerifyNativeStaged(checkCtx, staged, cfg.Driver, reportSHA, backup.DefaultLimits())
		}
		cancel()
		if err != nil {
			return maintenance.State{}, errors.New("maintenance source verification failed before deployment service control")
		}
	}
	controller := maintenance.NewComposeController()
	actions, err := newMaintenanceInstallActions(cfg, controller.ValidateNativeRuntime, controller.EnsureSafePostfixQueues)
	if err != nil {
		return maintenance.State{}, err
	}
	if mode == "maintain-recover" {
		if request.Transaction != nil {
			return maintenance.Recover(ctx, cfg, controller, actions, request.Operation, true, *request.Transaction)
		}
		return maintenance.Recover(ctx, cfg, controller, actions, request.Operation, true)
	}
	return maintenance.Run(ctx, cfg, controller, actions)
}

type maintenancePlanBinding struct {
	Version        int    `json:"version"`
	ConfigSHA256   string `json:"configSHA256"`
	PlanSHA256     string `json:"planSHA256"`
	ReportSHA256   string `json:"reportSHA256"`
	DatabaseSHA256 string `json:"databaseSHA256,omitempty"`
}

func newMaintenanceInstallActions(cfg maintenance.Config, verifyRuntime func(context.Context, maintenance.Config, string) error, queueChecks ...func(context.Context, maintenance.Config) error) (maintenance.Actions, error) {
	if len(queueChecks) > 1 {
		return maintenance.Actions{}, maintenance.ErrConfig
	}
	checkQueue := func(ctx context.Context) error {
		if len(queueChecks) != 1 || queueChecks[0] == nil {
			return maintenance.ErrRecovery
		}
		return queueChecks[0](ctx, cfg)
	}
	var target app.Config
	if cfg.Driver != "sqlite" {
		dsn, ok := os.LookupEnv(cfg.TargetDSNEnv)
		if !ok || dsn == "" || len(dsn) > 16384 {
			return maintenance.Actions{}, errors.New("maintenance NEW target database environment reference unavailable")
		}
		target = app.Config{DBDriver: cfg.Driver, DBDSN: dsn}
	}
	limits := backup.DefaultLimits()
	return maintenance.Actions{
		Install: func(ctx context.Context) (install.JournalInfo, error) {
			unchanged := func() (install.JournalInfo, error) { return install.JournalInfo{}, maintenance.ErrInstallUnchanged }
			status, err := maintenance.EnvironmentStatus(cfg)
			if err != nil || status != "original" {
				return unchanged()
			}
			if checkQueue(ctx) != nil {
				return install.JournalInfo{}, errors.Join(maintenance.ErrInstallUnchanged, maintenance.ErrPostfixQueueUnsafe)
			}
			staged, reportSHA, err := resolveMaintenanceSource(ctx, cfg)
			if err != nil {
				return unchanged()
			}
			var prepared *install.Prepared
			if cfg.Driver == "sqlite" {
				prepared, err = install.Prepare(ctx, staged, cfg.Workspace, cfg.RuntimeDataRoot, cfg.RuntimeMailRoot, limits, reportSHA)
			} else {
				prepared, err = app.PrepareNativeInstallation(ctx, target, staged, cfg.Workspace, cfg.RuntimeDataRoot, cfg.RuntimeMailRoot, cfg.Targets, reportSHA, limits)
			}
			if err != nil {
				return unchanged()
			}
			defer prepared.Close()
			plan, err := readMaintenanceFile(filepath.Join(prepared.Path, "data", "restore-install-plan.json"), 32<<10)
			if err != nil {
				return unchanged()
			}
			digest := sha256.Sum256(plan)
			configSHA, _ := maintenance.ConfigHash(cfg)
			binding := maintenancePlanBinding{Version: 1, ConfigSHA256: configSHA, PlanSHA256: hex.EncodeToString(digest[:]), ReportSHA256: reportSHA}
			if cfg.Driver == "sqlite" {
				binding.DatabaseSHA256, err = maintenanceFileSHA256(ctx, filepath.Join(prepared.Path, "data", "lanqin.db"), limits.MaxBytes)
				if err != nil {
					return unchanged()
				}
			}
			if err := writeMaintenanceBinding(cfg, binding); err != nil {
				return unchanged()
			}
			info, err := install.Apply(ctx, prepared.Path, cfg.Targets, cfg.Owners, cfg.StateDir, true, install.Access{SQLiteGroupRead: cfg.SQLiteGroupRead})
			cleanupErr := prepared.Close()
			if err != nil {
				return info, errors.New("maintenance directory switch failed; inspect retained transaction journal")
			}
			if cleanupErr != nil {
				return info, errors.New("maintenance private preparation cleanup failed")
			}
			if cfg.Driver != "sqlite" {
				if err := maintenance.ApplyEnvironment(ctx, cfg); err != nil {
					return info, errors.New("maintenance environment transition failed")
				}
				if _, err := app.ActivateNativeInstallation(ctx, target, info.Path, cfg.Targets, true); err != nil {
					return info, errors.New("maintenance native target activation failed")
				}
			}
			return info, nil
		},
		Validate: func(ctx context.Context, info install.JournalInfo) error {
			binding, err := readMaintenanceBinding(cfg)
			if err != nil {
				return err
			}
			if cfg.Driver == "sqlite" {
				if err := install.VerifySQLiteGroupAccess(ctx, info.Path, cfg.Targets, cfg.SQLiteGroupRead); err != nil {
					return err
				}
				return install.VerifySQLiteInstalled(ctx, info.Path, cfg.Targets, binding.PlanSHA256, binding.ReportSHA256, binding.DatabaseSHA256)
			}
			status, err := maintenance.EnvironmentStatus(cfg)
			if err != nil || status != "next" {
				return errors.New("native runtime environment is not the planned replacement")
			}
			if _, err := app.ActivateNativeInstallation(ctx, target, info.Path, cfg.Targets, true); err != nil {
				return errors.New("native database and installed directories could not be reverified")
			}
			if verifyRuntime == nil {
				return errors.New("native deployment runtime verifier unavailable")
			}
			return verifyRuntime(ctx, cfg, binding.PlanSHA256)
		},
		ValidateCommitted: func(ctx context.Context, info install.JournalInfo) error {
			if checkQueue(ctx) != nil {
				return maintenance.ErrPostfixQueueUnsafe
			}
			binding, err := readMaintenanceBinding(cfg)
			if err != nil {
				return err
			}
			if cfg.Driver == "sqlite" {
				if err := install.VerifySQLiteGroupAccess(ctx, info.Path, cfg.Targets, cfg.SQLiteGroupRead); err != nil {
					return err
				}
				return install.VerifySQLiteRuntime(ctx, info.Path, cfg.Targets, binding.PlanSHA256)
			}
			status, err := maintenance.EnvironmentStatus(cfg)
			if err != nil || status != "next" || verifyRuntime == nil {
				return errors.New("committed native environment binding unavailable")
			}
			if _, err := install.VerifyCommitted(ctx, info.Path, cfg.Targets); err != nil {
				return err
			}
			if _, err := install.VerifyPlanTransaction(ctx, info.Path, cfg.Targets, binding.PlanSHA256); err != nil {
				return err
			}
			return verifyRuntime(ctx, cfg, binding.PlanSHA256)
		},
		Rollback: func(ctx context.Context, info install.JournalInfo) error {
			if checkQueue(ctx) != nil {
				return maintenance.ErrPostfixQueueUnsafe
			}
			if cfg.Driver == "sqlite" {
				return install.Rollback(ctx, info.Path, cfg.Targets, true)
			}
			// Guard the new database before restoring directories. The environment
			// file is then restored before original services can be recreated.
			if err := app.RollbackNativeInstallation(ctx, target, info.Path, cfg.Targets, true); err != nil {
				return errors.New("native maintenance rollback incomplete")
			}
			return maintenance.RollbackEnvironment(ctx, cfg)
		},
	}, nil
}

func resolveMaintenanceSource(ctx context.Context, cfg maintenance.Config) (string, string, error) {
	if cfg.WebJob == "" {
		digest, err := hex.DecodeString(cfg.ExpectedReportSHA256)
		if err != nil || len(digest) != sha256.Size {
			return "", "", maintenance.ErrConfig
		}
		return cfg.Staged, hex.EncodeToString(digest), nil
	}
	if cfg.Driver == "sqlite" {
		return app.ResolveWebRestoreAt(ctx, cfg.WebStateDB, cfg.WebRestoreRoot, cfg.WebRecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), cfg.WebJob)
	}
	stateDSN, ok := os.LookupEnv(cfg.WebStateDSNEnv)
	targetDSN, targetOK := os.LookupEnv(cfg.TargetDSNEnv)
	if !ok || !targetOK || stateDSN == "" || targetDSN == "" || len(stateDSN) > 16384 || len(targetDSN) > 16384 || stateDSN == targetDSN {
		return "", "", maintenance.ErrConfig
	}
	return app.ResolveWebNativeRestoreAt(ctx, cfg.Driver, stateDSN, cfg.WebRestoreRoot, cfg.WebRecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), cfg.WebJob)
}

func readMaintenanceFile(name string, limit int64) ([]byte, error) {
	before, err := os.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return nil, maintenance.ErrState
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, maintenance.ErrState
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, maintenance.ErrState
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != before.Size() {
		return nil, maintenance.ErrState
	}
	return data, nil
}

func writeMaintenanceBinding(cfg maintenance.Config, binding maintenancePlanBinding) error {
	data, err := json.Marshal(binding)
	if err != nil {
		return maintenance.ErrState
	}
	file, err := os.OpenFile(filepath.Join(cfg.StateDir, "install-binding.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return maintenance.ErrState
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return maintenance.ErrState
	}
	dir, err := os.Open(cfg.StateDir)
	if err != nil {
		return maintenance.ErrState
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return maintenance.ErrState
	}
	return nil
}

func readMaintenanceBinding(cfg maintenance.Config) (maintenancePlanBinding, error) {
	var binding maintenancePlanBinding
	data, err := readMaintenanceFile(filepath.Join(cfg.StateDir, "install-binding.json"), 4096)
	if err != nil {
		return binding, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || decoder.Decode(new(any)) != io.EOF {
		return binding, maintenance.ErrState
	}
	hash, _ := maintenance.ConfigHash(cfg)
	if binding.Version != 1 || binding.ConfigSHA256 != hash {
		return binding, maintenance.ErrState
	}
	for _, digest := range []string{binding.PlanSHA256, binding.ReportSHA256} {
		value, err := hex.DecodeString(digest)
		if err != nil || len(value) != sha256.Size {
			return binding, maintenance.ErrState
		}
	}
	if cfg.Driver == "sqlite" {
		value, err := hex.DecodeString(binding.DatabaseSHA256)
		if err != nil || len(value) != sha256.Size {
			return binding, maintenance.ErrState
		}
	}
	return binding, nil
}

func maintenanceFileSHA256(ctx context.Context, name string, limit int64) (string, error) {
	before, err := os.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return "", maintenance.ErrState
	}
	file, err := os.Open(name)
	if err != nil {
		return "", maintenance.ErrState
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", maintenance.ErrState
	}
	hash := sha256.New()
	n, err := io.Copy(hash, maintenanceContextReader{ctx, io.LimitReader(file, limit+1)})
	after, statErr := file.Stat()
	if err != nil || statErr != nil || n != before.Size() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return "", maintenance.ErrState
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type maintenanceContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r maintenanceContextReader) Read(buffer []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.reader.Read(buffer)
}
