package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/app"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

func nativeInstallArguments() []string {
	return []string{"--driver=postgres", "--target-dsn-env=NATIVE_SYNTHETIC_NEW_DSN", "--confirm-maintenance", "--data-target=/live/data", "--mail-target=/live/mail", "--dkim-target=/live/dkim", "--staged=/private/recovery", "--workspace=/private/work", "--runtime-data-root=/runtime/data", "--runtime-mail-root=/runtime/mail", "--expected-report-sha256=" + strings.Repeat("a", 64), "--journal-dir=/private/journals", "--data-uid=100", "--data-gid=101", "--mail-uid=200", "--mail-gid=201", "--dkim-uid=300", "--dkim-gid=301"}
}

func TestNativeMaintenanceRejectsIncompleteAndSecretArgumentsBeforeAccess(t *testing.T) {
	base := nativeInstallArguments()
	preflight := 0
	actions := nativeMaintenanceActions{preflight: func() error { preflight++; return errors.New("unexpected access") }}
	for i := range base {
		args := append(append([]string{}, base[:i]...), base[i+1:]...)
		var out bytes.Buffer
		if err := runNativeMaintenanceWithActions(t.Context(), "install-native", args, &out, actions); err == nil || out.Len() != 0 {
			t.Fatal("missing argument accepted", base[i])
		}
	}
	for _, extra := range []string{"--driver=sqlite", "--target-dsn-env=postgres://do-not-echo@host/db", "--password=do-not-echo", "--expected-report-sha256=do-not-echo", "--data-uid=-1", "--mail-gid=2147483648", "--max-bytes=4095", "--max-bytes=1099511627777", "--max-entries=3", "--max-entries=100001", "--confirm-maintenance=false", "do-not-echo"} {
		var out bytes.Buffer
		err := runNativeMaintenanceWithActions(t.Context(), "install-native", append(append([]string{}, base...), extra), &out, actions)
		if err == nil || strings.Contains(err.Error(), "do-not-echo") || out.Len() != 0 {
			t.Fatal("invalid or secret argument accepted or echoed")
		}
	}
	if preflight != 0 {
		t.Fatal("validation accessed external state")
	}
}

func TestNativeMaintenancePlatformGatePrecedesDSNAndFiles(t *testing.T) {
	t.Setenv("NATIVE_SYNTHETIC_NEW_DSN", "")
	want := errors.New("synthetic platform gate")
	err := runNativeMaintenanceWithActions(t.Context(), "install-native", nativeInstallArguments(), io.Discard, nativeMaintenanceActions{preflight: func() error { return want }})
	if !errors.Is(err, want) {
		t.Fatal("platform gate did not precede DSN lookup", err)
	}
	if runtime.GOOS != "linux" {
		if err := runNativeMaintenance(t.Context(), "install-native", nativeInstallArguments(), io.Discard, io.Discard); !errors.Is(err, install.ErrSwitchPlatform) {
			t.Fatal("production platform gate bypassed", err)
		}
	}
}

