package storage

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

var errEndpointBlocked = errors.New("destination connection blocked by network policy")

// Additional special-use ranges are denied even when Go calls them global
// unicast: CGNAT, documentation, benchmarking, IPv6 translation/tunneling, etc.
var specialNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return false
	}
	for _, prefix := range specialNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

type lookupIP func(context.Context, string, string) ([]net.IP, error)

// Resolve once per connection, validate ALL answers, then dial the validated IP
// directly. Never re-resolve the hostname between validation and dialing.
func publicDial(ctx context.Context, network, address string, lookup lookupIP, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errEndpointBlocked
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil {
		return nil, errors.New("destination DNS lookup failed")
	}
	if len(ips) == 0 {
		return nil, errEndpointBlocked
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok || !publicIP(addr) {
			return nil, errEndpointBlocked
		}
	}
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("destination connection failed")
}

type restrictedTransport struct {
	hosts map[string]bool
	base  *http.Transport
}

func (t *restrictedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || !t.hosts[strings.ToLower(r.URL.Host)] || r.URL.User != nil || r.Method != http.MethodPut {
		return nil, errEndpointBlocked
	}
	return t.base.RoundTrip(r)
}

func secureClient(hosts ...string) (*http.Client, func()) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// No proxy environment: a proxy could bypass the validated-IP policy.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return publicDial(ctx, network, address, net.DefaultResolver.LookupIP, dialer.DialContext)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		IdleConnTimeout:        30 * time.Second,
		MaxConnsPerHost:        2,
		MaxIdleConnsPerHost:    2,
		MaxResponseHeaderBytes: 64 << 10,
	}
	allowed := make(map[string]bool)
	for _, host := range hosts {
		allowed[strings.ToLower(host)] = true
	}
	client := &http.Client{
		Transport:     &restrictedTransport{hosts: allowed, base: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport.CloseIdleConnections
}
