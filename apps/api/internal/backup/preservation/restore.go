package preservation

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// Restore only publishes a new private evidence directory. Payloads are opaque
// 0600 blobs; READY is written last after full authentication and trusted SHA.
// It does not expand queue tar files or apply any environment/deployment assets.
func Restore(ctx context.Context, src io.Reader, out, identity, expectedCipherSHA256 string, limits Limits) (Manifest, error) {
	if !limits.valid() {
		return Manifest{}, errInvalid
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	parent, base, err := openParent(out)
	if err != nil {
		return Manifest{}, errOutput
	}
	defer parent.Close()
	if err := parent.Mkdir(base, 0o700); err != nil {
		return Manifest{}, errOutput
	}
	created, err := parent.Lstat(base)
	if err != nil || !created.IsDir() || !privateInfo(created) {
		return Manifest{}, errOutput
	}
	// Only delete the new directory if its identity still matches our creation.
	// Existing paths and replacement/symlink targets are never removed.
	complete := false
	defer func() {
		if !complete {
			if current, err := parent.Lstat(base); err == nil && current.IsDir() && os.SameFile(created, current) {
				_ = parent.RemoveAll(base)
			}
		}
	}()
	root, err := parent.OpenRoot(base)
	if err != nil {
		return Manifest{}, errOutput
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(created, opened) {
		return Manifest{}, errOutput
	}
	if err := root.Mkdir("payloads", 0o700); err != nil {
		return Manifest{}, errOutput
	}
	sink := func(index int, _ Entry) (io.WriteCloser, error) {
		file, err := root.OpenFile(filepath.FromSlash(PayloadName(index)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, errOutput
		}
		return syncedFile{file}, nil
	}
	m, err := inspect(ctx, src, identity, expectedCipherSHA256, nil, limits, sink)
	if err != nil {
		return Manifest{}, err
	}
	metadata, err := json.Marshal(m)
	if err != nil || publishFile(root, manifestName, metadata) != nil {
		return Manifest{}, errOutput
	}
	payloads, err := root.OpenRoot("payloads")
	if err != nil {
		return Manifest{}, errOutput
	}
	syncErr := syncRoot(payloads)
	closeErr := payloads.Close()
	if syncErr != nil || closeErr != nil || syncRoot(root) != nil {
		return Manifest{}, errOutput
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	current, err := parent.Lstat(base)
	if err != nil || !current.IsDir() || !os.SameFile(created, current) || !privateInfo(current) {
		return Manifest{}, errOutput
	}
	if err := publishFile(root, "READY", []byte("Authenticated isolated preservation evidence only; no queue replay or deployment activation.\n")); err != nil {
		return Manifest{}, errOutput
	}
	if syncRoot(root) != nil || syncRoot(parent) != nil {
		return Manifest{}, errOutput
	}
	complete = true
	return m, nil
}

type syncedFile struct{ *os.File }

func (f syncedFile) Close() error {
	syncErr := f.Sync()
	closeErr := f.File.Close()
	if syncErr != nil || closeErr != nil {
		return errOutput
	}
	return nil
}

func publishFile(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errOutput
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errOutput
	}
	return nil
}

// Pin an existing safe parent, rejecting every symlink component before mkdir.
// Unix ancestors may be root-owned sticky directories (e.g. /tmp); the immediate
// parent must be owned by the effective UID and not writable by other users.
func openParent(out string) (*os.Root, string, error) {
	abs, err := filepath.Abs(out)
	if err != nil || filepath.Clean(out) == "." {
		return nil, "", errOutput
	}
	base, dir := filepath.Base(abs), filepath.Dir(abs)
	if base == "." || base == string(filepath.Separator) || base == filepath.VolumeName(abs) {
		return nil, "", errOutput
	}
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !safeAncestor(info) {
			return nil, "", errOutput
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	info, err := os.Lstat(dir)
	if err != nil || !safeParent(info) {
		return nil, "", errOutput
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", errOutput
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, "", errOutput
	}
	return root, base, nil
}
