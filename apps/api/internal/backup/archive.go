// Package backup implements prepared data-snapshot formats, not live-service
// orchestration or in-place disaster recovery. Never point it at running services.
package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	_ "modernc.org/sqlite"
)

const (
	manifestName         = "lanqin-backup.json"
	onlineMarkerName     = "data/backup-online.json"
	maxOnlineMarkerBytes = 64 << 20
)

type Limits struct {
	MaxBytes   int64
	MaxEntries int
}

func DefaultLimits() Limits { return Limits{MaxBytes: 10 << 30, MaxEntries: 100000} }

func (l Limits) valid() bool { return l.MaxBytes > 0 && l.MaxEntries > 0 }

// Manifest is encrypted along with the archive. This format deliberately does
// not claim to include deployment secrets, remote IMAP content, or Postfix queues.
type Manifest struct {
	Version int       `json:"version"`
	Scope   string    `json:"scope"`
	Driver  string    `json:"driver"`
	Created time.Time `json:"created"`
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

func allowed(name string) bool {
	if name == "" || len(name) > 4096 || strings.Count(name, "/") > 64 || strings.ContainsAny(name, "\\\x00") || strings.IndexFunc(name, unicode.IsControl) >= 0 || path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	switch strings.SplitN(name, "/", 2)[0] {
	case "data", "mail", "dkim":
		return true
	}
	return false
}

func checkSnapshot(ctx context.Context, root *os.Root) error {
	for _, name := range []string{"data", "mail", "dkim"} {
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() {
			return errors.New("snapshot requires data, mail and dkim directories")
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := root.Lstat("data/lanqin.db" + suffix); !errors.Is(err, fs.ErrNotExist) {
			return errors.New("snapshot database must be closed and checkpointed without sidecar files")
		}
	}
	info, err := root.Lstat("data/lanqin.db")
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("snapshot requires a regular SQLite data/lanqin.db")
	}
	// Read-only immutable mode is appropriate only for a prepared closed copy.
	// Its use here is NOT evidence that the operator stopped the original writers.
	databasePath, err := filepath.Abs(filepath.Join(root.Name(), "data", "lanqin.db"))
	if err != nil {
		return errors.New("cannot resolve snapshot database")
	}
	uriPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // Windows drive letters require file:///C:/..., not file://C:/...
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return errors.New("cannot inspect snapshot database")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema=OFF"); err != nil {
		return errors.New("cannot safely inspect snapshot schema")
	}
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return errors.New("snapshot database failed integrity check")
	}
	return nil
}

// Write encrypts a caller-prepared offline SQLite data snapshot. The recipient
// must be an age X25519 public key. Callers must discard incomplete output.
func Write(ctx context.Context, snapshot string, dst io.Writer, recipient string, limits Limits) (Manifest, error) {
	return writeSnapshot(ctx, snapshot, dst, recipient, limits, "offline-data")
}

// WriteOnline encrypts a caller-prepared online SQLite reference-closure copy.
// The completion marker is a collector contract, NOT proof of stopped writers,
// an atomic whole-site snapshot or the sender's identity. Unindexed mail stays
// under data/backup-unindexed; neither this function nor Restore promotes it.
// Older readers that only understand offline-data correctly reject this scope.
func WriteOnline(ctx context.Context, snapshot string, dst io.Writer, recipient string, limits Limits) (Manifest, error) {
	return writeSnapshot(ctx, snapshot, dst, recipient, limits, "online-data")
}

