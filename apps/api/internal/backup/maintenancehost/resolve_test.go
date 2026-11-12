package maintenancehost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/maintenance"
)

func TestHostResolveRejectsUnsafeParametersWithoutPublishingSuccess(t *testing.T) {
	cfg := hostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   string
	}{
		{"path", context.Background(), "../private"},
		{"missing-id", context.Background(), ""},
		{"cancelled", ctx, hostRequest(cfg, 1).ID},
		{"unknown", context.Background(), hostRequest(cfg, 2).ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := Resolve(tc.ctx, cfg, tc.id)
			if err == nil || status.State != "" {
				t.Fatalf("unverified recovery published status: %+v %v", status, err)
			}
		})
	}
}

func linuxRootResolution(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires actual Linux root maintenance-host resolution")
	}
}

func quarantinedHostRecord(cfg Config, interruptedPhase string) record {
	r := hostRequest(cfg, 1)
	now := time.Now().UTC()
	return record{Request: r, InterruptedPhase: interruptedPhase, Status: Status{Version: 1, ID: r.ID, PlanID: r.PlanID, WebJob: r.WebJob, State: "needsRecovery", Phase: "interrupted", ErrorCode: "interrupted", CreatedAt: now, UpdatedAt: now}}
}

func TestHostResolveLinuxExplicitlyAbandonsOnlyProvenPreEffectPhases(t *testing.T) {
	linuxRootResolution(t)
	for _, phase := range []string{"queued", "preparing", "importing", "maintenance", ""} {
		for _, journal := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "-no-journal", true: "-with-journal"}[journal], func(t *testing.T) {
				cfg := hostFixture(t)
				rec := quarantinedHostRecord(cfg, phase)
				s := protocolService(cfg)
				if err := s.persist(rec); err != nil {
					t.Fatal(err)
				}
				if journal {
					state := filepath.Join(cfg.StateDir, "operation-"+rec.Request.ID, "state")
					if os.MkdirAll(state, 0o700) != nil || os.WriteFile(filepath.Join(state, "state.journal"), []byte("unproven-state"), 0o600) != nil {
						t.Fatal("cannot create synthetic state journal")
					}
				}
				name := filepath.Join(cfg.StateDir, rec.Request.ID+".json")
				before, _ := os.ReadFile(name)
				status, err := Resolve(context.Background(), cfg, rec.Request.ID)
				wantAbort := !journal && (phase == "queued" || phase == "preparing")
				if wantAbort {
					if err != nil || status.State != "failed" || status.Phase != "completed" || status.ErrorCode != "interrupted" {
						t.Fatalf("safe explicit abort failed: %+v %v", status, err)
					}
					reloaded := protocolService(cfg)
					if reloaded.load() != nil || reloaded.records[rec.Request.ID].Status.State != "failed" || reloaded.active {
						t.Fatal("resolved pre-effect quarantine was not durable")
					}
				} else {
					after, _ := os.ReadFile(name)
					if err == nil || status.State != "" || string(after) != string(before) {
						t.Fatalf("unproven effects were abandoned: %+v %v", status, err)
					}
				}
			})
		}
	}
}

type resolutionController struct{ calls int }

func (c *resolutionController) check(ctx context.Context) error { c.calls++; return ctx.Err() }
func (c *resolutionController) Preflight(ctx context.Context, _ maintenance.Config) error {
	return c.check(ctx)
}
func (c *resolutionController) Stop(ctx context.Context, _ maintenance.Config) error {
	return c.check(ctx)
}
func (c *resolutionController) VerifyStopped(ctx context.Context, _ maintenance.Config) error {
	return c.check(ctx)
}
func (c *resolutionController) Recreate(ctx context.Context, _ maintenance.Config) error {
	return c.check(ctx)
}
func (c *resolutionController) Healthy(ctx context.Context, _ maintenance.Config) error {
	return c.check(ctx)
}

