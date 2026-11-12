package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"lanqin-email-api/internal/app"
)

func runVerifyNative(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runVerifyNativeWithVerifier(ctx, args, stdout, app.VerifyNativeRuntime)
}

func runVerifyNativeWithVerifier(ctx context.Context, args []string, stdout io.Writer, verify func(context.Context, app.Config, string) error) error {
	flags := flag.NewFlagSet("verify-native", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	driver := flags.String("driver", "", "native engine in this deployment namespace")
	dsnEnv := flags.String("dsn-env", "", "explicit runtime DSN environment reference, never connection value")
	data := flags.String("data-root", "", "actual container runtime data root")
	mail := flags.String("mail-root", "", "actual container runtime Maildir root")
	expected := flags.String("expected-plan-sha256", "", "installed plan digest independently retained by the coordinator")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*driver != "postgres" && *driver != "mysql") || !nativeDSNEnvPattern.MatchString(*dsnEnv) || *data == "" || *mail == "" || verify == nil {
		return errors.New("verify-native requires supported --driver, --dsn-env, --data-root, --mail-root and --expected-plan-sha256")
	}
	digest, err := hex.DecodeString(*expected)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("verify-native requires a trusted installed-plan SHA-256")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dsn, ok := os.LookupEnv(*dsnEnv)
	if !ok || dsn == "" || len(dsn) > 16384 {
		return errors.New("runtime database connection environment reference unavailable")
	}
	if err := verify(ctx, app.Config{DBDriver: *driver, DBDSN: dsn, DataDir: *data, MaildirRoot: *mail}, hex.EncodeToString(digest)); err != nil {
		return errors.New("runtime native database and installed files do not form the verified activated restore pair")
	}
	if _, err := fmt.Fprintln(stdout, "Verified native runtime database and installed plan; no migrations, services or workers started."); err != nil {
		return errors.New("native runtime pair verified but cannot write verification receipt")
	}
	return nil
}
