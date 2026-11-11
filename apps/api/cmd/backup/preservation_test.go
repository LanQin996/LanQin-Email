package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/preservation"
)

type preservationCLIFixture struct {
	root, input, identityFile, sha, identity string
	manifest                                 preservation.Manifest
	items                                    []preservation.Item
}

func newPreservationCLIFixture(t *testing.T, privateOperationID bool) preservationCLIFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	fixture := preservationCLIFixture{root: root, input: filepath.Join(root, "preservation.age"), identityFile: filepath.Join(root, "identity.txt"), identity: identity.String()}
	if err := os.WriteFile(fixture.identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.items = []preservation.Item{
		{Name: "assets/0000.bin", Source: filepath.Join(root, "never-apply.env"), Data: []byte("SYNTHETIC_DEPLOYMENT_SECRET=never-echo-preservation-secret\n")},
		{Name: "assets/0001.bin", Source: "../../untrusted-source-label-must-not-be-interpreted", Data: []byte{0, 255, 1, 2, 128, 65}},
		{Name: "spools/" + strings.Repeat("a", 64) + ".tar", Source: "synthetic-container-spool", Data: []byte("opaque synthetic queue bytes; never expand or replay")},
	}
	operation := strings.Repeat("c", 32)
	if privateOperationID {
		operation = identity.String()
	}
	var ciphertext bytes.Buffer
	fixture.manifest, err = preservation.Write(t.Context(), &ciphertext, identity.Recipient().String(), preservation.Binding{ConfigSHA256: strings.Repeat("b", 64), OperationID: operation}, fixture.items, preservation.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(ciphertext.Bytes())
	fixture.sha = hex.EncodeToString(digest[:])
	if err := os.WriteFile(fixture.input, ciphertext.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture preservationCLIFixture) args(mode, out string) []string {
	args := []string{mode, "--in=" + fixture.input, "--identity-file=" + fixture.identityFile, "--expected-sha256=" + fixture.sha}
	if mode == "preservation-restore" {
		args = append(args, "--out="+out)
	}
	return args
}

func TestPreservationCLIAuthenticatesAndRestoresOnlyIsolatedOpaqueEvidence(t *testing.T) {
	fixture := newPreservationCLIFixture(t, false)
	var expectedBytes int64
	for _, item := range fixture.items {
		expectedBytes += int64(len(item.Data))
	}
	for _, mode := range []string{"preservation-verify", "preservation-restore"} {
		t.Run(mode, func(t *testing.T) {
			out := filepath.Join(fixture.root, "isolated-evidence")
			var output, diagnostics bytes.Buffer
			if err := run(t.Context(), fixture.args(mode, out), &output, &diagnostics); err != nil {
				t.Fatal(mode, err)
			}
			var receipt preservationReceipt
			if json.Unmarshal(output.Bytes(), &receipt) != nil || !receipt.Verified || receipt.ServicesChanged || receipt.Restored != (mode == "preservation-restore") || receipt.Entries != 3 || receipt.Assets != 2 || receipt.Spools != 1 || receipt.Bytes != expectedBytes || receipt.ConfigSHA256 != fixture.manifest.Binding.ConfigSHA256 || receipt.OperationID != fixture.manifest.Binding.OperationID {
				t.Fatal("unsafe or incorrect preservation receipt", output.String())
			}
			for _, secret := range []string{fixture.identity, fixture.root, "never-echo-preservation-secret", "synthetic-container-spool", "untrusted-source-label"} {
				if strings.Contains(output.String()+diagnostics.String(), secret) {
					t.Fatal("preservation command leaked recovery/deployment data")
				}
			}
			if diagnostics.Len() != 0 {
				t.Fatal("successful preservation command wrote diagnostics")
			}
			if _, err := os.Lstat(filepath.Join(fixture.root, "never-apply.env")); !os.IsNotExist(err) {
				t.Fatal("Source label was applied as a deployment path")
			}
			if mode == "preservation-verify" {
				if _, err := os.Lstat(out); !os.IsNotExist(err) {
					t.Fatal("verification created plaintext output")
				}
				entries, err := os.ReadDir(fixture.root)
				if err != nil || len(entries) != 2 {
					t.Fatal("verification created temporary plaintext files", err)
				}
				return
			}
			if _, err := os.Stat(filepath.Join(out, "READY")); err != nil {
				t.Fatal("isolated authenticated evidence lacks READY", err)
			}
			for i, item := range fixture.items {
				content, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(preservation.PayloadName(i))))
				if err != nil || !bytes.Equal(content, item.Data) {
					t.Fatal("opaque evidence changed", err)
				}
			}
		})
	}
}

