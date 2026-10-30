package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func scheduleFixture(t *testing.T) (*App, string, func(time.Time)) {
	t.Helper()
	a := newTestApp(t)
	_, ids, _ := backupSnapshotJobFixture(t, a)
	var clock atomic.Pointer[time.Time]
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	clock.Store(&now)
	a.now = func() time.Time { return *clock.Load() }
	return a, ids[0], func(value time.Time) { clock.Store(&value) }
}

func setDailySchedule(t *testing.T, a *App, id string, enabled bool) {
	t.Helper()
	_, raw, err := a.loadBackupSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, vaultRaw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBackupSchedule(t.Context(), id, backupScheduleRequest{Revision: backupRevision(raw), DestinationsRevision: backupRevision(vaultRaw), ExpectedRecipient: a.cfg.BackupRecipient, Enabled: enabled, Time: "09:00", Timezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupDailyScheduleAtomicAdmissionAndMultiInstanceDedup(t *testing.T) {
	a, id, setTime := scheduleFixture(t)
	setDailySchedule(t, a, id, true)
	state, _, err := a.loadBackupSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	due := *state.Items[0].NextRunAt
	if !due.Equal(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)) {
		t.Fatal("wrong zoned time", due)
	}
	if err := a.runDueBackupSchedules(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if len(registry.Pending) != 0 {
		t.Fatal("schedule triggered early")
	}
	setTime(due.Add(time.Minute))
	other := &App{cfg: a.cfg, db: a.db, now: a.now}
	var wg sync.WaitGroup
	for _, instance := range []*App{a, other, a, other} {
		wg.Add(1)
		go func(v *App) {
			defer wg.Done()
			if err := v.runDueBackupSchedules(t.Context()); err != nil {
				t.Error(err)
			}
		}(instance)
	}
	wg.Wait()
	registry, _, err = a.backupRegistry(t.Context())
	if err != nil || len(registry.Pending) != 1 {
		t.Fatal("duplicate/missing scheduled work", err, len(registry.Pending))
	}
	state, raw, err := a.loadBackupSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p := state.Items[0]
	if p.LastJobID != registry.Pending[0] || p.NextRunAt == nil || !p.NextRunAt.After(a.now()) {
		t.Fatal("cursor and job not committed together")
	}
	if strings.Contains(raw, a.cfg.BackupRecipient) || strings.Contains(raw, "Asia/Shanghai") {
		t.Fatal("schedule state not encrypted")
	}
	job, _, err := a.backupReadJob(t.Context(), p.LastJobID)
	if err != nil || job.Kind != "snapshot" || len(job.Targets) != 1 || job.Targets[0].ID != id {
		t.Fatal("wrong scheduled target", err)
	}
	// Simulate restart after several missed days: at most one current catchup.
	setTime(due.Add(5*24*time.Hour + time.Minute))
	if err := other.runDueBackupSchedules(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry, _, _ = a.backupRegistry(t.Context())
	if len(registry.Pending) != 2 {
		t.Fatal("missed runs replayed as a storm", len(registry.Pending))
	}
}

func TestBackupDailyScheduleQueueFullDoesNotConsumeOccurrence(t *testing.T) {
	a, id, setTime := scheduleFixture(t)
	setDailySchedule(t, a, id, true)
	state, _, _ := a.loadBackupSchedules(t.Context())
	due := *state.Items[0].NextRunAt
	setTime(due)
	_, raw, _ := a.loadBackupDestinationState(t.Context())
	for n := range 16 {
		if _, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-filler", fmt.Sprintf("filler-request-%04d", n), []string{id}, backupRevision(raw), a.cfg.BackupRecipient); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.runDueBackupSchedules(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, _, _ = a.loadBackupSchedules(t.Context())
	if state.Items[0].LastJobID != "" || !state.Items[0].NextRunAt.Equal(due) {
		t.Fatal("full queue consumed due time")
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if err := a.cancelBackupUpload(t.Context(), registry.Pending[0]); err != nil {
		t.Fatal(err)
	}
	if err := a.runDueBackupSchedules(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, _, _ = a.loadBackupSchedules(t.Context())
	if state.Items[0].LastJobID == "" {
		t.Fatal("due work lost after capacity released")
	}
}

func TestBackupDailyScheduleDisableChangedConfigurationAndCAS(t *testing.T) {
	for _, scenario := range []string{"disabled", "deleted", "credentials", "recipient"} {
		t.Run(scenario, func(t *testing.T) {
			a, id, setTime := scheduleFixture(t)
			setDailySchedule(t, a, id, true)
			state, old, err := a.loadBackupSchedules(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			setTime(*state.Items[0].NextRunAt)
			if scenario == "recipient" {
				a.cfg.BackupRecipient = "misconfigured"
			} else {
				vault, raw, err := a.loadBackupDestinationState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "disabled" {
					vault.Items[0].Config.Disabled = true
				}
				if scenario == "deleted" {
					vault.Items = vault.Items[1:]
				}
				if scenario == "credentials" {
					vault.Items[0].Credentials.SecretAccessKey = "rotated-synthetic"
				}
				if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.runDueBackupSchedules(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _, _ = a.loadBackupSchedules(t.Context())
			if state.Items[0].Enabled || state.Items[0].LastErrorCode == "" {
				t.Fatal("changed config silently executed")
			}
			registry, _, _ := a.backupRegistry(t.Context())
			if len(registry.Pending) != 0 {
				t.Fatal("invalid scheduled work enqueued")
			}
			_, vaultRaw, _ := a.loadBackupDestinationState(t.Context())
			err = a.saveBackupSchedule(t.Context(), id, backupScheduleRequest{Revision: backupRevision(old), DestinationsRevision: backupRevision(vaultRaw), Time: "10:00", Timezone: "UTC"})
			if !errors.Is(err, errBackupConflict) {
				t.Fatal("stale schedule write accepted", err)
			}
		})
	}
}

func TestBackupDailyScheduleTimezoneDSTAndValidation(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	fall, ok := backupScheduleOccurrence(time.Date(2026, 11, 1, 12, 0, 0, 0, loc), 90, loc)
	if !ok || !fall.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Fatal("did not choose earliest repeated time", fall)
	}
	next, err := nextBackupSchedule(fall.Add(time.Minute), "01:30", "America/New_York")
	if err != nil || next.In(loc).Day() != 2 {
		t.Fatal("repeated hour triggered twice", next, err)
	}
	spring, ok := backupScheduleOccurrence(time.Date(2026, 3, 8, 12, 0, 0, 0, loc), 150, loc)
	if !ok || spring.In(loc).Hour() != 3 || spring.In(loc).Minute() != 0 {
		t.Fatal("missing wall time policy wrong", spring)
	}
	for _, clock := range []string{"9:00", "24:00", "01:60", "01:xx", "00:00\n"} {
		if _, _, err := parseBackupSchedule(clock, "UTC"); err == nil {
			t.Fatal("invalid time accepted")
		}
	}
	for _, zone := range []string{"Local", "CST", "not/a-zone", "../../zone"} {
		if _, _, err := parseBackupSchedule("09:00", zone); err == nil {
			t.Fatal("invalid zone accepted")
		}
	}
}

func TestBackupDailyScheduleGuardRollsBackWholeAdmission(t *testing.T) {
	a, id, _ := scheduleFixture(t)
	_, vaultRaw, _ := a.loadBackupDestinationState(t.Context())
	_, err := a.enqueueBackupJobGuarded(t.Context(), "backup-scheduler", "synthetic-guard-reject-001", "", []string{id}, "snapshot", a.cfg.BackupRecipient, []string{backupRevision(vaultRaw)}, func(dbExecutor) error { return errBackupConflict })
	if !errors.Is(err, errBackupConflict) {
		t.Fatal(err)
	}
	registry, _, _ := a.backupRegistry(t.Context())
	if len(registry.Pending) != 0 {
		t.Fatal("failed schedule CAS left queued job")
	}
	var count int
	if err := a.db.QueryRowContext(context.Background(), `SELECT count(*) FROM system_settings WHERE key LIKE 'backup.job.v1.%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed schedule CAS left job row", err)
	}
}

func TestBackupDailyScheduleHTTPPublicViewAndStrictBody(t *testing.T) {
	a, id, _ := scheduleFixture(t)
	client := backupLoggedInAdmin(t, a)
	_, vaultRaw, _ := a.loadBackupDestinationState(t.Context())
	var response map[string]any
	payload := map[string]any{"revision": "0", "destinationsRevision": backupRevision(vaultRaw), "expectedRecipient": a.cfg.BackupRecipient, "enabled": true, "time": "09:00", "timezone": "Asia/Shanghai"}
	if code := client.do("POST", "/api/admin/backups/destinations/"+id+"/schedule", payload, &response); code != http.StatusOK {
		t.Fatalf("save schedule status=%d", code)
	}
	if code := client.do("GET", "/api/admin/backups/schedules", nil, &response); code != http.StatusOK {
		t.Fatal(code)
	}
	view := jsonEncode(response)
	for _, secret := range []string{"generation", "recipient", "fingerprint", a.cfg.BackupSecretKey, a.cfg.BackupWorkspace} {
		if strings.Contains(view, secret) {
			t.Fatal("private schedule state leaked")
		}
	}
	payload["path"] = "/arbitrary/source"
	if code := client.do("POST", "/api/admin/backups/destinations/"+id+"/schedule", payload, &response); code != http.StatusBadRequest {
		t.Fatal("unknown path accepted", code)
	}
}
