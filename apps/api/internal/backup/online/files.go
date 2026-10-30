package online

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrSourceMissing = errors.New("backup source file is missing")
	ErrSourceChanged = errors.New("backup source changed during collection")
	ErrUnsafePath    = errors.New("backup path or file type is unsafe")
	ErrFileLimit     = errors.New("backup file collection exceeds limits")
	ErrOutputExists  = errors.New("backup output already exists")
	ErrFileIO        = errors.New("backup file collection failed")
)

// FileMapping uses canonical slash-separated paths relative to the two roots.
// ExpectedBytes, when non-nil, is the exact frozen database attachment size.
// ExpectedInfo binds a Maildir inventory observation to the same file identity,
// size and modification time, rejecting replacements before copying starts.
type FileMapping struct {
	Source, Destination string
	ExpectedBytes       *int64
	ExpectedInfo        os.FileInfo
}

type FileLimits struct {
	MaxBytes   int64
	MaxEntries int
}

type CollectedFile struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
}

// CopyFiles copies a bounded batch into a caller-owned PRIVATE staging root.
// The coordinator must precreate the destination parents and must not publish
// that root until every collector succeeds. Both roots stay open for the call.
// Symlinks, including root-internal links in intermediate paths, are rejected.
// Missing/changed sources are distinguishable for bounded whole-snapshot retry.
//
// This is not a cross-file point-in-time snapshot or a substitute for immutable
// Maildir publication / draft reconstruction. It pins each regular source, hashes
// the copied bytes and independently rereads it, checking identity, size, mtime
// and path before accepting the result. Concurrent changes detected by these
// checks abort the entire batch, without deleting pre-existing output files.
func CopyFiles(ctx context.Context, source, output *os.Root, files []FileMapping, limits FileLimits) ([]CollectedFile, error) {
	return copyFiles(ctx, source, output, files, limits, nil)
}

