package backup

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const validOnlineMarker = `{"version":1,"scope":"sqlite-reference-closure","complete":true,"summary":{},"sources":{"data":"/synthetic-private-source"},"files":[]}`

func writeOnlineMarkerFixture(t *testing.T, source, marker string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(onlineMarkerName)), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineSnapshotRoundTripKeepsUnindexedMailQuarantined(t *testing.T) {
	source := snapshotFixture(t)
	key := identityFixture(t)
	writeOnlineMarkerFixture(t, source, validOnlineMarker)
	if err := os.Mkdir(filepath.Join(source, "data", "backup-unindexed"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := []byte("From: synthetic@example.test\r\nSubject: unindexed candidate\r\n\r\nNot automatically imported\r\n")
	if err := os.WriteFile(filepath.Join(source, "data", "backup-unindexed", "candidate.eml"), candidate, 0o600); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	manifest, err := WriteOnline(t.Context(), source, &encrypted, key.Recipient().String(), DefaultLimits())
	if err != nil || manifest.Version != 1 || manifest.Scope != "online-data" {
		t.Fatal("online prepared snapshot did not get a distinct scope", manifest, err)
	}
	if bytes.Contains(encrypted.Bytes(), []byte("synthetic-private-source")) || bytes.Contains(encrypted.Bytes(), []byte("candidate.eml")) {
		t.Fatal("online metadata or quarantined filename exposed in ciphertext")
	}
	publicManifest, _ := json.Marshal(manifest)
	if bytes.Contains(publicManifest, []byte("sources")) || bytes.Contains(publicManifest, []byte("synthetic-private-source")) {
		t.Fatal("private collector metadata copied into public manifest")
	}
	destination := filepath.Join(t.TempDir(), "restored")
	got, err := restoreTest(t.Context(), bytes.NewReader(encrypted.Bytes()), destination, key.String(), DefaultLimits())
	if err != nil || got != manifest {
		t.Fatal("online archive round trip failed", err)
	}
	marker, err := os.ReadFile(filepath.Join(destination, "snapshot", filepath.FromSlash(onlineMarkerName)))
	if err != nil || string(marker) != validOnlineMarker {
		t.Fatal("collector marker was lost or changed", err)
	}
	quarantined, err := os.ReadFile(filepath.Join(destination, "snapshot", "data", "backup-unindexed", "candidate.eml"))
	if err != nil || !bytes.Equal(quarantined, candidate) {
		t.Fatal("unindexed candidate not preserved in quarantine", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "snapshot", "mail", "candidate.eml")); !os.IsNotExist(err) {
		t.Fatal("restore promoted unindexed mail automatically")
	}
	ready, err := os.ReadFile(filepath.Join(destination, "READY"))
	if err != nil || !bytes.Contains(ready, []byte("no automatic promotion")) {
		t.Fatal("READY misrepresented online staging as activation", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(destination, "snapshot", "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body string
	if err := db.QueryRow("SELECT body FROM synthetic_messages").Scan(&body); err != nil || body != "synthetic mail only" {
		t.Fatal("restored real SQLite cannot be queried", err)
	}
}

func TestOnlineSnapshotPreflightRejectsIncompleteOrInvalidMarkers(t *testing.T) {
	key := identityFixture(t)
	for name, marker := range map[string]string{
		"missing":          "",
		"malformed":        `{not JSON; synthetic-secret-do-not-log}`,
		"wrong-version":    strings.Replace(validOnlineMarker, `"version":1`, `"version":2`, 1),
		"wrong-scope":      strings.Replace(validOnlineMarker, "sqlite-reference-closure", "other-scope", 1),
		"incomplete":       strings.Replace(validOnlineMarker, `"complete":true`, `"complete":false`, 1),
		"missing-complete": strings.Replace(validOnlineMarker, `"complete":true,`, "", 1),
		"summary-scalar":   strings.Replace(validOnlineMarker, `"summary":{}`, `"summary":"synthetic-secret-do-not-log"`, 1),
		"sources-array":    strings.Replace(validOnlineMarker, `"sources":{"data":"/synthetic-private-source"}`, `"sources":[]`, 1),
		"files-object":     strings.Replace(validOnlineMarker, `"files":[]`, `"files":{}`, 1),
		"trailing-json":    validOnlineMarker + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			source := snapshotFixture(t)
			if marker != "" {
				writeOnlineMarkerFixture(t, source, marker)
			}
			var encrypted bytes.Buffer
			_, err := WriteOnline(t.Context(), source, &encrypted, key.Recipient().String(), DefaultLimits())
			if err == nil || strings.Contains(err.Error(), "synthetic-secret") || encrypted.Len() != 0 {
				t.Fatal("bad online marker was exported or leaked", err)
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		source := snapshotFixture(t)
		writeOnlineMarkerFixture(t, source, validOnlineMarker)
		if err := os.Truncate(filepath.Join(source, filepath.FromSlash(onlineMarkerName)), maxOnlineMarkerBytes+1); err != nil {
			t.Fatal(err)
		}
		var encrypted bytes.Buffer
		if _, err := WriteOnline(t.Context(), source, &encrypted, key.Recipient().String(), DefaultLimits()); err == nil || encrypted.Len() != 0 {
			t.Fatal("oversized marker read or exported")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		source := snapshotFixture(t)
		if err := os.WriteFile(filepath.Join(source, "data", "actual-marker.json"), []byte(validOnlineMarker), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("actual-marker.json", filepath.Join(source, filepath.FromSlash(onlineMarkerName))); err != nil {
			t.Skip("symlink creation unavailable")
		}
		if _, err := WriteOnline(t.Context(), source, io.Discard, key.Recipient().String(), DefaultLimits()); err == nil {
			t.Fatal("symlink marker accepted")
		}
	})
}

// A public age recipient does not authenticate the sender: manually construct a
// purported online archive to exercise Restore independently of writer checks.
func purportedOnlineArchive(t *testing.T, source string, key *age.X25519Identity, marker *string) []byte {
	t.Helper()
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(writer)
	manifest, _ := json.Marshal(Manifest{Version: 1, Scope: "online-data", Driver: "sqlite", Created: time.Now().UTC()})
	contents := []struct {
		name string
		data []byte
	}{{manifestName, manifest}}
	for _, name := range []string{"data/lanqin.db", "mail/synthetic.eml", "dkim/synthetic.txt"} {
		data, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, struct {
			name string
			data []byte
		}{name, data})
	}
	if marker != nil {
		contents = append(contents, struct {
			name string
			data []byte
		}{onlineMarkerName, []byte(*marker)})
	}
	for _, entry := range contents {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Size: int64(len(entry.data)), Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encrypted.Bytes()
}

func TestRestorePurportedOnlineScopeRequiresValidCompletionMarker(t *testing.T) {
	key := identityFixture(t)
	source := snapshotFixture(t)
	incomplete := strings.Replace(validOnlineMarker, `"complete":true`, `"complete":false`, 1)
	wrongShape := strings.Replace(validOnlineMarker, `"files":[]`, `"files":null`, 1)
	trailing := validOnlineMarker + `{}`
	for name, marker := range map[string]*string{"missing": nil, "incomplete": &incomplete, "wrong-shape": &wrongShape, "trailing-json": &trailing} {
		t.Run(name, func(t *testing.T) {
			data := purportedOnlineArchive(t, source, key, marker)
			destination := filepath.Join(t.TempDir(), "restore")
			if _, err := restoreTest(t.Context(), bytes.NewReader(data), destination, key.String(), DefaultLimits()); err == nil {
				t.Fatal("online scope alone was accepted as completed collection")
			}
			for _, name := range []string{"READY", "snapshot"} {
				if _, err := os.Stat(filepath.Join(destination, name)); !os.IsNotExist(err) {
					t.Fatal("invalid online snapshot published")
				}
			}
		})
	}
}

func TestRestoreOnlineMarkerLimitBeforeStagingFile(t *testing.T) {
	key := identityFixture(t)
	var output bytes.Buffer
	encrypted, err := age.Encrypt(&output, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(encrypted)
	manifest, _ := json.Marshal(Manifest{Version: 1, Scope: "online-data", Driver: "sqlite", Created: time.Now().UTC()})
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Typeflag: tar.TypeReg, Size: int64(len(manifest)), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := archive.WriteHeader(&tar.Header{Name: onlineMarkerName, Typeflag: tar.TypeReg, Size: maxOnlineMarkerBytes + 1, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	// The encrypted stream is valid, but its tar body is intentionally absent:
	// the marker-size limit must reject the header before reading/staging it.
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	_, err = restoreTest(t.Context(), bytes.NewReader(output.Bytes()), destination, key.String(), DefaultLimits())
	if err == nil || !strings.Contains(err.Error(), "completion marker exceeds limit") {
		t.Fatal("oversized marker not rejected at its header", err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".partial", filepath.FromSlash(onlineMarkerName))); !os.IsNotExist(err) {
		t.Fatal("oversized marker created a staging file")
	}
	if _, err := os.Stat(filepath.Join(destination, "READY")); !os.IsNotExist(err) {
		t.Fatal("oversized marker produced READY")
	}
}

func TestOfflineSnapshotBehaviorDoesNotRequireOnlineMarker(t *testing.T) {
	source := snapshotFixture(t)
	key := identityFixture(t)
	// An unrelated/incomplete collector marker must not change legacy Write's
	// version/scope or impose a new online contract on prepared offline copies.
	writeOnlineMarkerFixture(t, source, `{"version":1,"complete":false}`)
	var output bytes.Buffer
	manifest, err := Write(t.Context(), source, &output, key.Recipient().String(), DefaultLimits())
	if err != nil || manifest.Version != 1 || manifest.Scope != "offline-data" {
		t.Fatal("legacy offline export behavior changed", err)
	}
	if restored, err := restoreTest(t.Context(), bytes.NewReader(output.Bytes()), filepath.Join(t.TempDir(), "restore"), key.String(), DefaultLimits()); err != nil || restored != manifest {
		t.Fatal("legacy offline restore gained online restrictions", err)
	}
}

type markerMutationWriter struct {
	bytes.Buffer
	mutate func()
}

func (w *markerMutationWriter) Write(p []byte) (int, error) {
	if w.mutate != nil {
		action := w.mutate
		w.mutate = nil
		action()
	}
	return w.Buffer.Write(p)
}

func TestOnlineExportBindsPreflightToActuallyArchivedMarker(t *testing.T) {
	key := identityFixture(t)
	for _, scenario := range []string{"replace", "remove", "grow"} {
		t.Run(scenario, func(t *testing.T) {
			source := snapshotFixture(t)
			writeOnlineMarkerFixture(t, source, validOnlineMarker)
			output := &markerMutationWriter{mutate: func() {
				name := filepath.Join(source, filepath.FromSlash(onlineMarkerName))
				var err error
				switch scenario {
				case "replace":
					err = os.WriteFile(name, []byte(strings.Replace(validOnlineMarker, `"version":1`, `"version":2`, 1)), 0o600)
				case "remove":
					err = os.Remove(name)
				case "grow":
					err = os.Truncate(name, maxOnlineMarkerBytes+1)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			if _, err := WriteOnline(t.Context(), source, output, key.Recipient().String(), DefaultLimits()); err == nil {
				t.Fatal("marker changed after preflight but export succeeded")
			}
		})
	}
}
