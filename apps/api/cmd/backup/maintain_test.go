package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/maintenance"
)

func maintenanceCLIFixture(t *testing.T) (maintenance.Config, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"project", "state", "work", "staged", "data", "mail", "dkim"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pin := func(name, content string) maintenance.PathDigest {
		path := filepath.Join(root, "project", name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		sha := sha256.Sum256([]byte(content))
		return maintenance.PathDigest{Path: path, SHA256: hex.EncodeToString(sha[:])}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owners := install.Owner{UID: 0, GID: 0}
	cfg := maintenance.Config{Version: 1, Mode: "all-in-one", DockerPath: executable, ProjectDirectory: filepath.Join(root, "project"), ProjectName: "synthetic-maintenance-cli", ComposeFiles: []maintenance.PathDigest{pin("compose.yml", "services:\n  lanqin-email:\n    image: synthetic.example.invalid/mail:fixture\n")}, EnvironmentFile: pin("private.env", "LANQIN_DB_DRIVER=sqlite\nSYNTHETIC_SECRET=never-echo-maintenance-env\n"), StateDir: filepath.Join(root, "state"), Targets: install.Targets{Data: filepath.Join(root, "data"), Mail: filepath.Join(root, "mail"), DKIM: filepath.Join(root, "dkim")}, Owners: install.Owners{Data: owners, Mail: owners, DKIM: owners}, RuntimeDataRoot: "/data", RuntimeMailRoot: "/var/mail/vhosts", RuntimeDKIMRoot: "/var/lib/rspamd/dkim", Workspace: filepath.Join(root, "work"), Staged: filepath.Join(root, "staged"), ExpectedReportSHA256: strings.Repeat("a", 64), Driver: "sqlite"}
	path := filepath.Join(root, "private-plan.json")
	writeMaintenanceCLIPlan(t, path, cfg)
	return cfg, path
}

func writeMaintenanceCLIPlan(t *testing.T, path string, cfg maintenance.Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil || os.WriteFile(path, data, 0o600) != nil {
		t.Fatal("cannot write synthetic maintenance plan", err)
	}
}

func TestMaintenanceCLIMutationGatePrecedesConfigurationAndEnvironmentAccess(t *testing.T) {
	for _, mode := range []string{"maintain", "maintain-recover"} {
		args := []string{"--config", filepath.Join(t.TempDir(), "missing-private-plan.json"), "--confirm-maintenance"}
		if mode == "maintain-recover" {
			args = append(args, "--operation", "rollback")
		}
		called := false
		gate := errors.New("synthetic Linux-only gate")
		err := runMaintenanceWithExecutor(t.Context(), mode, args, io.Discard, func() error { return gate }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
			called = true
			return maintenance.State{}, nil
		})
		if !errors.Is(err, gate) || called {
			t.Fatal("mutation accessed config/executor before platform gate", mode, err)
		}
		if runtime.GOOS != "linux" {
			if err := runMaintenance(t.Context(), mode, args, io.Discard, io.Discard); !errors.Is(err, install.ErrSwitchPlatform) {
				t.Fatal("public mutation bypassed Linux gate", mode, err)
			}
		}
	}
}

func TestMaintenanceCLIRejectsMissingConfirmationAndNonFixedOperationsBeforeEffects(t *testing.T) {
	_, plan := maintenanceCLIFixture(t)
	cases := []struct {
		mode string
		args []string
	}{
		{"maintain", []string{"--config", plan}},
		{"maintain-recover", []string{"--config", plan, "--operation", "rollback"}},
		{"maintain-recover", []string{"--config", plan, "--confirm-maintenance"}},
		{"maintain-recover", []string{"--config", plan, "--confirm-maintenance", "--operation", "shell"}},
		{"maintain-recover", []string{"--config", plan, "--confirm-maintenance", "--operation", "stop"}},
		{"maintain", []string{"--config", plan, "--confirm-maintenance", "--operation", "rollback"}},
		{"maintain-plan", []string{"--config", plan, "--journal", "/arbitrary/path"}},
		{"maintain-status", []string{"--config", plan, "--shell-hook=never-echo-maintenance-secret"}},
		{"unknown-maintenance", []string{"--config", plan}},
	}
	for _, test := range cases {
		var output bytes.Buffer
		err := runMaintenanceWithExecutor(t.Context(), test.mode, test.args, &output, func() error { t.Fatal("invalid CLI touched platform/action stage"); return nil }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
			t.Fatal("invalid CLI touched executor")
			return maintenance.State{}, nil
		})
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "never-echo-maintenance-secret") {
			t.Fatal("invalid maintenance argument accepted or echoed", test.mode)
		}
	}
}

