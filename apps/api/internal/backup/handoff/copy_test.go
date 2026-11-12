package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/nativedb"
)

func nativeFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "api-ready")
	for _, name := range []string{"snapshot/data", "snapshot/mail", "snapshot/dkim"} {
		if err := os.MkdirAll(filepath.Join(source, filepath.FromSlash(name)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var native bytes.Buffer
	w, err := nativedb.NewWriter(&native, nativedb.Header{Version: 1, Driver: "postgres", Family: "postgres:16", SchemaVersion: 12, Tables: []nativedb.Table{{Name: "synthetic", Columns: []nativedb.Column{{Name: "value", Type: "text"}}}}}, nativedb.DefaultLimits())
	if err != nil || w.Close() != nil {
		t.Fatal("native fixture", err)
	}
	files := []map[string]any{}
	for _, item := range []struct {
		name, kind string
		data       []byte
	}{{"data/lanqin-native.jsonl", "database", native.Bytes()}, {"mail/synthetic.eml", "message", []byte("Subject: Synthetic\r\n\r\nData\r\n")}, {"dkim/synthetic.key", "dkim", []byte("not-a-real-key")}} {
		if err := os.WriteFile(filepath.Join(source, "snapshot", filepath.FromSlash(item.name)), item.data, 0o600); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(item.data)
		entry := map[string]any{"kind": item.kind, "destination": item.name, "bytes": len(item.data), "sha256": hex.EncodeToString(h[:])}
		if item.kind == "message" {
			entry["messageId"] = "synthetic-message"
		}
		files = append(files, entry)
	}
	report := map[string]any{"version": 2, "scope": "postgres-reference-closure", "complete": true, "summary": map[string]any{}, "sources": map[string]any{"data": "/old/data", "mail": "/old/mail"}, "files": files}
	data, _ := json.Marshal(report)
	h := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(source, "snapshot/data/backup-online.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "READY"), []byte("synthetic verified recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return source, hex.EncodeToString(h[:])
}

func sourceUID() int {
	if runtime.GOOS == "windows" {
		return 0
	}
	return os.Geteuid()
}
func testLimits() backup.Limits { return backup.Limits{MaxBytes: 1 << 30, MaxEntries: 100000} }

func TestCopyNativeIndependentPrivateTree(t *testing.T) {
	source, report := nativeFixture(t)
	if _, err := install.VerifyNativeStaged(t.Context(), source, "postgres", report, testLimits()); err != nil {
		t.Fatal("invalid native test fixture", err)
	}
	out := filepath.Join(t.TempDir(), "installer-ready")
	if err := Copy(t.Context(), source, out, sourceUID(), report, "postgres", testLimits()); err != nil {
		t.Fatal(err)
	}
	if err := backup.VerifyStagedOnlineReport(t.Context(), out, report, "postgres"); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		if rel == "." {
			return nil
		}
		a, _ := os.Lstat(path)
		b, e := os.Lstat(filepath.Join(out, rel))
		if e != nil || os.SameFile(a, b) {
			t.Fatal("not independent", rel, e)
		}
		if !d.IsDir() {
			x, _ := os.ReadFile(path)
			y, _ := os.ReadFile(filepath.Join(out, rel))
			if !bytes.Equal(x, y) {
				t.Fatal("changed payload", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), source, out, sourceUID(), report, "postgres", testLimits()); err == nil {
		t.Fatal("existing output accepted")
	}
}

func TestHandoffColonExceptionIsLimitedToMaildirFlags(t *testing.T) {
	for _, name := range []string{"snapshot/mail/example.test/user/Maildir/cur/message:2,S", "snapshot/mail/example.test/user/Maildir/.Sent/cur/message:2,RS"} {
		if allowed(name) != (runtime.GOOS != "windows") {
			t.Fatal("Maildir flag handoff rejected")
		}
	}
	for _, name := range []string{"snapshot/data/a:stream", "snapshot/dkim/a:stream", "snapshot/mail/a:stream", "snapshot/mail/example.test/user/Maildir/cur/a:2,S:stream", "snapshot/mail/example.test/user/Maildir/cur/../a:2,S", "snapshot/mail/example.test/user/Maildir/tmp/a:2,S"} {
		if allowed(name) {
			t.Fatal("unsafe handoff path accepted", name)
		}
	}
}

func TestCopyLinuxMaildirFlagPayloadPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colon filenames require the Linux filesystem")
	}
	source, _ := nativeFixture(t)
	name := "mail/example.test/user/Maildir/cur/synthetic:2,RS"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(source, "snapshot", name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(source, "snapshot/mail/synthetic.eml"), filepath.Join(source, "snapshot", name)); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(source, "snapshot/data/backup-online.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("mail/synthetic.eml"), []byte(name))
	if err := os.WriteFile(reportPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	out := filepath.Join(t.TempDir(), "copied")
	if err := Copy(t.Context(), source, out, sourceUID(), hex.EncodeToString(digest[:]), "postgres", testLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "snapshot", name)); err != nil {
		t.Fatal(err)
	}
}

func TestCopyRejectsChangedOrUnknownSource(t *testing.T) {
	for _, damage := range []string{"report", "payload", "extra", "missing", "bad-driver", "budget", "entries", "canceled", "uid"} {
		t.Run(damage, func(t *testing.T) {
			source, report := nativeFixture(t)
			parent := t.TempDir()
			out := filepath.Join(parent, "new")
			driver := "postgres"
			limits := testLimits()
			uid := sourceUID()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch damage {
			case "report":
				report = strings.Repeat("0", 64)
			case "payload":
				os.WriteFile(filepath.Join(source, "snapshot/mail/synthetic.eml"), []byte("tampered"), 0o600)
			case "extra":
				os.WriteFile(filepath.Join(source, "snapshot/data/extra"), []byte("unknown"), 0o600)
			case "missing":
				os.Remove(filepath.Join(source, "snapshot/dkim/synthetic.key"))
			case "bad-driver":
				driver = "mysql"
			case "budget":
				limits.MaxBytes = 1
			case "entries":
				limits.MaxEntries = 1
			case "canceled":
				cancel()
			case "uid":
				if runtime.GOOS == "windows" {
					t.Skip("Unix explicit UID enforcement")
				}
				uid++
			}
			if err := Copy(ctx, source, out, uid, report, driver, limits); err == nil {
				t.Fatal("damaged handoff accepted")
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed handoff retained output/helper dirs: %v %+v", err, entries)
			}
		})
	}
}

func TestCopyRejectsLinksAndOverlap(t *testing.T) {
	source, report := nativeFixture(t)
	if err := Copy(t.Context(), source, filepath.Join(source, "nested"), sourceUID(), report, "postgres", testLimits()); err == nil {
		t.Fatal("overlap accepted")
	}
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			source, report := nativeFixture(t)
			link := filepath.Join(source, "snapshot/data/link")
			target := filepath.Join(source, "snapshot/mail/synthetic.eml")
			var err error
			if kind == "symlink" {
				err = os.Symlink(target, link)
			} else {
				if runtime.GOOS == "windows" {
					t.Skip("Unix nlink enforcement")
				}
				err = os.Link(target, link)
			}
			if err != nil {
				t.Skip("link unavailable")
			}
			if err := Copy(t.Context(), source, filepath.Join(t.TempDir(), "new"), sourceUID(), report, "postgres", testLimits()); err == nil {
				t.Fatal("link accepted")
			}
		})
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	os.Mkdir(target, 0o700)
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("parent symlink unavailable")
	}
	if err := Copy(t.Context(), source, filepath.Join(link, "new"), sourceUID(), report, "postgres", testLimits()); err == nil {
		t.Fatal("linked output parent accepted")
	}
}

