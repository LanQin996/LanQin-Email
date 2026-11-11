// Package handoff copies one explicitly authorized API-owned recovery tree into
// the installer's private namespace. It never changes ownership of the source.
package handoff

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/install"
)

var errInvalid = errors.New("authenticated recovery handoff could not be verified or safely published")

type observed struct {
	info os.FileInfo
	sha  string
}

// Copy requires a separately authenticated report receipt and explicit source
// UID. out must not exist. It atomically publishes a fully verified independent
// READY + snapshot container, without chowning or modifying API staging.
func Copy(ctx context.Context, source, out string, sourceUID int, reportSHA, driver string, limits backup.Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sourceUID < 0 || sourceUID > 1<<31-1 || !validDigest(reportSHA) || driver != "sqlite" && driver != "mysql" && driver != "postgres" || limits.MaxBytes < 1 || limits.MaxBytes > 1<<30 || limits.MaxEntries < 1 || limits.MaxEntries > 100000 {
		return errInvalid
	}
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return errInvalid
	}
	outAbs, err := filepath.Abs(out)
	if err != nil || overlaps(sourceAbs, outAbs) || filepath.Base(outAbs) == "." || filepath.Dir(outAbs) == outAbs {
		return errInvalid
	}
	if validateAncestors(filepath.Dir(sourceAbs), sourceUID, true) != nil || validateAncestors(filepath.Dir(outAbs), sourceUID, false) != nil {
		return errInvalid
	}
	sourceInfo, err := os.Lstat(sourceAbs)
	if err != nil || !sourceInfo.IsDir() || !sourcePrivate(sourceInfo, sourceUID) {
		return errInvalid
	}
	input, err := os.OpenRoot(sourceAbs)
	if err != nil {
		return errInvalid
	}
	defer input.Close()
	opened, err := input.Stat(".")
	if err != nil || !os.SameFile(sourceInfo, opened) || !sourcePrivate(opened, sourceUID) {
		return errInvalid
	}
	files, err := inventory(ctx, input, sourceUID, limits)
	if err != nil {
		return failure(ctx)
	}
	parentPath, base := filepath.Dir(outAbs), filepath.Base(outAbs)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil || !outputParent(parentInfo) {
		return errInvalid
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errInvalid
	}
	defer parent.Close()
	opened, err = parent.Stat(".")
	if err != nil || !os.SameFile(parentInfo, opened) {
		return errInvalid
	}
	if _, err := parent.Lstat(base); !errors.Is(err, os.ErrNotExist) {
		return errInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errInvalid
	}
	stageName := ".handoff-" + hex.EncodeToString(nonce[:])
	if err := parent.Mkdir(stageName, 0o700); err != nil {
		return errInvalid
	}
	created, err := parent.Lstat(stageName)
	if err != nil || !created.IsDir() || !outputPrivate(created) {
		return errInvalid
	}
	cleanupName, complete := stageName, false
	defer func() {
		if !complete {
			if current, err := parent.Lstat(cleanupName); err == nil && current.IsDir() && os.SameFile(created, current) {
				_ = parent.RemoveAll(cleanupName)
			}
		}
	}()
	output, err := parent.OpenRoot(stageName)
	if err != nil {
		return errInvalid
	}
	defer output.Close()
	stageInfo, err := output.Stat(".")
	if err != nil || !os.SameFile(created, stageInfo) {
		return errInvalid
	}
	// Walk again in lexical order so parents are created before their children.
	err = fs.WalkDir(input.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return errInvalid
		}
		if name == "." {
			return nil
		}
		before, ok := files[name]
		if !ok {
			return errInvalid
		}
		if before.info.IsDir() {
			return output.Mkdir(filepath.FromSlash(name), 0o700)
		}
		file, err := input.Open(filepath.FromSlash(name))
		if err != nil {
			return errInvalid
		}
		opened, err := file.Stat()
		if err != nil || !same(before.info, opened) || !sourcePrivate(opened, sourceUID) {
			file.Close()
			return errInvalid
		}
		target, err := output.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			file.Close()
			return errInvalid
		}
		hash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(target, hash), contextReader{ctx, io.LimitReader(file, opened.Size()+1)})
		after, statErr := file.Stat()
		syncErr, targetErr, sourceErr := target.Sync(), target.Close(), file.Close()
		if copyErr != nil || statErr != nil || syncErr != nil || targetErr != nil || sourceErr != nil || n != opened.Size() || !same(opened, after) {
			return errInvalid
		}
		before.sha = hex.EncodeToString(hash.Sum(nil))
		files[name] = before
		return nil
	})
	if err != nil {
		return failure(ctx)
	}
	// The copied report and every database/mail/attachment/DKIM payload must be
	// independently verified as installer-owned data before any publication.
	stagePath := filepath.Join(parentPath, stageName)
	if backup.VerifyStagedOnlineReport(ctx, stagePath, reportSHA, driver) != nil {
		return failure(ctx)
	}
	if driver == "sqlite" {
		workName := stageName + "-verify"
		if err := parent.Mkdir(workName, 0o700); err != nil {
			return errInvalid
		}
		workInfo, err := parent.Lstat(workName)
		if err != nil || !outputPrivate(workInfo) {
			return errInvalid
		}
		defer func() {
			if current, err := parent.Lstat(workName); err == nil && current.IsDir() && os.SameFile(workInfo, current) {
				_ = parent.RemoveAll(workName)
			}
		}()
		prepared, err := install.Prepare(ctx, stagePath, filepath.Join(parentPath, workName), "/data", "/var/mail/vhosts", limits, reportSHA)
		if err != nil {
			return failure(ctx)
		}
		if prepared.Close() != nil {
			return errInvalid
		}
	} else if _, err := install.VerifyNativeStaged(ctx, stagePath, driver, reportSHA, limits); err != nil {
		return failure(ctx)
	}
	final, err := inventory(ctx, input, sourceUID, limits)
	if err != nil || len(final) != len(files) {
		return failure(ctx)
	}
	for name, before := range files {
		after, ok := final[name]
		if !ok || !same(before.info, after.info) {
			return errInvalid
		}
		if before.info.IsDir() {
			continue
		}
		file, err := input.Open(filepath.FromSlash(name))
		if err != nil {
			return errInvalid
		}
		opened, err := file.Stat()
		if err != nil || !same(before.info, opened) {
			file.Close()
			return errInvalid
		}
		hash := sha256.New()
		n, copyErr := io.Copy(hash, contextReader{ctx, io.LimitReader(file, opened.Size()+1)})
		last, statErr := file.Stat()
		closeErr := file.Close()
		if copyErr != nil || statErr != nil || closeErr != nil || n != opened.Size() || !same(opened, last) || hex.EncodeToString(hash.Sum(nil)) != before.sha {
			return failure(ctx)
		}
	}
	current, err := os.Lstat(sourceAbs)
	if err != nil || !same(sourceInfo, current) {
		return errInvalid
	}
	current, err = parent.Lstat(stageName)
	if err != nil || !os.SameFile(created, current) || !outputPrivate(current) {
		return errInvalid
	}
	if err := syncTree(output); err != nil {
		return errInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(parent, stageName, base); err != nil {
		return errInvalid
	}
	cleanupName = base
	if err := syncDirectory(parent); err != nil {
		return errInvalid
	}
	complete = true
	return nil
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
func within(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && filepath.IsLocal(rel)
}
func same(a, b os.FileInfo) bool {
	return b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func failure(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errInvalid
}

func allowed(name string) bool {
	if !fs.ValidPath(name) || !utf8.ValidString(name) || len(name) > 4096 || strings.ContainsAny(name, "\\:\x00") || strings.Count(name, "/") > 66 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return false
	}
	if name == "READY" || name == "snapshot" {
		return true
	}
	if !strings.HasPrefix(name, "snapshot/") {
		return false
	}
	first := strings.SplitN(strings.TrimPrefix(name, "snapshot/"), "/", 2)[0]
	return first == "data" || first == "mail" || first == "dkim"
}

func inventory(ctx context.Context, root *os.Root, uid int, limits backup.Limits) (map[string]observed, error) {
	files := map[string]observed{}
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return errInvalid
		}
		if name == "." {
			return nil
		}
		if !allowed(name) || len(files) >= limits.MaxEntries {
			return errInvalid
		}
		info, err := root.Lstat(filepath.FromSlash(name))
		if err != nil || !sourcePrivate(info, uid) || !info.IsDir() && !info.Mode().IsRegular() {
			return errInvalid
		}
		if name == "READY" && (!info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 4096) || name == "snapshot" && !info.IsDir() {
			return errInvalid
		}
		if !info.IsDir() {
			if info.Size() < 0 || info.Size() > limits.MaxBytes-total {
				return errInvalid
			}
			total += info.Size()
		}
		handle, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return errInvalid
		}
		pinned, statErr := handle.Stat()
		closeErr := handle.Close()
		if statErr != nil || closeErr != nil || !same(info, pinned) || !sourcePrivate(pinned, uid) {
			return errInvalid
		}
		files[name] = observed{info: pinned}
		return nil
	})
	if err != nil {
		return nil, errInvalid
	}
	for _, name := range []string{"READY", "snapshot", "snapshot/data", "snapshot/mail", "snapshot/dkim"} {
		if _, ok := files[name]; !ok {
			return nil, errInvalid
		}
	}
	return files, nil
}

func validateAncestors(dir string, uid int, source bool) error {
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !trustedAncestor(info, uid, source) {
			return errInvalid
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func syncTree(root *os.Root) error {
	var dirs []string
	if err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return errInvalid
		}
		if d.IsDir() {
			dirs = append(dirs, name)
		}
		return nil
	}); err != nil {
		return errInvalid
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		child, err := root.OpenRoot(filepath.FromSlash(dirs[i]))
		if err != nil {
			return errInvalid
		}
		syncErr, closeErr := syncDirectory(child), child.Close()
		if syncErr != nil || closeErr != nil {
			return errInvalid
		}
	}
	return nil
}
