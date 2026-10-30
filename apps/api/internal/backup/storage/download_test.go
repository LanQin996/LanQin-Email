package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
)

const downloadName = "lanqin-0123456789abcdef0123456789abcdef.age"

func downloadDestination(kind string) (Destination, Credentials, string) {
	switch kind {
	case "s3":
		d := s3Destination("s3_download")
		return d, Credentials{AccessKeyID: "synthetic-download-key", SecretAccessKey: "synthetic-download-secret", SessionToken: "synthetic-session-token"}, d.S3.Prefix + "/" + downloadName
	case "webdav":
		return davDestination("dav_download"), Credentials{Username: "synthetic-user", Password: "synthetic-password"}, downloadName
	default:
		return Destination{ID: "drive_download", Name: "Drive fixture", Type: "drive", Drive: &DriveConfig{FolderID: "synthetic-folder", AccessTokenEnv: "DRIVE_TOKEN"}}, Credentials{DriveAccessToken: "synthetic-drive-token"}, "synthetic-file-id"
	}
}

func downloadArchiveFixture(t *testing.T) ([]byte, string, string, []byte) {
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
	if _, err := db.Exec("CREATE TABLE sample(body TEXT); INSERT INTO sample VALUES('synthetic downloaded backup')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mail := bytes.Repeat([]byte("synthetic mail payload\r\n"), 8192) // >64KiB catches response-body truncation.
	if err := os.WriteFile(filepath.Join(root, "mail", "sample.eml"), mail, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if _, err := backup.Write(t.Context(), root, &encrypted, key.Recipient().String(), backup.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encrypted.Bytes())
	return encrypted.Bytes(), hex.EncodeToString(hash[:]), key.String(), mail
}

func TestDownloadCloudProtocolsActualEncryptedRecovery(t *testing.T) {
	data, receipt, identity, mail := downloadArchiveFixture(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-must-not-be-used")
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			d, c, object := downloadDestination(kind)
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" {
					t.Error("download sent non-GET request")
				}
				switch kind {
				case "s3":
					if r.URL.Path != "/backup-bucket/"+object || !strings.Contains(r.Header.Get("Authorization"), "Credential=synthetic-download-key/") || strings.Contains(r.Header.Get("Authorization"), "ambient-") || r.Header.Get("X-Amz-Security-Token") != c.SessionToken {
						t.Error("S3 used wrong target or credentials")
					}
				case "webdav":
					u, p, ok := r.BasicAuth()
					if !ok || u != c.Username || p != c.Password || r.URL.Path != "/existing/dav_download/"+object {
						t.Error("DAV used wrong target/credentials")
					}
				case "drive":
					if r.Host != "www.googleapis.com" || r.URL.Path != "/drive/v3/files/"+object || r.Header.Get("Authorization") != "Bearer "+c.DriveAccessToken {
						t.Error("Drive used wrong target/credentials")
					}
					if r.URL.Query().Get("alt") == "" {
						json.NewEncoder(w).Encode(map[string]any{"parents": []string{d.Drive.FolderID}, "appProperties": map[string]string{"lanqinDestination": d.ID, "lanqinSHA256": receipt}})
						return
					}
					if r.URL.Query().Get("alt") != "media" {
						t.Error("unexpected Drive query")
					}
				}
				w.Write(data)
			}))
			defer server.Close()
			outDir := t.TempDir()
			output := filepath.Join(outDir, "received.age")
			got, err := download(t.Context(), d, object, receipt, output, DefaultOptions(), c, testClient(t, server))
			if err != nil || got.SHA256 != receipt || got.Bytes != int64(len(data)) {
				t.Fatal("download failed", got, err)
			}
			expectedRequests := int32(1)
			if kind == "drive" {
				expectedRequests = 2
			}
			if requests.Load() != expectedRequests {
				t.Fatal("unexpected provider requests", requests.Load())
			}
			actual, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("ciphertext changed/truncated", err)
			}
			stage := filepath.Join(t.TempDir(), "recovered")
			if _, err := backup.Restore(t.Context(), bytes.NewReader(actual), stage, identity, receipt, backup.DefaultLimits()); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(stage, "snapshot", "mail", "sample.eml"))
			if err != nil || !bytes.Equal(body, mail) {
				t.Fatal("recovered mail differs", err)
			}
			db, err := sql.Open("sqlite", filepath.Join(stage, "snapshot", "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			var value string
			err = db.QueryRow("SELECT body FROM sample").Scan(&value)
			db.Close()
			if err != nil || value != "synthetic downloaded backup" {
				t.Fatal("recovered DB unreadable", err)
			}
			entries, err := os.ReadDir(outDir)
			if err != nil || len(entries) != 1 {
				t.Fatal("private download temp left behind", err)
			}
		})
	}
}

