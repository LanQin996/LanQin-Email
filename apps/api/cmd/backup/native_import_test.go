package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeImportCLIRequiresExplicitEmptyTargetAndPrivateInputs(t *testing.T) {
	base := []string{"--driver=mysql", "--target-dsn-env=NATIVE_SYNTHETIC_TARGET", "--in=encrypted.age", "--identity-file=private-identity", "--expected-sha256=" + strings.Repeat("a", 64), "--workspace=private-work", "--confirm-empty-target"}
	for i := range base {
		args := append(append([]string{}, base[:i]...), base[i+1:]...)
		var output, diagnostic bytes.Buffer
		if err := run(context.Background(), append([]string{"import-native"}, args...), &output, &diagnostic); err == nil || output.Len() != 0 || diagnostic.Len() != 0 {
			t.Fatal("missing native import argument accepted", i)
		}
	}
	for _, extra := range []string{"--driver=sqlite", "--target-dsn-env=postgres://synthetic:do-not-echo@host/database", "--identity=AGE-SECRET-KEY-synthetic-do-not-echo", "--confirm-empty-target=false"} {
		var output, diagnostic bytes.Buffer
		err := runImportNative(t.Context(), append(append([]string{}, base...), extra), &output, &diagnostic)
		if err == nil || output.Len() != 0 || diagnostic.Len() != 0 || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("unsafe native CLI input accepted or leaked")
		}
	}
}

func TestNativeImportWebRejectsMixedSourcesAndSharedDSN(t *testing.T) {
	base := []string{"--driver=postgres", "--target-dsn-env=NATIVE_SYNTHETIC_TARGET", "--web-job=bkj_0123456789abcdef0123456789abcdef", "--web-state-dsn-env=NATIVE_SYNTHETIC_STATE", "--web-restore-root=/private/recovery", "--workspace=/private/work", "--confirm-empty-target"}
	t.Setenv("NATIVE_SYNTHETIC_TARGET", "synthetic-identical-dsn-do-not-echo")
	t.Setenv("NATIVE_SYNTHETIC_STATE", "synthetic-identical-dsn-do-not-echo")
	for _, extra := range []string{"", "--web-state-dsn-env=NATIVE_SYNTHETIC_TARGET", "--web-state-dsn-env=postgres://do-not-echo@host/db", "--web-job=", "--web-restore-root=", "--in=archive.age", "--identity-file=private-key", "--password-file=private-password", "--expected-sha256=" + strings.Repeat("a", 64), "--max-entries=100001", "--max-bytes=1099511627777"} {
		args := append([]string{}, base...)
		if extra != "" {
			args = append(args, extra)
		}
		var output, diagnostic bytes.Buffer
		err := runImportNative(t.Context(), args, &output, &diagnostic)
		if err == nil || output.Len() != 0 || diagnostic.Len() != 0 || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("unsafe Web import source accepted or leaked")
		}
	}
}

func TestNativeImportPasswordCLIRejectsWrongScopeAndMixedSecrets(t *testing.T) {
	f := newOnlineCLIFixture(t)
	const password = "synthetic native password"
	file := passwordFileFixture(t, password)
	var output, diagnostic bytes.Buffer
	if err := run(t.Context(), f.args("--recipient=", "--password-file", file), &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	_, receipt, ok := strings.Cut(strings.TrimSpace(output.String()), "sha256=")
	if !ok || len(receipt) != 64 {
		t.Fatal("missing password archive receipt")
	}
	workspace := t.TempDir()
	t.Setenv("NATIVE_SYNTHETIC_PASSWORD_TARGET", "invalid-DSN-do-not-echo")
	args := []string{"import-native", "--driver=mysql", "--target-dsn-env=NATIVE_SYNTHETIC_PASSWORD_TARGET", "--in", f.out, "--password-file", file, "--expected-sha256", receipt, "--workspace", workspace, "--confirm-empty-target"}
	output.Reset()
	// The real password decryptor runs, but the SQLite archive is rejected
	// before any native target access. This must not become a schema bypass.
	err := run(t.Context(), args, &output, &diagnostic)
	if err == nil || !strings.Contains(err.Error(), "verified same-engine archive") || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "do-not-echo") || output.Len() != 0 {
		t.Fatal("password import skipped archive scope or exposed secrets", err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatal("failed password native import left private staging", err)
	}
	for _, extra := range []string{"--identity-file=private-key", "--password=synthetic-do-not-echo", "--password-file=" + filepath.Join(t.TempDir(), "missing-private-password")} {
		output.Reset()
		err := run(t.Context(), append(append([]string{}, args...), extra), &output, &diagnostic)
		if err == nil || strings.Contains(err.Error(), "synthetic-do-not-echo") || output.Len() != 0 || diagnostic.Len() != 0 {
			t.Fatal("invalid native import password options accepted or leaked", err)
		}
	}
}
