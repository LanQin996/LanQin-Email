package preservation

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

type fixture struct {
	key      *age.X25519Identity
	cipher   []byte
	manifest Manifest
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func sample(t *testing.T) fixture {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	items := []Item{
		{Name: "assets/0007.bin", Source: `/synthetic/deploy/.env`, Data: []byte("SYNTHETIC_TOKEN=fixture-only\n")},
		{Name: "spools/" + strings.Repeat("a", 64) + ".tar", Source: "synthetic-postfix-container-id", Data: []byte("opaque spool: never expand or execute")},
		{Name: "assets/0000.bin", Source: `C:\synthetic\empty`, Data: nil},
	}
	m, err := Write(context.Background(), &b, key.Recipient().String(), Binding{ConfigSHA256: strings.Repeat("b", 64), OperationID: "synthetic-operation-001"}, items, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{key: key, cipher: b.Bytes(), manifest: m}
}

func decrypt(t *testing.T, f fixture) []byte {
	t.Helper()
	r, err := age.Decrypt(bytes.NewReader(f.cipher), f.key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func encrypt(t *testing.T, f fixture, plain []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := age.Encrypt(&b, f.key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tarBytes(t *testing.T, metadata []byte, headers []*tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(canonicalHeader(manifestName, int64(len(metadata)))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(metadata); err != nil {
		t.Fatal(err)
	}
	for i, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func normalParts(t *testing.T, f fixture) ([]*tar.Header, [][]byte) {
	t.Helper()
	r := tar.NewReader(bytes.NewReader(decrypt(t, f)))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	var headers []*tar.Header
	var bodies [][]byte
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		headers = append(headers, h)
		bodies = append(bodies, b)
	}
	return headers, bodies
}

func TestRoundTripPreservesOrderAndOpaquePayloads(t *testing.T) {
	f := sample(t)
	if err := Verify(context.Background(), bytes.NewReader(f.cipher), f.key.String(), digest(f.cipher), f.manifest, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	m, err := Inspect(context.Background(), bytes.NewReader(f.cipher), f.key.String(), strings.ToUpper(digest(f.cipher)), DefaultLimits())
	if err != nil || !reflect.DeepEqual(m, f.manifest) {
		t.Fatalf("inspect: %v", err)
	}
	if m.Entries[0].Name != "assets/0007.bin" || m.Entries[2].Name != "assets/0000.bin" {
		t.Fatal("manifest reordered input")
	}
	out := filepath.Join(t.TempDir(), "new")
	m, err = Restore(context.Background(), bytes.NewReader(f.cipher), out, f.key.String(), digest(f.cipher), DefaultLimits())
	if err != nil || !reflect.DeepEqual(m, f.manifest) {
		t.Fatalf("restore: %v", err)
	}
	_, bodies := normalParts(t, f)
	for i, e := range f.manifest.Entries {
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(PayloadName(i))))
		if err != nil || !bytes.Equal(data, bodies[i]) || digest(data) != e.SHA256 {
			t.Fatalf("payload %d: %v", i, err)
		}
		info, err := os.Stat(filepath.Join(out, filepath.FromSlash(PayloadName(i))))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("payload type: %v", err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatal("payload not private")
		}
	}
	meta, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(f.manifest)
	if !bytes.Equal(meta, want) {
		t.Fatal("manifest mismatch")
	}
	if _, err := os.Stat(filepath.Join(out, "READY")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(out, "payloads")); err != nil || len(entries) != 3 {
		t.Fatalf("opaque payload directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "spools")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("spool unpacked")
	}
}

func TestRejectsCiphertextFaultsAndRedactsErrors(t *testing.T) {
	f := sample(t)
	other, _ := age.GenerateX25519Identity()
	tampered := append([]byte(nil), f.cipher...)
	tampered[len(tampered)-1] ^= 1
	tests := []struct {
		name     string
		cipher   []byte
		key, sha string
	}{
		{"wrong-key", f.cipher, other.String(), digest(f.cipher)},
		{"bad-key", f.cipher, "SECRET-invalid-key", digest(f.cipher)},
		{"bad-sha", f.cipher, f.key.String(), strings.Repeat("0", 64)},
		{"bad-sha-format", f.cipher, f.key.String(), "SECRET-invalid-sha"},
		{"tampered", tampered, f.key.String(), digest(tampered)},
		{"truncated", f.cipher[:len(f.cipher)-1], f.key.String(), digest(f.cipher[:len(f.cipher)-1])},
		{"trailing-cipher", append(append([]byte(nil), f.cipher...), 0), f.key.String(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sha == "" {
				tt.sha = digest(tt.cipher)
			}
			if _, err := Inspect(context.Background(), bytes.NewReader(tt.cipher), tt.key, tt.sha, DefaultLimits()); err == nil {
				t.Fatal("accepted bad ciphertext")
			} else {
				for _, secret := range []string{f.key.String(), f.manifest.Entries[0].Source, "SYNTHETIC_TOKEN", "SECRET"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("error leaked private data")
					}
				}
			}
			out := filepath.Join(t.TempDir(), "new")
			if _, err := Restore(context.Background(), bytes.NewReader(tt.cipher), out, tt.key, tt.sha, DefaultLimits()); err == nil {
				t.Fatal("restored bad ciphertext")
			}
			if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed restore did not remove own root")
			}
		})
	}
}

func TestManifestAndEveryPayloadAreVerified(t *testing.T) {
	f := sample(t)
	headers, bodies := normalParts(t, f)
	metadata, _ := json.Marshal(f.manifest)
	tests := map[string]func() []byte{
		"payload-digest": func() []byte {
			bs := append([][]byte(nil), bodies...)
			bs[1] = bytes.Repeat([]byte{'!'}, len(bodies[1]))
			return tarBytes(t, metadata, headers, bs)
		},
		"unknown-file": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[1] = canonicalHeader("unexpected", int64(len(bodies[1])))
			return tarBytes(t, metadata, h, bodies)
		},
		"duplicate-file": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[1] = canonicalHeader(headers[0].Name, int64(len(bodies[1])))
			return tarBytes(t, metadata, h, bodies)
		},
		"missing-file": func() []byte { return tarBytes(t, metadata, headers[:2], bodies[:2]) },
		"extra-file": func() []byte {
			return tarBytes(t, metadata, append(headers, canonicalHeader("assets/9999.bin", 0)), append(bodies, nil))
		},
		"duplicate-json": func() []byte {
			return tarBytes(t, bytes.Replace(metadata, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), headers, bodies)
		},
		"unknown-json": func() []byte {
			return tarBytes(t, append([]byte(`{"unknown":true,`), metadata[1:]...), headers, bodies)
		},
		"case-json": func() []byte {
			return tarBytes(t, bytes.Replace(metadata, []byte(`"scope"`), []byte(`"Scope"`), 1), headers, bodies)
		},
		"trailing-json": func() []byte {
			return tarBytes(t, append(append([]byte(nil), metadata...), []byte(`{}`)...), headers, bodies)
		},
		"wrong-scope": func() []byte {
			return tarBytes(t, bytes.Replace(metadata, []byte(scope), []byte("online-data"), 1), headers, bodies)
		},
		"trailing-plain":  func() []byte { return append(decrypt(t, f), 0) },
		"truncated-tar":   func() []byte { p := decrypt(t, f); return p[:len(p)-1] },
		"nonzero-padding": func() []byte { p := decrypt(t, f); p[512+len(metadata)] = 1; return p },
		"symlink": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[0] = &tar.Header{Name: headers[0].Name, Typeflag: tar.TypeSymlink, Linkname: "/synthetic/outside", Mode: 0o600, Format: tar.FormatUSTAR}
			bs := append([][]byte(nil), bodies...)
			bs[0] = nil
			return tarBytes(t, metadata, h, bs)
		},
		"hardlink": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[0] = &tar.Header{Name: headers[0].Name, Typeflag: tar.TypeLink, Linkname: "/synthetic/outside", Mode: 0o600, Format: tar.FormatUSTAR}
			bs := append([][]byte(nil), bodies...)
			bs[0] = nil
			return tarBytes(t, metadata, h, bs)
		},
		"directory": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[0] = &tar.Header{Name: headers[0].Name, Typeflag: tar.TypeDir, Mode: 0o700, Format: tar.FormatUSTAR}
			bs := append([][]byte(nil), bodies...)
			bs[0] = nil
			return tarBytes(t, metadata, h, bs)
		},
		"fifo": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			h[0] = &tar.Header{Name: headers[0].Name, Typeflag: tar.TypeFifo, Mode: 0o600, Format: tar.FormatUSTAR}
			bs := append([][]byte(nil), bodies...)
			bs[0] = nil
			return tarBytes(t, metadata, h, bs)
		},
		"pax-extension": func() []byte {
			h := append([]*tar.Header(nil), headers...)
			copy := *h[0]
			copy.Format = tar.FormatPAX
			copy.PAXRecords = map[string]string{"comment": "untrusted"}
			h[0] = &copy
			return tarBytes(t, metadata, h, bodies)
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			cipher := encrypt(t, f, change())
			if err := Verify(context.Background(), bytes.NewReader(cipher), f.key.String(), digest(cipher), f.manifest, DefaultLimits()); err == nil {
				t.Fatal("Verify accepted malformed archive")
			}
			if _, err := Inspect(context.Background(), bytes.NewReader(cipher), f.key.String(), digest(cipher), DefaultLimits()); err == nil {
				t.Fatal("Inspect accepted malformed archive")
			}
		})
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Binding.OperationID = "other" },
		func(m *Manifest) { m.Binding.ConfigSHA256 = strings.Repeat("c", 64) },
		func(m *Manifest) { m.Entries[0].Source = "different explanation" },
		func(m *Manifest) { m.Entries[0], m.Entries[1] = m.Entries[1], m.Entries[0] },
	} {
		want := f.manifest
		want.Entries = append([]Entry(nil), f.manifest.Entries...)
		change(&want)
		if err := Verify(context.Background(), bytes.NewReader(f.cipher), f.key.String(), digest(f.cipher), want, DefaultLimits()); err == nil {
			t.Fatal("Verify did not bind complete expected manifest")
		}
	}
}

