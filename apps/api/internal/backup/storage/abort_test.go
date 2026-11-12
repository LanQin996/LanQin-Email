package storage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMultipartAbortNeverDeletesCompletedObjects(t *testing.T) {
	for _, status := range []int{204, 404, 403, 202, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodDelete || r.URL.Query().Get("uploadId") != "synthetic-multipart-secret" || r.URL.Path != "/backup-bucket/resume/lanqin-0123456789abcdef0123456789abcdef.age" {
					t.Error("cleanup issued an object delete or wrong session request")
				}
				w.Header().Set("Location", "https://foreign.example.invalid/")
				w.WriteHeader(status)
				if status >= 400 {
					w.Write([]byte("<Error><Code>AccessDenied</Code><Message>synthetic-credential-must-not-leak</Message></Error>"))
				}
			}))
			defer server.Close()
			d := s3Destination("resume")
			uploader, err := buildUploader(d, func(string) (string, bool) { return "synthetic-credential", true }, testClient(t, server))
			if err != nil {
				t.Fatal(err)
			}
			defer uploader.close()
			err = uploader.(*s3Uploader).abort(t.Context(), Checkpoint{Name: "lanqin-0123456789abcdef0123456789abcdef.age", UploadID: "synthetic-multipart-secret"})
			wantSuccess := status == 204 || status == 404
			if (err == nil) != wantSuccess || requests != 1 {
				t.Fatal("unexpected cleanup response or retry", status, requests, err)
			}
			if err != nil && (strings.Contains(err.Error(), "synthetic-credential") || strings.Contains(err.Error(), "synthetic-multipart-secret")) {
				t.Fatal("cleanup leaked provider/session details")
			}
		})
	}
}

func TestMultipartAbortRejectsUnboundCheckpointBeforeNetworking(t *testing.T) {
	d := s3Destination("resume")
	c := Credentials{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret"}
	state := Checkpoint{Version: 1, Type: "s3", SHA256: strings.Repeat("a", 64), Bytes: 100, Name: "lanqin-0123456789abcdef0123456789abcdef.age", Phase: "uploading", UploadID: "synthetic-session", Binding: transferBinding(d, c)}
	for _, mutate := range []func(*Checkpoint){
		func(s *Checkpoint) { s.Binding = "different-target" },
		func(s *Checkpoint) { s.Name = "../unrelated" },
		func(s *Checkpoint) { s.SHA256 = strings.Repeat("b", 64) },
		func(s *Checkpoint) { s.Bytes = 101 },
		func(s *Checkpoint) { s.UploadID = "" },
	} {
		copy := state
		mutate(&copy)
		if err := AbortCheckpoint(t.Context(), d, copy, state.SHA256, state.Bytes, c); err == nil {
			t.Fatal("unbound checkpoint authorized a remote abort")
		}
	}
}
