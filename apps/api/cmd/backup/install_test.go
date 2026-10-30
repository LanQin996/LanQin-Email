package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

func validInstallArgs() []string {
	return []string{"--staged=staged", "--workspace=private", "--runtime-data-root=/data", "--runtime-mail-root=/mail", "--data-target=/host/data", "--mail-target=/host/mail", "--dkim-target=/host/dkim", "--journal-dir=/host/journals", "--data-uid=0", "--data-gid=1", "--mail-uid=2", "--mail-gid=3", "--dkim-uid=4", "--dkim-gid=2147483647", "--confirm-maintenance"}
}

func validRollbackArgs() []string {
	return []string{"--journal=/host/journals/restore.journal", "--data-target=/host/data", "--mail-target=/host/mail", "--dkim-target=/host/dkim", "--confirm-maintenance"}
}

func TestInstallCLIFlow(t *testing.T) {
	for _, customLimits := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "custom"}[customLimits], func(t *testing.T) {
			args := validInstallArgs()
			wantLimits := backup.DefaultLimits()
			if customLimits {
				args = append(args, "--max-bytes=12345", "--max-entries=67")
				wantLimits = backup.Limits{MaxBytes: 12345, MaxEntries: 67}
			}
			var steps []string
			actions := installActions{
				preflight: func() error { steps = append(steps, "preflight"); return nil },
				prepare: func(_ context.Context, staged, workspace, data, mail string, limits backup.Limits) (installPrepared, error) {
					steps = append(steps, "prepare")
					if staged != "staged" || workspace != "private" || data != "/data" || mail != "/mail" || limits != wantLimits {
						t.Fatal("preparation inputs changed")
					}
					return installPrepared{path: "prepared", close: func() error { steps = append(steps, "close"); return nil }}, nil
				},
				apply: func(_ context.Context, prepared string, targets install.Targets, owners install.Owners, journal string, confirmed bool) (install.JournalInfo, error) {
					steps = append(steps, "apply")
					wantOwners := install.Owners{Data: install.Owner{UID: 0, GID: 1}, Mail: install.Owner{UID: 2, GID: 3}, DKIM: install.Owner{UID: 4, GID: 2147483647}}
					if prepared != "prepared" || targets != (install.Targets{Data: "/host/data", Mail: "/host/mail", DKIM: "/host/dkim"}) || owners != wantOwners || journal != "/host/journals" || !confirmed {
						t.Fatal("installation inputs changed or guessed")
					}
					return install.JournalInfo{Path: "/host/journals/txn.journal", TransactionID: "txn", RollbackTargets: install.Targets{Data: "old-data", Mail: "old-mail", DKIM: "old-dkim"}}, nil
				},
			}
			var out, diagnostic bytes.Buffer
			if err := runInstallWithActions(context.Background(), args, &out, &diagnostic, actions); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(steps, []string{"preflight", "prepare", "apply", "close"}) {
				t.Fatal(steps)
			}
			if diagnostic.Len() != 0 || !strings.Contains(out.String(), "services NOT started; inspect health before start") || !strings.Contains(out.String(), "old-dkim") || strings.Contains(out.String(), "prepared") {
				t.Fatal("incorrect or excessive receipt")
			}
		})
	}
}

func TestInstallCLIRejectsBeforeSideEffects(t *testing.T) {
	// Each required input is independently removed. Nothing is inferred from
	// the host, old deployment or journal, including any of the six owners.
	cases := [][]string{nil}
	valid := validInstallArgs()
	for i := range valid {
		args := append([]string{}, valid[:i]...)
		cases = append(cases, append(args, valid[i+1:]...))
	}
	for _, extra := range []string{"--data-uid=-1", "--mail-gid=2147483648", "--dkim-uid=AGE-SECRET-KEY-test", "--max-bytes=0", "--max-entries=-1", "--confirm-maintenance=false", "--identity=AGE-SECRET-KEY-test", "--identity-file=private-key", "AGE-SECRET-KEY-test"} {
		cases = append(cases, append(append([]string{}, valid...), extra))
	}
	for i, args := range cases {
		var out, diagnostic bytes.Buffer
		// A zero actions value panics if validation attempts any operation.
		err := runInstallWithActions(context.Background(), args, &out, &diagnostic, installActions{})
		if err == nil || out.Len() != 0 || diagnostic.Len() != 0 || strings.Contains(err.Error(), "AGE-SECRET") {
			t.Fatalf("case %d: invalid input accepted or leaked", i)
		}
	}
}

