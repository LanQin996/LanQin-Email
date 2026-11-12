package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

const networkPolicyFixture = `{"version":1,"targets":[{"host":"nas.example.test","port":443,"ips":["192.168.1.10","fd12:3456::10"]},{"host":"192.168.1.10","port":8443,"ips":["192.168.1.10"]}]}`

func testNetworkPolicy(t *testing.T) *NetworkPolicy {
	t.Helper()
	policy, err := DecodeNetworkPolicy(strings.NewReader(networkPolicyFixture))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestBackupNetworkPolicyRejectsBroadSpecialAndAmbiguousBindings(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "100.64.0.1", "198.18.0.1", "192.0.2.10", "0.0.0.0", "8.8.8.8", "::1", "fe80::1", "64:ff9b::c0a8:10a", "::ffff:192.168.1.10", "192.168.1.0/24", "fd12:3456::10%zone", "224.0.0.1"} {
		data := `{"version":1,"targets":[{"host":"nas.example.test","port":443,"ips":["` + ip + `"]}]}`
		if _, err := DecodeNetworkPolicy(strings.NewReader(data)); err == nil {
			t.Fatal("unsafe private address binding accepted", ip)
		}
	}
	for _, data := range []string{
		`{"version":1,"targets":[{"host":"*.example.test","port":443,"ips":["192.168.1.10"]}]}`,
		`{"version":1,"targets":[{"host":"nas.example.test.","port":443,"ips":["192.168.1.10"]}]}`,
		`{"version":1,"targets":[{"host":"192.168.1.20","port":443,"ips":["192.168.1.10"]}]}`,
		`{"version":1,"targets":[{"host":"nas.example.test","port":0,"ips":["192.168.1.10"]}]}`,
		`{"version":1,"targets":[{"host":"nas.example.test","port":65536,"ips":["192.168.1.10"]}]}`,
		`{"version":1,"targets":[{"host":"nas.example.test","port":443,"ips":[]}]}`,
		`{"version":1,"targets":[{"host":"nas.example.test","port":443,"ips":["192.168.1.10","192.168.1.10"]}]}`,
		strings.Replace(networkPolicyFixture, `"host":"192.168.1.10","port":8443`, `"host":"NAS.EXAMPLE.TEST","port":443`, 1),
		strings.Replace(networkPolicyFixture, `"version":1`, `"version":2`, 1),
		strings.Replace(networkPolicyFixture, `"version":1`, `"do-not-echo":"synthetic-secret","version":1`, 1),
		networkPolicyFixture + `{}`,
		strings.Repeat(" ", (64<<10)+1),
	} {
		if _, err := DecodeNetworkPolicy(strings.NewReader(data)); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("invalid deployment policy accepted or echoed")
		}
	}
}

func TestBackupNetworkPolicyConfigIsRuntimeOnlyAndRequiresExactLiteral(t *testing.T) {
	policy := testNetworkPolicy(t)
	destination := davDestination("nas")
	destination.WebDAV.URL = "https://192.168.1.10:8443/backups"
	config := Config{Version: 1, Destinations: []Destination{destination}}
	data, _ := json.Marshal(config)
	if _, err := DecodeConfig(bytes.NewReader(data)); err == nil {
		t.Fatal("private literal enabled without deployment policy")
	}
	decoded, err := DecodeConfigWithNetworkPolicy(bytes.NewReader(data), policy)
	if err != nil || decoded.Destinations[0].NetworkPolicy != policy {
		t.Fatal("private policy not attached", err)
	}
	encoded, _ := json.Marshal(decoded)
	if !bytes.Equal(data, encoded) || bytes.Contains(encoded, []byte("ips")) {
		t.Fatal("runtime allowlist serialized into portable destination config")
	}
	if _, err := DecodeConfigWithNetworkPolicy(strings.NewReader(strings.Replace(string(data), `"type":"webdav"`, `"networkPolicy":{},"type":"webdav"`, 1)), policy); err == nil {
		t.Fatal("destination JSON accepted network policy authority")
	}
	for _, endpoint := range []string{"https://192.168.1.10/backups", "https://192.168.1.11:8443/backups", "http://192.168.1.10:8443/backups", "https://169.254.169.254:8443/"} {
		copy := decoded.Destinations[0]
		webdav := *copy.WebDAV
		webdav.URL = endpoint
		copy.WebDAV = &webdav
		if err := (Config{Version: 1, Destinations: []Destination{copy}}).Validate(); err == nil {
			t.Fatal("private URL policy widened", endpoint)
		}
	}
	s3 := s3Destination("nas_s3")
	s3.S3.Endpoint, s3.NetworkPolicy, s3.S3.PathStyle = "https://nas.example.test", policy, false
	if err := (Config{Version: 1, Destinations: []Destination{s3}}).Validate(); err == nil {
		t.Fatal("private virtual-host S3 enabled")
	}
	s3.S3.PathStyle = true
	if err := (Config{Version: 1, Destinations: []Destination{s3}}).Validate(); err != nil {
		t.Fatal("private path-style S3 rejected", err)
	}
}

