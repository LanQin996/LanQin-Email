package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Portable IANA zones, including minimal container images.
)

const backupSchedulesKey = "backup.schedules.v1"

type backupSchedule struct {
	DestinationID string     `json:"destinationId"`
	Enabled       bool       `json:"enabled"`
	Time          string     `json:"time"`
	Timezone      string     `json:"timezone"`
	NextRunAt     *time.Time `json:"nextRunAt,omitempty"`
	LastJobID     string     `json:"lastJobId,omitempty"`
	LastErrorCode string     `json:"lastErrorCode,omitempty"`
	Generation    string     `json:"generation"`
	Recipient     string     `json:"recipient"`
	Fingerprint   string     `json:"fingerprint"`
}

type backupSchedules struct {
	Version int              `json:"version"`
	Items   []backupSchedule `json:"items"`
}

type backupScheduleView struct {
	DestinationID string     `json:"destinationId"`
	Enabled       bool       `json:"enabled"`
	Time          string     `json:"time"`
	Timezone      string     `json:"timezone"`
	NextRunAt     *time.Time `json:"nextRunAt,omitempty"`
	LastJobID     string     `json:"lastJobId,omitempty"`
	LastErrorCode string     `json:"lastErrorCode,omitempty"`
}

func parseBackupSchedule(clock, zone string) (int, *time.Location, error) {
	if len(clock) != 5 || clock[2] != ':' || clock[0] < '0' || clock[0] > '9' || clock[1] < '0' || clock[1] > '9' || clock[3] < '0' || clock[3] > '9' || clock[4] < '0' || clock[4] > '9' || zone == "Local" || len(zone) > 128 || zone != "UTC" && !strings.Contains(zone, "/") {
		return 0, nil, errBackupInvalid
	}
	hour, e1 := strconv.Atoi(clock[:2])
	minute, e2 := strconv.Atoi(clock[3:])
	loc, e3 := time.LoadLocation(zone)
	if e1 != nil || e2 != nil || e3 != nil || hour > 23 || minute > 59 {
		return 0, nil, errBackupInvalid
	}
	return hour*60 + minute, loc, nil
}

// One canonical occurrence per local calendar day. During a repeated hour choose
// the earliest real instant; during a missing time choose the first real minute
// at/after the requested wall time. A completely skipped local date has no run.
func backupScheduleOccurrence(day time.Time, wall int, loc *time.Location) (time.Time, bool) {
	y, m, d := day.In(loc).Date()
	noon := time.Date(y, m, d, 12, 0, 0, 0, loc)
	start := noon.Add(-18 * time.Hour)
	for n := 0; n < 36*60; n++ {
		candidate := start.Add(time.Duration(n) * time.Minute).Truncate(time.Minute)
		local := candidate.In(loc)
		cy, cm, cd := local.Date()
		if cy == y && cm == m && cd == d && local.Hour()*60+local.Minute() >= wall {
			return candidate.UTC(), true
		}
	}
	return time.Time{}, false
}

func nextBackupSchedule(after time.Time, clock, zone string) (time.Time, error) {
	wall, loc, err := parseBackupSchedule(clock, zone)
	if err != nil {
		return time.Time{}, err
	}
	day := after.In(loc)
	for n := 0; n < 370; n++ {
		if candidate, ok := backupScheduleOccurrence(day, wall, loc); ok && candidate.After(after) {
			return candidate, nil
		}
		// Noon avoids a midnight transition changing the intended calendar day.
		y, m, d := day.Date()
		day = time.Date(y, m, d+1, 12, 0, 0, 0, loc)
	}
	return time.Time{}, errBackupInvalid
}

func latestBackupSchedule(now time.Time, clock, zone string) (time.Time, error) {
	wall, loc, err := parseBackupSchedule(clock, zone)
	if err != nil {
		return time.Time{}, err
	}
	day := now.In(loc)
	for n := 0; n < 4; n++ {
		if candidate, ok := backupScheduleOccurrence(day, wall, loc); ok && !candidate.After(now) {
			return candidate, nil
		}
		y, m, d := day.Date()
		day = time.Date(y, m, d-1, 12, 0, 0, 0, loc)
	}
	return time.Time{}, errBackupInvalid
}

func (a *App) loadBackupSchedules(ctx context.Context) (backupSchedules, string, error) {
	state := backupSchedules{Version: 1, Items: []backupSchedule{}}
	raw, err := a.backupRead(ctx, backupSchedulesKey, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return state, "", nil
	}
	if err != nil {
		return state, "", err
	}
	if state.Version != 1 || len(state.Items) > 64 {
		return state, "", errBackupInvalid
	}
	seen := map[string]bool{}
	for _, p := range state.Items {
		if p.DestinationID == "" || seen[p.DestinationID] || p.Generation == "" {
			return state, "", errBackupInvalid
		}
		if _, _, err := parseBackupSchedule(p.Time, p.Timezone); err != nil {
			return state, "", err
		}
		if p.Enabled && (p.NextRunAt == nil || p.Recipient == "" || p.Fingerprint == "") {
			return state, "", errBackupInvalid
		}
		seen[p.DestinationID] = true
	}
	return state, raw, nil
}

type backupScheduleRequest struct {
	Revision             string `json:"revision"`
	DestinationsRevision string `json:"destinationsRevision"`
	ExpectedRecipient    string `json:"expectedRecipient"`
	Enabled              bool   `json:"enabled"`
	Time                 string `json:"time"`
	Timezone             string `json:"timezone"`
}