func writeSnapshot(ctx context.Context, snapshot string, dst io.Writer, recipient string, limits Limits, scope string) (Manifest, error) {
	var empty Manifest
	if !limits.valid() {
		return empty, errors.New("backup limits must be positive")
	}
	key, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return empty, errors.New("invalid X25519 recipient")
	}
	root, err := os.OpenRoot(snapshot)
	if err != nil {
		return empty, errors.New("cannot open snapshot directory")
	}
	defer root.Close()
	if err := checkSnapshot(ctx, root); err != nil {
		return empty, err
	}
	var markerDigest [sha256.Size]byte
	if scope == "online-data" {
		markerDigest, err = checkOnlineMarker(ctx, root, limits.MaxBytes)
		if err != nil {
			return empty, err
		}
	}
	encrypted, err := age.Encrypt(dst, key)
	if err != nil {
		return empty, errors.New("cannot initialize backup encryption")
	}
	archive := tar.NewWriter(encrypted)
	manifest := Manifest{Version: 1, Scope: scope, Driver: "sqlite", Created: time.Now().UTC()}
	metadata, err := json.Marshal(manifest)
	if err != nil {
		return empty, err
	}
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(metadata)), Typeflag: tar.TypeReg}); err != nil {
		return empty, err
	}
	if _, err := archive.Write(metadata); err != nil {
		return empty, err
	}
	remaining, entries := limits.MaxBytes, 0
	markerArchived := scope != "online-data"
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot enumerate snapshot")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > limits.MaxEntries || !allowed(name) {
			return errors.New("snapshot entry is outside supported scope or exceeds entry limit")
		}
		info, err := entry.Info()
		if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
			return errors.New("snapshot contains a link or unsupported file type")
		}
		if info.Size() < 0 || (!info.IsDir() && info.Size() > remaining) {
			return errors.New("snapshot exceeds size limit")
		}
		if scope == "online-data" && name == onlineMarkerName && info.Size() > maxOnlineMarkerBytes {
			return errors.New("online completion marker exceeds limit")
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := root.Open(name)
		if err != nil {
			return errors.New("cannot open snapshot file")
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			return errors.New("snapshot changed during export")
		}
		var actualMarker hash.Hash
		var target io.Writer = archive
		if scope == "online-data" && name == onlineMarkerName {
			// Bind the preflight check to the marker bytes actually archived,
			// rather than rechecking a potentially different file afterward.
			actualMarker = sha256.New()
			target = io.MultiWriter(archive, actualMarker)
		}
		if _, err := io.CopyN(target, contextReader{ctx, file}, info.Size()); err != nil {
			return err
		}
		if actualMarker != nil {
			if !bytes.Equal(actualMarker.Sum(nil), markerDigest[:]) {
				return errors.New("online completion marker changed during export")
			}
			markerArchived = true
		}
		final, err := file.Stat()
		if err != nil || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
			return errors.New("snapshot changed during export")
		}
		remaining -= info.Size()
		return nil
	})
	if err != nil {
		return empty, err
	}
	if !markerArchived {
		return empty, errors.New("online completion marker disappeared during export")
	}
	if err := archive.Close(); err != nil {
		return empty, err
	}
	if err := encrypted.Close(); err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return manifest, nil
}