func TestBackupNetworkPolicyPinsAllDNSAnswersAndPortBeforeDial(t *testing.T) {
	policy := testNetworkPolicy(t)
	for _, test := range []struct {
		address string
		answers []string
		allowed bool
	}{
		{"nas.example.test:443", []string{"192.168.1.10"}, true},
		{"NAS.EXAMPLE.TEST:443", []string{"192.168.1.10", "fd12:3456::10"}, true},
		{"nas.example.test:8443", []string{"192.168.1.10"}, false},
		{"other.example.test:443", []string{"192.168.1.10"}, false},
		{"nas.example.test:443", []string{"192.168.1.11"}, false},
		{"nas.example.test:443", []string{"192.168.1.10", "8.8.8.8"}, false},
		{"nas.example.test:443", []string{"192.168.1.10", "169.254.169.254"}, false},
		{"nas.example.test:443", []string{"192.168.1.10", "127.0.0.1"}, false},
		{"nas.example.test:443", nil, false},
		{"public.example.test:443", []string{"8.8.8.8"}, true},
	} {
		lookups, dials := 0, 0
		conn, err := policyDial(t.Context(), "tcp", test.address, policy, func(context.Context, string, string) ([]net.IP, error) {
			lookups++
			if lookups != 1 {
				t.Fatal("DNS re-resolved before dial")
			}
			ips := []net.IP{}
			for _, value := range test.answers {
				ips = append(ips, net.ParseIP(value))
			}
			return ips, nil
		}, func(_ context.Context, _, target string) (net.Conn, error) {
			dials++
			if !test.allowed {
				t.Fatal("dialed before ALL DNS answers were authorized")
			}
			host, _, err := net.SplitHostPort(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := netip.ParseAddr(host); err != nil {
				t.Fatal("hostname reached dial instead of pinned IP")
			}
			left, right := net.Pipe()
			right.Close()
			return left, nil
		})
		if conn != nil {
			conn.Close()
		}
		if (err == nil) != test.allowed || dials != boolInt(test.allowed) {
			t.Fatal("unexpected network decision", test.address, err)
		}
	}
	lookups := 0
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := policyDial(t.Context(), "tcp", "nas.example.test:443", policy, func(context.Context, string, string) ([]net.IP, error) {
			lookups++
			if lookups == 1 {
				return []net.IP{net.ParseIP("192.168.1.10")}, nil
			}
			return []net.IP{net.ParseIP("192.168.1.11")}, nil
		}, func(context.Context, string, string) (net.Conn, error) {
			if attempt != 0 {
				t.Fatal("DNS rebinding reached dial")
			}
			left, right := net.Pipe()
			right.Close()
			return left, nil
		})
		if conn != nil {
			conn.Close()
		}
		if (err == nil) != (attempt == 0) {
			t.Fatal("connection did not revalidate current DNS")
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestBackupNetworkPolicyKeepsTLSOriginsAndDrivePublicOnly(t *testing.T) {
	policy := testNetworkPolicy(t)
	for _, kind := range []string{"s3", "webdav", "drive"} {
		d, _, _ := downloadDestination(kind)
		d.NetworkPolicy = policy
		if kind == "s3" {
			d.S3.Endpoint, d.S3.PathStyle = "https://192.168.1.10:8443", true
		}
		if kind == "webdav" {
			d.WebDAV.URL = "https://192.168.1.10:8443/backups"
		}
		client, closeClient := downloadClient(d)
		transport := client.Transport.(*restrictedTransport)
		if transport.base.Proxy != nil || transport.base.TLSClientConfig.InsecureSkipVerify || transport.base.TLSClientConfig.MinVersion == 0 || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
			t.Fatal("NAS policy weakened TLS/proxy/redirect restrictions")
		}
		request, _ := http.NewRequest(http.MethodGet, "https://other.example.test/object", nil)
		if _, err := client.Do(request); !errors.Is(err, errEndpointBlocked) {
			t.Fatal("NAS policy widened request origin")
		}
		if kind == "s3" && transport.hosts["backup-bucket.192.168.1.10:8443"] {
			t.Fatal("private S3 authorized virtual bucket origin")
		}
		if kind == "drive" {
			if _, err := transport.base.DialContext(t.Context(), "tcp", "192.168.1.10:8443"); !errors.Is(err, errEndpointBlocked) {
				t.Fatal("Drive inherited NAS allowlist")
			}
		}
		closeClient()
	}
	google, closeGoogle := GoogleDriveHTTPClient()
	defer closeGoogle()
	if _, err := google.Transport.(*restrictedTransport).base.DialContext(t.Context(), "tcp", "192.168.1.10:8443"); !errors.Is(err, errEndpointBlocked) {
		t.Fatal("Google OAuth network policy widened")
	}
}
