package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"lanqin-email-api/internal/app"
)

func TestVerifyNativeUsesOnlyExplicitRuntimeAuthorityWithoutStartup(t *testing.T) {
	base := []string{"--driver=postgres", "--dsn-env=SYNTHETIC_RUNTIME_DSN", "--data-root=/data", "--mail-root=/var/mail/vhosts", "--expected-plan-sha256=" + strings.Repeat("a", 64)}
	t.Setenv("SYNTHETIC_RUNTIME_DSN", "private-synthetic-runtime-dsn-do-not-echo")
	for i := range base {
		var output bytes.Buffer
		args := append(append([]string{}, base[:i]...), base[i+1:]...)
		if err := runVerifyNativeWithVerifier(t.Context(), args, &output, func(context.Context, app.Config, string) error {
			t.Fatal("incomplete authority reached database verifier")
			return nil
		}); err == nil || output.Len() != 0 {
			t.Fatal("missing verifier input accepted", i)
		}
	}
	for _, extra := range []string{"--driver=sqlite", "--dsn-env=postgres://secret-do-not-echo@host/db", "--expected-plan-sha256=secret-do-not-echo", "--secret=do-not-echo"} {
		var output bytes.Buffer
		err := runVerifyNativeWithVerifier(t.Context(), append(append([]string{}, base...), extra), &output, func(context.Context, app.Config, string) error { t.Fatal("unsafe value reached verifier"); return nil })
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("unsafe verification input accepted/leaked")
		}
	}
	for _, failure := range []bool{false, true} {
		called := false
		var output bytes.Buffer
		err := runVerifyNativeWithVerifier(t.Context(), base, &output, func(_ context.Context, cfg app.Config, sha string) error {
			called = true
			if cfg.DBDriver != "postgres" || cfg.DBDSN != "private-synthetic-runtime-dsn-do-not-echo" || cfg.DataDir != "/data" || cfg.MaildirRoot != "/var/mail/vhosts" || sha != strings.Repeat("a", 64) {
				t.Fatal("runtime namespace authority lost")
			}
			if failure {
				return errors.New("private-driver-error-do-not-echo")
			}
			return nil
		})
		if !called || (err != nil) != failure || err != nil && strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("runtime check result incorrect", err)
		}
		if failure && output.Len() != 0 || !failure && !strings.Contains(output.String(), "no migrations, services or workers") {
			t.Fatal("runtime check startup boundary missing")
		}
	}
}