func TestNativeMaintenanceInstallationOutcomes(t *testing.T) {
	for _, failure := range []string{"", "prepare", "apply", "activate", "rollback", "output"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("NATIVE_SYNTHETIC_NEW_DSN", "synthetic-private-dsn-do-not-echo")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := []string{}
			failed := errors.New("private-provider-error-do-not-echo")
			journal := install.JournalInfo{Path: "/private/journals/synthetic", TransactionID: strings.Repeat("a", 32)}
			actions := nativeMaintenanceActions{
				preflight: func() error { calls = append(calls, "preflight"); return nil },
				prepare: func(_ context.Context, cfg app.Config, staged, workspace, data, mail string, targets install.Targets, sha string, limits backup.Limits) (*install.Prepared, error) {
					calls = append(calls, "prepare")
					if cfg.DBDriver != "postgres" || cfg.DBDSN != "synthetic-private-dsn-do-not-echo" || staged != "/private/recovery" || workspace != "/private/work" || data != "/runtime/data" || mail != "/runtime/mail" || targets.Data != "/live/data" || sha != strings.Repeat("a", 64) || limits != backup.DefaultLimits() {
						t.Fatal("explicit preparation parameters lost")
					}
					if failure == "prepare" {
						return nil, failed
					}
					return &install.Prepared{Path: "synthetic-prepared"}, nil
				},
				apply: func(_ context.Context, path string, targets install.Targets, owners install.Owners, directory string, confirmed bool) (install.JournalInfo, error) {
					calls = append(calls, "apply")
					if path != "synthetic-prepared" || directory != "/private/journals" || !confirmed || owners != (install.Owners{Data: install.Owner{UID: 100, GID: 101}, Mail: install.Owner{UID: 200, GID: 201}, DKIM: install.Owner{UID: 300, GID: 301}}) {
						t.Fatal("switch parameters lost")
					}
					if failure == "apply" {
						return journal, failed
					}
					return journal, nil
				},
				activate: func(_ context.Context, _ app.Config, path string, _ install.Targets, confirmed bool) (install.JournalInfo, error) {
					calls = append(calls, "activate")
					if path != journal.Path || !confirmed {
						t.Fatal("activation lost journal or confirmation")
					}
					if failure == "activate" || failure == "rollback" {
						cancel()
						return install.JournalInfo{}, failed
					}
					return journal, nil
				},
				rollback: func(cleanup context.Context, _ app.Config, path string, _ install.Targets, confirmed bool) error {
					calls = append(calls, "rollback")
					if cleanup.Err() != nil || path != journal.Path || !confirmed {
						t.Fatal("rollback inherited canceled request or wrong journal")
					}
					if _, ok := cleanup.Deadline(); !ok {
						t.Fatal("rollback was unbounded")
					}
					if failure == "rollback" {
						return failed
					}
					return nil
				},
			}
			var out bytes.Buffer
			var writer io.Writer = &out
			if failure == "output" {
				writer = nativeFailWriter{}
			}
			err := runNativeMaintenanceWithActions(ctx, "install-native", nativeInstallArguments(), writer, actions)
			if (err != nil) != (failure != "") || err != nil && strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("wrong or leaking installation outcome", err)
			}
			wantCalls := []string{"preflight", "prepare"}
			if failure != "prepare" {
				wantCalls = append(wantCalls, "apply")
			}
			if failure != "prepare" && failure != "apply" {
				wantCalls = append(wantCalls, "activate")
			}
			if failure == "activate" || failure == "rollback" {
				wantCalls = append(wantCalls, "rollback")
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatal("unsafe operation order", calls)
			}
			if failure == "prepare" || failure == "output" {
				return
			}
			var receipt nativeMaintenanceReceipt
			if json.Unmarshal(out.Bytes(), &receipt) != nil || receipt.ServicesStarted || receipt.Journal != journal.Path {
				t.Fatal("missing or unsafe receipt", out.String())
			}
			wantState := "activated"
			if failure == "apply" || failure == "rollback" {
				wantState = "unknown"
			}
			if failure == "activate" {
				wantState = "guarded"
			}
			if receipt.DatabaseState != wantState || receipt.RollbackCompleted != (failure == "activate") {
				t.Fatal("receipt misrepresented state", receipt)
			}
		})
	}
}

type nativeFailWriter struct{}