// afterCopy is only used by same-package deterministic concurrency tests.
func copyFiles(ctx context.Context, source, output *os.Root, files []FileMapping, limits FileLimits, afterCopy func()) (_ []CollectedFile, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || output == nil {
		return nil, ErrUnsafePath
	}
	if limits.MaxBytes < 1 || limits.MaxEntries < 1 || len(files) > limits.MaxEntries {
		return nil, ErrFileLimit
	}
	seen := make(map[string]bool, len(files))
	for _, item := range files {
		if !validFilePath(item.Source) || !validFilePath(item.Destination) || seen[item.Destination] {
			return nil, ErrUnsafePath
		}
		if item.ExpectedBytes != nil && *item.ExpectedBytes < 0 {
			return nil, ErrFileLimit
		}
		if item.ExpectedInfo != nil && (!item.ExpectedInfo.Mode().IsRegular() || item.ExpectedInfo.Size() < 0) {
			return nil, ErrUnsafePath
		}
		seen[item.Destination] = true
	}
	type ownedFile struct {
		name string
		info os.FileInfo
	}
	written := make([]ownedFile, 0, len(files))
	defer func() {
		if result == nil {
			return
		}
		for _, item := range written {
			// output is a private caller-owned root. An independently replaced
			// path is not ours and must not be deleted during rollback.
			info, err := output.Lstat(item.name)
			if err == nil && info.Mode().IsRegular() && os.SameFile(item.info, info) {
				output.Remove(item.name)
			}
		}
	}()
	remaining := limits.MaxBytes
	results := make([]CollectedFile, 0, len(files))
	for _, item := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := checkFileParents(source, item.Source); err != nil {
			return nil, err
		}
		if err := checkFileParents(output, item.Destination); err != nil {
			if errors.Is(err, ErrSourceMissing) {
				return nil, ErrFileIO // The caller must precreate staging parents.
			}
			return nil, err
		}
		before, err := source.Lstat(item.Source)
		if err != nil {
			return nil, sourceFileError(err)
		}
		if !before.Mode().IsRegular() {
			return nil, ErrUnsafePath
		}
		if item.ExpectedInfo != nil && !sameStableFile(item.ExpectedInfo, before) {
			return nil, ErrSourceChanged
		}
		if before.Size() < 0 || before.Size() > remaining {
			return nil, ErrFileLimit
		}
		if item.ExpectedBytes != nil && before.Size() != *item.ExpectedBytes {
			return nil, ErrSourceChanged
		}
		in, err := source.Open(item.Source)
		if err != nil {
			return nil, sourceFileError(err)
		}
		collected, created, err := copyPinnedFile(ctx, source, output, in, before, item, afterCopy)
		in.Close()
		if created != nil {
			written = append(written, ownedFile{item.Destination, created})
		}
		if err != nil {
			return nil, err
		}
		remaining -= collected.Bytes
		results = append(results, collected)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func copyPinnedFile(ctx context.Context, source, output *os.Root, in *os.File, before os.FileInfo, item FileMapping, afterCopy func()) (_ CollectedFile, created os.FileInfo, result error) {
	var empty CollectedFile
	opened, err := in.Stat()
	if err != nil || !sameStableFile(before, opened) || (item.ExpectedInfo != nil && !sameStableFile(item.ExpectedInfo, opened)) {
		return empty, nil, ErrSourceChanged
	}
	out, err := output.OpenFile(item.Destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return empty, nil, ErrOutputExists
		}
		return empty, nil, ErrFileIO
	}
	created, err = out.Stat()
	if err != nil {
		out.Close()
		// O_EXCL just created this name in a caller-owned private root; a
		// descriptor-stat failure must not leave its empty staging file.
		output.Remove(item.Destination)
		return empty, nil, ErrFileIO
	}
	defer func() {
		if err := out.Close(); err != nil && result == nil {
			result = ErrFileIO
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), fileContextReader{ctx, io.NewSectionReader(in, 0, opened.Size())})
	if err != nil {
		if ctx.Err() != nil {
			return empty, created, ctx.Err()
		}
		return empty, created, ErrFileIO
	}
	if n != opened.Size() {
		return empty, created, ErrSourceChanged
	}
	if afterCopy != nil {
		afterCopy()
	}
	if err := ctx.Err(); err != nil {
		return empty, created, err
	}
	// Re-read through the same pinned descriptor. Size/mtime alone cannot detect
	// a same-size rewrite whose original timestamp has been restored.
	verify := sha256.New()
	n, err = io.Copy(verify, fileContextReader{ctx, io.NewSectionReader(in, 0, opened.Size())})
	if err != nil {
		if ctx.Err() != nil {
			return empty, created, ctx.Err()
		}
		return empty, created, ErrFileIO
	}
	if n != opened.Size() || !bytes.Equal(hash.Sum(nil), verify.Sum(nil)) {
		return empty, created, ErrSourceChanged
	}
	after, err := in.Stat()
	if err != nil || !sameStableFile(opened, after) || (item.ExpectedInfo != nil && !sameStableFile(item.ExpectedInfo, after)) {
		return empty, created, ErrSourceChanged
	}
	if err := checkFileParents(source, item.Source); err != nil {
		return empty, created, ErrSourceChanged
	}
	pathInfo, err := source.Lstat(item.Source)
	if err != nil || !sameStableFile(opened, pathInfo) {
		return empty, created, ErrSourceChanged
	}
	if err := checkFileParents(output, item.Destination); err != nil {
		return empty, created, ErrFileIO
	}
	pathInfo, err = output.Lstat(item.Destination)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(created, pathInfo) || pathInfo.Size() != opened.Size() {
		return empty, created, ErrFileIO
	}
	if err := out.Sync(); err != nil {
		return empty, created, ErrFileIO
	}
	if err := ctx.Err(); err != nil {
		return empty, created, err
	}
	return CollectedFile{Source: item.Source, Destination: item.Destination, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: opened.Size()}, created, nil
}

func validFilePath(name string) bool {
	if len(name) >= 2 && name[1] == ':' && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) {
		return false // Reject Windows drive forms even when collecting on Unix.
	}
	return name != "" && name != "." && len(name) <= 4096 && strings.Count(name, "/") <= 64 &&
		utf8.ValidString(name) && !strings.ContainsAny(name, "\\\x00") && strings.IndexFunc(name, unicode.IsControl) < 0 &&
		fs.ValidPath(name) && path.Clean(name) == name && filepath.IsLocal(filepath.FromSlash(name))
}

func checkFileParents(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		info, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil {
			return sourceFileError(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	}
	return nil
}

func sameStableFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && before.Mode().IsRegular() && after.Mode().IsRegular() &&
		os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func sourceFileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrSourceMissing
	}
	return ErrFileIO
}

type fileContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r fileContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
