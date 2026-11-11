package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup"
)

type recoveryProtocolFixture struct {
	d                                  Destination
	c                                  Credentials
	object, sha                        string
	data                               []byte
	indexes                            map[string][]byte
	files                              map[string]driveRecoveryFile
	client                             *http.Client
	writes, generates                  int
	badReadback, badIndex, wrongFolder bool
}

func newRecoveryProtocolFixture(t *testing.T, kind string, data []byte, sha string) *recoveryProtocolFixture {
	t.Helper()
	d, c, object := downloadDestination(kind)
	f := &recoveryProtocolFixture{d: d, c: c, object: object, sha: sha, data: data, indexes: map[string][]byte{}, files: map[string]driveRecoveryFile{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch kind {
		case "s3":
			if !strings.Contains(r.Header.Get("Authorization"), "Credential="+c.AccessKeyID+"/") || r.Header.Get("X-Amz-Security-Token") != c.SessionToken {
				t.Error("recovery S3 used wrong/ambient credentials")
			}
		case "webdav":
			u, p, ok := r.BasicAuth()
			if !ok || u != c.Username || p != c.Password {
				t.Error("recovery DAV used wrong credentials")
			}
		case "drive":
			if r.Host != "www.googleapis.com" || r.Header.Get("Authorization") != "Bearer "+c.DriveAccessToken {
				t.Error("recovery Drive used wrong origin/credentials")
			}
		}
		if kind == "drive" {
			f.serveDrive(t, w, r)
			return
		}
		base := "/existing/" + d.ID + "/"
		if kind == "s3" {
			base = "/backup-bucket/"
		}
		if kind == "s3" && r.URL.Query().Get("list-type") == "2" {
			if r.Method != "GET" || r.URL.Query().Get("prefix") != d.S3.Prefix+"/lanqin-" {
				t.Error("S3 discovery escaped selected prefix")
			}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated>")
			for name, data := range f.indexes {
				fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", name, len(data))
			}
			io.WriteString(w, "</ListBucketResult>")
			return
		}
		if r.Method == "PROPFIND" {
			if kind != "webdav" || r.URL.Path != base || r.Header.Get("Depth") != "1" {
				t.Error("DAV discovery used unexpected depth/path")
			}
			w.WriteHeader(207)
			io.WriteString(w, `<d:multistatus xmlns:d="DAV:">`)
			fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, base)
			for name := range f.indexes {
				fmt.Fprintf(w, `<d:response><d:href>%s%s</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, base, name)
			}
			io.WriteString(w, `</d:multistatus>`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, base) {
			t.Error("unexpected recovery path", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, base)
		if r.Method == "GET" && name == object {
			if f.badReadback {
				io.WriteString(w, "bad ciphertext")
				return
			}
			w.Write(data)
			return
		}
		if r.Method == "PUT" {
			f.writes++
			if r.Header.Get("If-None-Match") != "*" || name != object+recoveryIndexSuffix {
				t.Error("index publication could overwrite or escape object")
			}
			if _, exists := f.indexes[name]; exists {
				w.WriteHeader(412)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxRecoveryIndexBytes+1))
			if err != nil || len(body) > MaxRecoveryIndexBytes {
				t.Error("unbounded document upload")
			}
			f.indexes[name] = body
			w.WriteHeader(200)
			return
		}
		if r.Method == "GET" {
			body, exists := f.indexes[name]
			if !exists {
				w.WriteHeader(404)
				return
			}
			if f.badIndex {
				io.WriteString(w, `{"forged":true}`)
				return
			}
			w.Write(body)
			return
		}
		w.WriteHeader(405)
	}))
	t.Cleanup(server.Close)
	f.client = testClient(t, server)
	return f
}

func (f *recoveryProtocolFixture) serveDrive(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method == "GET" && r.URL.Path == "/drive/v3/files" {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "'"+f.d.Drive.FolderID+"' in parents") || !strings.Contains(q, "lanqinRecoveryIndex") || r.URL.Query().Get("spaces") != "drive" {
			t.Error("Drive discovery escaped bound folder")
		}
		files := []driveRecoveryFile{}
		for _, file := range f.files {
			if !strings.Contains(q, "name = '") || strings.Contains(q, "name = '"+file.Name+"'") {
				files = append(files, file)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/drive/v3/files/generateIds" {
		f.generates++
		json.NewEncoder(w).Encode(map[string]any{"ids": []string{"synthetic-index-" + strconv.Itoa(f.generates)}})
		return
	}
	if r.Method == "POST" && r.URL.Path == "/upload/drive/v3/files" {
		f.writes++
		if r.URL.Query().Get("uploadType") != "multipart" || r.Header.Get("If-None-Match") != "*" {
			t.Error("Drive index did not use independent conditional create")
		}
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "multipart/related" {
			t.Error("invalid Drive multipart content type")
			w.WriteHeader(400)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		meta, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		var file driveRecoveryFile
		if json.NewDecoder(meta).Decode(&file) != nil || !ValidDriveID(file.ID) || file.ID == f.object {
			t.Error("Drive create reused original object")
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		document, err := io.ReadAll(io.LimitReader(part, MaxRecoveryIndexBytes+1))
		if err != nil || len(document) > MaxRecoveryIndexBytes {
			t.Error("invalid Drive sidecar")
		}
		if _, err := reader.NextPart(); err != io.EOF {
			t.Error("unexpected multipart tail")
		}
		file.Size = strconv.Itoa(len(document))
		f.files[file.ID] = file
		f.indexes[file.ID] = document
		json.NewEncoder(w).Encode(map[string]string{"id": file.ID})
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/drive/v3/files/") {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		if id == f.object {
			if r.URL.Query().Get("alt") == "media" {
				if f.badReadback {
					io.WriteString(w, "bad ciphertext")
					return
				}
				w.Write(f.data)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"parents": []string{f.d.Drive.FolderID}, "appProperties": map[string]string{"lanqinDestination": f.d.ID, "lanqinSHA256": f.sha}})
			return
		}
		file, exists := f.files[id]
		if !exists {
			w.WriteHeader(404)
			return
		}
		if f.wrongFolder {
			file.Parents = []string{"foreign-folder"}
		}
		if r.URL.Query().Get("alt") == "media" {
			if f.badIndex {
				io.WriteString(w, `{"forged":true}`)
				return
			}
			w.Write(f.indexes[id])
			return
		}
		json.NewEncoder(w).Encode(file)
		return
	}
	t.Error("unexpected Drive recovery request", r.Method, r.URL.Path)
	w.WriteHeader(404)
}

func TestRecoveryRemoteProtocolsPublishDiscoverAndActualRecovery(t *testing.T) {
	data, sha, identity, _ := downloadArchiveFixture(t)
	key := bytes.Repeat([]byte{0x34}, 32)
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-must-not-be-used")
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryProtocolFixture(t, kind, data, sha)
			at := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
			if err := publishRecoveryIndex(t.Context(), f.d, f.object, sha, int64(len(data)), at, key, f.c, f.client); err != nil {
				t.Fatal("publication", err)
			}
			if err := publishRecoveryIndex(t.Context(), f.d, f.object, sha, int64(len(data)), at.Add(time.Minute), key, f.c, f.client); err != nil {
				t.Fatal("idempotent publication", err)
			}
			if len(f.indexes) != 1 || kind == "drive" && f.writes != 1 {
				t.Fatal("retry multiplied Drive or replaced sidecar", f.writes, len(f.indexes))
			}
			fresh := f.d
			fresh.ID = "new-instance-destination"
			entries, err := discoverRecoveryIndexes(t.Context(), fresh, key, f.c, f.client)
			if err != nil || len(entries) != 1 || entries[0].DestinationID != f.d.ID || entries[0].ObjectKey != f.object || entries[0].SHA256 != sha || entries[0].Bytes != int64(len(data)) || !entries[0].CompletedAt.Equal(at) {
				t.Fatal("new instance recovery discovery", entries, err)
			}
			output := filepath.Join(t.TempDir(), "cloud.age")
			fresh.ID = entries[0].DestinationID // Drive binds the original appProperty.
			if _, err := download(t.Context(), fresh, entries[0].ObjectKey, entries[0].SHA256, output, DefaultOptions(), f.c, f.client); err != nil {
				t.Fatal("discovered receipt download", err)
			}
			downloaded, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(downloaded, data) {
				t.Fatal("discovered ciphertext bytes changed", err)
			}
			if _, err := backup.Restore(t.Context(), bytes.NewReader(downloaded), filepath.Join(t.TempDir(), "recovered"), identity, entries[0].SHA256, backup.DefaultLimits()); err != nil {
				t.Fatal("discovered receipt age restore", err)
			}
			for _, wrongKey := range [][]byte{bytes.Repeat([]byte{0x35}, 32), nil} {
				if _, err := discoverRecoveryIndexes(t.Context(), f.d, wrongKey, f.c, f.client); err == nil {
					t.Fatal("discovery accepted wrong trust root")
				}
			}
			f.badIndex = true
			if _, err := discoverRecoveryIndexes(t.Context(), f.d, key, f.c, f.client); err == nil {
				t.Fatal("forged descriptor silently ignored")
			}
		})
	}
}

func TestRecoveryRemoteUploaderUsesExactInMemoryCredentials(t *testing.T) {
	_, sha, data := fixture(t)
	key := bytes.Repeat([]byte{0x38}, 32)
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryProtocolFixture(t, kind, data, sha)
			if f.d.S3 != nil {
				f.d.S3.SessionTokenEnv = "RECOVERY_SESSION"
			}
			values := map[string]string{}
			if f.d.S3 != nil {
				values[f.d.S3.AccessKeyIDEnv] = f.c.AccessKeyID
				values[f.d.S3.SecretAccessKeyEnv] = f.c.SecretAccessKey
				values[f.d.S3.SessionTokenEnv] = f.c.SessionToken
			}
			if f.d.WebDAV != nil {
				values[f.d.WebDAV.UsernameEnv] = f.c.Username
				values[f.d.WebDAV.PasswordEnv] = f.c.Password
			}
			if f.d.Drive != nil {
				values[f.d.Drive.AccessTokenEnv] = f.c.DriveAccessToken
			}
			uploader, err := buildUploader(f.d, func(name string) (string, bool) { value, ok := values[name]; return value, ok }, f.client)
			if err != nil {
				t.Fatal(err)
			}
			defer uploader.close()
			publisher, ok := uploader.(recoveryIndexPublisher)
			if !ok {
				t.Fatal("concrete uploader does not publish recovery descriptors")
			}
			if err := publisher.publishRecoveryIndex(t.Context(), f.object, sha, int64(len(data)), time.Now().UTC(), key, nil); err != nil {
				t.Fatal("concrete uploader recovery descriptor", err)
			}
			if f.writes != 1 {
				t.Fatal("uploader did not publish exactly one descriptor", f.writes)
			}
		})
	}
}

func TestRecoveryRemoteRejectsDiscoveryBudgetsAndUnsafePagination(t *testing.T) {
	key := bytes.Repeat([]byte{0x39}, 32)
	for _, kind := range []string{"s3", "drive", "webdav"} {
		for _, scenario := range []string{"huge-body", "pages", "repeated-token", "too-many", "incomplete"} {
			if kind == "webdav" && scenario != "huge-body" {
				continue
			}
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				d, c, _ := downloadDestination(kind)
				requests := 0
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if scenario == "huge-body" {
						if kind == "webdav" {
							w.WriteHeader(207)
						}
						io.WriteString(w, strings.Repeat("x", maxRecoveryPageBody+1))
						return
					}
					next := "page-" + strconv.Itoa(requests)
					if scenario == "repeated-token" {
						next = "same-token"
					}
					if kind == "s3" {
						if scenario == "incomplete" {
							io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
							return
						}
						io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>`+next+`</NextContinuationToken>`)
						if scenario == "too-many" {
							for range maxRecoveryIndexes + 1 {
								io.WriteString(w, `<Contents><Key>`+d.S3.Prefix+`/`+downloadName+recoveryIndexSuffix+`</Key><Size>100</Size></Contents>`)
							}
						}
						io.WriteString(w, `</ListBucketResult>`)
						return
					}
					result := map[string]any{"nextPageToken": next, "files": []driveRecoveryFile{}}
					if scenario == "incomplete" {
						result["incompleteSearch"] = true
					}
					if scenario == "too-many" {
						result["files"] = make([]driveRecoveryFile, maxRecoveryIndexes+1)
					}
					json.NewEncoder(w).Encode(result)
				}))
				defer server.Close()
				if entries, err := discoverRecoveryIndexes(t.Context(), d, key, c, testClient(t, server)); err == nil || entries != nil {
					t.Fatal("unsafe/partial discovery was returned")
				}
				if requests > maxRecoveryPages || scenario == "pages" && requests != maxRecoveryPages {
					t.Fatal("unbounded or incorrect pagination", requests)
				}
			})
		}
	}
}

