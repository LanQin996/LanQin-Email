// Package catalog is a host-published, authenticated inventory of encrypted
// artifacts. Web requests select IDs, never arbitrary filesystem paths or hashes.
package catalog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

var ErrInvalid = errors.New("invalid or unavailable backup artifact")
var idPattern = regexp.MustCompile(`^bac_[a-f0-9]{32}$`)

type Entry struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	SHA256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	PublishedAt time.Time `json:"publishedAt"`
}

type signedEntry struct {
	Entry Entry  `json:"entry"`
	MAC   string `json:"mac"`
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

func tag(entry Entry, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalid
	}
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, nil, []byte("lanqin/backup/artifact-catalog/v1")), derived); err != nil {
		return nil, ErrInvalid
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, ErrInvalid
	}
	mac := hmac.New(sha256.New, derived)
	mac.Write(data)
	return mac.Sum(nil), nil
}

// Publish copies the verified ciphertext, then publishes the authenticated
// descriptor last. It never overwrites objects, follows input symlinks, stores
// private recovery keys or claims to have decrypted the archive.
func Publish(ctx context.Context, directory, source, expected string, key []byte, maxBytes int64) (Entry, error) {
	var empty Entry
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size || len(key) != 32 || maxBytes < 1 || maxBytes > 5<<30 {
		return empty, ErrInvalid
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return empty, ErrInvalid
	}
	defer root.Close()
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return empty, ErrInvalid
	}
	in, err := os.Open(source)
	if err != nil {
		return empty, ErrInvalid
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() > maxBytes {
		return empty, ErrInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return empty, err
	}
	id := "bac_" + hex.EncodeToString(nonce[:])
	partial := ".partial-" + id
	file, err := root.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return empty, ErrInvalid
	}
	defer root.Remove(partial)
	defer file.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(cancelReader{ctx, in}, maxBytes+1))
	if err != nil || ctx.Err() != nil || n != opened.Size() || n > maxBytes || !hmac.Equal(digest.Sum(nil), want) {
		return empty, ErrInvalid
	}
	magic := make([]byte, len("age-encryption.org/v1\n"))
	if _, err := file.ReadAt(magic, 0); err != nil || string(magic) != "age-encryption.org/v1\n" {
		return empty, ErrInvalid
	}
	entry := Entry{Version: 1, ID: id, SHA256: hex.EncodeToString(want), Bytes: n, PublishedAt: time.Now().UTC()}
	mac, err := tag(entry, key)
	if err != nil {
		return empty, err
	}
	if err := file.Sync(); err != nil {
		return empty, err
	}
	if err := file.Close(); err != nil {
		return empty, err
	}
	if err := root.Link(partial, id+".age"); err != nil {
		return empty, ErrInvalid
	}
	committed := false
	defer func() {
		if !committed {
			root.Remove(id + ".age") // Only the object this invocation just created.
		}
	}()
	metadata := partial + ".json"
	manifest, err := root.OpenFile(metadata, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return empty, ErrInvalid
	}
	defer root.Remove(metadata)
	defer manifest.Close()
	if err := json.NewEncoder(manifest).Encode(signedEntry{Entry: entry, MAC: hex.EncodeToString(mac)}); err != nil {
		return empty, err
	}
	if err := manifest.Sync(); err != nil {
		return empty, err
	}
	if err := manifest.Close(); err != nil {
		return empty, err
	}
	if ctx.Err() != nil {
		return empty, ctx.Err()
	}
	if err := root.Link(metadata, id+".json"); err != nil {
		return empty, ErrInvalid
	}
	committed = true
	return entry, nil
}

// Open validates the authenticated descriptor and pins a regular file handle.
// The consumer must still hash the actual bytes before any network upload.
func Open(directory, id string, key []byte, maxBytes int64) (*os.File, Entry, error) {
	var empty Entry
	if !ValidID(id) || maxBytes < 1 {
		return nil, empty, ErrInvalid
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, empty, ErrInvalid
	}
	defer root.Close()
	info, err := root.Lstat(id + ".json")
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return nil, empty, ErrInvalid
	}
	manifest, err := root.Open(id + ".json")
	if err != nil {
		return nil, empty, ErrInvalid
	}
	defer manifest.Close()
	opened, err := manifest.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, empty, ErrInvalid
	}
	var signed signedEntry
	decoder := json.NewDecoder(io.LimitReader(manifest, 8193))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&signed) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, empty, ErrInvalid
	}
	e := signed.Entry
	want, err := tag(e, key)
	got, decodeErr := hex.DecodeString(signed.MAC)
	digest, digestErr := hex.DecodeString(e.SHA256)
	if err != nil || decodeErr != nil || !hmac.Equal(want, got) || e.Version != 1 || e.ID != id || e.Bytes < 1 || e.Bytes > maxBytes || e.PublishedAt.IsZero() || digestErr != nil || len(digest) != sha256.Size {
		return nil, empty, ErrInvalid
	}
	info, err = root.Lstat(id + ".age")
	if err != nil || !info.Mode().IsRegular() || info.Size() != e.Bytes {
		return nil, empty, ErrInvalid
	}
	file, err := root.Open(id + ".age")
	if err != nil {
		return nil, empty, ErrInvalid
	}
	opened, err = file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() != e.Bytes {
		file.Close()
		return nil, empty, ErrInvalid
	}
	return file, e, nil
}

// List bounds directory scans and validates metadata, not gigabytes of content
// on an HTTP request. Invalid entries fail closed instead of appearing trusted.
func List(ctx context.Context, directory string, key []byte, maxBytes int64) ([]Entry, error) {
	root, err := os.OpenRoot(directory)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalid
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, ErrInvalid
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1001)
	if err != nil && err != io.EOF || len(names) > 1000 {
		return nil, ErrInvalid
	}
	entries := []Entry{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if filepath.Ext(name) != ".json" || !ValidID(strings.TrimSuffix(name, ".json")) {
			continue
		}
		file, entry, err := Open(directory, strings.TrimSuffix(name, ".json"), key, maxBytes)
		if err != nil {
			return nil, ErrInvalid
		}
		file.Close()
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PublishedAt.After(entries[j].PublishedAt) })
	return entries, nil
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
