package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/maintenance"
)

func TestHostMaintenanceProgressReportsOnlyFiniteJournalStages(t *testing.T) {
	for _, tc := range []struct{ phase, pending, want string }{
		{"preflight", "preflight", "preflight"},
		{"stopping", "stop", "stopping"},
		{"stopped", "", "stopping"},
		{"installing", "install", "installing"},
		{"installed", "validate", "validating"},
		{"installed", "validate-committed", "validating"},
		{"validated", "", "committing"},
		{"committed", "", "committing"},
		{"starting", "recreate", "starting"},
		{"starting", "health", "health-check"},
		{"healthy", "", "health-check"},
		{"failed", "rollback", "rolling-back"},
		{"failed", "rollback-environment", "rolling-back"},
		{"failed", "start-original", "recovering-original"},
		{"failed", "recreate-original", "recovering-original"},
		{"failed", "health-original", "recovering-original"},
		{"failed", "", ""},
		{"/private/synthetic-secret", "untrusted-command", ""},
	} {
		state := maintenance.State{Phase: tc.phase, PendingAction: tc.pending, LastErrorCode: "synthetic-secret"}
		if got := hostMaintenanceStep(state); got != tc.want {
			t.Fatalf("phase=%q pending=%q got=%q want=%q", tc.phase, tc.pending, got, tc.want)
		}
	}
}

func TestMaintenanceHostCommandRejectsUnconfirmedOrUnboundedAuthority(t *testing.T) {
	for _, args := range [][]string{
		{"maintenance-host"},
		{"maintenance-host", "--config=nonexistent"},
		{"maintenance-host", "--config=nonexistent", "--confirm-maintenance-service", "--command=synthetic-private-command"},
		{"maintenance-host", "--config=nonexistent", "--confirm-maintenance-service", "--request=" + strings.Repeat("a", 32)},
		{"maintenance-host-resolve", "--config=nonexistent", "--confirm-maintenance-service"},
		{"maintenance-host-resolve", "--config=nonexistent", "--confirm-maintenance-service", "--request=../../synthetic-private-command"},
	} {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), args, &out, &diagnostic); err == nil || out.Len() != 0 || diagnostic.Len() != 0 || strings.Contains(err.Error(), "synthetic-private-command") {
			t.Fatal("host command accepted unsafe authority or echoed input", args[0])
		}
	}
	if runtime.GOOS != "linux" {
		var out, diagnostic bytes.Buffer
		if err := run(t.Context(), []string{"maintenance-host", "--config=nonexistent", "--confirm-maintenance-service"}, &out, &diagnostic); err == nil || !strings.Contains(err.Error(), "Linux") {
			t.Fatal("host mutation did not reject platform before opening configuration")
		}
	}
}
