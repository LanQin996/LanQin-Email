// Package preservation encrypts maintenance rollback evidence. It never applies
// deployment assets, changes ownership, or expands/replays Postfix queue data.
package preservation

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"filippo.io/age"
)

const (
	scope          = "maintenance-preservation"
	manifestName   = "manifest.json"
	maxManifest    = 1 << 20
	hardMaxBytes   = 256 << 20
	hardMaxEntries = 160
)

var errInvalid = errors.New("invalid, incomplete, or unauthenticated preservation bundle")
var errOutput = errors.New("cannot safely publish preservation bundle")

type Limits struct {
	MaxBytes   int64
	MaxEntries int
}

func DefaultLimits() Limits { return Limits{MaxBytes: hardMaxBytes, MaxEntries: hardMaxEntries} }

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxBytes <= hardMaxBytes && l.MaxEntries > 0 && l.MaxEntries <= hardMaxEntries
}

type Binding struct {
	ConfigSHA256 string `json:"configSHA256"`
	OperationID  string `json:"operationId"`
}

type Item struct {
	Name   string
	Source string
	Data   []byte
}

type Entry struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version int     `json:"version"`
	Scope   string  `json:"scope"`
	Binding Binding `json:"binding"`
	Entries []Entry `json:"entries"`
}

func validHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func validName(name string) bool {
	if len(name) == len("assets/0000.bin") && strings.HasPrefix(name, "assets/") && strings.HasSuffix(name, ".bin") {
		for _, c := range name[7:11] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return len(name) == len("spools/")+64+len(".tar") && strings.HasPrefix(name, "spools/") && strings.HasSuffix(name, ".tar") && validHex(name[7:71])
}

func validBinding(b Binding) bool {
	if !validHex(b.ConfigSHA256) || len(b.OperationID) < 1 || len(b.OperationID) > 128 {
		return false
	}
	for _, c := range b.OperationID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validManifest(m Manifest, limits Limits) bool {
	if !limits.valid() || m.Version != 1 || m.Scope != scope || !validBinding(m.Binding) || len(m.Entries) < 1 || len(m.Entries) > limits.MaxEntries {
		return false
	}
	seen, remaining := map[string]bool{}, limits.MaxBytes
	for _, e := range m.Entries {
		if !validName(e.Name) || seen[e.Name] || len(e.Source) > 4096 || !utf8.ValidString(e.Source) || strings.IndexFunc(e.Source, unicode.IsControl) >= 0 || e.Bytes < 0 || e.Bytes > remaining || !validHex(e.SHA256) {
			return false
		}
		seen[e.Name] = true
		remaining -= e.Bytes
	}
	return true
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

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

func failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Write preserves input order. Callers must discard output if it returns an error.
// Source is encrypted metadata only; it is never interpreted as a filesystem path.
func Write(ctx context.Context, dst io.Writer, recipient string, binding Binding, items []Item, limits Limits) (Manifest, error) {
	empty := Manifest{}
	if !limits.valid() || !validBinding(binding) || len(items) < 1 || len(items) > limits.MaxEntries {
		return empty, errInvalid
	}
	m := Manifest{Version: 1, Scope: scope, Binding: binding, Entries: make([]Entry, 0, len(items))}
	remaining := limits.MaxBytes
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if int64(len(item.Data)) > remaining {
			return empty, errInvalid
		}
		remaining -= int64(len(item.Data))
		digest := sha256.Sum256(item.Data)
		m.Entries = append(m.Entries, Entry{Name: item.Name, Source: item.Source, Bytes: int64(len(item.Data)), SHA256: hex.EncodeToString(digest[:])})
	}
	if !validManifest(m, limits) {
		return empty, errInvalid
	}
	metadata, err := json.Marshal(m)
	if err != nil || len(metadata) > maxManifest {
		return empty, errInvalid
	}
	key, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return empty, errInvalid
	}
	encrypted, err := age.Encrypt(contextWriter{ctx, dst}, key)
	if err != nil {
		return empty, failure(ctx, errOutput)
	}
	archive := tar.NewWriter(encrypted)
	write := func(name string, data []byte) error {
		if err := archive.WriteHeader(canonicalHeader(name, int64(len(data)))); err != nil {
			return errOutput
		}
		_, err := io.Copy(archive, contextReader{ctx, bytes.NewReader(data)})
		return err
	}
	if err := write(manifestName, metadata); err != nil {
		return empty, failure(ctx, errOutput)
	}
	for i, item := range items {
		if err := archive.WriteHeader(canonicalHeader(item.Name, int64(len(item.Data)))); err != nil {
			return empty, failure(ctx, errOutput)
		}
		digest := sha256.New()
		if _, err := io.Copy(io.MultiWriter(archive, digest), contextReader{ctx, bytes.NewReader(item.Data)}); err != nil {
			return empty, failure(ctx, errOutput)
		}
		if hex.EncodeToString(digest.Sum(nil)) != m.Entries[i].SHA256 {
			return empty, errInvalid
		}
	}
	if err := archive.Close(); err != nil {
		return empty, failure(ctx, errOutput)
	}
	if err := encrypted.Close(); err != nil {
		return empty, failure(ctx, errOutput)
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return m, nil
}

func canonicalHeader(name string, size int64) *tar.Header {
	return &tar.Header{Name: name, Size: size, Mode: 0o600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
}

// readHeader deliberately does not use tar.Reader on the entire stream: its
// transparent handling of PAX/GNU extension headers would hide forbidden entries.
// A single canonical USTAR header also forbids links, sparse files and metadata.
func readHeader(src io.Reader) (*tar.Header, error) {
	block := make([]byte, 512)
	if _, err := io.ReadFull(src, block); err != nil {
		return nil, errInvalid
	}
	h, err := tar.NewReader(bytes.NewReader(block)).Next()
	if err != nil || h.Typeflag != tar.TypeReg || h.Format != tar.FormatUSTAR || h.Size < 0 {
		return nil, errInvalid
	}
	var canonical bytes.Buffer
	if err := tar.NewWriter(&canonical).WriteHeader(canonicalHeader(h.Name, h.Size)); err != nil || !bytes.Equal(block, canonical.Bytes()) {
		return nil, errInvalid
	}
	return h, nil
}

func padding(src io.Reader, size int64) error {
	n := (512 - size%512) % 512
	b := make([]byte, n)
	if _, err := io.ReadFull(src, b); err != nil || !bytes.Equal(b, make([]byte, n)) {
		return errInvalid
	}
	return nil
}

// Inspect authenticates the entire ciphertext and every payload without writing
// plaintext files. The receipt must come from separately trusted local state.
func Inspect(ctx context.Context, src io.Reader, identity, expectedCipherSHA256 string, limits Limits) (Manifest, error) {
	return inspect(ctx, src, identity, expectedCipherSHA256, nil, limits, nil)
}

// Verify also binds the complete manifest (including entry order and sources) to
// the caller's expected manifest. It never writes plaintext files.
func Verify(ctx context.Context, src io.Reader, identity, expectedCipherSHA256 string, want Manifest, limits Limits) error {
	_, err := inspect(ctx, src, identity, expectedCipherSHA256, &want, limits, nil)
	return err
}

type payloadSink func(index int, e Entry) (io.WriteCloser, error)

func inspect(ctx context.Context, src io.Reader, identity, expectedCipherSHA256 string, want *Manifest, limits Limits, sink payloadSink) (Manifest, error) {
	m, err := read(ctx, src, identity, expectedCipherSHA256, want, limits, sink)
	if err != nil {
		return Manifest{}, failure(ctx, errInvalid)
	}
	return m, nil
}

func read(ctx context.Context, src io.Reader, identity, expectedCipherSHA256 string, want *Manifest, limits Limits, sink payloadSink) (Manifest, error) {
	empty := Manifest{}
	if !limits.valid() || !validHex(strings.ToLower(expectedCipherSHA256)) || want != nil && !validManifest(*want, limits) {
		return empty, errInvalid
	}
	key, err := age.ParseX25519Identity(identity)
	if err != nil {
		return empty, errInvalid
	}
	// Include manifest, headers, padding and ample age framing, but never allow
	// a malformed stream to consume an unbounded ciphertext source.
	bounded := &io.LimitedReader{R: contextReader{ctx, src}, N: limits.MaxBytes + maxManifest + int64(limits.MaxEntries+1)*1024 + (1 << 20)}
	digest := sha256.New()
	decrypted, err := age.Decrypt(io.TeeReader(bounded, digest), key)
	if err != nil {
		return empty, errInvalid
	}
	plain := contextReader{ctx, decrypted}
	h, err := readHeader(plain)
	if err != nil || h.Name != manifestName || h.Size < 1 || h.Size > maxManifest {
		return empty, errInvalid
	}
	metadata := make([]byte, h.Size)
	if _, err := io.ReadFull(plain, metadata); err != nil {
		return empty, errInvalid
	}
	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&m) != nil || !validManifest(m, limits) {
		return empty, errInvalid
	}
	canonical, err := json.Marshal(m)
	// Exact canonical JSON rejects duplicate/case-folded fields, omitted fields,
	// trailing data and alternate numeric encodings as well as unknown fields.
	if err != nil || !bytes.Equal(canonical, metadata) {
		return empty, errInvalid
	}
	if want != nil {
		expected, err := json.Marshal(want)
		if err != nil || !bytes.Equal(expected, canonical) {
			return empty, errInvalid
		}
	}
	if err := padding(plain, h.Size); err != nil {
		return empty, err
	}
	for i, e := range m.Entries {
		h, err := readHeader(plain)
		if err != nil || h.Name != e.Name || h.Size != e.Bytes {
			return empty, errInvalid
		}
		digest := sha256.New()
		var file io.WriteCloser
		var target io.Writer = digest
		if sink != nil {
			file, err = sink(i, e)
			if err != nil {
				return empty, errInvalid
			}
			target = io.MultiWriter(digest, file)
		}
		_, copyErr := io.CopyN(target, plain, e.Bytes)
		var closeErr error
		if file != nil {
			closeErr = file.Close()
		}
		if copyErr != nil || closeErr != nil || hex.EncodeToString(digest.Sum(nil)) != e.SHA256 {
			return empty, errInvalid
		}
		if err := padding(plain, e.Bytes); err != nil {
			return empty, err
		}
	}
	footer := make([]byte, 1024)
	if _, err := io.ReadFull(plain, footer); err != nil || !bytes.Equal(footer, make([]byte, 1024)) {
		return empty, errInvalid
	}
	// Require actual authenticated age EOF, not merely tar's two zero blocks.
	var one [1]byte
	if n, err := io.ReadFull(plain, one[:]); n != 0 || err != io.EOF || bounded.N <= 0 {
		return empty, errInvalid
	}
	if hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expectedCipherSHA256) {
		return empty, errInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return m, nil
}

// PayloadName maps manifest order to opaque isolated files. No source or archived
// filename participates in filesystem traversal, permission changes or execution.
func PayloadName(index int) string { return fmt.Sprintf("payloads/%04d.bin", index) }
