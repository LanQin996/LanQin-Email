package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"filippo.io/age"
)

func TestBackupRequiresAdministratorPasswordDespiteEnvironmentOrLegacyKey(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "environment-only"
		if legacy {
			name = "legacy-key-record"
		}
		t.Run(name, func(t *testing.T) {
			a, id, setTime := scheduleFixture(t)
			client := backupLoggedInAdmin(t, a)
			setDailySchedule(t, a, id, true)
			plans, _, err := a.loadBackupSchedules(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, destinationRaw, err := a.loadBackupDestinationState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			revision := backupRevision(destinationRaw)
			queued, err := a.enqueueBackupSnapshot(t.Context(), "synthetic-admin", "password-required-queued", []string{id}, revision, backupTestRecipient(t, a))
			if err != nil {
				t.Fatal(err)
			}
			current := backupTestEncryption(t, a)
			var old backupEncryptionRecord
			raw, err := a.backupRead(t.Context(), backupEncryptionSetting, &old)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				if err := a.backupCAS(t.Context(), a.db, backupEncryptionSetting, raw, backupEncryptionRecord{Version: 1, Mode: "key", Generation: newID("bep")}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := a.db.Exec(`DELETE FROM system_settings WHERE key=?`, backupEncryptionSetting); err != nil {
				t.Fatal(err)
			}
			// The valid environment public key must not become an implicit
			// password substitute or silently revive an old key configuration.
			if _, err := age.ParseX25519Recipient(a.cfg.BackupRecipient); err != nil {
				t.Fatal("fixture did not retain a valid environment public key")
			}
			var view backupEncryptionView
			if code := client.do("GET", "/api/admin/backups/encryption", nil, &view); code != http.StatusOK || view.Mode != "password" || view.Configured || view.PasswordSet || view.Recipient != "" || view.EnvironmentKeyConfigured {
				t.Fatal("missing administrator password advertised configured encryption", code, view)
			}
			if legacy && (view.Revision == "0" || view.Revision == current.Revision) || !legacy && view.Revision != "0" {
				t.Fatal("legacy configuration revision was lost")
			}
			var capabilities map[string]any
			if code := client.do("GET", "/api/admin/backups/capabilities", nil, &capabilities); code != http.StatusOK || capabilities["onlineBackupAvailable"] != false || capabilities["recipient"] != nil {
				t.Fatal("environment public key enabled new captures", code)
			}
			var response map[string]any
			payload := map[string]any{"destinationIds": []string{id}, "requestId": "password-required-rejected", "destinationsRevision": revision, "expectedRecipient": a.cfg.BackupRecipient}
			if code := client.do("POST", "/api/admin/backups/snapshots", payload, &response); code != http.StatusServiceUnavailable {
				t.Fatal("environment public key admitted immediate backup", code)
			}
			_, scheduleRaw, err := a.loadBackupSchedules(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := a.saveBackupSchedule(t.Context(), id, backupScheduleRequest{Revision: backupRevision(scheduleRaw), DestinationsRevision: revision, ExpectedRecipient: a.cfg.BackupRecipient, Enabled: true, Time: "09:00", Timezone: "UTC"}); !errors.Is(err, errBackupExecution) {
				t.Fatal("missing password enabled a new automatic schedule", err)
			}
			setTime(*plans.Items[0].NextRunAt)
			if err := a.runDueBackupSchedules(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _, err := a.loadBackupSchedules(t.Context())
			if err != nil || state.Items[0].Enabled || state.Items[0].LastErrorCode == "" {
				t.Fatal("existing schedule ran without administrator password", err)
			}
			lease, err := a.claimBackupUpload(t.Context())
			if err != nil || lease == nil || lease.ID != queued.ID {
				t.Fatal("queued capture fixture not claimed", err)
			}
			a.backupPrepareSnapshot = func(context.Context, Config, string, OnlineBackupOptions) (*PreparedOnlineBackup, error) {
				t.Error("missing password started collection")
				return nil, errors.New("unexpected collection")
			}
			a.runBackupUpload(t.Context(), *lease)
			finished, _, err := a.backupReadJob(t.Context(), queued.ID)
			if err != nil || finished.Status != "failed" || finished.ErrorCode != "capture_configuration_changed" || finished.Artifact.ID != "" {
				t.Fatal("queued capture silently fell back to environment public key", err)
			}
			// Historical key records can be replaced using the preserved CAS
			// revision without a nonexistent old backup password.
			const password = "synthetic administrator recovery password"
			if err := a.saveBackupEncryption(t.Context(), view.Revision, "password", password, password, ""); err != nil {
				t.Fatal("administrator could not configure mandatory password", err)
			}
			if recipient, reason := a.backupCaptureCapability(); reason != "" || !passwordDescriptor(recipient) {
				t.Fatal("administrator password did not enable captures", reason)
			}
		})
	}
}
