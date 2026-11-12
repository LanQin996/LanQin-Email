package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func snapshotFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"data", "mail", "dkim"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE synthetic_messages(body TEXT); INSERT INTO synthetic_messages VALUES('synthetic mail only')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mail", "synthetic.eml"), []byte("Synthetic private message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dkim", "synthetic.txt"), []byte("Synthetic DKIM sample, not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func identityFixture(t *testing.T) *age.X25519Identity {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func restoreTest(ctx context.Context, src io.Reader, destination, identity string, limits Limits) (Manifest, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return Manifest{}, err
	}
	digest := sha256.Sum256(data)
	return Restore(ctx, bytes.NewReader(data), destination, identity, hex.EncodeToString(digest[:]), limits)
}

func TestSnapshotRoundTrip(t *testing.T) {
	source := snapshotFixture(t)
	key := identityFixture(t)
	var encrypted bytes.Buffer
	manifest, err := Write(t.Context(), source, &encrypted, key.Recipient().String(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Bytes(), []byte("Synthetic private message")) || bytes.Contains(encrypted.Bytes(), []byte("synthetic.eml")) {
		t.Fatal("ciphertext exposes content or filenames")
	}
	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := restoreTest(t.Context(), bytes.NewReader(encrypted.Bytes()), destination, key.String(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if manifest != restored {
		t.Fatal("manifest changed during round trip")
	}
	for _, name := range []string{"data/lanqin.db", "mail/synthetic.eml", "dkim/synthetic.txt"} {
		want, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(destination, "snapshot", name)
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("round trip differs for synthetic file: %v", err)
		}
		if info, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("restored file is not private: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "READY")); err != nil {
		t.Fatal("verified restore has no READY marker")
	}
	if _, err := restoreTest(t.Context(), bytes.NewReader(encrypted.Bytes()), destination, key.String(), DefaultLimits()); err == nil {
		t.Fatal("existing restore directory was overwritten")
	}
}

func TestWrongKeyTamperingAndTruncationNeverPublish(t *testing.T) {
	key := identityFixture(t)
	other := identityFixture(t)
	var output bytes.Buffer
	if _, err := Write(t.Context(), snapshotFixture(t), &output, key.Recipient().String(), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	data := output.Bytes()
	tampered := bytes.Clone(data)
	tampered[len(tampered)-1] ^= 1
	for _, test := range []struct {
		name, identity string
		data           []byte
	}{
		{"wrong key", other.String(), data},
		{"truncated final frame", key.String(), data[:len(data)-1]},
		{"modified final frame", key.String(), tampered},
		{"appended ciphertext", key.String(), append(bytes.Clone(data), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "restore")
			if _, err := restoreTest(t.Context(), bytes.NewReader(test.data), destination, test.identity, DefaultLimits()); err == nil {
				t.Fatal("unauthenticated restore succeeded")
			}
			for _, name := range []string{"READY", "snapshot"} {
				if _, err := os.Stat(filepath.Join(destination, name)); !os.IsNotExist(err) {
					t.Fatal("invalid backup was published")
				}
			}
		})
	}
}

func maliciousArchive(t *testing.T, key *age.X25519Identity, headers []*tar.Header, trailing bool) []byte {
	t.Helper()
	var out bytes.Buffer
	encrypted, err := age.Encrypt(&out, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(encrypted)
	manifest, _ := json.Marshal(Manifest{Version: 1, Scope: "offline-data", Driver: "sqlite", Created: time.Now().UTC()})
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(manifest); err != nil {
		t.Fatal(err)
	}
	for _, header := range headers {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.CopyN(archive, strings.NewReader(strings.Repeat("x", int(header.Size))), header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if trailing {
		if _, err := encrypted.Write([]byte("extra plaintext after tar EOF")); err != nil {
			t.Fatal(err)
		}
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestUntrustedArchivePathsAndTypes(t *testing.T) {
	key := identityFixture(t)
	for _, name := range []string{"../escape", "/absolute", "data/../../escape", "data\\escape", "C:/escape", "secrets/env", "data/./file", "mail/control\nname"} {
		t.Run(name, func(t *testing.T) {
			data := maliciousArchive(t, key, []*tar.Header{{Name: name, Typeflag: tar.TypeReg}}, false)
			if _, err := restoreTest(t.Context(), bytes.NewReader(data), filepath.Join(t.TempDir(), "out"), key.String(), DefaultLimits()); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo, tar.TypeChar} {
		data := maliciousArchive(t, key, []*tar.Header{{Name: "data/unsafe", Typeflag: kind, Linkname: "../../escape"}}, false)
		if _, err := restoreTest(t.Context(), bytes.NewReader(data), filepath.Join(t.TempDir(), "out"), key.String(), DefaultLimits()); err == nil {
			t.Fatal("link or special file accepted")
		}
	}
	duplicate := maliciousArchive(t, key, []*tar.Header{{Name: "data/same", Typeflag: tar.TypeReg}, {Name: "data/same", Typeflag: tar.TypeReg}}, false)
	if _, err := restoreTest(t.Context(), bytes.NewReader(duplicate), filepath.Join(t.TempDir(), "out"), key.String(), DefaultLimits()); err == nil {
		t.Fatal("duplicate path accepted")
	}
	trailing := maliciousArchive(t, key, nil, true)
	if _, err := restoreTest(t.Context(), bytes.NewReader(trailing), filepath.Join(t.TempDir(), "out"), key.String(), DefaultLimits()); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing plaintext was not rejected: %v", err)
	}
}

func TestSnapshotPreflightLimitsAndCancellation(t *testing.T) {
	key := identityFixture(t)
	source := snapshotFixture(t)
	for _, limits := range []Limits{{}, {MaxBytes: 1, MaxEntries: 100}, {MaxBytes: 1 << 30, MaxEntries: 1}} {
		if _, err := Write(t.Context(), source, io.Discard, key.Recipient().String(), limits); err == nil {
			t.Fatal("export resource limit was ignored")
		}
	}
	var encrypted bytes.Buffer
	if _, err := Write(t.Context(), source, &encrypted, key.Recipient().String(), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []Limits{{MaxBytes: 1, MaxEntries: 100}, {MaxBytes: 1 << 30, MaxEntries: 1}} {
		if _, err := restoreTest(t.Context(), bytes.NewReader(encrypted.Bytes()), filepath.Join(t.TempDir(), "out"), key.String(), limits); err == nil {
			t.Fatal("restore resource limit was ignored")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Write(ctx, source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
		t.Fatal("cancelled export succeeded")
	}
	if err := os.WriteFile(filepath.Join(source, "data", "lanqin.db-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(t.Context(), source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
		t.Fatal("database with WAL accepted as a closed snapshot")
	}
}

func TestSnapshotRejectsUnscopedFilesAndInvalidDatabase(t *testing.T) {
	key := identityFixture(t)
	t.Run("unexpected root file", func(t *testing.T) {
		source := snapshotFixture(t)
		if err := os.WriteFile(filepath.Join(source, ".env"), []byte("SYNTHETIC_ONLY=value"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Write(t.Context(), source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
			t.Fatal("unscoped configuration file exported")
		}
	})
	t.Run("invalid SQLite", func(t *testing.T) {
		source := snapshotFixture(t)
		if err := os.WriteFile(filepath.Join(source, "data", "lanqin.db"), []byte("not a SQLite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Write(t.Context(), source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
			t.Fatal("invalid database exported")
		}
	})
	t.Run("source symlink", func(t *testing.T) {
		source := snapshotFixture(t)
		if err := os.Symlink("synthetic.eml", filepath.Join(source, "mail", "link")); err != nil {
			t.Skip("symlink creation is unavailable on this host")
		}
		if _, err := Write(t.Context(), source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
			t.Fatal("source symlink exported")
		}
	})
}
