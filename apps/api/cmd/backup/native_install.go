package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"runtime"
	"time"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

type nativeMaintenanceActions struct {
	preflight     func() error
	prepare       func(context.Context, app.Config, string, string, string, string, install.Targets, string, backup.Limits) (*install.Prepared, error)
	apply         func(context.Context, string, install.Targets, install.Owners, string, bool) (install.JournalInfo, error)
	activate      func(context.Context, app.Config, string, install.Targets, bool) (install.JournalInfo, error)
	rollback      func(context.Context, app.Config, string, install.Targets, bool) error
	resolve       func(context.Context, string, string, string, string, string) (string, string, error)
	resolveMapped func(context.Context, string, string, string, string, string, string) (string, string, error)
}

type nativeMaintenanceReceipt struct {
	Transaction       string          `json:"transaction,omitempty"`
	Journal           string          `json:"journal"`
	Targets           install.Targets `json:"targets"`
	DatabaseState     string          `json:"databaseState"`
	ServicesStarted   bool            `json:"servicesStarted"`
	RollbackCompleted bool            `json:"rollbackCompleted,omitempty"`
}

func runNativeMaintenance(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	return runNativeMaintenanceWithActions(ctx, mode, args, stdout, nativeMaintenanceActions{
		preflight: func() error {
			if runtime.GOOS != "linux" {
				return install.ErrSwitchPlatform
			}
			return nil
		},
		prepare: app.PrepareNativeInstallation,
		apply: func(ctx context.Context, path string, targets install.Targets, owners install.Owners, journal string, confirmed bool) (install.JournalInfo, error) {
			return install.Apply(ctx, path, targets, owners, journal, confirmed)
		},
		activate: app.ActivateNativeInstallation, rollback: app.RollbackNativeInstallation,
		resolve:       app.ResolveWebNativeRestore,
		resolveMapped: app.ResolveWebNativeRestoreAt,
	})
}

