package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetentionProtocolsVerifyBeforeConditionalDelete(t *testing.T) {
	_, hash, data := fixture(t)
	for _, kind := range []string{"s3", "webdav", "drive"} {
		for _, scenario := range []string{"success", "wrong-hash", "changed", "weak-tag", "no-tag", "wrong-parent", "delete-async", "redirect", "missing", "revoked-during-readback"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				d, c, object := downloadDestination(kind)
				var deletes atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "DELETE" {
						deletes.Add(1)
						if r.Header.Get("If-Match") != `"synthetic-version"` {
							t.Error("unconditional delete")
						}
						if scenario == "changed" {
							w.WriteHeader(412)
						} else if scenario == "delete-async" {
							w.WriteHeader(202)
						} else {
							w.WriteHeader(204)
						}
						return
					}
					if r.Method != "GET" {
						t.Error("unexpected method", r.Method)
					}
					if scenario == "missing" {
						w.WriteHeader(404)
						return
					}
					if scenario == "redirect" {
						w.Header().Set("Location", "https://unrelated.example.invalid/")
						w.WriteHeader(302)
						return
					}
					if scenario != "no-tag" {
						tag := `"synthetic-version"`
						if scenario == "weak-tag" {
							tag = "W/" + tag
						}
						w.Header().Set("ETag", tag)
					}
					if kind == "drive" && r.URL.Query().Get("alt") == "" {
						folder := d.Drive.FolderID
						if scenario == "wrong-parent" {
							folder = "unrelated-folder"
						}
						json.NewEncoder(w).Encode(map[string]any{"parents": []string{folder}, "appProperties": map[string]string{"lanqinDestination": d.ID, "lanqinSHA256": hash}})
						return
					}
					if scenario == "wrong-hash" {
						w.Write(append(append([]byte{}, data...), 'x'))
					} else {
						w.Write(data)
					}
				}))
				defer server.Close()
				err := retainRemote(t.Context(), d, object, hash, int64(len(data)), c, true, testClient(t, server), func(ctx context.Context) error {
					if scenario == "revoked-during-readback" {
						return errors.New("synthetic authorization revoked")
					}
					return ctx.Err()
				})
				success := scenario == "success" || scenario == "wrong-parent" && kind != "drive"
				if (err == nil) != success {
					t.Fatal("unexpected result", err)
				}
				expectDelete := success || scenario == "changed" || scenario == "delete-async"
				if (deletes.Load() == 1) != expectDelete {
					t.Fatal("unsafe delete count", deletes.Load())
				}
				if err != nil && (strings.Contains(err.Error(), c.Password) && c.Password != "" || strings.Contains(err.Error(), c.SecretAccessKey) && c.SecretAccessKey != "") {
					t.Fatal("credentials leaked")
				}
			})
		}
	}
}

func TestRetentionRejectsUnownedObjectsBeforeNetwork(t *testing.T) {
	_, hash, data := fixture(t)
	for _, kind := range []string{"s3", "webdav"} {
		d, c, _ := downloadDestination(kind)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid object reached network") }))
		defer server.Close()
		for _, object := range []string{"../lanqin-0123456789abcdef0123456789abcdef.age", "other/lanqin-0123456789abcdef0123456789abcdef.age", "unrelated.age", "", "/"} {
			if err := retainRemote(t.Context(), d, object, hash, int64(len(data)), c, true, testClient(t, server)); err == nil {
				t.Fatal("unowned object accepted")
			}
		}
	}
}
