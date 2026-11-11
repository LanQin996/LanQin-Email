package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackupSchedulePeriodsCalendarAndIntervalBoundaries(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                string
		plan                backupSchedule
		after, next, latest time.Time
	}{
		{"hourly", backupSchedule{Period: "hourly", Time: "00:00", Timezone: "UTC", Minute: 15}, time.Date(2026, 1, 1, 2, 16, 0, 0, time.UTC), time.Date(2026, 1, 1, 3, 15, 0, 0, time.UTC), time.Date(2026, 1, 1, 2, 15, 0, 0, time.UTC)},
		{"weekly", backupSchedule{Period: "weekly", Time: "09:30", Timezone: "UTC", Weekdays: []int{1, 5}}, time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC), time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC), time.Date(2026, 1, 2, 9, 30, 0, 0, time.UTC)},
		{"month-end", backupSchedule{Period: "monthly", Time: "08:00", Timezone: "UTC", DayOfMonth: 31}, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 8, 0, 0, 0, time.UTC), time.Date(2026, 1, 31, 8, 0, 0, 0, time.UTC)},
		{"leap-month-end", backupSchedule{Period: "monthly", Time: "08:00", Timezone: "UTC", DayOfMonth: 31}, time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 8, 0, 0, 0, time.UTC), time.Date(2028, 1, 31, 8, 0, 0, 0, time.UTC)},
		{"interval", backupSchedule{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 5, Unit: "minutes", AnchorAt: &anchor}, anchor.Add(17 * time.Minute), anchor.Add(20 * time.Minute), anchor.Add(15 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, err := nextBackupSchedulePeriod(test.after, test.plan)
			if err != nil || !next.Equal(test.next) {
				t.Fatal("wrong next", next, err)
			}
			latest, err := latestBackupSchedulePeriod(test.after, test.plan)
			if err != nil || !latest.Equal(test.latest) {
				t.Fatal("wrong catchup", latest, err)
			}
		})
	}
}

func TestBackupSchedulePeriodsDSTAndFixedInterval(t *testing.T) {
	hourly := backupSchedule{Period: "hourly", Time: "00:00", Timezone: "America/New_York", Minute: 30}
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	next, err := nextBackupSchedulePeriod(first, hourly)
	if err != nil || !next.Equal(time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)) {
		t.Fatal("repeated local hour replayed", next, err)
	}
	weekly := backupSchedule{Period: "weekly", Time: "02:30", Timezone: "America/New_York", Weekdays: []int{0}}
	next, err = nextBackupSchedulePeriod(time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), weekly)
	if err != nil || !next.Equal(time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)) {
		t.Fatal("missing calendar time did not advance", next, err)
	}
	anchor := time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC)
	interval := backupSchedule{Period: "interval", Time: "00:00", Timezone: "America/New_York", Interval: 1, Unit: "days", AnchorAt: &anchor}
	next, err = nextBackupSchedulePeriod(anchor, interval)
	if err != nil || next.Sub(anchor) != 24*time.Hour {
		t.Fatal("fixed duration changed across DST", next, err)
	}
}

func TestBackupScheduleMonthlySkipsACompletelyMissingLocalDate(t *testing.T) {
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	p := backupSchedule{Period: "monthly", Time: "09:00", Timezone: "Pacific/Apia", DayOfMonth: 30}
	before := time.Date(2011, 12, 29, 12, 0, 0, 0, loc)
	next, err := nextBackupSchedulePeriod(before, p)
	if err != nil || !next.Equal(time.Date(2012, 1, 30, 9, 0, 0, 0, loc)) {
		t.Fatal("skipped monthly date was normalized into a different execution day", next, err)
	}
	latest, err := latestBackupSchedulePeriod(time.Date(2012, 1, 29, 12, 0, 0, 0, loc), p)
	if err != nil || !latest.Equal(time.Date(2011, 11, 30, 9, 0, 0, 0, loc)) {
		t.Fatal("previous monthly occurrence lost across an entirely skipped date", latest, err)
	}
}

func TestBackupSchedulePeriodsRejectAmbiguousAndTooFrequentRequests(t *testing.T) {
	anchor := time.Now().UTC()
	for _, p := range []backupSchedule{
		{Period: "seconds", Time: "09:00", Timezone: "UTC"},
		{Period: "daily", Time: "09:00", Timezone: "UTC", Minute: 1},
		{Period: "hourly", Time: "01:00", Timezone: "UTC", Minute: 15},
		{Period: "hourly", Time: "00:00", Timezone: "UTC", Minute: 60},
		{Period: "weekly", Time: "09:00", Timezone: "UTC"},
		{Period: "weekly", Time: "09:00", Timezone: "UTC", Weekdays: []int{1, 1}},
		{Period: "weekly", Time: "09:00", Timezone: "UTC", Weekdays: []int{7}},
		{Period: "monthly", Time: "09:00", Timezone: "UTC", DayOfMonth: 32},
		{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 4, Unit: "minutes", AnchorAt: &anchor},
		{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 1, Unit: "seconds", AnchorAt: &anchor},
		{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 8761, Unit: "hours", AnchorAt: &anchor},
		{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 367, Unit: "days", AnchorAt: &anchor},
		{Period: "interval", Time: "00:00", Timezone: "UTC", Interval: 5, Unit: "minutes"},
	} {
		if validateBackupSchedulePeriod(p) == nil {
			t.Fatal("unsafe schedule accepted", p.Period)
		}
	}
}

