// Package storage uploads already-encrypted offline backups. It has no access
// to the application database, recovery keys, live services or remote deletion.
package storage

import (
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const maxDestinations = 64

// Config contains references to operator-provided environment variables, never
// credential values. Persistent encrypted credential storage belongs to the
// future admin API, not this standalone CLI.
type Config struct {
	Version      int           `json:"version"`
	Destinations []Destination `json:"destinations"`
}

type Destination struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Disabled bool          `json:"disabled,omitempty"`
	S3       *S3Config     `json:"s3,omitempty"`
	WebDAV   *WebDAVConfig `json:"webdav,omitempty"`
}

type S3Config struct {
	Endpoint           string `json:"endpoint"`
	Region             string `json:"region"`
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix,omitempty"`
	PathStyle          bool   `json:"path_style"`
	AccessKeyIDEnv     string `json:"access_key_id_env"`
	SecretAccessKeyEnv string `json:"secret_access_key_env"`
	SessionTokenEnv    string `json:"session_token_env,omitempty"`
}

type WebDAVConfig struct {
	URL         string `json:"url"`
	UsernameEnv string `json:"username_env"`
	PasswordEnv string `json:"password_env"`
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envPattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	regionPattern     = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	bucketPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern     = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,512}$`)
)

// DecodeConfig rejects unknown fields, trailing JSON and oversized input. Errors
// deliberately omit input fragments, which could contain misplaced secrets.
func DecodeConfig(r io.Reader) (Config, error) {
	var cfg Config
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return cfg, errors.New("cannot read destination config (maximum 1 MiB)")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid destination config JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("destination config must contain one JSON value")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.Version != 1 || len(c.Destinations) == 0 || len(c.Destinations) > maxDestinations {
		return errors.New("destination config requires version 1 and 1..64 destinations")
	}
	ids := make(map[string]bool)
	for _, d := range c.Destinations {
		if !identifierPattern.MatchString(d.ID) || ids[d.ID] || strings.TrimSpace(d.Name) == "" || len(d.Name) > 128 || strings.IndexFunc(d.Name, unicode.IsControl) >= 0 {
			return errors.New("destination IDs must be unique; invalid ID or name")
		}
		ids[d.ID] = true
		switch d.Type {
		case "s3":
			if d.S3 == nil || d.WebDAV != nil {
				return errors.New("s3 destination requires only s3 settings")
			}
			s := d.S3
			u, err := endpointURL(s.Endpoint)
			if err != nil || (u.Path != "" && u.Path != "/") || !regionPattern.MatchString(s.Region) || !validBucket(s.Bucket) || !validPrefix(s.Prefix) || !envPattern.MatchString(s.AccessKeyIDEnv) || !envPattern.MatchString(s.SecretAccessKeyEnv) || (s.SessionTokenEnv != "" && !envPattern.MatchString(s.SessionTokenEnv)) {
				return errors.New("invalid s3 destination settings")
			}
		case "webdav":
			if d.WebDAV == nil || d.S3 != nil {
				return errors.New("webdav destination requires only webdav settings")
			}
			if _, err := endpointURL(d.WebDAV.URL); err != nil || !envPattern.MatchString(d.WebDAV.UsernameEnv) || !envPattern.MatchString(d.WebDAV.PasswordEnv) {
				return errors.New("invalid webdav destination settings")
			}
		default:
			return errors.New("unsupported destination type (supported: s3, webdav)")
		}
	}
	return nil
}

func validBucket(s string) bool {
	if !bucketPattern.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, ".-") || strings.Contains(s, "-.") {
		return false
	}
	_, err := netip.ParseAddr(s)
	return err != nil // No IP buckets, ARNs, access points or endpoint overrides.
}

func validPrefix(s string) bool {
	return s == "" || (prefixPattern.MatchString(s) && path.Clean(s) == s && !strings.HasPrefix(s, "/") && s != "." && s != ".." && !strings.HasPrefix(s, "../"))
}

func endpointURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") || u.RawPath != "" || strings.ContainsAny(u.Path, "\\%") || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return nil, errors.New("destination must be an unambiguous HTTPS URL without credentials, query or fragment")
	}
	if u.Path != "" && u.Path != "/" && path.Clean(strings.TrimSuffix(u.Path, "/")) != strings.TrimSuffix(u.Path, "/") {
		return nil, errors.New("destination path must not contain dot segments")
	}
	if strings.Contains(u.Hostname(), "%") || strings.HasSuffix(u.Hostname(), ".") {
		return nil, errors.New("invalid destination host")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return nil, errors.New("private and special-use destination addresses are not allowed")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid destination port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("empty destination port")
	}
	return u, nil
}
