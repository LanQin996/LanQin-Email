package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func driveDestination(id string) Destination {
	return Destination{ID: id, Name: id, Type: "drive", Drive: &DriveConfig{FolderID: "folder_" + id, AccessTokenEnv: id + "_TOKEN"}}
}

type driveUploadMetadata struct {
	Name          string            `json:"name"`
	Parents       []string          `json:"parents"`
	AppProperties map[string]string `json:"appProperties"`
}

func driveSuccess(t *testing.T, w http.ResponseWriter, data []byte, id string, metadata driveUploadMetadata) {
	t.Helper()
	md := md5.Sum(data)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"id": id, "size": strconv.Itoa(len(data)), "md5Checksum": hex.EncodeToString(md[:]), "appProperties": metadata.AppProperties}); err != nil {
		t.Error(err)
	}
}

func TestDriveMultipleDestinationsIsolationRetriesAndProviderObjectIDs(t *testing.T) {
	file, receipt, data := fixture(t)
	var mu sync.Mutex
	sessions := map[string]driveUploadMetadata{}
	names := map[string][]string{}
	puts := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "www.googleapis.com" || r.URL.Path != "/upload/drive/v3/files" {
			t.Error("unexpected provider origin/path")
			w.WriteHeader(400)
			return
		}
		if r.Method == "POST" {
			var metadata driveUploadMetadata
			if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
				t.Error(err)
			}
			id := metadata.AppProperties["lanqinDestination"]
			if metadata.AppProperties["lanqinSHA256"] != receipt || len(metadata.Parents) != 1 || metadata.Parents[0] != "folder_"+id || r.Header.Get("Authorization") != "Bearer "+id+"-synthetic-token" || r.Header.Get("X-Upload-Content-Length") != strconv.Itoa(len(data)) || r.Header.Get("X-Upload-Content-Type") != "application/octet-stream" || r.URL.Query().Get("uploadType") != "resumable" {
				t.Error("cross-target credentials/folder or invalid resumable metadata")
			}
			mu.Lock()
			names[id] = append(names[id], metadata.Name)
			session := fmt.Sprintf("%s_%d", id, len(names[id]))
			sessions[session] = metadata
			mu.Unlock()
			w.Header().Set("Location", "https://www.googleapis.com/upload/drive/v3/files?upload_id="+session)
			w.WriteHeader(200)
			return
		}
		if r.Method != "PUT" {
			t.Error("unexpected remote mutation")
			w.WriteHeader(400)
			return
		}
		session := r.URL.Query().Get("upload_id")
		mu.Lock()
		metadata, ok := sessions[session]
		id := metadata.AppProperties["lanqinDestination"]
		puts[id]++
		attempt := puts[id]
		mu.Unlock()
		body, err := io.ReadAll(r.Body)
		if !ok || err != nil || !bytes.Equal(body, data) || r.ContentLength != int64(len(data)) || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)) || r.Header.Get("Authorization") != "Bearer "+id+"-synthetic-token" {
			t.Error("wrong independent content stream, token, framing or session")
		}
		if id == "forbidden" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte("do-not-log-forbidden-synthetic-token"))
			return
		}
		if id == "first" && attempt == 1 {
			w.WriteHeader(503)
			return
		}
		driveSuccess(t, w, data, "file_"+session, metadata)
	}))
	defer server.Close()
	client := testClient(t, server)
	cfg := Config{Version: 1, Destinations: []Destination{driveDestination("first"), driveDestination("second"), driveDestination("forbidden")}}
	opts := DefaultOptions()
	opts.Attempts = 2
	opts.SpoolDir = t.TempDir()
	report, err := upload(t.Context(), file, receipt, cfg, opts, func(d Destination) (uploader, error) {
		return buildUploader(d, func(ref string) (string, bool) { return d.ID + "-synthetic-token", ref == d.Drive.AccessTokenEnv }, client)
	})
	if !errors.Is(err, ErrPartialFailure) || len(report.Destinations) != 3 {
		t.Fatal("expected independent partial result", err)
	}
	for i, result := range report.Destinations {
		if i == 2 {
			if result.State != "failed" || result.ErrorCode != "access_denied" || result.Attempts != 1 || result.ObjectKey != "" {
				t.Fatal("failed target became successful", result)
			}
		} else if result.State != "succeeded" || !strings.HasPrefix(result.ObjectKey, "file_"+cfg.Destinations[i].ID+"_") || result.CompletedAt == nil {
			t.Fatal("provider object ID not retained", result)
		}
	}
	if report.Destinations[0].Attempts != 2 || len(names["first"]) != 2 || names["first"][0] == names["first"][1] || len(names["second"]) != 1 {
		t.Fatal("retry reused object name or affected another target")
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "synthetic-token") || strings.Contains(string(encoded), "upload_id") {
		t.Fatal("secret/session exposed in public report")
	}
}