func (nativeFailWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

func TestNativeMaintenanceWebHandoffValidationAndBinding(t *testing.T) {
	base := []string{}
	for _, arg := range nativeInstallArguments() {
		if !strings.HasPrefix(arg, "--staged=") && !strings.HasPrefix(arg, "--expected-report-sha256=") {
			base = append(base, arg)
		}
	}
	base = append(base, "--web-job=bkj_0123456789abcdef0123456789abcdef", "--web-state-dsn-env=NATIVE_SYNTHETIC_STATE_DSN", "--web-restore-root=/private/web")
	for _, extra := range []string{"--web-job=", "--web-state-dsn-env=", "--web-restore-root=", "--staged=/other", "--expected-report-sha256=" + strings.Repeat("a", 64), "--web-state-dsn-env=NATIVE_SYNTHETIC_NEW_DSN", "--web-state-dsn-env=postgres://do-not-echo@host/db"} {
		called := false
		err := runNativeMaintenanceWithActions(t.Context(), "install-native", append(append([]string{}, base...), extra), io.Discard, nativeMaintenanceActions{preflight: func() error { called = true; return errors.New("unexpected access") }})
		if err == nil || called || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("unsafe Web handoff flags accepted or leaked")
		}
	}
	t.Setenv("NATIVE_SYNTHETIC_NEW_DSN", "synthetic-new-private-dsn")
	t.Setenv("NATIVE_SYNTHETIC_STATE_DSN", "synthetic-state-private-dsn")
	t.Setenv("LANQIN_BACKUP_SECRET_KEY", "synthetic-master-key")
	for _, scenario := range []string{"valid", "resolve-failed", "same-dsn", "missing-state"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "same-dsn" {
				t.Setenv("NATIVE_SYNTHETIC_STATE_DSN", "synthetic-new-private-dsn")
			}
			if scenario == "missing-state" {
				t.Setenv("NATIVE_SYNTHETIC_STATE_DSN", "")
			}
			resolved, prepared := false, false
			actions := nativeMaintenanceActions{
				preflight: func() error { return nil },
				resolve: func(_ context.Context, driver, dsn, root, master, job string) (string, string, error) {
					resolved = true
					if driver != "postgres" || dsn != "synthetic-state-private-dsn" || root != "/private/web" || master != "synthetic-master-key" || job != "bkj_0123456789abcdef0123456789abcdef" {
						t.Fatal("Web task authority was not explicit")
					}
					if scenario == "resolve-failed" {
						return "", "", errors.New("private-do-not-echo")
					}
					return "/private/authenticated-recovered", strings.Repeat("b", 64), nil
				},
				prepare: func(_ context.Context, cfg app.Config, staged, workspace, data, mail string, targets install.Targets, sha string, limits backup.Limits) (*install.Prepared, error) {
					prepared = true
					if !resolved || cfg.DBDSN != "synthetic-new-private-dsn" || staged != "/private/authenticated-recovered" || sha != strings.Repeat("b", 64) {
						t.Fatal("installation ignored authenticated handoff")
					}
					return nil, errors.New("synthetic preparation stop")
				},
			}
			var out bytes.Buffer
			err := runNativeMaintenanceWithActions(t.Context(), "install-native", base, &out, actions)
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "private-do-not-echo") {
				t.Fatal("handoff error missing or leaked")
			}
			if resolved != (scenario == "valid" || scenario == "resolve-failed") || prepared != (scenario == "valid") {
				t.Fatal("handoff failed to gate target preparation", scenario)
			}
		})
	}
}

