package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
)

type resumeChildConfig struct {
	Kind, Root, Input, SHA256, Address, Certificate, Report string
}

// Invoked in separate OS processes by the parent test. The TLS route injection
// exists ONLY in tests; production has no private-IP or certificate bypass flag.
func TestResumeProcessHelper(t *testing.T) {
	path := os.Getenv("LANQIN_SYNTHETIC_RESUME_HELPER")
	if path == "" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg resumeChildConfig
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("invalid synthetic configuration")
	}
	der, err := base64.StdEncoding.DecodeString(cfg.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	target, err := url.Parse(cfg.Address)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.Host = r.URL.Host
		copy.URL.Host, copy.URL.Scheme = target.Host, target.Scheme
		return transport.RoundTrip(copy)
	})}
	d := s3Destination("resume")
	if cfg.Kind == "drive" {
		d = driveDestination("resume")
	} else if cfg.Kind == "webdav" {
		d = davDestination("resume")
	}
	store, err := OpenFileCheckpointStore(cfg.Root, "process-resume", bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := DefaultOptions()
	opts.Attempts = 1
	opts.Checkpoints = store
	opts.SpoolDir = cfg.Root
	report, err := upload(t.Context(), cfg.Input, cfg.SHA256, Config{Version: 1, Destinations: []Destination{d}}, opts, func(d Destination) (uploader, error) {
		return buildUploader(d, func(string) (string, bool) { return "synthetic-transfer-credential", true }, client)
	})
	if err != nil && err != ErrPartialFailure {
		t.Fatal("protocol upload failed before its report", err)
	}
	output, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Report, output, 0o600); err != nil {
		t.Fatal(err)
	}
}

type processProtocolServer struct {
	t                                            *testing.T
	mu                                           sync.Mutex
	kind, scenario, name, fileID, uploadID, hash string
	data                                         []byte
	parts                                        map[int]int
	offset                                       int64
	completed, blocked                           bool
	creates, completes                           int
	started                                      chan struct{}
	release                                      chan struct{}
	releaseOnce                                  sync.Once
}

func (s *processProtocolServer) block(r *http.Request) bool {
	if s.blocked {
		return false
	}
	s.blocked = true
	close(s.started)
	s.mu.Unlock()
	select {
	case <-r.Context().Done():
	case <-s.release:
	}
	s.mu.Lock()
	return true
}

func (s *processProtocolServer) metadata() map[string]any {
	digest := md5.Sum(s.data)
	return map[string]any{"id": s.fileID, "size": strconv.Itoa(len(s.data)), "md5Checksum": hex.EncodeToString(digest[:]), "parents": []string{"folder_resume"}, "appProperties": map[string]string{"lanqinDestination": "resume", "lanqinSHA256": s.hash, "lanqinTransfer": s.name}}
}

func (s *processProtocolServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind == "drive" {
		s.drive(w, r)
	} else if s.kind == "webdav" {
		s.webdav(w, r)
	} else {
		s.s3(w, r)
	}
}

