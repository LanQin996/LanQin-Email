package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	themeGitHubHost        = "github.com"
	themeGitHubAPIHost     = "api.github.com"
	themeGitHubAssetHost   = "release-assets.githubusercontent.com"
	themeGitHubTimeout     = 20 * time.Second
	themeGitHubMaxMetadata = 512 << 10
)

var (
	themeGitHubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	themeGitHubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	themeGitHubTagPattern   = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,128}$`)
)

type themeGitHubSource struct{ owner, repo, tag string }

func parseThemeGitHubSource(input string) (themeGitHubSource, error) {
	invalid := errors.New("enter a public GitHub repository or release HTTPS URL")
	u, err := url.Parse(input)
	if err != nil || !validThemeGitHubURL(u, themeGitHubHost) || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(input, "#") || len(input) > 512 {
		return themeGitHubSource{}, invalid
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	if len(parts) != 2 && len(parts) != 5 && len(parts) != 6 {
		return themeGitHubSource{}, invalid
	}
	owner, repo := parts[0], parts[1]
	if !themeGitHubOwnerPattern.MatchString(owner) || !themeGitHubRepoPattern.MatchString(repo) ||
		strings.HasSuffix(strings.ToLower(repo), ".git") || repo == "." || repo == ".." {
		return themeGitHubSource{}, invalid
	}
	result := themeGitHubSource{owner: strings.ToLower(owner), repo: strings.ToLower(repo)}
	if len(parts) == 2 {
		return result, nil
	}
	if parts[2] != "releases" || !validThemeGitHubTag(parts[4]) ||
		(len(parts) == 5 && parts[3] != "tag") ||
		(len(parts) == 6 && (parts[3] != "download" || parts[5] != themePackageFilename)) {
		return themeGitHubSource{}, invalid
	}
	result.tag = parts[4]
	return result, nil
}

func validThemeGitHubTag(tag string) bool {
	return tag != "." && tag != ".." && themeGitHubTagPattern.MatchString(tag)
}

func validThemeGitHubURL(u *url.URL, host string) bool {
	return u != nil && u.Scheme == "https" && u.Host == host && u.User == nil &&
		u.Opaque == "" && u.Fragment == "" && u.RawFragment == ""
}

type themeGitHubRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		State  string `json:"state"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func downloadGitHubThemePackage(ctx context.Context, input string) (themePackage, error) {
	// The application never uses an environment proxy, cookies, or GitHub
	// credentials for imports. Pin each connection to a fresh, public IP
	// returned for an approved host rather than resolving a second time.
	transport := &http.Transport{
		DialContext:           dialThemeGitHub,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		DisableCompression:    true,
	}
	defer transport.CloseIdleConnections()
	return downloadGitHubThemePackageWithTransport(ctx, input, transport)
}

func dialThemeGitHub(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" || (host != themeGitHubHost && host != themeGitHubAPIHost && host != themeGitHubAssetHost) {
		return nil, errors.New("unapproved GitHub connection")
	}
	resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for _, candidate := range resolved {
		ip := candidate.Unmap()
		if !validThemeGitHubIP(ip) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("unable to connect to public GitHub")
}

var themeGitHubDeniedIPRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
}

func validThemeGitHubIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, blocked := range themeGitHubDeniedIPRanges {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}