func TestMaintenanceCLIRequiresEveryExplicitUIDGIDAndPrivatePlan(t *testing.T) {
	_, plan := maintenanceCLIFixture(t)
	base, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"data", "mail", "dkim"} {
		for _, identity := range []string{"uid", "gid"} {
			var body map[string]any
			if json.Unmarshal(base, &body) != nil {
				t.Fatal("bad plan fixture")
			}
			delete(body["owners"].(map[string]any)[role].(map[string]any), identity)
			encoded, _ := json.Marshal(body)
			if err := os.WriteFile(plan, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			err := runMaintenanceWithExecutor(t.Context(), "maintain-plan", []string{"--config", plan}, io.Discard, func() error { return nil }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
				called = true
				return maintenance.State{}, nil
			})
			if err == nil || called {
				t.Fatal("missing explicit UID/GID inferred root identity", role, identity)
			}
		}
	}
	if err := os.WriteFile(plan, base, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if err := os.Chmod(plan, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runMaintenanceWithExecutor(t.Context(), "maintain-plan", []string{"--config", plan}, io.Discard, func() error { return nil }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
			t.Fatal("public maintenance plan executed")
			return maintenance.State{}, nil
		}); err == nil {
			t.Fatal("non-private plan accepted")
		}
	}
}

func TestMaintenanceCLIRecoveryReceiptCannotEscapeStateDirectoryOrTransaction(t *testing.T) {
	cfg, plan := maintenanceCLIFixture(t)
	txn := strings.Repeat("1", 32)
	valid := filepath.Join(cfg.StateDir, "restore-"+txn+".journal")
	for _, test := range []struct {
		journal, transaction string
		valid                bool
	}{
		{valid, txn, true},
		{filepath.Join(filepath.Dir(cfg.StateDir), "restore-"+txn+".journal"), txn, false},
		{filepath.Join(cfg.StateDir, "arbitrary.journal"), txn, false},
		{valid, strings.Repeat("2", 32), false},
		{valid, "not-a-transaction", false},
		{valid, "", false},
		{"", txn, false},
	} {
		called := false
		args := []string{"--config", plan, "--confirm-maintenance", "--operation", "rollback", "--journal", test.journal, "--transaction", test.transaction}
		err := runMaintenanceWithExecutor(t.Context(), "maintain-recover", args, io.Discard, func() error { return nil }, func(_ context.Context, mode string, actual maintenance.Config, request maintenanceCLIRequest) (maintenance.State, error) {
			called = true
			if !test.valid || mode != "maintain-recover" || request.Operation != "rollback" || request.Transaction == nil || request.Transaction.Path != valid || request.Transaction.TransactionID != txn || request.Transaction.RollbackTargets != cfg.Targets || actual.StateDir != cfg.StateDir {
				t.Fatal("recovery receipt was not explicitly bound")
			}
			return maintenance.State{}, nil
		})
		if called != test.valid || (err == nil) != test.valid {
			t.Fatal("unsafe recovery receipt accepted or valid receipt lost", test.journal, err)
		}
	}
}

type maintenanceStatusController struct{}

func (maintenanceStatusController) Preflight(context.Context, maintenance.Config) error { return nil }
func (maintenanceStatusController) Stop(context.Context, maintenance.Config) error      { return nil }
func (maintenanceStatusController) VerifyStopped(context.Context, maintenance.Config) error {
	return nil
}
func (maintenanceStatusController) Recreate(context.Context, maintenance.Config) error { return nil }
func (maintenanceStatusController) Healthy(context.Context, maintenance.Config) error  { return nil }

