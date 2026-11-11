package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/maintenance"
)

type maintenanceLinuxTestController struct {
	running bool
	starts  int
}

func (c *maintenanceLinuxTestController) Preflight(context.Context, maintenance.Config) error {
	return nil
}
func (c *maintenanceLinuxTestController) Stop(context.Context, maintenance.Config) error {
	c.running = false
	return nil
}
func (c *maintenanceLinuxTestController) VerifyStopped(context.Context, maintenance.Config) error {
	if c.running {
		return maintenance.ErrRecovery
	}
	return nil
}
func (c *maintenanceLinuxTestController) Recreate(context.Context, maintenance.Config) error {
	c.running = true
	c.starts++
	return nil
}
func (c *maintenanceLinuxTestController) Healthy(context.Context, maintenance.Config) error {
	if !c.running {
		return maintenance.ErrRecovery
	}
	return nil
}

func TestMaintenanceLinuxRealSQLiteActionsNeverReplayCommittedData(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real ownership/fsync/no-replace switching requires Linux; fake controller does not claim Docker execution")
	}
	f := newOnlineCLIFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE send_queue(id TEXT PRIMARY KEY,status TEXT,mime_base64 TEXT);
	CREATE TABLE scheduled_sends(id TEXT PRIMARY KEY,status TEXT,payload_json TEXT,send_at TEXT);
	INSERT INTO send_queue VALUES('synthetic-send','queued','preserved synthetic payload');`); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(t.Context(), f.args(), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(output.String()), "sha256=")
	if !ok {
		t.Fatal("missing ciphertext receipt")
	}
	archive, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatal(err)
	}
	cfg, planFile := maintenanceCLIFixture(t)
	staged := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(t.Context(), bytes.NewReader(archive), staged, f.identity.String(), receipt, backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	reportSHA, err := backup.StagedOnlineReportSHA256(t.Context(), staged)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Staged, cfg.ExpectedReportSHA256 = staged, reportSHA
	owner := install.Owner{UID: os.Getuid(), GID: os.Getgid()}
	cfg.Owners = install.Owners{Data: owner, Mail: owner, DKIM: owner}
	for _, directory := range []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM} {
		if err := os.WriteFile(filepath.Join(directory, "old"), []byte("synthetic original tree"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMaintenanceCLIPlan(t, planFile, cfg)
	controller := &maintenanceLinuxTestController{running: true}
	installs := 0
	apply := func(ctx context.Context, mode string, current maintenance.Config, _ maintenanceCLIRequest) (maintenance.State, error) {
		actions, err := newMaintenanceInstallActions(current, nil, func(ctx context.Context, cfg maintenance.Config) error { return controller.VerifyStopped(ctx, cfg) })
		if err != nil {
			return maintenance.State{}, err
		}
		installActual := actions.Install
		actions.Install = func(ctx context.Context) (install.JournalInfo, error) { installs++; return installActual(ctx) }
		return maintenance.Run(ctx, current, controller, actions)
	}
	output.Reset()
	if err := runMaintenanceWithExecutor(t.Context(), "maintain", []string{"--config", planFile, "--confirm-maintenance"}, &output, func() error { return nil }, apply); err != nil {
		t.Fatal("real SQLite coordinator install", err, output.String())
	}
	var result maintenance.State
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Phase != "healthy" || !result.Committed || result.NeedsRecovery || !controller.running || installs != 1 {
		t.Fatal("coordinator outcome incorrect", output.String())
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Targets.Mail, f.relative)); err != nil || !bytes.Equal(got, f.raw) {
		t.Fatal("installed actual MIME changed", err)
	}
	current, err := sql.Open("sqlite", filepath.Join(cfg.Targets.Data, "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Exec("UPDATE send_queue SET mime_base64='new committed synthetic work' WHERE id='synthetic-send'"); err != nil {
		current.Close()
		t.Fatal(err)
	}
	current.Close()
	newMail := filepath.Join(cfg.Targets.Mail, "new-committed-mail.eml")
	if err := os.WriteFile(newMail, []byte("new synthetic accepted mail"), 0o600); err != nil {
		t.Fatal(err)
	}
	actions, err := newMaintenanceInstallActions(cfg, nil, func(ctx context.Context, cfg maintenance.Config) error { return controller.VerifyStopped(ctx, cfg) })
	if err != nil {
		t.Fatal(err)
	}
	actions.Install = func(context.Context) (install.JournalInfo, error) {
		t.Fatal("recovery replayed archive installation")
		return install.JournalInfo{}, maintenance.ErrState
	}
	resumed, err := maintenance.Recover(t.Context(), cfg, controller, actions, "resume-start", true)
	if err != nil || resumed.Phase != "healthy" || !resumed.Committed || installs != 1 {
		t.Fatal("committed recovery rejected new work or reinstalled", err)
	}
	if _, err := os.Stat(newMail); err != nil {
		t.Fatal("committed new mail lost", err)
	}
	rolled, err := maintenance.Recover(t.Context(), cfg, controller, actions, "rollback", true)
	if err != nil || !rolled.RollbackCompleted || !rolled.OriginalServicesRestored {
		t.Fatal("explicit rollback failed", err)
	}
	for _, directory := range []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM} {
		if body, err := os.ReadFile(filepath.Join(directory, "old")); err != nil || string(body) != "synthetic original tree" {
			t.Fatal("old tree lost", err)
		}
	}
	if _, err := os.Stat(cfg.Targets.Mail + ".failed-" + result.Transaction.TransactionID + string(filepath.Separator) + "new-committed-mail.eml"); err != nil {
		t.Fatal("newly accepted mail not retained in quarantine", err)
	}
	t.Log("real Linux install/fsync/owners/rollback exercised with fake service controller; no Docker or real mail service was started")
}
