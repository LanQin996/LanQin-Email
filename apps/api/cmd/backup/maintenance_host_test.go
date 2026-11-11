package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

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