func TestMaintenanceCLIPlanAndStatusNeverExecuteDockerOrReadNativeDSN(t *testing.T) {
	cfg, plan := maintenanceCLIFixture(t)
	txn := strings.Repeat("3", 32)
	info := install.JournalInfo{Path: filepath.Join(cfg.StateDir, "restore-"+txn+".journal"), TransactionID: txn, RollbackTargets: cfg.Targets}
	_, err := maintenance.Run(t.Context(), cfg, maintenanceStatusController{}, maintenance.Actions{Install: func(context.Context) (install.JournalInfo, error) { return info, nil }, Validate: func(context.Context, install.JournalInfo) error { return nil }, Rollback: func(context.Context, install.JournalInfo) error { return nil }})
	if err != nil {
		t.Fatal("cannot seed real durable status journal", err)
	}
	var output bytes.Buffer
	if err := runMaintenanceWithExecutor(t.Context(), "maintain-status", []string{"--config", plan}, &output, func() error { t.Fatal("status invoked platform mutation gate"); return nil }, executeMaintenance); err != nil {
		t.Fatal("status attempted Docker/env action instead of ReadState", err)
	}
	var state maintenance.State
	if json.Unmarshal(output.Bytes(), &state) != nil || state.Phase != "healthy" || !state.Committed {
		t.Fatal("status did not return durable state")
	}
	// A native plan can be reviewed without retrieving its host target DSN or
	// constructing a Compose controller. Only the pinned file metadata is read.
	next := filepath.Join(t.TempDir(), "next-private.env")
	content := []byte("LANQIN_DB_DRIVER=postgres\nLANQIN_DB_NAME=new\n")
	if err := os.WriteFile(next, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256(content)
	cfg.NextEnvironmentFile = &maintenance.PathDigest{Path: next, SHA256: hex.EncodeToString(sha[:])}
	cfg.Driver, cfg.TargetDSNEnv = "postgres", "UNAVAILABLE_SYNTHETIC_HOST_TARGET_DSN"
	cfg.RuntimeDatabase = &maintenance.RuntimeDatabaseBinding{Host: "database", Port: 5432, Database: "new", User: "new"}
	t.Setenv(cfg.TargetDSNEnv, "")
	writeMaintenanceCLIPlan(t, plan, cfg)
	output.Reset()
	if err := runMaintenanceWithExecutor(t.Context(), "maintain-plan", []string{"--config", plan}, &output, func() error { t.Fatal("plan invoked mutation gate"); return nil }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
		t.Fatal("plan constructed service executor")
		return maintenance.State{}, nil
	}); err != nil {
		t.Fatal("native plan required secret DSN or Docker", err)
	}
	if !strings.Contains(output.String(), `"environmentTransition":true`) || !strings.Contains(output.String(), `"servicesStarted":false`) || strings.Contains(output.String(), "never-echo-maintenance-env") {
		t.Fatal("plan output overclaimed activation or exposed environment")
	}
}

func TestMaintenanceCLIActionErrorsAndReceiptFailuresPreserveUncertainOutcome(t *testing.T) {
	_, plan := maintenanceCLIFixture(t)
	var output bytes.Buffer
	state := maintenance.State{Version: 1, OperationID: strings.Repeat("4", 32), Phase: "failed", Committed: true, NeedsRecovery: true, LastErrorCode: "health_failed"}
	executor := func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
		return state, errors.New("postgres://user:never-echo-private-password@host/db never-echo-maintenance-env")
	}
	err := runMaintenanceWithExecutor(t.Context(), "maintain", []string{"--config", plan, "--confirm-maintenance"}, &output, func() error { return nil }, executor)
	if err == nil || !strings.Contains(output.String(), `"needsRecovery":true`) || strings.Contains(err.Error()+output.String(), "never-echo") {
		t.Fatal("action failure erased uncertainty or exposed backend exception")
	}
	err = runMaintenanceWithExecutor(t.Context(), "maintain", []string{"--config", plan, "--confirm-maintenance"}, nativeFailWriter{}, func() error { return nil }, func(context.Context, string, maintenance.Config, maintenanceCLIRequest) (maintenance.State, error) {
		return state, nil
	})
	if err == nil || !strings.Contains(err.Error(), "may already have changed") || strings.Contains(err.Error(), "not changed") {
		t.Fatal("stdout failure falsely claimed no deployment changes", err)
	}
}

