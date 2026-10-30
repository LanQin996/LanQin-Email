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
}

func TestBackupScheduleSQLiteDatabaseContract(t *testing.T) {
	assertBackupScheduleDatabaseContract(t, newTestApp(t))
}