func TestInputAndResourceLimits(t *testing.T) {
	f := sample(t)
	for _, limits := range []Limits{{}, {MaxBytes: -1, MaxEntries: 1}, {MaxBytes: hardMaxBytes + 1, MaxEntries: 1}, {MaxBytes: 1, MaxEntries: hardMaxEntries + 1}, {MaxBytes: 1, MaxEntries: 1}, {MaxBytes: hardMaxBytes, MaxEntries: 2}} {
		if _, err := Inspect(context.Background(), bytes.NewReader(f.cipher), f.key.String(), digest(f.cipher), limits); err == nil {
			t.Fatalf("accepted limits %+v", limits)
		}
	}
	for _, name := range []string{"../assets/0000.bin", "assets/000.bin", "assets/00000.bin", "assets/000a.bin", "assets/0000.bin/", "assets\\0000.bin", "spools/" + strings.Repeat("A", 64) + ".tar", "spools/../x.tar", "READY", "manifest.json"} {
		if _, err := Write(context.Background(), io.Discard, f.key.Recipient().String(), f.manifest.Binding, []Item{{Name: name}}, DefaultLimits()); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	for _, items := range [][]Item{nil, {{Name: "assets/0000.bin"}, {Name: "assets/0000.bin"}}, {{Name: "assets/0000.bin", Source: "bad\nsource"}}, {{Name: "assets/0000.bin", Source: strings.Repeat("x", 4097)}}, {{Name: "assets/0000.bin", Source: string([]byte{255})}}} {
		if _, err := Write(context.Background(), io.Discard, f.key.Recipient().String(), f.manifest.Binding, items, DefaultLimits()); err == nil {
			t.Fatal("accepted invalid items")
		}
	}
	if _, err := Write(context.Background(), io.Discard, f.key.Recipient().String(), f.manifest.Binding, []Item{{Name: "assets/0000.bin", Data: make([]byte, 2)}}, Limits{MaxBytes: 1, MaxEntries: 1}); err == nil {
		t.Fatal("Write exceeded payload budget")
	}
	// Exact limits remain usable and count payload bytes, not tar/age overhead.
	var total int64
	for _, e := range f.manifest.Entries {
		total += e.Bytes
	}
	if _, err := Inspect(context.Background(), bytes.NewReader(f.cipher), f.key.String(), digest(f.cipher), Limits{MaxBytes: total, MaxEntries: len(f.manifest.Entries)}); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationAndNoOverwrite(t *testing.T) {
	f := sample(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Write(ctx, io.Discard, f.key.Recipient().String(), f.manifest.Binding, []Item{{Name: "assets/0000.bin"}}, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation: %v", err)
	}
	if _, err := Inspect(ctx, bytes.NewReader(f.cipher), f.key.String(), digest(f.cipher), DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("inspect cancellation: %v", err)
	}
	base := t.TempDir()
	out := filepath.Join(base, "existing")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "sentinel"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(f.cipher), out, f.key.String(), digest(f.cipher), DefaultLimits()); err == nil {
		t.Fatal("overwrote existing output")
	}
	if b, err := os.ReadFile(filepath.Join(out, "sentinel")); err != nil || string(b) != "preserve me" {
		t.Fatal("existing directory modified")
	}
	newOut := filepath.Join(base, "cancelled")
	if _, err := Restore(ctx, bytes.NewReader(f.cipher), newOut, f.key.String(), digest(f.cipher), DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore cancellation: %v", err)
	}
	if _, err := os.Lstat(newOut); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled restore created output")
	}
}

func TestRestoreRejectsLinkedPaths(t *testing.T) {
	f := sample(t)
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink creation unavailable")
	}
	for _, out := range []string{link, filepath.Join(link, "new")} {
		if _, err := Restore(context.Background(), bytes.NewReader(f.cipher), out, f.key.String(), digest(f.cipher), DefaultLimits()); err == nil {
			t.Fatal("accepted linked output/parent")
		}
	}
	if children, err := os.ReadDir(target); err != nil || len(children) != 0 {
		t.Fatal("symlink target modified")
	}
}

func TestUnsafeUnixParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix owner/mode enforcement")
	}
	f := sample(t)
	parent := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(f.cipher), filepath.Join(parent, "new"), f.key.String(), digest(f.cipher), DefaultLimits()); err == nil {
		t.Fatal("accepted writable parent")
	}
}

type hookReader struct {
	r    io.Reader
	hook func()
}

func (r *hookReader) Read(p []byte) (int, error) {
	if r.hook != nil {
		hook := r.hook
		r.hook = nil
		hook()
	}
	return r.r.Read(p)
}

func TestMidStreamCancellationCleansOnlyOwnOutput(t *testing.T) {
	f := sample(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := filepath.Join(t.TempDir(), "new")
	r := &hookReader{r: bytes.NewReader(f.cipher), hook: cancel}
	if _, err := Restore(ctx, r, out, f.key.String(), digest(f.cipher), DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("midstream cancellation: %v", err)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled output remains")
	}
	ctx, cancel = context.WithCancel(context.Background())
	r = &hookReader{r: bytes.NewReader(f.cipher), hook: cancel}
	if _, err := Inspect(ctx, r, f.key.String(), digest(f.cipher), DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("midstream inspect cancellation: %v", err)
	}
}

func TestFailedRestoreDoesNotDeleteReplacementRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory replacement with pinned open handles")
	}
	f := sample(t)
	parent := t.TempDir()
	out, parked := filepath.Join(parent, "new"), filepath.Join(parent, "parked-original")
	r := &hookReader{r: bytes.NewReader(f.cipher), hook: func() {
		if err := os.Rename(out, parked); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(out, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "sentinel"), []byte("replacement must survive"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := Restore(context.Background(), r, out, f.key.String(), digest(f.cipher), DefaultLimits()); err == nil {
		t.Fatal("published replaced root")
	}
	if data, err := os.ReadFile(filepath.Join(out, "sentinel")); err != nil || string(data) != "replacement must survive" {
		t.Fatal("removed replacement output")
	}
	if _, err := os.Stat(filepath.Join(parked, "READY")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("READY published to displaced root")
	}
}

func TestEncryptedManifestPathsCannotEscape(t *testing.T) {
	f := sample(t)
	headers, bodies := normalParts(t, f)
	for _, name := range []string{"../outside", "/absolute", "assets/../../outside", "assets\\0000.bin", "spools/" + strings.Repeat("a", 64) + ".tar/child"} {
		t.Run(name, func(t *testing.T) {
			m := f.manifest
			m.Entries = append([]Entry(nil), m.Entries...)
			m.Entries[0].Name = name
			metadata, _ := json.Marshal(m)
			h := append([]*tar.Header(nil), headers...)
			copy := *h[0]
			copy.Name = name
			h[0] = &copy
			cipher := encrypt(t, f, tarBytes(t, metadata, h, bodies))
			out := filepath.Join(t.TempDir(), "new")
			if _, err := Restore(context.Background(), bytes.NewReader(cipher), out, f.key.String(), digest(cipher), DefaultLimits()); err == nil {
				t.Fatal("accepted escaping path")
			}
			if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed restore left output")
			}
		})
	}
	// Source is explicitly metadata, even when it looks like a dangerous path.
	m := f.manifest
	m.Entries = append([]Entry(nil), m.Entries...)
	m.Entries[0].Source = "../../must-never-be-created"
	metadata, _ := json.Marshal(m)
	cipher := encrypt(t, f, tarBytes(t, metadata, headers, bodies))
	parent := t.TempDir()
	out := filepath.Join(parent, "new")
	if _, err := Restore(context.Background(), bytes.NewReader(cipher), out, f.key.String(), digest(cipher), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 {
		t.Fatal("Source interpreted as path")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("SYNTHETIC_PRIVATE_SOURCE_AND_PAYLOAD")
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) {
	return 0, errors.New("SYNTHETIC_PRIVATE_SOURCE_AND_PAYLOAD")
}

func TestIOFailuresDoNotLeakPrivateDetails(t *testing.T) {
	f := sample(t)
	_, writeErr := Write(context.Background(), brokenWriter{}, f.key.Recipient().String(), f.manifest.Binding, []Item{{Name: "assets/0000.bin", Source: "synthetic-sensitive-source", Data: []byte("synthetic-sensitive-payload")}}, DefaultLimits())
	_, readErr := Inspect(context.Background(), brokenReader{}, f.key.String(), digest(f.cipher), DefaultLimits())
	for _, err := range []error{writeErr, readErr} {
		if err == nil || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") || strings.Contains(err.Error(), "synthetic-sensitive") {
			t.Fatalf("I/O failure was not redacted: %v", err)
		}
	}
}
