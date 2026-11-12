package online

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func maildirTestRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return dir, root
}

func maildirTestWrite(t *testing.T, root, relative string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("synthetic MIME fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func maildirTestSeparator() string {
	if runtime.GOOS == "windows" {
		return "!2,"
	}
	return ":2,"
}

func TestMaildirScanIncludesFoldersCatchAllAndSkipsOperationalFiles(t *testing.T) {
	dir, root := maildirTestRoot(t)
	for _, relative := range []string{
		"example.test/alice/Maildir/new/inbox",
		"example.test/alice/Maildir/cur/read" + maildirTestSeparator() + "Sa",
		"example.test/alice/Maildir/.Projects.2026/cur/project" + maildirTestSeparator() + "SF",
		"example.test/bob/Maildir/.Drafts/new/draft",
		"example.test/__unregistered__/Maildir/new/catchall",
	} {
		maildirTestWrite(t, dir, relative)
	}
	for _, relative := range []string{
		"example.test/alice/Maildir/tmp/incomplete",
		"example.test/alice/Maildir/dovecot.index",
		"example.test/alice/Maildir/dovecot-uidlist",
		"example.test/alice/Maildir/new/.temporary",
		"example.test/alice/Maildir/.Projects.2026/tmp/incomplete",
		"example.test/alice/Maildir/.Projects.2026/dovecot.index.cache",
		"example.test/alice/Maildir/new/ignored-directory/inside",
		".ignored-root/nobody/Maildir/new/ignored",
	} {
		maildirTestWrite(t, dir, relative)
	}
	entries, err := ScanMaildir(t.Context(), root, DefaultMaildirLimits())
	if err != nil || len(entries) != 5 {
		t.Fatalf("expected five real MIME files, got %d: %v", len(entries), err)
	}
	identities := map[string]bool{}
	for _, entry := range entries {
		if entry.Bytes != int64(len("synthetic MIME fixture")) || entry.ModTime.IsZero() || strings.Contains(entry.Path, "\\") || strings.Contains(entry.Path, "/tmp/") {
			t.Fatal("invalid inventory entry")
		}
		identities[entry.Identity] = true
	}
	for _, name := range []string{"inbox", "read", "project", "draft", "catchall"} {
		if !identities[name] {
			t.Fatal("message absent from inventory", name)
		}
	}
}

func TestMaildirReferenceResolutionAllowsMovesButNeverCrossesMailbox(t *testing.T) {
	dir, root := maildirTestRoot(t)
	moved := "example.test/alice/Maildir/.Archive/cur/stable" + maildirTestSeparator() + "SF"
	maildirTestWrite(t, dir, moved)
	maildirTestWrite(t, dir, "example.test/bob/Maildir/new/stable")
	maildirTestWrite(t, dir, "other.test/alice/Maildir/new/stable")
	maildirTestWrite(t, dir, "example.test/__unregistered__/Maildir/new/stable")
	entries, err := ScanMaildir(t.Context(), root, DefaultMaildirLimits())
	if err != nil {
		t.Fatal(err)
	}
	index, err := NewMaildirIndex(entries)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := index.ResolveReference("example.test/alice/Maildir/new/stable")
	if err != nil || entry.Path != moved || entry.Mailbox != "example.test/alice/Maildir" || entry.Identity != "stable" {
		t.Fatal("could not resolve a move within the original mailbox", err)
	}
	if _, err := index.ResolveReference("example.test/nobody/Maildir/new/stable"); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("cross-mailbox filename was used as a fallback", err)
	}
	if _, err := index.ResolveReference("example.test/alice/Maildir/new/absent"); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("missing reference was not reported", err)
	}
	entries[0].Path = "caller mutation must not affect private index"
	if _, err := index.ResolveReference("example.test/alice/Maildir/new/stable"); err != nil {
		t.Fatal("index retained mutable caller slice", err)
	}
}