func TestNativeMaintenanceMappedWebRootPassesExplicitAuthority(t *testing.T) {
	args := []string{}
	for _, arg := range nativeInstallArguments() {
		if !strings.HasPrefix(arg, "--staged=") && !strings.HasPrefix(arg, "--expected-report-sha256=") {
			args = append(args, arg)
		}
	}
	args = append(args, "--web-job=bkj_0123456789abcdef0123456789abcdef", "--web-state-dsn-env=NATIVE_SYNTHETIC_STATE_DSN", "--web-restore-root=/host/private-recovery", "--web-recorded-root=/backups/restore")
	t.Setenv("NATIVE_SYNTHETIC_NEW_DSN", "synthetic-new-private-dsn")
	t.Setenv("NATIVE_SYNTHETIC_STATE_DSN", "synthetic-state-private-dsn")
	t.Setenv("LANQIN_BACKUP_SECRET_KEY", "synthetic-mapped-master")
	for _, scenario := range []string{"valid", "resolve-failed", "mapped-resolver-missing"} {
		t.Run(scenario, func(t *testing.T) {
			resolved, prepared := false, false
			actions := nativeMaintenanceActions{
				preflight: func() error { return nil },
				resolve: func(context.Context, string, string, string, string, string) (string, string, error) {
					t.Fatal("mapped native handoff used unmapped resolver")
					return "", "", nil
				},
				resolveMapped: func(_ context.Context, driver, dsn, root, recorded, master, job string) (string, string, error) {
					resolved = true
					if driver != "postgres" || dsn != "synthetic-state-private-dsn" || root != "/host/private-recovery" || recorded != "/backups/restore" || master != "synthetic-mapped-master" || job != "bkj_0123456789abcdef0123456789abcdef" {
						t.Fatal("mapped native authority changed")
					}
					if scenario == "resolve-failed" {
						return "", "", errors.New("synthetic-state-private-dsn synthetic-mapped-master /backups/restore /host/private-recovery")
					}
					return "/host/authenticated/recovered", strings.Repeat("c", 64), nil
				},
				prepare: func(_ context.Context, cfg app.Config, staged, workspace, data, mail string, targets install.Targets, sha string, limits backup.Limits) (*install.Prepared, error) {
					prepared = true
					if !resolved || cfg.DBDSN != "synthetic-new-private-dsn" || staged != "/host/authenticated/recovered" || sha != strings.Repeat("c", 64) {
						t.Fatal("native mapped installation dropped handoff")
					}
					return nil, errors.New("synthetic private preparation stop")
				},
			}
			if scenario == "mapped-resolver-missing" {
				actions.resolveMapped = nil
			}
			var out bytes.Buffer
			err := runNativeMaintenanceWithActions(t.Context(), "install-native", args, &out, actions)
			if err == nil || out.Len() != 0 || resolved != (scenario != "mapped-resolver-missing") || prepared != (scenario == "valid") {
				t.Fatal("wrong mapped native handoff gating", scenario)
			}
			for _, private := range []string{"synthetic-state-private-dsn", "synthetic-new-private-dsn", "synthetic-mapped-master", "/backups/restore", "/host/private-recovery"} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("native mapped failure exposed authority")
				}
			}
		})
	}
	if err := runNativeMaintenanceWithActions(t.Context(), "install-native", append(nativeInstallArguments(), "--web-recorded-root=/backups/restore"), io.Discard, nativeMaintenanceActions{}); err == nil {
		t.Fatal("recorded-root accepted with standalone staged input")
	}
}

func TestNativeMaintenanceRecoveryModes(t *testing.T) {
	t.Setenv("NATIVE_SYNTHETIC_NEW_DSN", "synthetic-private-dsn")
	for _, mode := range []string{"activate-native", "rollback-native"} {
		base := append(append([]string{}, nativeInstallArguments()[:6]...), "--journal=/private/journal")
		for i := range base {
			args := append(append([]string{}, base[:i]...), base[i+1:]...)
			if err := runNativeMaintenanceWithActions(t.Context(), mode, args, io.Discard, nativeMaintenanceActions{}); err == nil {
				t.Fatal("incomplete recovery accepted", mode, i)
			}
		}
		called := ""
		actions := nativeMaintenanceActions{
			preflight: func() error { return nil },
			activate: func(_ context.Context, _ app.Config, journal string, _ install.Targets, confirmed bool) (install.JournalInfo, error) {
				if journal != "/private/journal" || !confirmed {
					t.Fatal("wrong recovery journal")
				}
				called = "activate-native"
				return install.JournalInfo{Path: journal}, nil
			},
			rollback: func(_ context.Context, _ app.Config, journal string, _ install.Targets, confirmed bool) error {
				if journal != "/private/journal" || !confirmed {
					t.Fatal("wrong rollback journal")
				}
				called = "rollback-native"
				return nil
			},
		}
		var out bytes.Buffer
		if err := runNativeMaintenanceWithActions(t.Context(), mode, base, &out, actions); err != nil || called != mode {
			t.Fatal("recovery route failed", mode, err)
		}
		var receipt nativeMaintenanceReceipt
		if json.Unmarshal(out.Bytes(), &receipt) != nil || receipt.ServicesStarted || receipt.RollbackCompleted != (mode == "rollback-native") {
			t.Fatal("invalid recovery receipt")
		}
	}
}