func TestRecoveryRemoteRejectsDriveScopeChangesAndConflictingDuplicates(t *testing.T) {
	_, sha, data := fixture(t)
	key := bytes.Repeat([]byte{0x40}, 32)
	at := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
	for _, scenario := range []string{"wrong-folder", "foreign-id", "foreign-name", "duplicate-valid", "duplicate-conflict", "duplicate-size-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveryProtocolFixture(t, "drive", data, sha)
			if err := publishRecoveryIndex(t.Context(), f.d, f.object, sha, int64(len(data)), at, key, f.c, f.client); err != nil {
				t.Fatal(err)
			}
			for id, item := range f.files {
				switch scenario {
				case "wrong-folder":
					item.Parents = []string{"foreign-folder"}
				case "foreign-id":
					item.Properties["lanqinDestination"] = "foreign-original-id"
				case "foreign-name":
					item.Name = "lanqin-recovery-other-object" + recoveryIndexSuffix
				case "duplicate-valid", "duplicate-conflict", "duplicate-size-mismatch":
					duplicate := item
					duplicate.ID = "duplicate-sidecar"
					sha2 := sha
					if scenario == "duplicate-conflict" {
						sha2 = strings.Repeat("a", 64)
					}
					// Different RFC3339Nano precision makes the repeated document's
					// size differ deterministically, rather than depending on Now's
					// fractional trailing zeroes and accidentally stale metadata.
					document, err := SealRecoveryIndex(f.d, f.object, sha2, int64(len(data)), at.Add(time.Minute+123456789*time.Nanosecond), key)
					if err != nil {
						t.Fatal(err)
					}
					if duplicate.Size == strconv.Itoa(len(document)) {
						t.Fatal("fixture must exercise different descriptor lengths")
					}
					if scenario != "duplicate-size-mismatch" {
						duplicate.Size = strconv.Itoa(len(document))
					}
					f.files[duplicate.ID] = duplicate
					f.indexes[duplicate.ID] = document
				}
				f.files[id] = item
				break
			}
			entries, err := discoverRecoveryIndexes(t.Context(), f.d, key, f.c, f.client)
			if scenario == "duplicate-valid" {
				if err != nil || len(entries) != 1 {
					t.Fatal("valid repeated publication not deduplicated", err)
				}
				if !entries[0].CompletedAt.Equal(at) {
					t.Fatal("repeated publication did not retain earliest completion")
				}
				return
			}
			if err == nil || entries != nil {
				t.Fatal("foreign/conflicting Drive descriptor accepted")
			}
		})
	}
}