func TestHostResolveLinuxRequiresExactGeneratedPlanAndRealCoordinatorReceipt(t *testing.T) {
	linuxRootResolution(t)
	for _, scenario := range []string{"healthy", "original-restored", "tampered-generated-plan", "journal-missing"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := hostFixture(t)
			rec := quarantinedHostRecord(cfg, "maintenance")
			template, err := LoadTemplate(cfg.Plans[0])
			if err != nil {
				t.Fatal(err)
			}
			operation := filepath.Join(cfg.StateDir, "operation-"+rec.Request.ID)
			for _, directory := range []string{operation, filepath.Join(operation, "state"), filepath.Join(operation, "workspace"), filepath.Join(operation, "recovered")} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			generated := template
			generated.StateDir, generated.Workspace = filepath.Join(operation, "state"), filepath.Join(operation, "workspace")
			generated.Staged, generated.ExpectedReportSHA256 = filepath.Join(operation, "recovered"), rec.Request.ReportSHA256
			controller := &resolutionController{}
			installs := 0
			transaction := install.JournalInfo{Path: filepath.Join(generated.StateDir, "restore-"+strings.Repeat("1", 32)+".journal"), TransactionID: strings.Repeat("1", 32), RollbackTargets: generated.Targets}
			actions := maintenance.Actions{
				Install: func(context.Context) (install.JournalInfo, error) {
					installs++
					if scenario == "original-restored" {
						return install.JournalInfo{}, maintenance.ErrInstallUnchanged
					}
					return transaction, nil
				},
				Validate: func(context.Context, install.JournalInfo) error { return nil },
				Rollback: func(context.Context, install.JournalInfo) error { t.Fatal("unexpected rollback"); return nil },
			}
			state, runErr := maintenance.Run(context.Background(), generated, controller, actions)
			if scenario == "original-restored" {
				if runErr == nil || !state.OriginalServicesRestored || !state.OriginalDataUnchanged || state.NeedsRecovery {
					t.Fatalf("missing proven original receipt: %+v %v", state, runErr)
				}
			} else if runErr != nil || !state.Committed || state.Phase != "healthy" || state.NeedsRecovery {
				t.Fatalf("real coordinator did not produce healthy receipt: %+v %v", state, runErr)
			}
			callsBefore := controller.calls
			if scenario == "tampered-generated-plan" {
				generated.ProjectName += "-foreign"
			}
			encoded, _ := json.Marshal(generated)
			if err := os.WriteFile(filepath.Join(operation, "maintenance-plan.json"), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "journal-missing" {
				if err := os.Remove(filepath.Join(generated.StateDir, "state.journal")); err != nil {
					t.Fatal(err)
				}
			}
			s := protocolService(cfg)
			if err := s.persist(rec); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(cfg.StateDir, rec.Request.ID+".json"))
			status, err := Resolve(context.Background(), cfg, rec.Request.ID)
			if scenario == "healthy" || scenario == "original-restored" {
				wantState := "succeeded"
				if scenario == "original-restored" {
					wantState = "failed"
				}
				if err != nil || status.State != wantState || status.Phase != "completed" {
					t.Fatalf("proven resolution failed: %+v %v", status, err)
				}
				reloaded := protocolService(cfg)
				if reloaded.load() != nil || reloaded.active || reloaded.records[rec.Request.ID].Status.State != wantState {
					t.Fatal("resolution not durably published")
				}
			} else {
				after, _ := os.ReadFile(filepath.Join(cfg.StateDir, rec.Request.ID+".json"))
				if err == nil || status.State != "" || string(after) != string(before) {
					t.Fatalf("unproven generated plan resolved: %+v %v", status, err)
				}
			}
			if installs != 1 || controller.calls != callsBefore {
				t.Fatal("Resolve replayed install or service actions")
			}
		})
	}
}

func TestHostResolveRefusesUnprovenRecoveryAndRetainsQuarantine(t *testing.T) {
	for _, tc := range []string{"not-quarantined", "missing-generated-plan", "mismatched-template", "invalid-generated-plan"} {
		t.Run(tc, func(t *testing.T) {
			cfg := hostFixture(t)
			r := hostRequest(cfg, 1)
			now := time.Now().UTC()
			rec := record{Request: r, Status: Status{Version: 1, ID: r.ID, PlanID: r.PlanID, WebJob: r.WebJob, State: "needsRecovery", Phase: "interrupted", ErrorCode: "interrupted", CreatedAt: now, UpdatedAt: now}}
			if tc == "not-quarantined" {
				rec.Status.State, rec.Status.Phase, rec.Status.ErrorCode = "succeeded", "completed", ""
			}
			s := protocolService(cfg)
			if err := s.persist(rec); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(cfg.StateDir, r.ID+".json")
			before, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if tc == "mismatched-template" {
				cfg.Plans[0].Template.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			if tc == "invalid-generated-plan" {
				operation := filepath.Join(cfg.StateDir, "operation-"+r.ID)
				if os.Mkdir(operation, 0o700) != nil || os.WriteFile(filepath.Join(operation, "maintenance-plan.json"), []byte(`{"command":"do-not-execute"}`), 0o600) != nil {
					t.Fatal("cannot create synthetic invalid plan")
				}
			}
			status, err := Resolve(context.Background(), cfg, r.ID)
			if err == nil || status.State != "" {
				t.Fatalf("unproven recovery accepted: %+v %v", status, err)
			}
			after, readErr := os.ReadFile(name)
			if readErr != nil || string(after) != string(before) {
				t.Fatal("rejected resolution changed the durable quarantine")
			}
			var retained record
			if json.Unmarshal(after, &retained) != nil || retained.Status.State != rec.Status.State {
				t.Fatal("quarantine record lost")
			}
		})
	}
}
