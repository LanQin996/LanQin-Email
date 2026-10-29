package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func s3Destination(id string) Destination {
	return Destination{ID: id, Name: id, Type: "s3", S3: &S3Config{
		Endpoint: "https://s3.example.invalid", Region: "auto", Bucket: "backup-bucket",
		Prefix: id, PathStyle: true, AccessKeyIDEnv: id + "_KEY", SecretAccessKeyEnv: id + "_SECRET",
	}}
}

func davDestination(id string) Destination {
	return Destination{ID: id, Name: id, Type: "webdav", WebDAV: &WebDAVConfig{
		URL:         "https://dav.example.invalid/existing/" + id,
		UsernameEnv: id + "_USER", PasswordEnv: id + "_PASSWORD",
	}}
}

func fixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("synthetic offline backup payload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backup.age")
	data := encrypted.Bytes()
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return file, hex.EncodeToString(digest[:]), data
}

func TestConfigAllowsMultipleInstancesAndRejectsAmbiguity(t *testing.T) {
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("r2"), s3Destination("b2"), s3Destination("s3_3"), davDestination("dav1"), davDestination("dav2")}}
	data, _ := json.Marshal(cfg)
	decoded, err := DecodeConfig(bytes.NewReader(data))
	if err != nil || len(decoded.Destinations) != 5 {
		t.Fatalf("same-type instances were rejected: %v", err)
	}
	for name, input := range map[string]string{
		"unknown credential": strings.Replace(string(data), `"region":"auto"`, `"secret_access_key":"DO-NOT-ECHO","region":"auto"`, 1),
		"trailing JSON":      string(data) + `{}`,
		"oversized":          strings.Repeat(" ", (1<<20)+1),
		"version":            strings.Replace(string(data), `"version":1`, `"version":2`, 1),
		"duplicate IDs":      strings.Replace(string(data), `"id":"b2"`, `"id":"r2"`, 1),
		"unsupported Drive":  strings.Replace(string(data), `"type":"s3"`, `"type":"drive"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeConfig(strings.NewReader(input))
			if err == nil || strings.Contains(err.Error(), "DO-NOT-ECHO") {
				t.Fatalf("invalid config accepted or leaked input: %v", err)
			}
		})
	}
	for _, endpoint := range []string{
		"http://public.example", "https://user:secret@public.example", "https://public.example/?secret=x",
		"https://public.example/#", "https://public.example/?", "https://public.example/a/../b",
		"https://public.example/a%2fb", "https://public.example/a%5cb", "https://public.example:0/",
		"https://public.example/%00", "https://public.example/a%252fb",
		"https://127.0.0.1/", "https://[::1]/", "https://169.254.169.254/", "https://100.64.0.1/",
	} {
		if _, err := endpointURL(endpoint); err == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Map only the test transport to a local TLS server, leaving production config,
// HTTP semantics and SDK request construction intact. No public network is used.
func testClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := server.Client()
	transport := client.Transport
	target, _ := url.Parse(server.URL)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.Host = r.URL.Host
		copy.URL.Host, copy.URL.Scheme = target.Host, target.Scheme
		return transport.RoundTrip(copy)
	})
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func TestMultipleS3AndWebDAVUploadIsolation(t *testing.T) {
	file, hash, data := fixture(t)
	var active, peak atomic.Int32
	var mutex sync.Mutex
	requests := make(map[string][]string)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, data) || r.ContentLength != int64(len(data)) || r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" {
			t.Error("invalid request or independent reader was not rewound")
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 {
			t.Errorf("unexpected object path: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		id := parts[1]
		if r.Host == "s3.example.invalid" {
			if !strings.Contains(r.Header.Get("Authorization"), "Credential="+id+"-key/") || !strings.Contains(r.Header.Get("Authorization"), "Signature=") {
				t.Error("S3 signing credentials were mixed between destinations")
			}
			md := md5.Sum(data)
			if r.Header.Get("Content-Md5") != base64.StdEncoding.EncodeToString(md[:]) {
				t.Error("S3 transport checksum missing")
			}
			// Verify the actual signature with this destination's secret, not
			// just the access-key name visible in the Authorization header.
			check := r.Clone(r.Context())
			check.URL.Scheme, check.URL.Host = "https", r.Host
			check.Header.Del("Authorization")
			signedAt, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
			if err != nil {
				t.Error("missing S3 signing timestamp")
			}
			signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
			err = signer.SignHTTP(r.Context(), aws.Credentials{AccessKeyID: id + "-key", SecretAccessKey: id + "-secret"}, check, r.Header.Get("X-Amz-Content-Sha256"), "s3", "auto", signedAt)
			if err != nil || check.Header.Get("Authorization") != r.Header.Get("Authorization") {
				t.Error("S3 signature does not match this destination's independent secret")
			}
		} else {
			user, password, ok := r.BasicAuth()
			if !ok || user != id+"-user" || password != id+"-password" {
				t.Error("WebDAV credentials were mixed between destinations")
			}
		}
		mutex.Lock()
		requests[id] = append(requests[id], parts[2])
		count := len(requests[id])
		mutex.Unlock()
		if id == "dav2" {
			w.WriteHeader(403)
			w.Write([]byte("untrusted error: dav2-password"))
			return
		}
		if id == "b2" && count == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(503)
			w.Write([]byte(`<Error><Code>SlowDown</Code><Message>b2-secret must not leak</Message></Error>`))
			return
		}
		if r.Host == "s3.example.invalid" {
			w.Header().Set("ETag", `"fixture"`)
			w.WriteHeader(200)
		} else {
			w.WriteHeader(201)
		}
	}))
	defer server.Close()
	client := testClient(t, server)
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("r2"), s3Destination("b2"), s3Destination("s3_3"), davDestination("dav1"), davDestination("dav2"), davDestination("disabled")}}
	cfg.Destinations[5].Disabled = true
	env := func(ref string) (string, bool) {
		for _, d := range cfg.Destinations {
			for suffix, value := range map[string]string{"_KEY": "-key", "_SECRET": "-secret", "_USER": "-user", "_PASSWORD": "-password"} {
				if ref == d.ID+suffix {
					return d.ID + value, true
				}
			}
		}
		return "", false
	}
	opts := DefaultOptions()
	opts.Attempts, opts.SpoolDir = 2, t.TempDir()
	report, err := upload(t.Context(), file, hash, cfg, opts, func(d Destination) (uploader, error) { return buildUploader(d, env, client) })
	if !errors.Is(err, ErrPartialFailure) || report.SHA256 != hash || report.Bytes != int64(len(data)) {
		t.Fatalf("incorrect partial outcome: %#v %v", report, err)
	}
	for i, result := range report.Destinations {
		switch i {
		case 4:
			if result.State != "failed" || result.ErrorCode != "access_denied" || result.CompletedAt != nil || result.Attempts != 1 || result.ObjectKey != "" {
				t.Fatalf("failed target marked successful: %#v", result)
			}
		case 5:
			if result.State != "skipped" || result.Attempts != 0 {
				t.Fatalf("disabled target uploaded: %#v", result)
			}
		default:
			if result.State != "succeeded" || result.CompletedAt == nil || result.ObjectKey == "" || result.ErrorCode != "" {
				t.Fatalf("independent target failed: %#v", result)
			}
		}
	}
	if len(requests["b2"]) != 2 || requests["b2"][0] == requests["b2"][1] || report.Destinations[1].Attempts != 2 {
		t.Fatal("retry was missing, unbounded, or reused a possibly accepted object name")
	}
	if peak.Load() != 2 || len(requests) != 5 {
		t.Fatalf("concurrency/target count incorrect: peak=%d targets=%d", peak.Load(), len(requests))
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "secret") {
		t.Fatal("remote error or credentials leaked into report")
	}
	entries, err := os.ReadDir(opts.SpoolDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("ciphertext spool was not cleaned up")
	}
}

type stubUploader struct {
	fn func(context.Context, string, *io.SectionReader, *artifact) error
}

func (s stubUploader) put(ctx context.Context, key string, body *io.SectionReader, data *artifact) error {
	return s.fn(ctx, key, body, data)
}
func (stubUploader) close() {}

func TestPreflightNeverContactsRemoteOnInvalidSourceOrLimits(t *testing.T) {
	file, receipt, data := fixture(t)
	plain := filepath.Join(t.TempDir(), "plaintext.age")
	os.WriteFile(plain, []byte("private synthetic plaintext"), 0o600)
	plainHash := sha256.Sum256([]byte("private synthetic plaintext"))
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("r2")}}
	for _, tc := range []struct {
		name, file, hash string
		modify           func(*Options)
	}{
		{"wrong receipt", file, strings.Repeat("0", 64), nil},
		{"missing receipt", file, "", nil},
		{"plaintext", plain, hex.EncodeToString(plainHash[:]), nil},
		{"size limit", file, receipt, func(o *Options) { o.MaxBytes = int64(len(data) - 1) }},
		{"concurrency limit", file, receipt, func(o *Options) { o.Concurrency = 9 }},
		{"attempts limit", file, receipt, func(o *Options) { o.Attempts = 4 }},
		{"timeout limit", file, receipt, func(o *Options) { o.AttemptTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := DefaultOptions()
			opts.SpoolDir = t.TempDir()
			if tc.modify != nil {
				tc.modify(&opts)
			}
			_, err := upload(t.Context(), tc.file, tc.hash, cfg, opts, func(Destination) (uploader, error) {
				t.Fatal("remote client constructed before preflight completed")
				return nil, nil
			})
			if err == nil {
				t.Fatal("invalid source or limits accepted")
			}
			entries, _ := os.ReadDir(opts.SpoolDir)
			if len(entries) != 0 {
				t.Fatal("failed stage left a spool file")
			}
		})
	}
}

func TestUploadCancellationTimeoutAndMissingCredentials(t *testing.T) {
	file, receipt, _ := fixture(t)
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("first"), s3Destination("second")}}
	t.Run("cancel running and queued", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opts := DefaultOptions()
		opts.Concurrency = 1
		calls := 0
		report, err := upload(ctx, file, receipt, cfg, opts, func(Destination) (uploader, error) {
			calls++
			return stubUploader{func(context.Context, string, *io.SectionReader, *artifact) error {
				cancel()
				return classify(context.Canceled)
			}}, nil
		})
		if !errors.Is(err, ErrPartialFailure) || calls != 1 {
			t.Fatalf("cancelled queued tasks were started: %d %v", calls, err)
		}
		for _, result := range report.Destinations {
			if result.State != "cancelled" || result.CompletedAt != nil {
				t.Fatalf("cancelled upload published: %#v", result)
			}
		}
	})
	t.Run("per-attempt deadline", func(t *testing.T) {
		opts := DefaultOptions()
		opts.Attempts, opts.AttemptTimeout = 1, time.Millisecond
		report, err := upload(t.Context(), file, receipt, cfg, opts, func(Destination) (uploader, error) {
			return stubUploader{func(ctx context.Context, _ string, _ *io.SectionReader, _ *artifact) error {
				<-ctx.Done()
				return classify(ctx.Err())
			}}, nil
		})
		if !errors.Is(err, ErrPartialFailure) {
			t.Fatal(err)
		}
		for _, result := range report.Destinations {
			if result.ErrorCode != "timeout" || result.CompletedAt != nil || result.Attempts != 1 {
				t.Fatalf("timeout handling incorrect: %#v", result)
			}
		}
	})
	t.Run("credential failure isolated", func(t *testing.T) {
		report, err := upload(t.Context(), file, receipt, cfg, DefaultOptions(), func(d Destination) (uploader, error) {
			if d.ID == "first" {
				return buildUploader(d, func(string) (string, bool) { return "", false }, nil)
			}
			return stubUploader{func(context.Context, string, *io.SectionReader, *artifact) error { return nil }}, nil
		})
		if !errors.Is(err, ErrPartialFailure) || report.Destinations[0].ErrorCode != "destination_unavailable" || report.Destinations[0].Attempts != 0 || report.Destinations[1].State != "succeeded" {
			t.Fatalf("credential failure affected other destinations: %#v %v", report, err)
		}
	})
}

func TestPublicNetworkPolicyAndPinnedDNS(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.1.2.3", "100.64.0.1", "198.18.0.1", "192.0.2.1", "224.0.0.1", "255.255.255.255", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1", "2001::1"} {
		if publicIP(netip.MustParseAddr(value)) {
			t.Errorf("special-use IP allowed: %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(value)) {
			t.Errorf("public IP rejected: %s", value)
		}
	}
	for _, answers := range [][]net.IP{nil, {net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}} {
		_, err := publicDial(t.Context(), "tcp", "storage.example:443", func(context.Context, string, string) ([]net.IP, error) { return answers, nil }, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialed mixed or empty DNS answers")
			return nil, nil
		})
		if !errors.Is(err, errEndpointBlocked) {
			t.Fatal("unsafe DNS answers accepted")
		}
	}
	lookups := 0
	conn, err := publicDial(t.Context(), "tcp", "storage.example:443", func(context.Context, string, string) ([]net.IP, error) {
		lookups++
		if lookups != 1 {
			t.Fatal("DNS resolved again before dialing")
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Fatal("hostname, rather than validated IP, passed to dial")
		}
		left, right := net.Pipe()
		right.Close()
		return left, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	client, cleanup := secureClient("storage.example")
	defer cleanup()
	transport := client.Transport.(*restrictedTransport)
	if transport.base.Proxy != nil || transport.base.TLSClientConfig.InsecureSkipVerify || transport.base.TLSClientConfig.MinVersion == 0 || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("insecure transport configuration")
	}
	for _, value := range []string{"https://other.example/object", "http://storage.example/object"} {
		req, _ := http.NewRequest("PUT", value, nil)
		if _, err := transport.RoundTrip(req); !errors.Is(err, errEndpointBlocked) {
			t.Fatal("cross-target request was not blocked")
		}
	}
}

func TestWebDAVRedirectAndPreconditionAreNotRetried(t *testing.T) {
	for _, status := range []int{302, 307, 412, 401} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			file, receipt, _ := fixture(t)
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", "https://other.example/steal")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := testClient(t, server)
			cfg := Config{Version: 1, Destinations: []Destination{davDestination("dav")}}
			report, err := upload(t.Context(), file, receipt, cfg, DefaultOptions(), func(d Destination) (uploader, error) {
				return buildUploader(d, func(string) (string, bool) { return "fixture", true }, client)
			})
			if !errors.Is(err, ErrPartialFailure) || requests.Load() != 1 || report.Destinations[0].Attempts != 1 || report.Destinations[0].CompletedAt != nil {
				t.Fatalf("redirect or permanent failure was followed/retried: %#v %v", report, err)
			}
		})
	}
}

func TestSourceChangesCannotChangeStagedUploads(t *testing.T) {
	file, receipt, original := fixture(t)
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("one"), s3Destination("two")}}
	opts := DefaultOptions()
	opts.Concurrency = 1
	report, err := upload(t.Context(), file, receipt, cfg, opts, func(Destination) (uploader, error) {
		if err := os.WriteFile(file, []byte("changed original"), 0o600); err != nil {
			t.Fatal(err)
		}
		return stubUploader{func(_ context.Context, _ string, body *io.SectionReader, _ *artifact) error {
			data, err := io.ReadAll(body)
			if err != nil || !bytes.Equal(data, original) {
				t.Error("an upload observed mutable original source instead of private ciphertext spool")
			}
			return nil
		}}, nil
	})
	if err != nil || report.Destinations[1].State != "succeeded" {
		t.Fatal(err)
	}
}

func TestProductionClientBlocksPrivateDNS(t *testing.T) {
	file, receipt, _ := fixture(t)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(201) }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	d := davDestination("dav")
	d.WebDAV.URL = "https://localhost:" + u.Port() + "/existing"
	t.Setenv("dav_USER", "synthetic-user")
	t.Setenv("dav_PASSWORD", "synthetic-password")
	report, err := Upload(t.Context(), file, receipt, Config{Version: 1, Destinations: []Destination{d}}, DefaultOptions())
	if !errors.Is(err, ErrPartialFailure) || requests.Load() != 0 || report.Destinations[0].ErrorCode != "endpoint_blocked" || report.Destinations[0].Attempts != 1 {
		t.Fatalf("production DNS guard did not block localhost: %#v %v", report, err)
	}
}

func TestS3DoesNotTreatAsyncAcceptanceAsComplete(t *testing.T) {
	file, receipt, _ := fixture(t)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(202) }))
	defer server.Close()
	client := testClient(t, server)
	cfg := Config{Version: 1, Destinations: []Destination{s3Destination("r2")}}
	report, err := upload(t.Context(), file, receipt, cfg, DefaultOptions(), func(d Destination) (uploader, error) {
		return buildUploader(d, func(string) (string, bool) { return "fixture", true }, client)
	})
	if !errors.Is(err, ErrPartialFailure) || requests.Load() != 1 || report.Destinations[0].ErrorCode != "unexpected_success_status" || report.Destinations[0].CompletedAt != nil {
		t.Fatalf("asynchronous acknowledgement marked complete: %#v %v", report, err)
	}
}
