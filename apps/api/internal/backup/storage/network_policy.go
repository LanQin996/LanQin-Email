package storage

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// NetworkPolicy is a deployment-owned, immutable allowlist. It is never decoded
// from a destination request, serialized into a vault or accepted by Drive.
// Every private target is bound to one exact hostname, port and set of IPs.
type NetworkPolicy struct {
	private map[string]map[netip.Addr]bool
}

// DecodeNetworkPolicy accepts only RFC1918/ULA addresses, not CIDRs, wildcard
// hosts or a blanket allow-private option. Loopback, link-local/metadata, CGNAT,
// multicast and translation addresses cannot be authorized by this format.
func DecodeNetworkPolicy(input io.Reader) (*NetworkPolicy, error) {
	invalid := errors.New("invalid backup network allowlist; require version 1 and exact private host/port/IP bindings")
	data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, invalid
	}
	var config struct {
		Version int `json:"version"`
		Targets []struct {
			Host string   `json:"host"`
			Port int      `json:"port"`
			IPs  []string `json:"ips"`
		} `json:"targets"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF || config.Version != 1 || config.Targets == nil || len(config.Targets) > 64 {
		return nil, invalid
	}
	policy := &NetworkPolicy{private: make(map[string]map[netip.Addr]bool)}
	for _, target := range config.Targets {
		host, ok := policyHost(target.Host)
		if !ok || target.Port < 1 || target.Port > 65535 || len(target.IPs) < 1 || len(target.IPs) > 16 {
			return nil, invalid
		}
		key := net.JoinHostPort(host, strconv.Itoa(target.Port))
		if policy.private[key] != nil {
			return nil, invalid
		}
		ips := make(map[netip.Addr]bool)
		for _, raw := range target.IPs {
			ip, err := netip.ParseAddr(raw)
			if err != nil || !privateBackupIP(ip) || ips[ip] {
				return nil, invalid
			}
			if literal, err := netip.ParseAddr(host); err == nil && literal != ip {
				return nil, invalid
			}
			ips[ip] = true
		}
		policy.private[key] = ips
	}
	return policy, nil
}

func privateBackupIP(ip netip.Addr) bool {
	return ip.IsValid() && ip.Zone() == "" && !ip.Is4In6() && ip.IsGlobalUnicast() && ip.IsPrivate()
}

func policyHost(host string) (string, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || ip.Is4In6() {
			return "", false
		}
		return ip.String(), true
	}
	if len(host) < 1 || len(host) > 253 || strings.HasSuffix(host, ".") {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, char := range label {
			if char != '-' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
				return "", false
			}
		}
	}
	return strings.ToLower(host), true
}

func (p *NetworkPolicy) addresses(host, port string) map[netip.Addr]bool {
	if p == nil {
		return nil
	}
	host, ok := policyHost(host)
	number, err := strconv.Atoi(port)
	if !ok || err != nil || number < 1 || number > 65535 {
		return nil
	}
	return p.private[net.JoinHostPort(host, strconv.Itoa(number))]
}

// WithNetworkPolicy returns an independent destination slice, keeping runtime
// deployment policy out of serialized config and of Google's fixed origins.
func (c Config) WithNetworkPolicy(policy *NetworkPolicy) Config {
	c.Destinations = append([]Destination(nil), c.Destinations...)
	for i := range c.Destinations {
		c.Destinations[i].NetworkPolicy = nil
		if c.Destinations[i].Type == "s3" || c.Destinations[i].Type == "webdav" {
			c.Destinations[i].NetworkPolicy = policy
		}
	}
	return c
}
