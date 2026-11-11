package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

// Private callbacks exercise CLI validation on Windows without exposing a
// production platform bypass or pretending to perform filesystem switching.
type installPrepared struct {
	path  string
	close func() error
}

type installActions struct {
	preflight       func() error
	prepare         func(context.Context, string, string, string, string, backup.Limits) (installPrepared, error)
	apply           func(context.Context, string, install.Targets, install.Owners, string, bool) (install.JournalInfo, error)
	applyAccess     func(context.Context, string, install.Targets, install.Owners, string, bool, install.Access) (install.JournalInfo, error)
	rollback        func(context.Context, string, install.Targets, bool) error
	resolve         func(context.Context, string, string, string, string) (string, string, error)
	resolveMapped   func(context.Context, string, string, string, string, string) (string, string, error)
	prepareVerified func(context.Context, string, string, string, string, backup.Limits, string) (installPrepared, error)
}

func productionInstallActions() installActions {
	return installActions{
		preflight: func() error {
			if runtime.GOOS != "linux" {
				return install.ErrSwitchPlatform
			}
			return nil
		},
		prepare: func(ctx context.Context, staged, workspace, data, mail string, limits backup.Limits) (installPrepared, error) {
			p, err := install.Prepare(ctx, staged, workspace, data, mail, limits)
			if err != nil {
				return installPrepared{}, err
			}
			return installPrepared{path: p.Path, close: p.Close}, nil
		},
		apply: func(ctx context.Context, path string, targets install.Targets, owners install.Owners, journal string, confirmed bool) (install.JournalInfo, error) {
			return install.Apply(ctx, path, targets, owners, journal, confirmed)
		},
		applyAccess: func(ctx context.Context, path string, targets install.Targets, owners install.Owners, journal string, confirmed bool, access install.Access) (install.JournalInfo, error) {
			return install.Apply(ctx, path, targets, owners, journal, confirmed, access)
		},
		rollback:      install.Rollback,
		resolve:       app.ResolveWebRestore,
		resolveMapped: app.ResolveWebRestoreAt,
		prepareVerified: func(ctx context.Context, staged, workspace, data, mail string, limits backup.Limits, expected string) (installPrepared, error) {
			p, err := install.Prepare(ctx, staged, workspace, data, mail, limits, expected)
			if err != nil {
				return installPrepared{}, err
			}
			return installPrepared{path: p.Path, close: p.Close}, nil
		},
	}
}

func runInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runInstallWithActions(ctx, args, stdout, stderr, productionInstallActions())
}

func installTargetFlags(flags *flag.FlagSet) *install.Targets {
	targets := new(install.Targets)
	flags.StringVar(&targets.Data, "data-target", "", "existing stopped-service data directory")
	flags.StringVar(&targets.Mail, "mail-target", "", "existing stopped-service mail directory")
	flags.StringVar(&targets.DKIM, "dkim-target", "", "existing stopped-service DKIM directory")
	return targets
}

func completeInstallTargets(targets install.Targets) bool {
	return targets.Data != "" && targets.Mail != "" && targets.DKIM != ""
}