func savePeriodSchedule(t *testing.T, a *App, id string, req backupScheduleRequest) {
	t.Helper()
	_, raw, err := a.loadBackupSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, vault, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	req.Revision, req.DestinationsRevision, req.ExpectedRecipient, req.Enabled = backupRevision(raw), backupRevision(vault), backupTestRecipient(t, a), true
	if err := a.saveBackupSchedule(t.Context(), id, req); err != nil {
		t.Fatal(err)
	}
}

func TestBackupScheduleHourlyAndIntervalActualAdmissionDoesNotCollapseSameDay(t *testing.T) {
	for _, period := range []string{"hourly", "interval", "weekly", "monthly"} {
		t.Run(period, func(t *testing.T) {
			a, id, setTime := scheduleFixture(t)
			req := backupScheduleRequest{Period: period, Timezone: "UTC", Time: "09:00"}
			switch period {
			case "hourly":
				req.Time = ""
				req.Minute = 15
			case "interval":
				req.Time = ""
				req.Interval = 5
				req.Unit = "minutes"
			case "weekly":
				req.Weekdays = []int{1, 5}
			case "monthly":
				req.DayOfMonth = 31
			}
			savePeriodSchedule(t, a, id, req)
			state, _, err := a.loadBackupSchedules(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			first := *state.Items[0].NextRunAt
			setTime(first)
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
			registry, _, err := a.backupRegistry(t.Context())
			if err != nil || len(registry.Pending) != 1 {
				t.Fatal("first period duplicated", err, len(registry.Pending))
			}
			state, _, _ = a.loadBackupSchedules(t.Context())
			second := *state.Items[0].NextRunAt
			setTime(second)
			if err := a.runDueBackupSchedules(t.Context()); err != nil {
				t.Fatal(err)
			}
			registry, _, _ = a.backupRegistry(t.Context())
			if len(registry.Pending) != 2 || registry.Pending[0] == registry.Pending[1] {
				t.Fatal("distinct period collapsed into same daily ID")
			}
			// A long outage admits only the latest due occurrence, not every miss.
			setTime(second.Add(100 * 24 * time.Hour))
			if err := a.runDueBackupSchedules(t.Context()); err != nil {
				t.Fatal(err)
			}
			registry, _, _ = a.backupRegistry(t.Context())
			if len(registry.Pending) != 3 {
				t.Fatal("missed periods replay storm", len(registry.Pending))
			}
		})
	}
}

func TestBackupSchedulePeriodsHTTPAndPrivateAnchor(t *testing.T) {
	a, id, _ := scheduleFixture(t)
	client := backupLoggedInAdmin(t, a)
	_, vault, _ := a.loadBackupDestinationState(t.Context())
	payload := map[string]any{"revision": "0", "destinationsRevision": backupRevision(vault), "expectedRecipient": backupTestRecipient(t, a), "enabled": true, "period": "interval", "interval": 5, "unit": "minutes", "timezone": "UTC"}
	var response map[string]any
	if code := client.do("POST", "/api/admin/backups/destinations/"+id+"/schedule", payload, &response); code != 200 {
		t.Fatal(code)
	}
	if code := client.do("GET", "/api/admin/backups/schedules", nil, &response); code != 200 {
		t.Fatal(code)
	}
	encoded, _ := json.Marshal(response)
	if !strings.Contains(string(encoded), `"period":"interval"`) || strings.Contains(string(encoded), "anchorAt") || strings.Contains(string(encoded), "generation") {
		t.Fatal("period missing or private binding leaked")
	}
	state, _, _ := a.loadBackupSchedules(t.Context())
	if state.Items[0].AnchorAt == nil || state.Items[0].Time != "00:00" {
		t.Fatal("server anchor not persisted")
	}
	payload["anchorAt"] = "2020-01-01T00:00:00Z"
	if code := client.do("POST", "/api/admin/backups/destinations/"+id+"/schedule", payload, &response); code != http.StatusBadRequest {
		t.Fatal("client-selected anchor accepted", code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.runDueBackupSchedules(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel not observed", err)
	}
}
