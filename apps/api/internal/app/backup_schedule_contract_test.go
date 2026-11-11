package app

import (
	"errors"
	"testing"
)

func assertBackupScheduleDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	_, records, _ := backupJobFixture(t, a)
	_, scheduleRaw, err := a.loadBackupSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, vaultRaw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	req := backupScheduleRequest{Revision: backupRevision(scheduleRaw), DestinationsRevision: backupRevision(vaultRaw), Enabled: false, Time: "04:30", Timezone: "UTC"}
	if err := a.saveBackupSchedule(t.Context(), records[0].Config.ID, req); err != nil {
		t.Fatal(err)
	}
	state, raw, err := a.loadBackupSchedules(t.Context())
	if err != nil || len(state.Items) != 1 || state.Items[0].Time != "04:30" || state.Items[0].Enabled || state.Items[0].NextRunAt != nil {
		t.Fatal("schedule contract not persisted", err)
	}
	if raw == "" || raw == scheduleRaw {
		t.Fatal("schedule CAS did not update ciphertext")
	}
	if err := a.saveBackupSchedule(t.Context(), records[0].Config.ID, req); !errors.Is(err, errBackupConflict) {
		t.Fatal("stale schedule update accepted", err)
	}
	// Extended periods use the same encrypted CAS setting on every dialect.
	for _, period := range []string{"hourly", "weekly", "monthly", "interval"} {
		_, currentRaw, err := a.loadBackupSchedules(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		updated := backupScheduleRequest{Revision: backupRevision(currentRaw), DestinationsRevision: backupRevision(vaultRaw),
			Period: period, Time: "04:30", Timezone: "UTC"}
		switch period {
		case "hourly":
			updated.Time = ""
			updated.Minute = 35
		case "weekly":
			updated.Weekdays = []int{1, 4}
		case "monthly":
			updated.DayOfMonth = 31
		case "interval":
			updated.Time = ""
			updated.Interval = 5
			updated.Unit = "minutes"
		}
		if err := a.saveBackupSchedule(t.Context(), records[0].Config.ID, updated); err != nil {
			t.Fatal("period contract save", period, err)
		}
		stored, _, err := a.loadBackupSchedules(t.Context())
		if err != nil || len(stored.Items) != 1 || backupSchedulePeriod(stored.Items[0]) != period {
			t.Fatal("period contract read", period, err)
		}
		if period == "interval" && stored.Items[0].AnchorAt == nil {
			t.Fatal("interval contract lost server anchor")
		}
	}
}

func TestBackupScheduleSQLiteDatabaseContract(t *testing.T) {
	assertBackupScheduleDatabaseContract(t, newTestApp(t))
}