func TestPreservationCLIRejectsFailuresWithoutOverwritingOrLeaking(t *testing.T) {
	for _, scenario := range []string{"wrong-key", "wrong-hash", "tampered", "plaintext", "max-bytes", "max-entries", "huge-key", "key-directory", "existing-output", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPreservationCLIFixture(t, false)
			out := filepath.Join(fixture.root, "new-evidence")
			args := fixture.args("preservation-restore", out)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "wrong-key":
				identity, _ := age.GenerateX25519Identity()
				if err := os.WriteFile(fixture.identityFile, []byte(identity.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong-hash":
				args = append(args, "--expected-sha256="+strings.Repeat("0", 64))
			case "tampered":
				data, err := os.ReadFile(fixture.input)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 1
				if err := os.WriteFile(fixture.input, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "plaintext":
				data := []byte("never-echo-preservation-secret plaintext")
				if err := os.WriteFile(fixture.input, data, 0o600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				args = append(args, "--expected-sha256="+hex.EncodeToString(digest[:]))
			case "max-bytes":
				args = append(args, "--max-bytes=1")
			case "max-entries":
				args = append(args, "--max-entries=1")
			case "huge-key":
				if err := os.WriteFile(fixture.identityFile, bytes.Repeat([]byte{'s'}, 4097), 0o600); err != nil {
					t.Fatal(err)
				}
			case "key-directory":
				args = append(args, "--identity-file="+fixture.root)
			case "existing-output":
				if err := os.Mkdir(out, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, "keep"), []byte("unrelated-owner-evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			var output, diagnostics bytes.Buffer
			err := run(ctx, args, &output, &diagnostics)
			if err == nil || output.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatal("failed preservation operation reported success or echoed inputs")
			}
			if strings.Contains(err.Error(), fixture.identity) || strings.Contains(err.Error(), "never-echo") || strings.Contains(err.Error(), fixture.root) {
				t.Fatal("preservation error leaked private input")
			}
			if scenario == "existing-output" {
				content, err := os.ReadFile(filepath.Join(out, "keep"))
				if err != nil || string(content) != "unrelated-owner-evidence" {
					t.Fatal("existing evidence overwritten", err)
				}
			} else if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatal("failed restoration published plaintext evidence")
			}
		})
	}
}

func TestPreservationCLIRequiresDocumentedExplicitPrivateInputs(t *testing.T) {
	fixture := newPreservationCLIFixture(t, false)
	for _, mode := range []string{"preservation-verify", "preservation-restore"} {
		base := fixture.args(mode, filepath.Join(fixture.root, "out"))
		for i := 1; i < len(base); i++ {
			args := append(append([]string{}, base[:i]...), base[i+1:]...)
			var output, diagnostics bytes.Buffer
			if err := run(t.Context(), args, &output, &diagnostics); err == nil || output.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatal("missing preservation argument accepted", mode, i)
			}
		}
		defaults := preservation.DefaultLimits()
		for _, extra := range []string{"--unknown=" + fixture.identity, "--identity=" + fixture.identity, "--source=/must-not-be-used", "--max-bytes=0", "--max-entries=0", "--max-bytes=" + strconv.FormatInt(defaults.MaxBytes+1, 10), "--max-entries=" + strconv.Itoa(defaults.MaxEntries+1), "--expected-sha256=do-not-echo", fixture.identity} {
			var output, diagnostics bytes.Buffer
			err := run(t.Context(), append(append([]string{}, base...), extra), &output, &diagnostics)
			if err == nil || output.Len() != 0 || diagnostics.Len() != 0 || strings.Contains(err.Error(), fixture.identity) || strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("unknown/unbounded/private argument accepted or leaked")
			}
		}
	}
	var output, diagnostics bytes.Buffer
	if err := run(t.Context(), append(fixture.args("preservation-verify", ""), "--out="+filepath.Join(fixture.root, "no-output")), &output, &diagnostics); err == nil {
		t.Fatal("verify accepted plaintext output option")
	}
}

type preservationReceiptFailureWriter struct{}

func (preservationReceiptFailureWriter) Write([]byte) (int, error) {
	return 0, errors.New("private output sink failure")
}

func TestPreservationCLIReceiptFailuresAndUntrustedBindingCannotEchoIdentity(t *testing.T) {
	fixture := newPreservationCLIFixture(t, false)
	var diagnostics bytes.Buffer
	if err := run(t.Context(), fixture.args("preservation-verify", ""), preservationReceiptFailureWriter{}, &diagnostics); err == nil || strings.Contains(err.Error(), "private output sink") || diagnostics.Len() != 0 {
		t.Fatal("receipt failure was ignored or leaked")
	}
	private := newPreservationCLIFixture(t, true)
	var output bytes.Buffer
	err := run(t.Context(), private.args("preservation-verify", ""), &output, &diagnostics)
	if err == nil || output.Len() != 0 || diagnostics.Len() != 0 || strings.Contains(err.Error(), private.identity) {
		t.Fatal("untrusted binding echoed the recovery identity")
	}
}