func (s *processProtocolServer) webdav(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != "synthetic-transfer-credential" || password != user {
		s.t.Error("WebDAV credentials crossed boundary")
	}
	name := strings.TrimPrefix(r.URL.Path, "/existing/resume/")
	if !backupObjectName.MatchString(name) || s.name != "" && s.name != name {
		s.t.Error("WebDAV checkpoint object changed")
		w.WriteHeader(400)
		return
	}
	if r.Method == http.MethodGet {
		if s.offset == 0 {
			w.WriteHeader(404)
			return
		}
		w.Write(s.data[:s.offset])
		return
	}
	if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" || s.offset > 0 {
		s.t.Error("WebDAV attempted an unsafe overwrite")
		w.WriteHeader(412)
		return
	}
	s.name = name
	s.creates++
	s.parts[1]++
	if s.scenario == "partial" {
		prefix := make([]byte, 8<<20)
		if _, err := io.ReadFull(r.Body, prefix); err != nil || !bytes.Equal(prefix, s.data[:len(prefix)]) {
			s.t.Error("WebDAV partial source differs")
		}
		s.offset = int64(len(prefix))
		s.block(r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || !bytes.Equal(body, s.data) {
		s.t.Error("WebDAV source differs")
	}
	s.offset = int64(len(body))
	s.completed = true
	s.completes++
	if s.block(r) {
		return
	}
	w.WriteHeader(201)
}

func (s *processProtocolServer) drive(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer synthetic-transfer-credential" {
		s.t.Error("Drive credential crossed boundary")
	}
	if r.URL.Path == "/drive/v3/files/generateIds" {
		s.fileID = "preallocated-synthetic-file"
		json.NewEncoder(w).Encode(map[string]any{"ids": []string{s.fileID}})
		return
	}
	if r.URL.Path == "/drive/v3/files/"+s.fileID {
		if !s.completed {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			w.Write(s.data)
		} else {
			json.NewEncoder(w).Encode(s.metadata())
		}
		return
	}
	if r.URL.Path != "/upload/drive/v3/files" {
		s.t.Error("unexpected Drive endpoint")
		w.WriteHeader(400)
		return
	}
	if r.Method == http.MethodPost {
		var metadata struct {
			ID, Name      string
			Parents       []string
			AppProperties map[string]string
		}
		if json.NewDecoder(r.Body).Decode(&metadata) != nil || metadata.ID != s.fileID || len(metadata.Parents) != 1 || metadata.Parents[0] != "folder_resume" || metadata.AppProperties["lanqinSHA256"] != s.hash || metadata.AppProperties["lanqinTransfer"] != metadata.Name {
			s.t.Error("invalid preallocated Drive session metadata")
		}
		s.creates++
		s.name = metadata.Name
		s.uploadID = "synthetic-sensitive-drive-session"
		w.Header().Set("Location", driveUploadEndpoint+"?upload_id="+s.uploadID)
		return
	}
	if r.URL.Query().Get("upload_id") != s.uploadID {
		s.t.Error("Drive session changed across processes")
	}
	contentRange := r.Header.Get("Content-Range")
	if strings.HasPrefix(contentRange, "bytes */") {
		if s.completed {
			json.NewEncoder(w).Encode(s.metadata())
			return
		}
		if s.offset > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", s.offset-1))
		}
		w.WriteHeader(308)
		return
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(contentRange, "bytes %d-%d/%d", &start, &end, &total); err != nil || start != s.offset || end >= int64(len(s.data)) || total != int64(len(s.data)) {
		s.t.Error("invalid Drive resume range")
		w.WriteHeader(400)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || !bytes.Equal(body, s.data[start:end+1]) {
		s.t.Error("Drive resumed different source bytes")
	}
	part := int(start/transferPartBytes) + 1
	s.parts[part]++
	if s.scenario == "partial" && part == 2 && s.block(r) {
		return
	}
	s.offset = end + 1
	if s.offset == total {
		s.completed = true
		s.completes++
		if s.scenario == "completion" && s.block(r) {
			return
		}
		json.NewEncoder(w).Encode(s.metadata())
		return
	}
	w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", s.offset-1))
	w.WriteHeader(308)
}

func (s *processProtocolServer) s3(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=synthetic-transfer-credential/") {
		s.t.Error("S3 used ambient credentials")
	}
	q := r.URL.Query()
	if _, ok := q["uploads"]; ok {
		s.creates++
		s.name = strings.TrimPrefix(r.URL.Path, "/backup-bucket/resume/")
		s.uploadID = "synthetic-sensitive-s3-session"
		if r.Header.Get("X-Amz-Meta-Lanqin-Sha256") != s.hash || r.Header.Get("X-Amz-Meta-Lanqin-Transfer") != s.name {
			s.t.Error("S3 session metadata not bound to receipt")
		}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>backup-bucket</Bucket><Key>resume/%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", s.name, s.uploadID)
		return
	}
	if r.URL.Path != "/backup-bucket/resume/"+s.name {
		s.t.Error("S3 object changed across processes")
		w.WriteHeader(400)
		return
	}
	if q.Get("uploadId") == "" && r.Method == http.MethodGet {
		if !s.completed {
			w.WriteHeader(404)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Header().Set("X-Amz-Meta-Lanqin-Sha256", s.hash)
		w.Header().Set("X-Amz-Meta-Lanqin-Destination", "resume")
		w.Header().Set("X-Amz-Meta-Lanqin-Transfer", s.name)
		// A reverse proxy may stream GetObject without Content-Length. Actual
		// bounded readback/hash, rather than that optional header, proves size.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		w.Write(s.data)
		return
	}
	if q.Get("uploadId") != s.uploadID {
		s.t.Error("wrong S3 upload ID")
	}
	if r.Method == http.MethodGet {
		io.WriteString(w, "<ListPartsResult><IsTruncated>false</IsTruncated>")
		for n := 1; n <= 2; n++ {
			if s.offset >= min(int64(n)*transferPartBytes, int64(len(s.data))) {
				start := int64(n-1) * transferPartBytes
				size := min(transferPartBytes, int64(len(s.data))-start)
				hash := md5.Sum(s.data[start : start+size])
				fmt.Fprintf(w, "<Part><PartNumber>%d</PartNumber><Size>%d</Size><ETag>&quot;%s&quot;</ETag></Part>", n, size, hex.EncodeToString(hash[:]))
			}
		}
		io.WriteString(w, "</ListPartsResult>")
		return
	}
	if r.Method == http.MethodPut {
		n, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || n < 1 || n > 2 {
			s.t.Error("invalid S3 part")
			w.WriteHeader(400)
			return
		}
		start := int64(n-1) * transferPartBytes
		size := min(transferPartBytes, int64(len(s.data))-start)
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, s.data[start:start+size]) {
			s.t.Error("S3 resumed different source bytes")
		}
		hash := md5.Sum(body)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(hash[:]) {
			s.t.Error("S3 part transport checksum missing")
		}
		s.parts[n]++
		if s.scenario == "partial" && n == 2 && s.block(r) {
			return
		}
		s.offset = max(s.offset, start+size)
		w.Header().Set("ETag", `"`+hex.EncodeToString(hash[:])+`"`)
		return
	}
	if r.Method == http.MethodPost {
		var complete struct {
			Parts []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		if xml.NewDecoder(r.Body).Decode(&complete) != nil || len(complete.Parts) != 2 || complete.Parts[0].PartNumber != 1 || complete.Parts[1].PartNumber != 2 || r.Header.Get("If-None-Match") != "*" {
			s.t.Error("unsafe multipart completion")
		}
		s.completed = true
		s.completes++
		if s.scenario == "completion" && s.block(r) {
			return
		}
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><Bucket>backup-bucket</Bucket><Key>resume/%s</Key><ETag>&quot;synthetic-complete&quot;</ETag></CompleteMultipartUploadResult>", s.name)
		return
	}
	s.t.Error("unexpected S3 method", r.Method)
	w.WriteHeader(400)
}

func TestResumableUploadsSurviveActualProcessDeath(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer, err := age.Encrypt(&buffer, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte("synthetic protocol fixture\n"), int(transferPartBytes)/26+1024)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	if int64(len(data)) <= transferPartBytes || int64(len(data)) >= 2*transferPartBytes {
		t.Fatal("fixture does not span exactly two parts")
	}
	hash := sha256.Sum256(data)
	receipt := hex.EncodeToString(hash[:])
	input := filepath.Join(t.TempDir(), "synthetic.age")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"s3", "drive", "webdav"} {
		for _, scenario := range []string{"partial", "completion"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				protocol := &processProtocolServer{t: t, kind: kind, scenario: scenario, data: data, hash: receipt, parts: map[int]int{}, started: make(chan struct{}), release: make(chan struct{})}
				server := httptest.NewTLSServer(protocol)
				defer server.Close()
				defer protocol.releaseOnce.Do(func() { close(protocol.release) })
				directory := t.TempDir()
				cfg := resumeChildConfig{Kind: kind, Root: directory, Input: input, SHA256: receipt, Address: server.URL, Certificate: base64.StdEncoding.EncodeToString(server.Certificate().Raw), Report: filepath.Join(t.TempDir(), "report.json")}
				cfgPath := filepath.Join(t.TempDir(), "child.json")
				encoded, _ := json.Marshal(cfg)
				if err := os.WriteFile(cfgPath, encoded, 0o600); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, executable, "-test.run=^TestResumeProcessHelper$", "-test.count=1")
				child.Env = append(os.Environ(), "LANQIN_SYNTHETIC_RESUME_HELPER="+cfgPath)
				var output bytes.Buffer
				child.Stdout = &output
				child.Stderr = &output
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if child.ProcessState == nil {
						child.Process.Kill()
						child.Wait()
					}
				}()
				select {
				case <-protocol.started:
				case <-ctx.Done():
					child.Process.Kill()
					child.Wait()
					t.Fatal("first subprocess never reached persisted checkpoint", output.String())
				}
				if err := child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := child.Wait(); err == nil {
					t.Fatal("test failed to terminate uploading subprocess")
				}
				protocol.releaseOnce.Do(func() { close(protocol.release) })
				resumed := exec.CommandContext(ctx, executable, "-test.run=^TestResumeProcessHelper$", "-test.count=1")
				resumed.Env = append(os.Environ(), "LANQIN_SYNTHETIC_RESUME_HELPER="+cfgPath)
				if output, err := resumed.CombinedOutput(); err != nil {
					t.Fatal("new process could not resume upload", err, string(output))
				}
				value, err := os.ReadFile(cfg.Report)
				if err != nil {
					t.Fatal(err)
				}
				var report Report
				wantState := "succeeded"
				if kind == "webdav" && scenario == "partial" {
					wantState = "failed"
				}
				if json.Unmarshal(value, &report) != nil || len(report.Destinations) != 1 || report.Destinations[0].State != wantState || report.SHA256 != receipt {
					t.Fatal("resumed report not successful", string(value))
				}
				if bytes.Contains(value, []byte("sensitive")) || bytes.Contains(value, []byte("upload_id")) {
					t.Fatal("upload session leaked into result")
				}
				protocol.mu.Lock()
				defer protocol.mu.Unlock()
				if kind == "webdav" {
					wantComplete := 1
					if scenario == "partial" {
						wantComplete = 0
						if report.Destinations[0].ErrorCode != "remote_verification_failed" {
							t.Fatal("partial WebDAV object was not explicitly rejected")
						}
					}
					if protocol.creates != 1 || protocol.completes != wantComplete || protocol.parts[1] != 1 {
						t.Fatal("WebDAV retry overwrote or duplicated an object")
					}
					return
				}
				wantSecond := 1
				if scenario == "partial" {
					wantSecond = 2
				}
				if protocol.creates != 1 || protocol.completes != 1 || protocol.parts[1] != 1 || protocol.parts[2] != wantSecond {
					t.Fatal("resume duplicated an object/session or resent acknowledged part", protocol.creates, protocol.completes, protocol.parts)
				}
			})
		}
	}
}