func TestDriveRejectsUntrustedSessionsAndInvalidCompletion(t *testing.T) {
	for _, scenario := range []string{"http-session", "foreign-host", "wrong-path", "userinfo", "fragment", "missing-id", "308", "wrong-md5", "wrong-size", "wrong-sha", "wrong-destination", "missing-object-id", "huge-body", "error-body", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			file, receipt, data := fixture(t)
			puts := 0
			var metadata driveUploadMetadata
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
						t.Error(err)
					}
					location := "https://www.googleapis.com/upload/drive/v3/files?upload_id=synthetic-session-secret"
					switch scenario {
					case "http-session":
						location = strings.Replace(location, "https:", "http:", 1)
					case "foreign-host":
						location = strings.Replace(location, "www.googleapis.com", "evil.example.invalid", 1)
					case "wrong-path":
						location = strings.Replace(location, "/upload/drive/v3/files", "/arbitrary", 1)
					case "userinfo":
						location = strings.Replace(location, "https://", "https://user:secret@", 1)
					case "fragment":
						location += "#secret"
					case "missing-id":
						location = "https://www.googleapis.com/upload/drive/v3/files"
					case "redirect":
						w.Header().Set("Location", "https://evil.example.invalid/")
						w.WriteHeader(302)
						return
					}
					w.Header().Set("Location", location)
					w.WriteHeader(200)
					return
				}
				puts++
				_, _ = io.Copy(io.Discard, r.Body)
				if scenario == "308" {
					w.Header().Set("Range", "bytes=0-1")
					w.WriteHeader(308)
					return
				}
				if scenario == "error-body" {
					w.WriteHeader(403)
					_, _ = w.Write([]byte("synthetic-access-token synthetic-session-secret"))
					return
				}
				if scenario == "huge-body" {
					_, _ = w.Write([]byte(strings.Repeat("synthetic-access-token", 4000)))
					return
				}
				md := md5.Sum(data)
				result := map[string]any{"id": "drive-file", "size": strconv.Itoa(len(data)), "md5Checksum": hex.EncodeToString(md[:]), "appProperties": metadata.AppProperties}
				switch scenario {
				case "wrong-md5":
					result["md5Checksum"] = strings.Repeat("0", 32)
				case "wrong-size":
					result["size"] = "1"
				case "wrong-sha":
					metadata.AppProperties["lanqinSHA256"] = "wrong"
				case "wrong-destination":
					metadata.AppProperties["lanqinDestination"] = "different-target"
				case "missing-object-id":
					result["id"] = ""
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			d := driveDestination("first")
			cfg := Config{Version: 1, Destinations: []Destination{d}}
			opts := DefaultOptions()
			opts.Attempts = 1
			opts.SpoolDir = t.TempDir()
			report, err := upload(t.Context(), file, receipt, cfg, opts, func(d Destination) (uploader, error) {
				return buildUploader(d, func(string) (string, bool) { return "synthetic-access-token", true }, testClient(t, server))
			})
			if !errors.Is(err, ErrPartialFailure) || report.Destinations[0].State != "failed" || report.Destinations[0].ObjectKey != "" {
				t.Fatal("invalid completion marked successful", err)
			}
			if strings.HasSuffix(scenario, "session") || scenario == "foreign-host" || scenario == "wrong-path" || scenario == "userinfo" || scenario == "fragment" || scenario == "missing-id" || scenario == "redirect" {
				if puts != 0 {
					t.Fatal("untrusted session followed")
				}
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "synthetic-access-token") || strings.Contains(string(encoded), "synthetic-session-secret") {
				t.Fatal("provider secrets echoed")
			}
		})
	}
}

func TestDriveFolderCreationIsScopedAndBounded(t *testing.T) {
	for _, scenario := range []string{"valid", "redirect", "bad-id", "large"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var metadata struct {
					Name       string            `json:"name"`
					MIME       string            `json:"mimeType"`
					Properties map[string]string `json:"appProperties"`
				}
				if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
					t.Error(err)
				}
				if r.Host != "www.googleapis.com" || r.URL.Path != "/drive/v3/files" || r.Method != "POST" || r.URL.Query().Get("fields") != "id" || metadata.Name != "LanQin Backups - Synthetic" || metadata.MIME != "application/vnd.google-apps.folder" || metadata.Properties["lanqinDestination"] != "first" || r.Header.Get("Authorization") != "Bearer synthetic-access-token" {
					t.Error("folder request crossed provider/destination boundary")
				}
				switch scenario {
				case "redirect":
					w.Header().Set("Location", "https://evil.example.invalid/")
					w.WriteHeader(302)
				case "bad-id":
					_, _ = w.Write([]byte(`{"id":"../unsafe"}`))
				case "large":
					_, _ = w.Write([]byte(strings.Repeat("synthetic-access-token", 4000)))
				default:
					_, _ = w.Write([]byte(`{"id":"app_owned_folder"}`))
				}
			}))
			defer server.Close()
			id, err := CreateDriveBackupFolder(t.Context(), testClient(t, server), "synthetic-access-token", "Synthetic", "first")
			if scenario == "valid" {
				if err != nil || id != "app_owned_folder" {
					t.Fatal(err)
				}
			} else if err == nil || id != "" || strings.Contains(err.Error(), "synthetic-access-token") {
				t.Fatal("unsafe folder response accepted or token exposed")
			}
			if requests != 1 {
				t.Fatal("followed redirect")
			}
		})
	}
}