func TestRecoveryRemoteDiscoveryCancellationAndClientPolicy(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			d, c, _ := downloadDestination(kind)
			for _, publish := range []bool{false, true} {
				client, closeClient := recoveryIndexClient(d, publish)
				transport := client.Transport.(*restrictedTransport)
				if transport.base.Proxy != nil || transport.base.TLSClientConfig.InsecureSkipVerify || !transport.methods[http.MethodGet] || transport.methods[http.MethodDelete] || transport.methods[http.MethodPatch] || transport.methods["PROPFIND"] != (kind == "webdav" && !publish) || transport.methods[http.MethodPost] != (kind == "drive" && publish) || transport.methods[http.MethodPut] != (kind != "drive" && publish) {
					t.Fatal("index client widened unrelated HTTP/TLS/network policy")
				}
				request, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
				if _, err := client.Do(request); err == nil {
					t.Fatal("insecure metadata origin accepted")
				}
				closeClient()
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("canceled discovery reached network")
				return nil, io.EOF
			})}
			if _, err := discoverRecoveryIndexes(ctx, d, key, c, client); err != context.Canceled {
				t.Fatal("canceled discovery did not preserve context", err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			}))
			defer server.Close()
			ctx, cancel = context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			if _, err := discoverRecoveryIndexes(ctx, d, key, c, testClient(t, server)); err != context.DeadlineExceeded {
				t.Fatal("deadline did not stop discovery", err)
			}
		})
	}
}

