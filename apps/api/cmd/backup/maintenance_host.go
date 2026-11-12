package main

import (
	"context"
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
	"lanqin-email-api/internal/backup/handoff"
	"lanqin-email-api/internal/backup/maintenance"
	"lanqin-email-api/internal/backup/maintenancehost"
)

func runMaintenanceHost(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runMaintenanceHostMode(ctx, "maintenance-host", args, stdout, stderr)
}

func runMaintenanceHostMode(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("maintenance-host", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "root-owned private approved host configuration")
	confirmed := flags.Bool("confirm-maintenance-service", false, "allow only preapproved maintenance plans from the configured local API UID")
	id := flags.String("request", "", "root-inspected interrupted host request to acknowledge after separate maintenance recovery")
	if mode != "maintenance-host" && mode != "maintenance-host-resolve" {
		return maintenancehost.ErrUnavailable
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 || *config == "" || !*confirmed || mode == "maintenance-host" && *id != "" || mode == "maintenance-host-resolve" && *id == "" {
		return errors.New("maintenance-host requires --config and --confirm-maintenance-service")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("maintenance-host requires a root-owned Linux deployment helper")
	}
	if maintenance.ValidatePlanFile(*config) != nil {
		return errors.New("maintenance host configuration is not private or trusted")
	}
	file, err := os.Open(*config)
	if err != nil {
		return maintenancehost.ErrUnavailable
	}
	defer file.Close()
	before, se := os.Lstat(*config)
	opened, oe := file.Stat()
	if se != nil || oe != nil || !os.SameFile(before, opened) {
		return maintenancehost.ErrUnavailable
	}
	cfg, err := maintenancehost.DecodeConfig(file)
	if err != nil {
		return err
	}
	if mode == "maintenance-host-resolve" {
		status, err := maintenancehost.Resolve(ctx, cfg, *id)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(status)
	}
	return maintenancehost.Serve(ctx, cfg, func(ctx context.Context, request maintenancehost.Request, plan maintenancehost.ApprovedPlan, phase func(string) error) (maintenancehost.Outcome, error) {
		return executeHostMaintenance(ctx, cfg, request, plan, phase)
	})
}

func executeHostMaintenance(ctx context.Context, host maintenancehost.Config, req maintenancehost.Request, plan maintenancehost.ApprovedPlan, phase func(string) error) (maintenancehost.Outcome, error) {
	failure := func(code string) (maintenancehost.Outcome, error) {
		return maintenancehost.Outcome{State: "failed", ErrorCode: code}, maintenancehost.ErrUnavailable
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return failure("unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	cfg, err := maintenancehost.LoadTemplate(plan)
	if err != nil {
		return failure("unavailable")
	}
	sourceConfig := app.Config{DBDriver: plan.Source.Driver, DBPath: plan.Source.DBPath}
	if plan.Source.Driver != "sqlite" {
		sourceConfig.DBDSN = os.Getenv(plan.Source.DSNEnv)
		if len(sourceConfig.DBDSN) == 0 || len(sourceConfig.DBDSN) > 16384 {
			return failure("source_unavailable")
		}
	}
	source, err := app.ResolveWebMaintenanceSource(ctx, sourceConfig, plan.Source.RestoreRoot, plan.Source.RecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), req.WebJob, req.Actor, req.ReportSHA256)
	if err != nil || source.Driver != cfg.Driver {
		return failure("source_unavailable")
	}
	// No Web-selected path participates here. A fresh helper-owned operation
	// directory prevents either a repeated request or foreign API UID replacing
	// the independently verified installation input.
	operation := filepath.Join(host.StateDir, "operation-"+req.ID)
	if os.Mkdir(operation, 0o700) != nil {
		return failure("preparation_failed")
	}
	for _, name := range []string{"state", "workspace"} {
		if os.Mkdir(filepath.Join(operation, name), 0o700) != nil {
			return failure("preparation_failed")
		}
	}
	staged := filepath.Join(operation, "recovered")
	limits := backup.Limits{MaxBytes: 1 << 30, MaxEntries: 100000}
	if phase("copying") != nil {
		return failure("interrupted")
	}
	if handoff.Copy(ctx, source.Directory, staged, host.APIUID, source.ReportSHA256, source.Driver, limits) != nil {
		return failure("preparation_failed")
	}
	// Recheck the authenticated record and current actor after potentially long
	// copying, before any empty-database import or deployment service control.
	if phase("checking-copy") != nil {
		return failure("interrupted")
	}
	after, err := app.ResolveWebMaintenanceSource(ctx, sourceConfig, plan.Source.RestoreRoot, plan.Source.RecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), req.WebJob, req.Actor, req.ReportSHA256)
	if err != nil || after != source {
		return failure("source_unavailable")
	}
	cfg.Staged, cfg.ExpectedReportSHA256 = staged, source.ReportSHA256
	cfg.StateDir, cfg.Workspace = filepath.Join(operation, "state"), filepath.Join(operation, "workspace")
	if maintenance.ValidateConfig(cfg) != nil {
		return failure("preparation_failed")
	}
	planPath := filepath.Join(operation, "maintenance-plan.json")
	if writeHostPlan(planPath, cfg) != nil {
		return failure("preparation_failed")
	}
	imported := false
	if cfg.Driver != "sqlite" {
		if phase("importing") != nil {
			return failure("interrupted")
		}
		dsn := os.Getenv(cfg.TargetDSNEnv)
		if len(dsn) == 0 || len(dsn) > 16384 || dsn == sourceConfig.DBDSN {
			return failure("import_failed")
		}
		if _, err := app.ImportVerifiedNativeStage(ctx, app.Config{DBDriver: cfg.Driver, DBDSN: dsn}, staged, source.ReportSHA256, source.SourceSHA256, cfg.Workspace, limits); err != nil {
			return maintenancehost.Outcome{State: "needsRecovery", ErrorCode: "import_failed"}, maintenancehost.ErrUnavailable
		}
		imported = true
	}
	after, err = app.ResolveWebMaintenanceSource(ctx, sourceConfig, plan.Source.RestoreRoot, plan.Source.RecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), req.WebJob, req.Actor, req.ReportSHA256)
	if err != nil || after != source {
		if imported {
			return maintenancehost.Outcome{State: "needsRecovery", ErrorCode: "source_unavailable"}, maintenancehost.ErrUnavailable
		}
		return failure("source_unavailable")
	}
	if phase("maintenance") != nil {
		return maintenancehost.Outcome{State: "needsRecovery", ErrorCode: "interrupted"}, maintenancehost.ErrUnavailable
	}
	state, err := executeMaintenanceWithProgress(ctx, "maintain", cfg, maintenanceCLIRequest{}, func(state maintenance.State) {
		// A reporting failure must not change the durable transaction decision.
		_ = phase(hostMaintenanceStep(state))
	})
	if err != nil {
		if state.NeedsRecovery || imported {
			return maintenancehost.Outcome{State: "needsRecovery", ErrorCode: "maintenance_failed"}, maintenancehost.ErrUnavailable
		}
		return failure("maintenance_failed")
	}
	if state.Phase != "healthy" || !state.Committed {
		return maintenancehost.Outcome{State: "needsRecovery", ErrorCode: "maintenance_failed"}, maintenancehost.ErrUnavailable
	}
	return maintenancehost.Outcome{State: "succeeded"}, nil
}

func hostMaintenanceStep(state maintenance.State) string {
	switch state.PendingAction {
	case "validate", "validate-committed":
		return "validating"
	case "recreate", "recreate-original", "start-original":
		if state.Phase == "failed" {
			return "recovering-original"
		}
		return "starting"
	case "health", "health-original":
		if state.Phase == "failed" {
			return "recovering-original"
		}
		return "health-check"
	case "rollback", "rollback-environment":
		return "rolling-back"
	case "stop":
		return "stopping"
	}
	switch state.Phase {
	case "preflight":
		return "preflight"
	case "stopping", "stopped":
		return "stopping"
	case "installing", "installed":
		return "installing"
	case "validated", "committed":
		return "committing"
	case "starting":
		return "starting"
	case "healthy":
		return "health-check"
	}
	return ""
}

func writeHostPlan(name string, cfg maintenance.Config) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if json.NewEncoder(file).Encode(cfg) != nil || file.Sync() != nil || file.Close() != nil {
		return maintenancehost.ErrUnavailable
	}
	directory, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