// Restore validates and decrypts to a NEW private container directory. Successful
// data appears under destination/snapshot with a destination/READY marker. It
// never replaces an existing destination or replays archived permissions/owners.
// On failure, a private .partial directory may remain for operator inspection.
// expectedSHA256 must come from a separately trusted receipt, not from an
// untrusted storage provider alongside the archive. age is not a sender signature.
func Restore(ctx context.Context, src io.Reader, destination, identity, expectedSHA256 string, limits Limits) (Manifest, error) {
	var manifest Manifest
	if !limits.valid() {
		return manifest, errors.New("backup limits must be positive")
	}
	want, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(want) != sha256.Size {
		return manifest, errors.New("a valid separately trusted SHA-256 receipt is required")
	}
	digest := sha256.New()
	src = io.TeeReader(src, digest)
	key, err := age.ParseX25519Identity(identity)
	if err != nil {
		return manifest, errors.New("invalid X25519 identity")
	}
	decrypted, err := age.Decrypt(contextReader{ctx, src}, key)
	if err != nil {
		return manifest, errors.New("cannot decrypt backup with supplied identity")
	}
	archive := tar.NewReader(contextReader{ctx, decrypted})
	header, err := archive.Next()
	if err != nil || header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 65536 {
		return manifest, errors.New("invalid backup manifest")
	}
	decoder := json.NewDecoder(archive)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || manifest.Version != 1 || (manifest.Scope != "offline-data" && manifest.Scope != "online-data") || manifest.Driver != "sqlite" || manifest.Created.IsZero() {
		return manifest, errors.New("unsupported backup manifest")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, errors.New("invalid backup manifest trailing content")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return manifest, errors.New("restore destination must not exist and its parent must exist")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return manifest, err
	}
	defer root.Close()
	if err := root.Mkdir(".partial", 0o700); err != nil {
		return manifest, err
	}
	stage, err := root.OpenRoot(".partial")
	if err != nil {
		return manifest, err
	}
	defer stage.Close()
	seen := map[string]bool{}
	remaining := limits.MaxBytes
	for entries := 0; ; entries++ {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, errors.New("invalid or incomplete backup archive")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if entries >= limits.MaxEntries || !allowed(name) || seen[name] || header.Size < 0 || header.Size > remaining {
			return manifest, errors.New("invalid backup path, duplicate entry, or resource limit exceeded")
		}
		if manifest.Scope == "online-data" && name == onlineMarkerName && header.Size > maxOnlineMarkerBytes {
			return manifest, errors.New("online completion marker exceeds limit")
		}
		seen[name] = true
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return manifest, errors.New("invalid directory entry")
			}
			if err := stage.MkdirAll(filepath.FromSlash(name), 0o700); err != nil {
				return manifest, err
			}
		case tar.TypeReg:
			if strings.HasSuffix(header.Name, "/") {
				return manifest, errors.New("invalid regular-file path")
			}
			if err := stage.MkdirAll(filepath.FromSlash(path.Dir(name)), 0o700); err != nil {
				return manifest, err
			}
			file, err := stage.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return manifest, err
			}
			_, copyErr := io.CopyN(file, contextReader{ctx, archive}, header.Size)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return manifest, errors.New("cannot stage complete backup file")
			}
			remaining -= header.Size
		default:
			return manifest, errors.New("backup links and special file types are forbidden")
		}
	}
	// tar EOF alone is not authentication of the entire age stream: it can occur
	// before the final encrypted frame. Explicitly require the authenticated EOF.
	if n, err := io.CopyN(io.Discard, contextReader{ctx, decrypted}, 1); n != 0 || err != io.EOF {
		return manifest, errors.New("backup has trailing content or failed final authentication")
	}
	// Bind the receipt to the bytes actually decrypted, not to an earlier pass
	// over a file which could change between hashing and restoration.
	if hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expectedSHA256) {
		return manifest, errors.New("backup does not match trusted receipt")
	}
	if err := checkSnapshot(ctx, stage); err != nil {
		return manifest, err
	}
	if manifest.Scope == "online-data" {
		if _, err := checkOnlineMarker(ctx, stage, limits.MaxBytes); err != nil {
			return manifest, err
		}
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := stage.Close(); err != nil {
		return manifest, err
	}
	if err := root.Rename(".partial", "snapshot"); err != nil {
		return manifest, err
	}
	ready := "Verified offline data snapshot; NOT an in-place service restore.\n"
	if manifest.Scope == "online-data" {
		ready = "Verified online-data archive staged; collector completion marker validated, NOT an in-place service restore. Unindexed mail remains quarantined; no automatic promotion.\n"
	}
	if err := root.WriteFile("READY", []byte(ready), 0o600); err != nil {
		return manifest, err
	}
	return manifest, nil
}

// checkOnlineMarker only validates the versioned collector completion contract.
// Its private source/summary fields are neither interpreted as trusted authority
// nor returned in Manifest or errors. A separately trusted ciphertext receipt is
// still required, and READY still means staging, never production activation.
func checkOnlineMarker(ctx context.Context, root *os.Root, maxBytes int64) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	invalid := errors.New("online completion marker is missing or invalid")
	parent, err := root.Lstat("data")
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return empty, invalid
	}
	info, err := root.Lstat(onlineMarkerName)
	limit := min(maxBytes, int64(maxOnlineMarkerBytes))
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return empty, invalid
	}
	file, err := root.Open(onlineMarkerName)
	if err != nil {
		return empty, invalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return empty, invalid
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, limit+1))
	if err != nil || int64(len(data)) != info.Size() || ctx.Err() != nil {
		return empty, invalid
	}
	var marker struct {
		Version  int             `json:"version"`
		Scope    string          `json:"scope"`
		Complete bool            `json:"complete"`
		Summary  json.RawMessage `json:"summary"`
		Sources  json.RawMessage `json:"sources"`
		Files    json.RawMessage `json:"files"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&marker) != nil || marker.Version != 1 || marker.Scope != "sqlite-reference-closure" || !marker.Complete {
		return empty, invalid
	}
	object := func(raw json.RawMessage, start byte) bool {
		raw = bytes.TrimSpace(raw)
		return len(raw) > 0 && raw[0] == start
	}
	if !object(marker.Summary, '{') || !object(marker.Sources, '{') || !object(marker.Files, '[') {
		return empty, invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return empty, invalid
	}
	final, err := file.Stat()
	if err != nil || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
		return empty, invalid
	}
	return sha256.Sum256(data), nil
}