func TestRecoveryRemoteReadbackAndGuardPrecedePublication(t *testing.T) {
	_, sha, data := fixture(t)
	key := bytes.Repeat([]byte{0x36}, 32)
	for _, kind := range []string{"s3", "webdav", "drive"} {
		for _, scenario := range []string{"bad-ciphertext", "guard", "canceled", "bad-published-index"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				f := newRecoveryProtocolFixture(t, kind, data, sha)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				f.badReadback = scenario == "bad-ciphertext"
				f.badIndex = scenario == "bad-published-index"
				if scenario == "canceled" {
					cancel()
				}
				guard := func(context.Context) error {
					if scenario == "guard" {
						return fmt.Errorf("authorization changed")
					}
					return nil
				}
				err := publishRecoveryIndex(ctx, f.d, f.object, sha, int64(len(data)), time.Now().UTC(), key, f.c, f.client, guard)
				if err == nil {
					t.Fatal("unsafe index publication succeeded")
				}
				if scenario != "bad-published-index" && f.writes != 0 {
					t.Fatal("sidecar write preceded full verification/live guard")
				}
			})
		}
	}
}

func TestRecoveryDAVRejectsForeignNestedAndEncodedHrefsAndXML(t *testing.T) {
	d, c, _ := downloadDestination("webdav")
	key := bytes.Repeat([]byte{0x37}, 32)
	for _, href := range []string{"https://foreign.example.invalid/x", "/existing/other/" + downloadName + recoveryIndexSuffix, "/existing/" + d.ID + "/nested/" + downloadName + recoveryIndexSuffix, "../" + downloadName + recoveryIndexSuffix, "%2e%2e/secret", downloadName + recoveryIndexSuffix + "?q=secret", downloadName + recoveryIndexSuffix + "#fragment"} {
		t.Run(href, func(t *testing.T) {
			gets := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PROPFIND" {
					gets++
					t.Error("untrusted href fetched")
					return
				}
				w.WriteHeader(207)
				io.WriteString(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>`)
				xml.EscapeText(w, []byte(href))
				io.WriteString(w, `</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
			}))
			defer server.Close()
			if _, err := discoverRecoveryIndexes(t.Context(), d, key, c, testClient(t, server)); err == nil || gets != 0 {
				t.Fatal("unsafe href accepted")
			}
		})
	}
	for _, document := range []string{`<!DOCTYPE x [<!ENTITY payload SYSTEM "file:///secret">]><d:multistatus xmlns:d="DAV:"></d:multistatus>`, `<d:multistatus xmlns:d="DAV:">&unknown;</d:multistatus>`, strings.Repeat("<x>", 33) + strings.Repeat("</x>", 33), `<d:multistatus xmlns:d="DAV:"></d:multistatus><x/>`} {
		if _, err := strictRecoveryDAVXML([]byte(document)); err == nil {
			t.Fatal("untrusted or trailing XML accepted")
		}
	}
}