func TestInstallCLIFailuresArePrivateAndCleanupRuns(t *testing.T) {
	const secret = "provider-body AGE-SECRET-KEY-test /old/source/private-mail"
	for _, stage := range []string{"prepare", "apply", "close", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			closed, applied := false, false
			actions := installActions{
				preflight: func() error { return nil },
				prepare: func(context.Context, string, string, string, string, backup.Limits) (installPrepared, error) {
					if stage == "prepare" {
						return installPrepared{}, errors.New(secret)
					}
					return installPrepared{path: "prepared", close: func() error {
						closed = true
						if stage == "close" {
							return errors.New(secret)
						}
						return nil
					}}, nil
				},
				apply: func(context.Context, string, install.Targets, install.Owners, string, bool) (install.JournalInfo, error) {
					applied = true
					info := install.JournalInfo{Path: "journal", TransactionID: "txn"}
					if stage == "apply" {
						return info, errors.New(secret)
					}
					return info, nil
				},
			}
			var out, diagnostic bytes.Buffer
			var output io.Writer = &out
			if stage == "receipt" {
				output = installFailWriter{}
			}
			err := runInstallWithActions(context.Background(), validInstallArgs(), output, &diagnostic, actions)
			if err == nil || strings.Contains(err.Error()+out.String()+diagnostic.String(), secret) {
				t.Fatal("missing or unsafe failure")
			}
			if applied != (stage != "prepare") || closed != applied {
				t.Fatal("unexpected effects or skipped cleanup")
			}
			if stage == "apply" && !strings.Contains(out.String(), "journal") {
				t.Fatal("partial transaction receipt lost")
			}
		})
	}
}

type installFailWriter struct{}

func (installFailWriter) Write([]byte) (int, error) { return 0, errors.New("private writer error") }

func TestRollbackCLIValidationAndFlow(t *testing.T) {
	valid := validRollbackArgs()
	for i := range valid {
		args := append([]string{}, valid[:i]...)
		args = append(args, valid[i+1:]...)
		if err := runRollbackWithActions(context.Background(), args, io.Discard, io.Discard, installActions{}); err == nil {
			t.Fatal("accepted missing rollback input")
		}
	}
	for _, extra := range []string{"AGE-SECRET-KEY-test", "--identity=AGE-SECRET-KEY-test", "--confirm-maintenance=false"} {
		var diagnostic bytes.Buffer
		err := runRollbackWithActions(context.Background(), append(append([]string{}, valid...), extra), io.Discard, &diagnostic, installActions{})
		if err == nil || diagnostic.Len() != 0 || strings.Contains(err.Error(), "AGE-SECRET") {
			t.Fatal("unsafe parser")
		}
	}
	called := false
	actions := installActions{preflight: func() error { return nil }, rollback: func(_ context.Context, journal string, targets install.Targets, confirmed bool) error {
		called = true
		if journal != "/host/journals/restore.journal" || targets != (install.Targets{Data: "/host/data", Mail: "/host/mail", DKIM: "/host/dkim"}) || !confirmed {
			t.Fatal("rollback inferred or changed inputs")
		}
		return nil
	}}
	var out bytes.Buffer
	if err := runRollbackWithActions(context.Background(), valid, &out, io.Discard, actions); err != nil || !called || !strings.Contains(out.String(), "services NOT started") {
		t.Fatal("rollback failed", err)
	}
	actions.rollback = func(context.Context, string, install.Targets, bool) error { return errors.New("private provider body") }
	out.Reset()
	err := runRollbackWithActions(context.Background(), valid, &out, io.Discard, actions)
	if err == nil || strings.Contains(err.Error(), "private provider body") || out.Len() != 0 {
		t.Fatal("rollback leaked error or success")
	}
}

func TestInstallCLIPlatformAndCancellationPreflight(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Nonexistent source/workspace paths prove the public entry rejects the
		// platform before it can copy or inspect staging/live files.
		if err := run(context.Background(), append([]string{"install"}, validInstallArgs()...), io.Discard, io.Discard); !errors.Is(err, install.ErrSwitchPlatform) {
			t.Fatal(err)
		}
		if err := run(context.Background(), append([]string{"rollback"}, validRollbackArgs()...), io.Discard, io.Discard); !errors.Is(err, install.ErrSwitchPlatform) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runInstallWithActions(ctx, validInstallArgs(), io.Discard, io.Discard, installActions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := runRollbackWithActions(ctx, validRollbackArgs(), io.Discard, io.Discard, installActions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
