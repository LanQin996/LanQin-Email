package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func TestDriveResumeRejectsUntrustedProgressAndReadback(t *testing.T) {
	for _, scenario := range []string{"wrong-origin", "invalid-range", "no-progress", "corrupt-readback", "wrong-parent", "wrong-file-id"} {
		t.Run(scenario, func(t *testing.T) {
			input, receipt, data := fixture(t)
			var metadata map[string]any
			dataPuts, mediaGets := 0, 0
			result := func() map[string]any {
				digest := md5.Sum(data)
				id := "reserved-file"
				if scenario == "wrong-file-id" {
					id = "unrelated-file"
				}
				return map[string]any{"id": id, "size": strconv.Itoa(len(data)), "md5Checksum": hex.EncodeToString(digest[:]), "parents": []string{"folder_resume"}, "appProperties": metadata["appProperties"]}
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/drive/v3/files/generateIds":
					json.NewEncoder(w).Encode(map[string]any{"ids": []string{"reserved-file"}})
				case r.Method == http.MethodPost:
					if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
						t.Error(err)
					}
					endpoint := driveUploadEndpoint + "?upload_id=synthetic-session"
					if scenario == "wrong-origin" {
						endpoint = "https://foreign.example.invalid/?upload_id=synthetic-session"
					}
					w.Header().Set("Location", endpoint)
				case r.Method == http.MethodPut && r.ContentLength == 0:
					if scenario == "invalid-range" {
						w.Header().Set("Range", fmt.Sprintf("bytes=1-%d", len(data)-1))
					}
					w.WriteHeader(308)
				case r.Method == http.MethodPut:
					dataPuts++
					body, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, data) {
						t.Error("different ciphertext was uploaded")
					}
					if scenario == "no-progress" {
						w.WriteHeader(308)
						return
					}
					json.NewEncoder(w).Encode(result())
				case r.URL.Query().Get("alt") == "media":
					mediaGets++
					copy := append([]byte{}, data...)
					if scenario == "corrupt-readback" {
						copy[len(copy)-1] ^= 1
					}
					w.Write(copy)
				default:
					value := result()
					if scenario == "wrong-parent" {
						value["parents"] = []string{"unrelated-folder"}
					}
					json.NewEncoder(w).Encode(value)
				}
			}))
			defer server.Close()
			store, err := OpenFileCheckpointStore(privateCheckpointDirectory(t), "validation", bytes.Repeat([]byte{0x56}, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			opts := DefaultOptions()
			opts.Attempts = 1
			opts.Checkpoints = store
			d := driveDestination("resume")
			report, err := upload(t.Context(), input, receipt, Config{Version: 1, Destinations: []Destination{d}}, opts, func(d Destination) (uploader, error) {
				return buildUploader(d, func(string) (string, bool) { return "synthetic-credential", true }, testClient(t, server))
			})
			if err == nil || len(report.Destinations) != 1 || report.Destinations[0].State != "failed" || report.Destinations[0].ObjectKey != "" {
				t.Fatal("unverified resumable completion accepted", report, err)
			}
			if (scenario == "wrong-origin" || scenario == "invalid-range") && dataPuts != 0 {
				t.Fatal("invalid session/progress reached data upload")
			}
			if scenario == "no-progress" && dataPuts != 1 {
				t.Fatal("unbounded non-progressing upload loop")
			}
			if scenario == "wrong-parent" && mediaGets != 0 {
				t.Fatal("readback crossed Drive folder boundary")
			}
		})
	}
}

func TestResumeRejectsChangedSourceAndCredentialsBeforeNetwork(t *testing.T) {
	input, receipt, data := fixture(t)
	for _, scenario := range []string{"source", "credentials", "type", "name"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := OpenFileCheckpointStore(privateCheckpointDirectory(t), "validation", bytes.Repeat([]byte{0x56}, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			d := s3Destination("resume")
			c := Credentials{AccessKeyID: "synthetic-credential", SecretAccessKey: "synthetic-credential"}
			state := Checkpoint{Version: 1, Type: "s3", SHA256: receipt, Bytes: int64(len(data)), Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "prepared", Binding: transferBinding(d, c)}
			switch scenario {
			case "source":
				state.SHA256 = hex.EncodeToString(make([]byte, 32))
			case "credentials":
				state.Binding = "different-credential-generation"
			case "type":
				state.Type = "drive"
			case "name":
				state.Name = "../unrelated"
			}
			if err := store.Save(t.Context(), d.ID, state); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("mismatched checkpoint contacted storage") }))
			defer server.Close()
			opts := DefaultOptions()
			opts.Checkpoints = store
			opts.Attempts = 1
			report, err := upload(t.Context(), input, receipt, Config{Version: 1, Destinations: []Destination{d}}, opts, func(d Destination) (uploader, error) {
				return buildUploader(d, func(string) (string, bool) { return "synthetic-credential", true }, testClient(t, server))
			})
			if err == nil || report.Destinations[0].ErrorCode != "checkpoint_mismatch" {
				t.Fatal("mismatched checkpoint accepted", err)
			}
		})
	}
}

func TestDriveOwnedPlaceholderIsNotMistakenForCompletedOrConflictingFile(t *testing.T) {
	input, receipt, _ := fixture(t)
	opts := DefaultOptions()
	a, err := stage(t.Context(), input, receipt, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.file.Close(); os.Remove(a.file.Name()) }()
	state := &Checkpoint{Phase: "completing", FileID: "saved-file", Name: "lanqin-0123456789abcdef0123456789abcdef.age", Session: driveUploadEndpoint + "?upload_id=synthetic-session"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			t.Error("placeholder caused unverified media read")
		}
		json.NewEncoder(w).Encode(map[string]any{"id": state.FileID, "size": "0", "parents": []string{"folder_resume"}, "appProperties": map[string]string{"lanqinSHA256": receipt, "lanqinDestination": "resume", "lanqinTransfer": state.Name}})
	}))
	defer server.Close()
	d := &driveUploader{client: testClient(t, server), folder: "folder_resume", destination: "resume", token: "synthetic-token"}
	if found, err := d.verifyTransferObject(t.Context(), state, a); err != nil || found {
		t.Fatal("owned upload placeholder blocked continuation", err)
	}
	state.Phase = "completed"
	if _, err := d.verifyTransferObject(t.Context(), state, a); err == nil {
		t.Fatal("placeholder accepted as completed checkpoint")
	}
}
