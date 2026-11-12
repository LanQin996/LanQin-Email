package maintenancehost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lanqin-email-api/internal/backup/maintenance"
)

// Resolve only clears the host's quarantine after an operator has separately
// recovered the exact generated maintenance plan. The daemon must be stopped.
// It never resumes an executor, imports a database, or starts/stops a service.
func Resolve(ctx context.Context, cfg Config, id string) (Status, error) {
	if !rootHost() || !hexID.MatchString(id) || ctx.Err() != nil {
		return Status{}, ErrUnavailable
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "host.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Status{}, ErrUnavailable
	}
	defer lock.Close()
	if !trustedPath(lock.Name(), false, true) || lockFile(lock) != nil {
		return Status{}, ErrConflict
	}
	defer unlockFile(lock)
	name := filepath.Join(cfg.StateDir, id+".json")
	data, err := readTrusted(name, 64<<10)
	if err != nil {
		return Status{}, ErrUnavailable
	}
	var rec record
	if decode(strings.NewReader(string(data)), &rec, 64<<10) != nil || !validRecord(rec) || rec.Request.ID != id || rec.Status.State != "needsRecovery" {
		return Status{}, ErrUnavailable
	}
	var approved *ApprovedPlan
	for _, plan := range cfg.Plans {
		if plan.ID == rec.Request.PlanID && plan.Template.SHA256 == rec.Request.TemplateSHA256 {
			p := plan
			approved = &p
			break
		}
	}
	if approved == nil {
		return Status{}, ErrUnavailable
	}
	template, err := LoadTemplate(*approved)
	if err != nil {
		return Status{}, ErrUnavailable
	}
	operation := filepath.Join(cfg.StateDir, "operation-"+id)
	// A durable phase before any database import or service control can be
	// explicitly abandoned without inventing a directory-switch receipt.
	if (rec.InterruptedPhase == "queued" || rec.InterruptedPhase == "preparing") && rec.Status.Phase == "interrupted" {
		if _, err := os.Lstat(filepath.Join(operation, "state", "state.journal")); !os.IsNotExist(err) {
			return Status{}, ErrUnavailable
		}
		rec.Status.State, rec.Status.Phase, rec.Status.ErrorCode = "failed", "completed", "interrupted"
		rec.Status.UpdatedAt = time.Now().UTC()
		s := service{cfg: cfg}
		if s.persist(rec) != nil {
			return Status{}, ErrUnavailable
		}
		return rec.Status, nil
	}
	data, err = readTrusted(filepath.Join(operation, "maintenance-plan.json"), 64<<10)
	if err != nil {
		return Status{}, ErrUnavailable
	}
	generated, err := maintenance.DecodeStatusConfig(strings.NewReader(string(data)))
	if err != nil {
		return Status{}, ErrUnavailable
	}
	expected := template
	expected.StateDir, expected.Workspace = filepath.Join(operation, "state"), filepath.Join(operation, "workspace")
	expected.Staged, expected.ExpectedReportSHA256 = filepath.Join(operation, "recovered"), rec.Request.ReportSHA256
	expectedSHA, _ := maintenance.ConfigHash(expected)
	generatedSHA, _ := maintenance.ConfigHash(generated)
	if generatedSHA != expectedSHA {
		return Status{}, ErrUnavailable
	}
	state, err := maintenance.ReadState(generated)
	if err != nil || state.NeedsRecovery || state.PendingAction != "" {
		return Status{}, ErrUnavailable
	}
	if state.Phase == "healthy" && state.Committed {
		rec.Status.State, rec.Status.ErrorCode = "succeeded", ""
	} else if state.OriginalServicesRestored && (state.RollbackCompleted || state.OriginalDataUnchanged) {
		rec.Status.State, rec.Status.ErrorCode = "failed", "maintenance_failed"
	} else {
		return Status{}, ErrUnavailable
	}
	rec.Status.Phase, rec.Status.UpdatedAt = "completed", time.Now().UTC()
	s := service{cfg: cfg}
	if s.persist(rec) != nil {
		return Status{}, ErrUnavailable
	}
	return rec.Status, nil
}
