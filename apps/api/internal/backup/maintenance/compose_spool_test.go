package maintenance

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func emptyPostfixTar(t *testing.T) []byte {
	t.Helper()
	return postfixTarFixture(t, "", nil)
}

func postfixTarFixture(t *testing.T, skip string, extra []*tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, name := range []string{"postfix", "postfix/active", "postfix/deferred", "postfix/incoming", "postfix/hold", "postfix/maildrop", "postfix/corrupt", "postfix/bounce", "postfix/defer", "postfix/trace", "postfix/flush", "postfix/saved", "postfix/etc", "postfix/pid", "postfix/private", "postfix/public", "postfix/dev", "postfix/lib"} {
		if name == skip {
			continue
		}
		if err := writer.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	for _, header := range extra {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte{'x'}, int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestPostfixQueueArchiveRefusesAnyQueuedFileWithoutExtracting(t *testing.T) {
	for _, queue := range []string{"active", "deferred", "incoming", "hold", "maildrop", "corrupt", "bounce", "defer", "trace", "flush", "saved"} {
		for _, kind := range []byte{tar.TypeReg, tar.TypeSymlink, tar.TypeLink, tar.TypeFifo, tar.TypeChar} {
			t.Run(queue+"/"+fmt.Sprint(kind), func(t *testing.T) {
				header := &tar.Header{Name: "postfix/" + queue + "/synthetic-private-message", Typeflag: kind, Mode: 0o600}
				if kind == tar.TypeSymlink || kind == tar.TypeLink {
					header.Linkname = "/private/never-extract"
				}
				data := postfixTarFixture(t, "", []*tar.Header{header})
				if err := validateEmptyPostfixArchive(t.Context(), data); !errors.Is(err, ErrPostfixQueue) || strings.Contains(err.Error(), "synthetic-private-message") {
					t.Fatal("queued entry accepted or name leaked")
				}
			})
		}
	}
	positive := postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/deferred/A/", Typeflag: tar.TypeDir, Mode: 0o700}, {Name: "postfix/etc/services", Typeflag: tar.TypeReg, Mode: 0o600, Size: 4}})
	if err := validateEmptyPostfixArchive(t.Context(), positive); err != nil {
		t.Fatal("empty queues with ordinary spool metadata rejected", err)
	}
}

func TestPostfixQueueArchiveMalformedUnknownAndBudgetFailClosed(t *testing.T) {
	valid := emptyPostfixTar(t)
	badChecksum := append([]byte{}, valid...)
	badChecksum[0] ^= 1
	for _, data := range [][]byte{
		postfixTarFixture(t, "postfix/hold", nil),
		postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/unknown-queue/", Typeflag: tar.TypeDir, Mode: 0o700}}),
		postfixTarFixture(t, "", []*tar.Header{{Name: "../escape/", Typeflag: tar.TypeDir, Mode: 0o700}}),
		postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/deferred/../escape/", Typeflag: tar.TypeDir, Mode: 0o700}}),
		postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/active/", Typeflag: tar.TypeDir, Mode: 0o700}}),
		valid[:len(valid)-512], badChecksum, append(append([]byte{}, valid...), bytes.Repeat([]byte{'x'}, 512)...),
		append(append([]byte{}, valid...), valid...), make([]byte, maxPostfixArchiveBytes+512),
	} {
		if err := validateEmptyPostfixArchive(t.Context(), data); err == nil {
			t.Fatal("malformed/ambiguous/oversized queue proof accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateEmptyPostfixArchive(ctx, valid); err == nil {
		t.Fatal("canceled proof accepted")
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "postfix/", Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "postfix/deferred/", Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPostfixArchiveEntries; i++ {
		if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("postfix/deferred/%06d/", i), Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateEmptyPostfixArchive(t.Context(), buffer.Bytes()); err == nil {
		t.Fatal("unbounded archive-entry count accepted")
	}
}

func TestPostfixQueueArchiveAllowsOnlyExactEmptyRestartMarker(t *testing.T) {
	valid := postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/restart", Typeflag: tar.TypeReg, Mode: 0o644}})
	for _, empty := range []bool{true, false} {
		if err := validatePostfixArchive(t.Context(), valid, empty); err != nil {
			t.Fatal("standard empty stopped-spool restart marker rejected", err)
		}
		for _, header := range []*tar.Header{
			{Name: "postfix/restart", Typeflag: tar.TypeReg, Size: 1},
			{Name: "postfix/restart", Typeflag: tar.TypeSymlink, Linkname: "active/private-message"},
			{Name: "postfix/restart", Typeflag: tar.TypeLink, Linkname: "postfix/active/private-message"},
			{Name: "postfix/restart", Typeflag: tar.TypeFifo},
			{Name: "postfix/restart/", Typeflag: tar.TypeDir},
			{Name: "postfix/restart/child", Typeflag: tar.TypeReg},
			{Name: "postfix/arbitrary-root-file", Typeflag: tar.TypeReg},
		} {
			if err := validatePostfixArchive(t.Context(), postfixTarFixture(t, "", []*tar.Header{header}), empty); err == nil {
				t.Fatal("restart-marker compatibility accepted a nonempty, linked, or unrelated entry")
			}
		}
	}
}

func TestComposeQueueProofRequiresStoppedVerifiedAllPostfixContainers(t *testing.T) {
	for _, mode := range []string{"all-in-one", "stack"} {
		t.Run(mode, func(t *testing.T) {
			cfg, controller, fake := newFakeComposeDocker(t, mode)
			if err := controller.Preflight(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := controller.EnsureEmptyPostfixQueues(t.Context(), cfg); err == nil {
				t.Fatal("read running spool as an empty-queue proof")
			}
			if err := controller.Stop(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := controller.EnsureEmptyPostfixQueues(t.Context(), cfg); err != nil {
				t.Fatal("empty stopped spool rejected", err)
			}
			var id string
			for _, container := range fake.containers {
				if container.Config.Labels["com.docker.compose.service"] == postfixService(cfg) {
					id = container.ID
				}
			}
			fake.spoolArchives = map[string][]byte{id: postfixTarFixture(t, "", []*tar.Header{{Name: "postfix/deferred/synthetic-mail", Typeflag: tar.TypeReg, Mode: 0o600, Size: 10}})}
			if err := controller.EnsureEmptyPostfixQueues(t.Context(), cfg); err == nil {
				t.Fatal("pending SMTP queue accepted")
			}
			before := len(fake.calls)
			if err := controller.Recreate(t.Context(), cfg); err == nil {
				t.Fatal("force-recreate could discard pending SMTP queue")
			}
			for _, call := range fake.calls[before:] {
				if call[0] == "compose" && call[len(composePrefix(cfg))] == "up" {
					t.Fatal("nonempty queue reached actual container replacement")
				}
			}
			if err := controller.StartOriginal(t.Context(), cfg); err != nil {
				t.Fatal("precommit refusal could not preserve/start original queued-mail container", err)
			}
		})
	}
}