func runInstallWithActions(ctx context.Context, args []string, stdout, _ io.Writer, actions installActions) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	// flag's default diagnostics echo rejected values, potentially including an
	// accidentally pasted private key. Never propagate parser errors or values.
	flags.SetOutput(io.Discard)
	staged := flags.String("staged", "", "verified restore container containing READY and snapshot")
	webJob := flags.String("web-job", "", "completed verified Web recovery task ID (instead of --staged)")
	webDatabase := flags.String("web-state-db", "", "original SQLite application database containing the authenticated Web recovery task")
	webRoot := flags.String("web-restore-root", "", "explicit original server recovery workspace root")
	webRecordedRoot := flags.String("web-recorded-root", "", "optional original container recovery root for explicit host mapping")
	workspace := flags.String("workspace", "", "existing private preparation workspace")
	dataRoot := flags.String("runtime-data-root", "", "destination Linux/container runtime data root")
	mailRoot := flags.String("runtime-mail-root", "", "destination Linux/container runtime mail root")
	targets := installTargetFlags(flags)
	journalDir := flags.String("journal-dir", "", "existing private persistent journal directory")
	confirmed := flags.Bool("confirm-maintenance", false, "acknowledge ALL writers are stopped for the maintenance window")
	groupRead := flags.Bool("sqlite-group-read", false, "explicitly allow the selected non-root data GID to read SQLite main/WAL/SHM; other data remains private")
	owners := install.Owners{}
	ownerValues := []*int{&owners.Data.UID, &owners.Data.GID, &owners.Mail.UID, &owners.Mail.GID, &owners.DKIM.UID, &owners.DKIM.GID}
	for i, name := range []string{"data-uid", "data-gid", "mail-uid", "mail-gid", "dkim-uid", "dkim-gid"} {
		flags.IntVar(ownerValues[i], name, -1, "explicit destination service numeric identity (0..2147483647)")
	}
	limits := backup.DefaultLimits()
	flags.Int64Var(&limits.MaxBytes, "max-bytes", limits.MaxBytes, "maximum preparation payload bytes")
	flags.IntVar(&limits.MaxEntries, "max-entries", limits.MaxEntries, "maximum preparation payload entries")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid install arguments; use only documented flags, never private key values")
	}
	web := *webJob != "" || *webDatabase != "" || *webRoot != "" || *webRecordedRoot != ""
	if web && (*webJob == "" || *webDatabase == "" || *webRoot == "" || *staged != "") {
		return errors.New("install web recovery requires --web-job, --web-state-db and --web-restore-root together, without --staged")
	}
	if flags.NArg() != 0 || !web && *staged == "" || *workspace == "" || *dataRoot == "" || *mailRoot == "" || *journalDir == "" || !completeInstallTargets(*targets) {
		return errors.New("install requires --staged, --workspace, --runtime-data-root, --runtime-mail-root, --data-target, --mail-target, --dkim-target and --journal-dir; no positional arguments")
	}
	for _, value := range ownerValues {
		if *value < 0 || int64(*value) > 2147483647 {
			return errors.New("install requires all six explicit --data-uid, --data-gid, --mail-uid, --mail-gid, --dkim-uid and --dkim-gid values in 0..2147483647")
		}
	}
	if *groupRead && (owners.Data.GID <= 0 || actions.applyAccess == nil) {
		return errors.New("SQLite group reading requires an explicit non-root --data-gid and an access-aware installer")
	}
	if !*confirmed {
		return install.ErrMaintenanceRequired
	}
	if limits.MaxBytes < 1 || limits.MaxEntries < 1 {
		return errors.New("install limits must be positive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := actions.preflight(); err != nil {
		return install.ErrSwitchPlatform
	}
	var p installPrepared
	var err error
	if web {
		if *webRecordedRoot == "" && actions.resolve == nil || *webRecordedRoot != "" && actions.resolveMapped == nil || actions.prepareVerified == nil {
			return errors.New("web recovery handoff unavailable")
		}
		var resolved, receipt string
		if *webRecordedRoot == "" {
			resolved, receipt, err = actions.resolve(ctx, *webDatabase, *webRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), *webJob)
		} else {
			resolved, receipt, err = actions.resolveMapped(ctx, *webDatabase, *webRoot, *webRecordedRoot, os.Getenv("LANQIN_BACKUP_SECRET_KEY"), *webJob)
		}
		if err != nil {
			return errors.New("cannot verify Web recovery handoff; no live directory changes made")
		}
		p, err = actions.prepareVerified(ctx, resolved, *workspace, *dataRoot, *mailRoot, limits, receipt)
		if err != nil {
			return errors.New("cannot prepare authenticated Web recovery installation; services NOT started")
		}
	} else {
		p, err = actions.prepare(ctx, *staged, *workspace, *dataRoot, *mailRoot, limits)
	}
	if err != nil {
		return errors.New("cannot prepare restore installation; services NOT started")
	}
	var info install.JournalInfo
	var applyErr error
	if *groupRead {
		info, applyErr = actions.applyAccess(ctx, p.path, *targets, owners, *journalDir, *confirmed, install.Access{SQLiteGroupRead: true})
	} else {
		info, applyErr = actions.apply(ctx, p.path, *targets, owners, *journalDir, *confirmed)
	}
	closeErr := p.close()
	// A failed/partial transaction may still need recovery. Emit only the safe
	// transaction receipt, never raw filesystem/provider/database errors.
	if info.Path != "" {
		if err := writeInstallReceipt(stdout, info); err != nil {
			return err
		}
	}
	if applyErr != nil {
		return errors.New("restore installation failed; retain journal and rollback directories, inspect transaction before retry; services NOT started")
	}
	if closeErr != nil {
		return errors.New("restore switched but private preparation cleanup failed; retain journal; services NOT started")
	}
	return nil
}

func writeInstallReceipt(stdout io.Writer, info install.JournalInfo) error {
	_, err := fmt.Fprintf(stdout, "transaction=%q\njournal=%q\nrollback-data=%q\nrollback-mail=%q\nrollback-dkim=%q\nrollback-targets above are the original live target arguments, not the saved trees\nsaved-old-data=%q\nsaved-old-mail=%q\nsaved-old-dkim=%q\nreview-plan=%q\nservices NOT started; inspect health before start\n", info.TransactionID, info.Path, info.RollbackTargets.Data, info.RollbackTargets.Mail, info.RollbackTargets.DKIM, info.RollbackTargets.Data+".rollback-"+info.TransactionID, info.RollbackTargets.Mail+".rollback-"+info.TransactionID, info.RollbackTargets.DKIM+".rollback-"+info.TransactionID, filepath.Join(info.RollbackTargets.Data, "restore-install-plan.json"))
	if err != nil {
		return errors.New("cannot write restore transaction receipt; retain journal; services NOT started")
	}
	return nil
}

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runRollbackWithActions(ctx, args, stdout, stderr, productionInstallActions())
}

func runRollbackWithActions(ctx context.Context, args []string, stdout, _ io.Writer, actions installActions) error {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	journal := flags.String("journal", "", "retained transaction journal")
	targets := installTargetFlags(flags)
	confirmed := flags.Bool("confirm-maintenance", false, "acknowledge ALL writers are stopped for the maintenance window")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid rollback arguments; use only documented flags, never private key values")
	}
	if flags.NArg() != 0 || *journal == "" || !completeInstallTargets(*targets) {
		return errors.New("rollback requires --journal, --data-target, --mail-target and --dkim-target; no positional arguments")
	}
	if !*confirmed {
		return install.ErrMaintenanceRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := actions.preflight(); err != nil {
		return install.ErrSwitchPlatform
	}
	if err := actions.rollback(ctx, *journal, *targets, *confirmed); err != nil {
		return errors.New("restore rollback failed; retain journal and all transaction directories; services NOT started")
	}
	if _, err := fmt.Fprintf(stdout, "journal=%q\nservices NOT started; inspect health before start\n", *journal); err != nil {
		return errors.New("cannot write rollback receipt; retain journal; services NOT started")
	}
	return nil
}