func TestMaildirReferenceRejectsAmbiguousIdentityEvenWithExactPath(t *testing.T) {
	dir, root := maildirTestRoot(t)
	original := "example.test/alice/Maildir/new/duplicate"
	maildirTestWrite(t, dir, original)
	maildirTestWrite(t, dir, "example.test/alice/Maildir/.Sent/cur/duplicate"+maildirTestSeparator()+"S")
	entries, err := ScanMaildir(t.Context(), root, DefaultMaildirLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveMaildirReference(original, entries); !errors.Is(err, ErrMaildirAmbiguous) {
		t.Fatal("ambiguous version was guessed from exact original path", err)
	}
}

func TestMaildirPortableFlagsAndUnsafeReferences(t *testing.T) {
	entry, err := parseMaildirPath("example.test/alice/Maildir/cur/stable!2,Sa")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveMaildirReference("example.test/alice/Maildir/new/stable:2,F", []MaildirEntry{entry}); err != nil || got.Identity != "stable" {
		t.Fatal("Unix and Windows flags did not retain the same identity", err)
	}
	for _, relative := range []string{
		"../example.test/alice/Maildir/new/stable", "/example.test/alice/Maildir/new/stable",
		"example.test/alice/Maildir/../new/stable", "example.test/alice/Maildir/new/../../secret",
		"example.test\\alice\\Maildir\\new\\stable", "example.test/alice/Maildir/tmp/stable",
		"example.test/alice/Maildir/.Drafts/other/stable", "example.test/alice/Maildir/.Drafts/sub/new/stable",
		"example.test/alice/Maildir/new/.hidden", "example.test/alice/Maildir/new/:2,S",
		"example.test/alice/Maildir/new/stable:2,../escape", "example.test/alice/Maildir/new/stable:2,S0",
		"example.test/alice/Maildir/new/colon:filename", "example.test/alice/Maildir/new/control\nvalue",
	} {
		if _, err := ResolveMaildirReference(relative, []MaildirEntry{entry}); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("unsafe reference accepted: %q: %v", relative, err)
		}
	}
	for _, tamper := range []func(*MaildirEntry){
		func(e *MaildirEntry) { e.Mailbox = "example.test/bob/Maildir" },
		func(e *MaildirEntry) { e.Identity = "different" },
		func(e *MaildirEntry) { e.Bytes = -1 },
	} {
		changed := entry
		tamper(&changed)
		if _, err := NewMaildirIndex([]MaildirEntry{changed}); !errors.Is(err, ErrUnsafePath) {
			t.Fatal("untrusted inventory metadata accepted", err)
		}
	}
}

func TestMaildirInventoryBoundsAndCancellation(t *testing.T) {
	dir, root := maildirTestRoot(t)
	maildirTestWrite(t, dir, "example.test/alice/Maildir/new/one")
	maildirTestWrite(t, dir, "example.test/alice/Maildir/new/two")
	for _, change := range []func(*MaildirLimits){
		func(l *MaildirLimits) { l.MaxEntries = 1; l.MaxFiles = 1 },
		func(l *MaildirLimits) { l.MaxFiles = 1 },
		func(l *MaildirLimits) { l.MaxPathBytes = 10 },
		func(l *MaildirLimits) { l.MaxEntries = 0 },
	} {
		limits := DefaultMaildirLimits()
		change(&limits)
		if _, err := ScanMaildir(t.Context(), root, limits); !errors.Is(err, ErrFileLimit) {
			t.Fatal("inventory bounds not enforced", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ScanMaildir(ctx, root, DefaultMaildirLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal("inventory ignored cancellation", err)
	}
}

func TestMaildirInventoryRejectsCandidateFileAndDirectoryLinks(t *testing.T) {
	for _, position := range []string{"file", "new-directory", "folder-directory", "mailbox-directory"} {
		t.Run(position, func(t *testing.T) {
			dir, root := maildirTestRoot(t)
			outside := t.TempDir()
			maildirTestWrite(t, outside, "synthetic-outside")
			name := filepath.Join(dir, "example.test", "alice", "Maildir", "new", "linked")
			target := filepath.Join(outside, "synthetic-outside")
			if position == "new-directory" {
				name = filepath.Join(dir, "example.test", "alice", "Maildir", "new")
				target = outside
			} else if position == "folder-directory" {
				name = filepath.Join(dir, "example.test", "alice", "Maildir", ".Linked")
				target = outside
			} else if position == "mailbox-directory" {
				name = filepath.Join(dir, "example.test", "alice")
				target = outside
			}
			if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, name); err != nil {
				t.Skipf("symbolic-link creation unavailable: %v", err)
			}
			if _, err := ScanMaildir(t.Context(), root, DefaultMaildirLimits()); !errors.Is(err, ErrUnsafePath) {
				t.Fatal("candidate link followed or silently omitted", err)
			}
		})
	}
}

func TestMaildirInventoryRejectsReplacementBeforeCopyWithRestoredTimestamp(t *testing.T) {
	dir, root := maildirTestRoot(t)
	relative := "example.test/alice/Maildir/new/stable"
	maildirTestWrite(t, dir, relative)
	entries, err := ScanMaildir(t.Context(), root, DefaultMaildirLimits())
	if err != nil || len(entries) != 1 || entries[0].Info == nil {
		t.Fatal("inventory did not pin file identity metadata", err)
	}
	index, err := NewMaildirIndex(entries)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := index.ResolveReference(relative)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.Rename(name, name+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, bytes.Repeat([]byte{'x'}, int(observed.Bytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, observed.ModTime, observed.ModTime); err != nil {
		t.Fatal(err)
	}
	_, out := maildirTestRoot(t)
	if _, err := CopyFiles(t.Context(), root, out, []FileMapping{{Source: relative, Destination: "snapshot.eml", ExpectedInfo: observed.Info, ExpectedBytes: &observed.Bytes}}, FileLimits{MaxBytes: 1 << 20, MaxEntries: 1}); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("same-size same-time replacement crossed the frozen inventory boundary", err)
	}
	if _, err := out.Lstat("snapshot.eml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement was left in snapshot output", err)
	}
}
