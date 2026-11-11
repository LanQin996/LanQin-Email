package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The production policy, restricted origins, signing, auth, HTTP and TLS checks
// remain active. Only this test's resolver/dial plumbing maps the authorized NAS
// IP to a loopback TLS fixture; it is not a real NAS/provider acceptance test.
func TestBackupNASPolicyTLSUploadDownloadAndConditionalRetention(t *testing.T) {
	policy := testNetworkPolicy(t)
	for _, kind := range []string{"s3", "webdav"} {
		t.Run(kind, func(t *testing.T) {
			file, receipt, ciphertext := fixture(t)
			var remote []byte
			var remoteMu sync.Mutex
			var puts, gets, deletes atomic.Int32
			server := networkPolicyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				remoteMu.Lock()
				defer remoteMu.Unlock()
				if r.Host != "nas.example.test" {
					t.Error("request changed origin", r.Host)
				}
				if kind == "webdav" {
					username, password, ok := r.BasicAuth()
					if !ok || username != "fixture-user" || password != "fixture-password" {
						t.Error("WebDAV credentials not scoped to NAS")
					}
				} else if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("S3 NAS request not signed")
				}
				switch r.Method {
				case http.MethodPut:
					puts.Add(1)
					if r.Header.Get("If-None-Match") != "*" {
						t.Error("NAS upload could overwrite existing object")
					}
					var err error
					remote, err = io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(remote, ciphertext) {
						t.Error("NAS received changed ciphertext", err)
					}
					w.WriteHeader(http.StatusOK)
				case http.MethodGet:
					gets.Add(1)
					w.Header().Set("ETag", `"synthetic-nas-object"`)
					w.Write(remote)
				case http.MethodDelete:
					deletes.Add(1)
					if r.Header.Get("If-Match") != `"synthetic-nas-object"` {
						t.Error("NAS retention lost conditional delete")
					}
					remote = nil
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Error("unexpected NAS request method", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			client, closeClient := networkPolicyTLSClient(t, server, policy, true)
			defer closeClient()
			d := davDestination("nas")
			d.WebDAV.URL = "https://nas.example.test/backups"
			credentials := Credentials{Username: "fixture-user", Password: "fixture-password"}
			if kind == "s3" {
				d = s3Destination("nas")
				d.S3.Endpoint = "https://nas.example.test"
				credentials = Credentials{AccessKeyID: "fixture-key", SecretAccessKey: "fixture-secret"}
			}
			d.NetworkPolicy = policy
			cfg := Config{Version: 1, Destinations: []Destination{d}}
			opts := DefaultOptions()
			opts.Attempts = 1
			report, err := upload(t.Context(), file, receipt, cfg, opts, func(d Destination) (uploader, error) {
				return buildUploader(d, func(name string) (string, bool) {
					switch name {
					case "nas_KEY":
						return credentials.AccessKeyID, true
					case "nas_SECRET":
						return credentials.SecretAccessKey, true
					case "nas_USER":
						return credentials.Username, true
					case "nas_PASSWORD":
						return credentials.Password, true
					}
					return "", false
				}, client)
			})
			if err != nil || len(report.Destinations) != 1 || report.Destinations[0].State != "succeeded" || puts.Load() != 1 {
				t.Fatal("NAS upload failed", err, report)
			}
			object := report.Destinations[0].ObjectKey
			client.Transport.(*restrictedTransport).methods = map[string]bool{http.MethodGet: true, http.MethodDelete: true}
			output := filepath.Join(t.TempDir(), "download.age")
			if _, err := download(t.Context(), d, object, receipt, output, opts, credentials, client); err != nil {
				t.Fatal("NAS verified download failed", err)
			}
			if data, err := os.ReadFile(output); err != nil || !bytes.Equal(data, ciphertext) {
				t.Fatal("NAS download changed ciphertext", err)
			}
			if err := retainRemote(t.Context(), d, object, receipt, int64(len(ciphertext)), credentials, true, client); err != nil {
				t.Fatal("NAS conditional retention failed", err)
			}
			remoteMu.Lock()
			defer remoteMu.Unlock()
			if gets.Load() != 2 || deletes.Load() != 1 || len(remote) != 0 {
				t.Fatal("NAS object lifecycle differed from upload/download/readback/delete", puts.Load(), gets.Load(), deletes.Load())
			}
		})
	}
}

func TestBackupNASPolicyCannotBypassTLSCertificateValidation(t *testing.T) {
	var requests atomic.Int32
	server := networkPolicyTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	client, closeClient := networkPolicyTLSClient(t, server, testNetworkPolicy(t), false)
	defer closeClient()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://nas.example.test/backups/test", strings.NewReader("synthetic"))
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("private allowlist bypassed untrusted TLS certificate")
	}
	if requests.Load() != 0 {
		t.Fatal("credentials/body reached untrusted NAS")
	}
}

func networkPolicyTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := x509.Certificate{SerialNumber: big.NewInt(34), Subject: pkix.Name{CommonName: "nas.example.test"}, DNSNames: []string{"nas.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func networkPolicyTLSClient(t *testing.T, server *httptest.Server, policy *NetworkPolicy, trusted bool) (*http.Client, func()) {
	t.Helper()
	client, cleanup := secureClientWithPolicy(policy, "nas.example.test")
	transport := client.Transport.(*restrictedTransport).base
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	if trusted {
		transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	}
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return policyDial(ctx, network, address, policy, func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("192.168.1.10")}, nil
		}, func(ctx context.Context, network, pinned string) (net.Conn, error) {
			if pinned != "192.168.1.10:443" {
				t.Fatal("test network tunnel received unvalidated address", pinned)
			}
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		})
	}
	return client, cleanup
}