func TestRecoveryDAVRootCollectionHrefAndDirectIndex(t *testing.T) {
	d, c, object := downloadDestination("webdav")
	d.WebDAV.URL = "https://dav.example.invalid/"
	key := bytes.Repeat([]byte{0x42}, 32)
	document, err := SealRecoveryIndex(d, object, strings.Repeat("a", 64), 100, time.Now().UTC(), key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.WriteHeader(207)
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/</d:href></d:response><d:response><d:href>https://dav.example.invalid/%s%s</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, object, recoveryIndexSuffix)
			return
		}
		if r.URL.Path != "/"+object+recoveryIndexSuffix {
			t.Error("root index escaped direct child")
		}
		w.Write(document)
	}))
	defer server.Close()
	entries, err := discoverRecoveryIndexes(t.Context(), d, key, c, testClient(t, server))
	if err != nil || len(entries) != 1 || entries[0].ObjectKey != object {
		t.Fatal("valid root collection failed", err)
	}
	base, _ := url.Parse(d.WebDAV.URL)
	for _, href := range []string{"/../secret", "/./secret", "/nested/../secret", "//foreign.example.invalid/secret"} {
		if _, _, err := recoveryDAVChild(base, href); err == nil {
			t.Fatal("dot/foreign href accepted", href)
		}
	}
}