func TestMaintenancePlanBindingRejectsChangedConfigUnknownFieldsAndDigests(t *testing.T) {
	cfg, _ := maintenanceCLIFixture(t)
	hash, _ := maintenance.ConfigHash(cfg)
	binding := maintenancePlanBinding{Version: 1, ConfigSHA256: hash, PlanSHA256: strings.Repeat("b", 64), ReportSHA256: cfg.ExpectedReportSHA256, DatabaseSHA256: strings.Repeat("c", 64)}
	file := filepath.Join(cfg.StateDir, "install-binding.json")
	data, _ := json.Marshal(binding)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if actual, err := readMaintenanceBinding(cfg); err != nil || actual != binding {
		t.Fatal("valid independently captured binding rejected", err)
	}
	for _, document := range [][]byte{
		bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"secret":"never-echo-private-password"`), 1),
		bytes.Replace(data, []byte(binding.PlanSHA256), []byte("invalid-plan-hash"), 1),
		bytes.Replace(data, []byte(binding.ReportSHA256), []byte("invalid-report-hash"), 1),
		bytes.Replace(data, []byte(binding.DatabaseSHA256), []byte("invalid-prepared-database-hash"), 1),
		append(append([]byte{}, data...), []byte(`{}`)...), bytes.Repeat([]byte{' '}, 4097),
	} {
		if err := os.WriteFile(file, document, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readMaintenanceBinding(cfg); err == nil || strings.Contains(err.Error(), "never-echo-private-password") {
			t.Fatal("invalid binding accepted or exposed private content")
		}
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.ProjectName += "-other"
	if _, err := readMaintenanceBinding(changed); err == nil {
		t.Fatal("binding crossed deployment plan")
	}
	if runtime.GOOS == "linux" {
		if err := writeMaintenanceBinding(cfg, binding); err == nil {
			t.Fatal("existing binding overwritten")
		}
	}
}

func TestMaintenanceActionsOnlyCertifyUnchangedBeforeDirectoryApply(t *testing.T) {
	cfg, _ := maintenanceCLIFixture(t)
	actions, err := newMaintenanceInstallActions(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := actions.Install(t.Context())
	if info.Path != "" || !errors.Is(err, maintenance.ErrInstallUnchanged) {
		t.Fatal("pre-Apply validation failure lost unchanged sentinel", err)
	}
	cfg.Driver, cfg.TargetDSNEnv = "postgres", "UNAVAILABLE_SYNTHETIC_HOST_TARGET_DSN"
	t.Setenv(cfg.TargetDSNEnv, "")
	if _, err := newMaintenanceInstallActions(cfg, nil); err == nil || strings.Contains(err.Error(), "UNAVAILABLE_SYNTHETIC_HOST_TARGET_DSN") {
		t.Fatal("native action accepted absent target authority or echoed variable")
	}
}

func TestVerifyNativeCLIRequiresExplicitAuthorityAndRedactsVerifierErrors(t *testing.T) {
	base := []string{"--driver=postgres", "--dsn-env=SYNTHETIC_RUNTIME_VERIFY_DSN", "--data-root=/data", "--mail-root=/var/mail/vhosts", "--expected-plan-sha256=" + strings.Repeat("AB", 32)}
	t.Setenv("SYNTHETIC_RUNTIME_VERIFY_DSN", "postgres://synthetic:never-echo-private-password@database/new")
	for i := range base {
		args := append(append([]string{}, base[:i]...), base[i+1:]...)
		if err := runVerifyNativeWithVerifier(t.Context(), args, io.Discard, func(context.Context, app.Config, string) error {
			t.Fatal("invalid verify-native touched runtime database")
			return nil
		}); err == nil {
			t.Fatal("missing runtime authority accepted")
		}
	}
	for _, failed := range []bool{false, true} {
		var output bytes.Buffer
		err := runVerifyNativeWithVerifier(t.Context(), base, &output, func(_ context.Context, cfg app.Config, sha string) error {
			if cfg.DBDriver != "postgres" || cfg.DBDSN != "postgres://synthetic:never-echo-private-password@database/new" || cfg.DataDir != "/data" || cfg.MaildirRoot != "/var/mail/vhosts" || sha != strings.Repeat("ab", 32) {
				t.Fatal("runtime verifier authority changed")
			}
			if failed {
				return errors.New(cfg.DBDSN)
			}
			return nil
		})
		if (err != nil) != failed || strings.Contains(output.String(), "never-echo-private-password") || err != nil && strings.Contains(err.Error(), "never-echo-private-password") {
			t.Fatal("runtime verify status/secret handling incorrect")
		}
		if !failed && !strings.Contains(output.String(), "no migrations, services or workers started") {
			t.Fatal("runtime verifier omitted read-only boundary")
		}
	}
}