func (a *App) saveBackupSchedule(ctx context.Context, id string, req backupScheduleRequest) error {
	if _, _, err := parseBackupSchedule(req.Time, req.Timezone); err != nil {
		return err
	}
	state, raw, err := a.loadBackupSchedules(ctx)
	if err != nil {
		return err
	}
	if req.Revision != backupRevision(raw) {
		return errBackupConflict
	}
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return err
	}
	if req.DestinationsRevision != backupRevision(vaultRaw) {
		return errBackupConflict
	}
	// Deleting a destination must not permanently consume one of the 64 plan
	// slots. Prune only entries whose destination no longer exists, as part of
	// this administrator-authorized CAS write; history jobs remain untouched.
	exists := map[string]bool{}
	for _, record := range vault.Items {
		exists[record.Config.ID] = true
	}
	kept := make([]backupSchedule, 0, len(state.Items))
	for _, plan := range state.Items {
		if exists[plan.DestinationID] {
			kept = append(kept, plan)
		}
	}
	state.Items = kept
	var target *backupDestinationRecord
	for i := range vault.Items {
		if vault.Items[i].Config.ID == id {
			target = &vault.Items[i]
			break
		}
	}
	if target == nil {
		return errBackupMissing
	}
	index := -1
	for i, p := range state.Items {
		if p.DestinationID == id {
			index = i
			break
		}
	}
	plan := backupSchedule{DestinationID: id, Enabled: req.Enabled, Time: req.Time, Timezone: req.Timezone, Generation: newID("bsg")}
	if index >= 0 {
		plan.LastJobID = state.Items[index].LastJobID
	}
	if req.Enabled {
		if target.Config.Disabled {
			return errBackupInvalid
		}
		if target.Config.Type == "drive" && !backupDriveBound(*target) {
			return errBackupAuth
		}
		recipient, err := a.backupCaptureConfiguration()
		if err != nil {
			return err
		}
		if req.ExpectedRecipient != recipient {
			return errBackupConflict
		}
		next, err := nextBackupSchedule(a.now(), req.Time, req.Timezone)
		if err != nil {
			return err
		}
		plan.Recipient, plan.Fingerprint, plan.NextRunAt = recipient, backupTargetFingerprint(*target), &next
	}
	if index < 0 {
		state.Items = append(state.Items, plan)
	} else {
		state.Items[index] = plan
	}
	if len(state.Items) > 64 {
		return errBackupLimit
	}
	return a.backupCAS(ctx, a.db, backupSchedulesKey, raw, state)
}

// runDueBackupSchedules only creates bounded queue work. It performs no snapshot
// collection or network transfer and is safe for concurrent process ticks.
func (a *App) runDueBackupSchedules(ctx context.Context) error {
	state, _, err := a.loadBackupSchedules(ctx)
	if err != nil {
		return err
	}
	for _, p := range state.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !p.Enabled || p.NextRunAt == nil || p.NextRunAt.After(a.now()) {
			continue
		}
		if err := a.enqueueDueBackupSchedule(ctx, p.DestinationID); err != nil && !errors.Is(err, errBackupConflict) && !errors.Is(err, errBackupQueue) {
			return err
		}
	}
	return nil
}

func (a *App) enqueueDueBackupSchedule(ctx context.Context, id string) error {
	state, raw, err := a.loadBackupSchedules(ctx)
	if err != nil {
		return err
	}
	index := -1
	for i, p := range state.Items {
		if p.DestinationID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	plan := state.Items[index]
	now := a.now().UTC()
	if !plan.Enabled || plan.NextRunAt == nil || plan.NextRunAt.After(now) {
		return nil
	}
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return err
	}
	var target *backupDestinationRecord
	for i := range vault.Items {
		if vault.Items[i].Config.ID == id {
			target = &vault.Items[i]
			break
		}
	}
	code := ""
	if target == nil || target.Config.Disabled || backupTargetFingerprint(*target) != plan.Fingerprint {
		code = "destination_changed"
	}
	recipient, capErr := a.backupCaptureConfiguration()
	if code == "" && capErr != nil {
		code = "configuration_unavailable"
	}
	if code == "" && recipient != plan.Recipient {
		code = "capture_configuration_changed"
	}
	if code != "" {
		state.Items[index].Enabled = false
		state.Items[index].NextRunAt = nil
		state.Items[index].LastErrorCode = code
		return a.backupCAS(ctx, a.db, backupSchedulesKey, raw, state)
	}
	occurrence, err := latestBackupSchedule(now, plan.Time, plan.Timezone)
	if err != nil {
		return err
	}
	_, loc, _ := parseBackupSchedule(plan.Time, plan.Timezone)
	digest := sha256.Sum256([]byte(id + "\x00" + plan.Generation + "\x00" + occurrence.In(loc).Format("2006-01-02")))
	requestID := "schedule_" + hex.EncodeToString(digest[:])
	identity := sha256.Sum256([]byte("backup-scheduler\x00" + requestID))
	jobID := "bkj_" + hex.EncodeToString(identity[:16])
	next, err := nextBackupSchedule(now, plan.Time, plan.Timezone)
	if err != nil {
		return err
	}
	state.Items[index].NextRunAt = &next
	state.Items[index].LastJobID = jobID
	state.Items[index].LastErrorCode = ""
	_, err = a.enqueueBackupJobGuarded(ctx, "backup-scheduler", requestID, "", []string{id}, "snapshot", plan.Recipient, []string{backupRevision(vaultRaw)}, func(db dbExecutor) error { return a.backupCAS(ctx, db, backupSchedulesKey, raw, state) })
	return err
}
