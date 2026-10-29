package storage

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
)

// This covers the real offline archive -> TLS upload -> downloaded ciphertext
// -> verified SQLite/Maildir staging flow, without a production cloud account.
func TestEncryptedSnapshotCloudProtocolRestoreRoundTrip(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "offline")
	for _, name := range []string{"data", "mail", "dkim"} {
		if err := os.MkdirAll(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mail := []byte("Subject: synthetic backup fixture\r\n\r\nSynthetic body, no personal data.\r\n")
	if err := os.WriteFile(filepath.Join(source, "mail", "sample.eml"), mail, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(source, "data", "lanqin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE fixture(value TEXT); INSERT INTO fixture VALUES('synthetic round-trip')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if _, err := backup.Write(t.Context(), source, &encrypted, identity.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "snapshot.age")
	if err := os.WriteFile(file, encrypted.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encrypted.Bytes())
	receipt := hex.EncodeToString(digest[:])
	var mutex sync.Mutex
	objects := make(map[string][]byte)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		mutex.Lock()
		objects[r.URL.Path] = data
		mutex.Unlock()
		if r.Host == "s3.example.invalid" {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(201)
		}
	}))
	defer server.Close()
	client := testClient(t, server)
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("r2"), davDestination("dav")}}
	report, err := upload(t.Context(), file, receipt, cfg, DefaultOptions(), func(d Destination) (uploader, error) {
		return buildUploader(d, func(string) (string, bool) { return "synthetic-credential", true }, client)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.Destinations {
		remotePath := "/backup-bucket/" + result.ObjectKey
		if result.DestinationID == "dav" {
			remotePath = "/existing/dav/" + result.ObjectKey
		}
		mutex.Lock()
		downloaded := append([]byte(nil), objects[remotePath]...)
		mutex.Unlock()
		if !bytes.Equal(downloaded, encrypted.Bytes()) {
			t.Fatal("uploaded object is not the original encrypted snapshot")
		}
		recovered := filepath.Join(directory, "recovered-"+result.DestinationID)
		if _, err := backup.Restore(t.Context(), bytes.NewReader(downloaded), recovered, identity.String(), receipt, backup.DefaultLimits()); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(recovered, "snapshot", "mail", "sample.eml"))
		if err != nil || !bytes.Equal(got, mail) {
			t.Fatal("mail did not round-trip through remote upload and verified restore")
		}
		restored, err := sql.Open("sqlite", filepath.Join(recovered, "snapshot", "data", "lanqin.db"))
		if err != nil {
			t.Fatal(err)
		}
		var value string
		err = restored.QueryRow(`SELECT value FROM fixture`).Scan(&value)
		restored.Close()
		if err != nil || value != "synthetic round-trip" {
			t.Fatalf("restored SQLite state incorrect: %v", err)
		}
		if _, err := os.Stat(filepath.Join(recovered, "READY")); err != nil {
			t.Fatal("verified restore has no READY marker")
		}
	}
}