func TestCopyRootConsumesOnlyExplicitAPIUID(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("requires actual Unix root chown fixture")
	}
	source, report := nativeFixture(t)
	const apiUID = 1001
	if err := filepath.WalkDir(source, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, apiUID, apiUID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Dir(source), apiUID, apiUID); err != nil {
		t.Fatal(err)
	}
	// testing.TempDir creates its numbered child using 0777 subject to umask.
	// Once that synthetic ancestor is owned by APIUID it must itself satisfy
	// the explicit private API-ancestor contract, not rely on root ownership.
	if err := os.Chmod(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateAncestors(filepath.Dir(source), apiUID, true); err != nil {
		t.Fatal("synthetic API ancestor is not private", err)
	}
	if _, err := inventory(t.Context(), mustOpenSource(t, source), apiUID, testLimits()); err != nil {
		t.Fatal("synthetic API source inventory is invalid", err)
	}
	out := filepath.Join(t.TempDir(), "root-ready")
	if err := Copy(t.Context(), source, out, apiUID, report, "postgres", testLimits()); err != nil {
		t.Fatal("cross-UID handoff", err)
	}
	if err := filepath.WalkDir(out, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !sourcePrivate(info, 0) {
			t.Fatal("copy not root-private", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), source, filepath.Join(t.TempDir(), "wrong"), apiUID+1, report, "postgres", testLimits()); err == nil {
		t.Fatal("wrong source UID accepted")
	}
	info, _ := os.Lstat(source)
	if !sourcePrivate(info, apiUID) {
		t.Fatal("source ownership changed")
	}
}

func mustOpenSource(t *testing.T, source string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestCopyRejectsUnsafeModesAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	source, report := nativeFixture(t)
	path := filepath.Join(source, "snapshot/mail/synthetic.eml")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), source, filepath.Join(t.TempDir(), "new"), sourceUID(), report, "postgres", testLimits()); err == nil {
		t.Fatal("nonprivate file accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Copy(ctx, source, filepath.Join(t.TempDir(), "new"), sourceUID(), report, "postgres", testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

type replacementContext struct {
	context.Context
	parent   string
	replaced string
	t        *testing.T
}

func (ctx *replacementContext) Err() error {
	if ctx.replaced != "" {
		return context.Canceled
	}
	entries, err := os.ReadDir(ctx.parent)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".handoff-") {
			continue
		}
		name := filepath.Join(ctx.parent, entry.Name())
		if err := os.Rename(name, name+"-parked"); err != nil {
			ctx.t.Fatal(err)
		}
		if err := os.Mkdir(name, 0o700); err != nil {
			ctx.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(name, "sentinel"), []byte("replacement"), 0o600); err != nil {
			ctx.t.Fatal(err)
		}
		ctx.replaced = name
		return context.Canceled
	}
	return nil
}

func TestCopyFailureNeverDeletesReplacedStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory rename with pinned handles")
	}
	source, report := nativeFixture(t)
	parent := t.TempDir()
	ctx := &replacementContext{Context: t.Context(), parent: parent, t: t}
	if err := Copy(ctx, source, filepath.Join(parent, "new"), sourceUID(), report, "postgres", testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled replacement: %v", err)
	}
	if ctx.replaced == "" {
		t.Fatal("did not exercise replacement")
	}
	if b, err := os.ReadFile(filepath.Join(ctx.replaced, "sentinel")); err != nil || string(b) != "replacement" {
		t.Fatal("replacement root deleted")
	}
	if _, err := os.Lstat(filepath.Join(parent, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled tree published")
	}
}