func runNativeMaintenanceWithActions(ctx context.Context, mode string, args []string, stdout io.Writer, actions nativeMaintenanceActions) error {
	if mode != "install-native" && mode != "activate-native" && mode != "rollback-native" {
		return errors.New("unknown native maintenance mode")
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	driver := flags.String("driver", "", "native database driver: mysql or postgres")
	dsnEnv := flags.String("target-dsn-env", "", "explicit variable for the guarded NEW imported database, never the original live database")
	confirmed := flags.Bool("confirm-maintenance", false, "acknowledge ALL writers stopped; no service control or deployment DSN changes will be performed")
	targets := installTargetFlags(flags)
	staged, workspace, dataRoot, mailRoot, reportSHA, journalDir, journal := "", "", "", "", "", "", ""
	webJob, webStateDSNEnv, webRoot := "", "", ""
	webRecordedRoot := ""
	owners := install.Owners{}
	limits := backup.DefaultLimits()
	if mode == "install-native" {
		flags.StringVar(&staged, "staged", "", "native restore container with READY and snapshot")
		flags.StringVar(&webJob, "web-job", "", "completed verified native Web recovery task, instead of --staged and --expected-report-sha256")
		flags.StringVar(&webStateDSNEnv, "web-state-dsn-env", "", "explicit variable for the ORIGINAL same-engine Web task database, distinct from target DSN")
		flags.StringVar(&webRoot, "web-restore-root", "", "original private Web recovery workspace root")
		flags.StringVar(&webRecordedRoot, "web-recorded-root", "", "optional original container recovery root for explicit host mapping")
		flags.StringVar(&workspace, "workspace", "", "existing private preparation workspace")
		flags.StringVar(&dataRoot, "runtime-data-root", "", "destination API/container runtime data root")
		flags.StringVar(&mailRoot, "runtime-mail-root", "", "destination runtime Maildir root")
		flags.StringVar(&reportSHA, "expected-report-sha256", "", "trusted report receipt from authenticated native restore/import")
		flags.StringVar(&journalDir, "journal-dir", "", "existing private durable journal directory")
		values := []*int{&owners.Data.UID, &owners.Data.GID, &owners.Mail.UID, &owners.Mail.GID, &owners.DKIM.UID, &owners.DKIM.GID}
		for i, name := range []string{"data-uid", "data-gid", "mail-uid", "mail-gid", "dkim-uid", "dkim-gid"} {
			flags.IntVar(values[i], name, -1, "explicit target service numeric identity")
		}
		flags.Int64Var(&limits.MaxBytes, "max-bytes", limits.MaxBytes, "maximum prepared bytes")
		flags.IntVar(&limits.MaxEntries, "max-entries", limits.MaxEntries, "maximum prepared file entries")
	} else {
		flags.StringVar(&journal, "journal", "", "retained directory transaction journal")
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*driver != "mysql" && *driver != "postgres") || !nativeDSNEnvPattern.MatchString(*dsnEnv) || !completeInstallTargets(*targets) {
		return errors.New("native maintenance requires a supported --driver, explicit --target-dsn-env and all three target directories; use only documented flags")
	}
	if !*confirmed {
		return install.ErrMaintenanceRequired
	}
	web := webJob != "" || webStateDSNEnv != "" || webRoot != "" || webRecordedRoot != ""
	if mode == "install-native" {
		digest, err := hex.DecodeString(reportSHA)
		if web && (webJob == "" || !nativeDSNEnvPattern.MatchString(webStateDSNEnv) || webStateDSNEnv == *dsnEnv || webRoot == "" || staged != "" || reportSHA != "") {
			return errors.New("install-native Web handoff requires --web-job, --web-state-dsn-env and --web-restore-root together; state and target DSN references must differ; omit --staged and --expected-report-sha256")
		}
		if !web && (staged == "" || err != nil || len(digest) != 32) || workspace == "" || dataRoot == "" || mailRoot == "" || journalDir == "" || limits.MaxBytes < 4096 || limits.MaxBytes > 1<<40 || limits.MaxEntries < 4 || limits.MaxEntries > 100000 {
			return errors.New("install-native requires --staged, --workspace, --runtime-data-root, --runtime-mail-root, --expected-report-sha256, --journal-dir and valid limits")
		}
		for _, owner := range []install.Owner{owners.Data, owners.Mail, owners.DKIM} {
			if owner.UID < 0 || owner.GID < 0 || int64(owner.UID) > 2147483647 || int64(owner.GID) > 2147483647 {
				return errors.New("install-native requires all six explicit valid UID/GID values")
			}
		}
	} else if journal == "" {
		return errors.New("native maintenance recovery requires --journal")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := actions.preflight(); err != nil {
		return err
	}
	dsn, ok := os.LookupEnv(*dsnEnv)
	if !ok || dsn == "" || len(dsn) > 16384 {
		return errors.New("native NEW target DSN environment variable unavailable")
	}
	cfg := app.Config{DBDriver: *driver, DBDSN: dsn}
	if web {
		stateDSN, ok := os.LookupEnv(webStateDSNEnv)
		if !ok || stateDSN == "" || len(stateDSN) > 16384 || stateDSN == dsn || webRecordedRoot == "" && actions.resolve == nil || webRecordedRoot != "" && actions.resolveMapped == nil {
			return errors.New("native Web task DSN unavailable or not separate from the NEW target")
		}
		var err error
		if webRecordedRoot == "" {
			staged, reportSHA, err = actions.resolve(ctx, *driver, stateDSN, webRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), webJob)
		} else {
			staged, reportSHA, err = actions.resolveMapped(ctx, *driver, stateDSN, webRoot, webRecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), webJob)
		}
		if err != nil {
			return errors.New("cannot authenticate native Web recovery handoff; no target database or directory changes made")
		}
	}
	if mode == "rollback-native" {
		if err := actions.rollback(ctx, cfg, journal, *targets, true); err != nil {
			return errors.New("native rollback incomplete; retain journal, old/new trees and the guarded new database; services NOT started")
		}
		return writeNativeMaintenanceReceipt(stdout, nativeMaintenanceReceipt{Journal: journal, Targets: *targets, DatabaseState: "guarded", RollbackCompleted: true})
	}
	if mode == "activate-native" {
		info, err := actions.activate(ctx, cfg, journal, *targets, true)
		if err != nil {
			return errors.New("native activation not confirmed; inspect journal and database guard before retry; services NOT started")
		}
		return writeNativeMaintenanceReceipt(stdout, nativeMaintenanceReceipt{Transaction: info.TransactionID, Journal: info.Path, Targets: *targets, DatabaseState: "activated"})
	}
	p, err := actions.prepare(ctx, cfg, staged, workspace, dataRoot, mailRoot, *targets, reportSHA, limits)
	if err != nil {
		return errors.New("native installation preparation failed; the new database remains guarded; services NOT started")
	}
	info, applyErr := actions.apply(ctx, p.Path, *targets, owners, journalDir, true)
	closeErr := p.Close()
	receipt := nativeMaintenanceReceipt{Transaction: info.TransactionID, Journal: info.Path, Targets: *targets, DatabaseState: "unknown"}
	if applyErr == nil {
		_, activationErr := actions.activate(ctx, cfg, info.Path, *targets, true)
		if activationErr == nil {
			receipt.DatabaseState = "activated"
		} else {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rollbackErr := actions.rollback(rollbackCtx, cfg, info.Path, *targets, true)
			cancel()
			if rollbackErr == nil {
				receipt.DatabaseState = "guarded"
				receipt.RollbackCompleted = true
			}
			applyErr = errors.New("native activation failed after directory switching")
		}
	}
	if info.Path != "" {
		if err := writeNativeMaintenanceReceipt(stdout, receipt); err != nil {
			return err
		}
	}
	if applyErr != nil {
		return errors.New("native installation did not finish; retain receipt, journal and all old/new trees; inspect guarded database before retry; services NOT started")
	}
	if closeErr != nil {
		return errors.New("native directories/database switched but private preparation cleanup failed; retain receipt and journal; services NOT started")
	}
	return nil
}

func writeNativeMaintenanceReceipt(stdout io.Writer, receipt nativeMaintenanceReceipt) error {
	if err := json.NewEncoder(stdout).Encode(receipt); err != nil {
		return errors.New("cannot write native maintenance receipt; database/directory outcome may already have changed; retain the selected journal directory")
	}
	return nil
}