func downloadGitHubThemePackageWithTransport(ctx context.Context, input string, transport http.RoundTripper) (themePackage, error) {
	source, err := parseThemeGitHubSource(input)
	if err != nil {
		return themePackage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, themeGitHubTimeout)
	defer cancel()
	releasePath := "/repos/" + source.owner + "/" + source.repo + "/releases/latest"
	if source.tag != "" {
		releasePath = "/repos/" + source.owner + "/" + source.repo + "/releases/tags/" + url.PathEscape(source.tag)
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) == 1 && validThemeGitHubURL(via[0].URL, themeGitHubHost) && validThemeGitHubAssetRedirect(r.URL) {
				r.Header.Del("Referer")
				return nil
			}
			return http.ErrUseLastResponse
		},
	}
	request, err := newThemeGitHubRequest(ctx, "https://"+themeGitHubAPIHost+releasePath)
	if err != nil {
		return themePackage{}, errors.New("invalid GitHub release request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return themePackage{}, errors.New("unable to look up the public GitHub release")
	}
	body, readErr := func() ([]byte, error) {
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("public GitHub release not available")
		}
		return readThemePackageBytes(response.Body, themeGitHubMaxMetadata)
	}()
	var release themeGitHubRelease
	if readErr != nil || json.Unmarshal(body, &release) != nil || release.Draft || release.Prerelease ||
		!validThemeGitHubTag(release.Tag) || (source.tag != "" && source.tag != release.Tag) {
		return themePackage{}, errors.New("public GitHub release not available")
	}

	assetURL := ""
	var assetSize int64
	var assetDigest string
	for _, asset := range release.Assets {
		if asset.Name != themePackageFilename {
			continue
		}
		assetSource, err := parseThemeGitHubSource(asset.URL)
		if err != nil || assetURL != "" || asset.State != "uploaded" || asset.Size <= 0 ||
			asset.Size > themePackageMaxCompressed || assetSource.owner != source.owner ||
			assetSource.repo != source.repo || assetSource.tag != release.Tag ||
			!strings.HasSuffix(asset.URL, "/releases/download/"+url.PathEscape(release.Tag)+"/"+themePackageFilename) {
			return themePackage{}, errors.New("invalid GitHub release theme.tar.gz asset")
		}
		assetURL = asset.URL
		assetSize = asset.Size
		assetDigest = asset.Digest
	}
	if assetURL == "" {
		return themePackage{}, errors.New("release does not include theme.tar.gz")
	}
	request, err = newThemeGitHubRequest(ctx, assetURL)
	if err != nil {
		return themePackage{}, errors.New("invalid GitHub asset request")
	}
	response, err = client.Do(request)
	if err != nil {
		return themePackage{}, errors.New("unable to download GitHub theme archive")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > themePackageMaxCompressed {
		return themePackage{}, errors.New("GitHub theme archive is unavailable or too large")
	}
	archive, err := readThemePackageBytes(response.Body, themePackageMaxCompressed)
	if err != nil || int64(len(archive)) != assetSize {
		return themePackage{}, errors.New("GitHub theme archive size does not match release metadata")
	}
	if assetDigest != "" {
		algorithm, encoded, ok := strings.Cut(assetDigest, ":")
		expected, err := hex.DecodeString(encoded)
		actual := sha256.Sum256(archive)
		if !ok || algorithm != "sha256" || err != nil || !bytes.Equal(expected, actual[:]) {
			return themePackage{}, errors.New("GitHub theme archive digest does not match release metadata")
		}
	}
	theme, err := readThemePackage(bytes.NewReader(archive))
	if err != nil {
		return themePackage{}, err
	}
	theme.Source = "https://" + themeGitHubHost + "/" + source.owner + "/" + source.repo
	theme.Release = theme.Source + "/releases/tag/" + url.PathEscape(release.Tag)
	return theme, nil
}

func newThemeGitHubRequest(ctx context.Context, address string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err == nil {
		req.Header.Set("User-Agent", "LanQin-Email-Theme-Importer")
	}
	return req, err
}

var themeGitHubAssetPath = regexp.MustCompile(`^/github-production-release-asset/[0-9]+/[A-Fa-f0-9-]{36}$`)

func validThemeGitHubAssetRedirect(u *url.URL) bool {
	// GitHub signs release download links on this dedicated host. Only this
	// path, not the rest of *.githubusercontent.com, is eligible for redirect.
	return validThemeGitHubURL(u, themeGitHubAssetHost) && u.RawPath == "" &&
		themeGitHubAssetPath.MatchString(u.Path) && len(u.RawQuery) <= 4096
}