func TestDownloadRejectsUntrustedBytesAndConcurrentOutputWithoutLeaking(t *testing.T) {
	_, receipt, data := fixture(t)
	d, c, object := downloadDestination("webdav")
	for _, scenario := range []string{"wrong-hash", "size-limit", "plaintext", "forbidden", "redirect", "concurrent-output", "cancel", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "out.age")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "forbidden":
					w.WriteHeader(403)
					io.WriteString(w, c.Password)
					return
				case "redirect":
					w.Header().Set("Location", "https://foreign.example.invalid/?secret="+c.Password)
					w.WriteHeader(302)
					return
				case "concurrent-output":
					if err := os.WriteFile(out, []byte("other owner"), 0o600); err != nil {
						t.Error(err)
					}
				case "cancel":
					cancel()
					return
				case "timeout":
					<-r.Context().Done()
					return
				case "plaintext":
					io.WriteString(w, "synthetic plaintext must not be published")
					return
				}
				w.Write(data)
			}))
			defer server.Close()
			opts := DefaultOptions()
			hash := receipt
			if scenario == "wrong-hash" {
				hash = strings.Repeat("0", 64)
			}
			if scenario == "size-limit" {
				opts.MaxBytes = int64(len(data) - 1)
			}
			if scenario == "plaintext" {
				h := sha256.Sum256([]byte("synthetic plaintext must not be published"))
				hash = hex.EncodeToString(h[:])
			}
			if scenario == "timeout" {
				opts.AttemptTimeout = 10 * time.Millisecond
			}
			_, err := download(ctx, d, object, hash, out, opts, c, testClient(t, server))
			if err == nil || strings.Contains(err.Error(), c.Password) || strings.Contains(err.Error(), "foreign.example") {
				t.Fatal("failed transfer published or leaked provider detail", err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "concurrent-output" {
				got, _ := os.ReadFile(out)
				if string(got) != "other owner" || len(entries) != 1 {
					t.Fatal("concurrent output was removed or overwritten")
				}
			} else if len(entries) != 0 {
				t.Fatal("failed download left published/temp files")
			}
		})
	}
}

func TestDownloadPreflightRejectsForeignObjectsBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"s3", "webdav", "drive"} {
		d, c, _ := downloadDestination(kind)
		for _, object := range []string{"../secret", "/secret", downloadName + "?secret=1", "foreign/" + downloadName, "plain-file.txt"} {
			if kind == "drive" && ValidDriveID(object) {
				continue
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unsafe object reached network")
				return nil, io.EOF
			})}
			_, err := download(t.Context(), d, object, strings.Repeat("a", 64), filepath.Join(t.TempDir(), "out.age"), DefaultOptions(), c, client)
			if err == nil {
				t.Error("unsafe key accepted", kind, object)
			}
		}
	}
}

func TestDownloadDriveRequiresMatchingFolderDestinationAndReceipt(t *testing.T) {
	_, receipt, data := fixture(t)
	d, c, object := downloadDestination("drive")
	for _, scenario := range []string{"wrong-parent", "wrong-destination", "wrong-receipt", "huge-metadata", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var media atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("alt") == "media" {
					media.Add(1)
					w.Write(data)
					return
				}
				folder, destination, sha := d.Drive.FolderID, d.ID, receipt
				switch scenario {
				case "wrong-parent":
					folder = "other-folder"
				case "wrong-destination":
					destination = "other-target"
				case "wrong-receipt":
					sha = strings.Repeat("0", 64)
				case "huge-metadata":
					io.WriteString(w, strings.Repeat("x", (64<<10)+1))
					return
				case "redirect":
					w.Header().Set("Location", "https://foreign.example.invalid/secret")
					w.WriteHeader(302)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"parents": []string{folder}, "appProperties": map[string]string{"lanqinDestination": destination, "lanqinSHA256": sha}})
			}))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "out.age")
			if _, err := download(t.Context(), d, object, receipt, out, DefaultOptions(), c, testClient(t, server)); err == nil {
				t.Fatal("foreign Drive object accepted")
			}
			if media.Load() != 0 {
				t.Fatal("foreign Drive bytes were fetched")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("foreign Drive object published")
			}
		})
	}
}

func TestDownloadProductionClientPreservesGetOnlyNetworkPolicy(t *testing.T) {
	for _, kind := range []string{"s3", "webdav", "drive"} {
		d, _, _ := downloadDestination(kind)
		client, closeClient := downloadClient(d)
		transport := client.Transport.(*restrictedTransport)
		if transport.base.Proxy != nil || !transport.methods[http.MethodGet] || transport.methods[http.MethodPut] {
			t.Fatal("download widened upload/network policy")
		}
		request, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/secret", nil)
		if _, err := client.Do(request); err == nil {
			t.Fatal("insecure/private request not blocked")
		}
		closeClient()
	}
}
