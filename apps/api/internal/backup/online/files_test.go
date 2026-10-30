package online

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fileCollectionFixture(t *testing.T) (string, string, *os.Root, *os.Root) {
	t.Helper()
	source, output := t.TempDir(), t.TempDir()
	for _, dir := range []string{filepath.Join(source, "nested"), filepath.Join(output, "nested")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenRoot(output)
	if err != nil {
		src.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close(); dst.Close() })
	return source, output, src, dst
}

func TestCopyFilesVerifiedMappingAndExactAttachmentSize(t *testing.T) {
	source, output, src, dst := fileCollectionFixture(t)
	data := []byte("synthetic\x00attachment\xff")
	if err := os.WriteFile(filepath.Join(source, "nested", "source.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	items, err := CopyFiles(t.Context(), src, dst, []FileMapping{{Source: "nested/source.bin", Destination: "nested/renamed.bin", ExpectedBytes: &size}, {Source: "empty", Destination: "empty-copy"}}, FileLimits{MaxBytes: size, MaxEntries: 2})
	if err != nil || len(items) != 2 {
		t.Fatal("bounded mapped copy failed", err)
	}
	hash := sha256.Sum256(data)
	if items[0].Bytes != size || items[0].SHA256 != hex.EncodeToString(hash[:]) || items[0].Source != "nested/source.bin" || items[0].Destination != "nested/renamed.bin" || items[1].Bytes != 0 {
		t.Fatal("copy receipt did not describe actual bytes", items)
	}
	got, err := os.ReadFile(filepath.Join(output, "nested", "renamed.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("output bytes changed", err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(output, "nested", "renamed.bin"))
		if info.Mode().Perm() != 0o600 {
			t.Fatal("staged file has broader Unix permissions")
		}
	}
}

func TestCopyFilesRejectsUnsafePathsAndTypes(t *testing.T) {
	source, output, src, dst := fileCollectionFixture(t)
	if err := os.WriteFile(filepath.Join(source, "nested", "source"), []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := FileLimits{MaxBytes: 100, MaxEntries: 2}
	for _, bad := range []string{"", ".", "../source", "/source", "nested/../source", "nested//source", "nested\\source", "nested/\x00source", "nested/\nsource", "C:/source", "C:source", "//host/source"} {
		for _, side := range []string{"source", "destination"} {
			t.Run(side+"_"+strings.ReplaceAll(bad, "/", "_"), func(t *testing.T) {
				item := FileMapping{Source: "nested/source", Destination: "nested/result"}
				if side == "source" {
					item.Source = bad
				} else {
					item.Destination = bad
				}
				if _, err := CopyFiles(t.Context(), src, dst, []FileMapping{item}, limits); !errors.Is(err, ErrUnsafePath) {
					t.Fatal("unsafe path accepted", err)
				}
			})
		}
	}
	if _, err := CopyFiles(t.Context(), src, dst, []FileMapping{{Source: "nested", Destination: "directory"}}, limits); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("directory treated as a regular file", err)
	}
	if _, err := CopyFiles(t.Context(), src, dst, []FileMapping{{Source: "nested/source", Destination: "duplicate"}, {Source: "nested/source", Destination: "duplicate"}}, limits); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("duplicate output mapping accepted", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 1 || entries[0].Name() != "nested" {
		t.Fatal("unsafe input created output", err)
	}
}

func TestCopyFilesLimitsMissingAndOutputPreservation(t *testing.T) {
	for _, scenario := range []string{"missing", "bytes", "entries", "expected-size", "negative-size", "existing", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			source, output, src, dst := fileCollectionFixture(t)
			if err := os.WriteFile(filepath.Join(source, "one"), []byte("1234"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "two"), []byte("5678"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(output, "keep"), []byte("owned by someone else"), 0o600); err != nil {
				t.Fatal(err)
			}
			items := []FileMapping{{Source: "one", Destination: "one-copy"}, {Source: "two", Destination: "two-copy"}}
			limits := FileLimits{MaxBytes: 8, MaxEntries: 2}
			want := ErrFileLimit
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "missing":
				items[1].Source = "missing-private-name"
				want = ErrSourceMissing
			case "bytes":
				limits.MaxBytes = 7
			case "entries":
				limits.MaxEntries = 1
			case "expected-size":
				size := int64(3)
				items[1].ExpectedBytes = &size
				want = ErrSourceChanged
			case "negative-size":
				size := int64(-1)
				items[1].ExpectedBytes = &size
			case "existing":
				items[1].Destination = "keep"
				want = ErrOutputExists
			case "cancelled":
				cancel()
				want = context.Canceled
			}
			_, err := CopyFiles(ctx, src, dst, items, limits)
			if !errors.Is(err, want) || strings.Contains(err.Error(), "missing-private-name") {
				t.Fatal("wrong or path-leaking collection failure", err)
			}
			entries, err := os.ReadDir(output)
			if err != nil || len(entries) != 2 {
				t.Fatal("failure left owned partial output", err)
			}
			keep, _ := os.ReadFile(filepath.Join(output, "keep"))
			if string(keep) != "owned by someone else" {
				t.Fatal("failure removed or overwrote unrelated output")
			}
		})
	}
}

func TestCopyFilesDetectsConcurrentMutationAndCancellation(t *testing.T) {
	for _, scenario := range []string{"same-size-restored-mtime", "append", "truncate", "cancel-during-copy", "rename"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "rename" && runtime.GOOS == "windows" {
				t.Skip("Windows disallows renaming an os.Open-pinned source; Unix exercises namespace race")
			}
			source, output, src, dst := fileCollectionFixture(t)
			name := filepath.Join(source, "sample")
			if err := os.WriteFile(name, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, err = copyFiles(ctx, src, dst, []FileMapping{{Source: "sample", Destination: "nested/copy"}}, FileLimits{MaxBytes: 100, MaxEntries: 1}, func() {
				switch scenario {
				case "same-size-restored-mtime":
					if err := os.WriteFile(name, []byte("mutating"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(name, before.ModTime(), before.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "append":
					if err := os.WriteFile(name, []byte("original appended"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "truncate":
					if err := os.Truncate(name, 1); err != nil {
						t.Fatal(err)
					}
				case "cancel-during-copy":
					cancel()
				case "rename":
					if err := os.Rename(name, name+".moved"); err != nil {
						t.Fatal(err)
					}
				}
			})
			want := ErrSourceChanged
			if scenario == "cancel-during-copy" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("source mutation/cancellation accepted", err)
			}
			entries, err := os.ReadDir(filepath.Join(output, "nested"))
			if err != nil || len(entries) != 0 {
				t.Fatal("invalid copy remained in staging output", err)
			}
		})
	}
}

func TestCopyFilesRejectsSourceAndOutputSymlinks(t *testing.T) {
	for _, scenario := range []string{"source-file", "source-parent", "source-outside", "output-parent"} {
		t.Run(scenario, func(t *testing.T) {
			source, output, src, dst := fileCollectionFixture(t)
			if err := os.WriteFile(filepath.Join(source, "nested", "sample"), []byte("sample"), 0o600); err != nil {
				t.Fatal(err)
			}
			item := FileMapping{Source: "nested/sample", Destination: "nested/copy"}
			var link, target string
			switch scenario {
			case "source-file":
				link, target, item.Source = filepath.Join(source, "link"), filepath.Join(source, "nested", "sample"), "link"
			case "source-parent":
				link, target, item.Source = filepath.Join(source, "alias"), filepath.Join(source, "nested"), "alias/sample"
			case "source-outside":
				outside := filepath.Join(t.TempDir(), "private")
				if err := os.WriteFile(outside, []byte("not in the source root"), 0o600); err != nil {
					t.Fatal(err)
				}
				link, target, item.Source = filepath.Join(source, "link"), outside, "link"
			case "output-parent":
				link, target, item.Destination = filepath.Join(output, "alias"), filepath.Join(output, "nested"), "alias/copy"
			}
			if err := os.Symlink(target, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skip("Windows symlink privilege / developer mode unavailable")
				}
				t.Fatal(err)
			}
			_, err := CopyFiles(t.Context(), src, dst, []FileMapping{item}, FileLimits{MaxBytes: 100, MaxEntries: 1})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatal("root-internal or external symlink accepted", err)
			}
			if _, err := os.Stat(filepath.Join(output, "nested", "copy")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("symlink input generated an output", err)
			}
		})
	}
}

func TestCopyFilesBindsExpectedInventoryIdentitySizeAndTime(t *testing.T) {
	for _, scenario := range []string{"unchanged", "mtime", "size", "replaced-same-size-and-time"} {
		t.Run(scenario, func(t *testing.T) {
			source, output, src, dst := fileCollectionFixture(t)
			name := filepath.Join(source, "sample")
			if err := os.WriteFile(name, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			pinned, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			// Capture identity from a handle, including eagerly loaded Windows ID.
			observed, err := pinned.Stat()
			closeErr := pinned.Close()
			if err != nil || closeErr != nil {
				t.Fatal("cannot capture source observation", err, closeErr)
			}
			switch scenario {
			case "mtime":
				stamp := observed.ModTime().Add(2 * time.Second)
				if err := os.Chtimes(name, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.WriteFile(name, []byte("different length"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "replaced-same-size-and-time":
				// No source descriptor is held during this replacement, so this
				// exercises the inventory->copy race on Windows too, without skip.
				if err := os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("replaced"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(name, observed.ModTime(), observed.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := CopyFiles(t.Context(), src, dst, []FileMapping{{Source: "sample", Destination: "nested/copy", ExpectedInfo: observed}}, FileLimits{MaxBytes: 100, MaxEntries: 1})
			if scenario == "unchanged" {
				if err != nil || len(got) != 1 {
					t.Fatal("unchanged observed source was rejected", err)
				}
			} else {
				if !errors.Is(err, ErrSourceChanged) {
					t.Fatal("source changed after inventory but before copy was accepted", err)
				}
				entries, err := os.ReadDir(filepath.Join(output, "nested"))
				if err != nil || len(entries) != 0 {
					t.Fatal("stale observation created an output", err)
				}
			}
		})
	}
}
