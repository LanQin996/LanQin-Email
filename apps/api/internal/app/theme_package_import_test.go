package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

const themeTestManifest = `{"id":"example-theme","name":"示例主题","version":"1.2.3","apiVersion":1,"surfaces":{"front":{"layout":"workbench","light":{"background":"360 100% 100%","radius":"2rem"},"dark":{}}}}`

func writeTestThemeArchive(t *testing.T, name, data string, fileType byte, other bool) []byte {
	t.Helper()
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	w := tar.NewWriter(g)
	h := tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: fileType}
	if fileType == tar.TypeSymlink {
		h.Linkname = "/tmp/theme.json"
	}
	if err := w.WriteHeader(&h); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if other {
		if err := w.WriteHeader(&tar.Header{Name: "payload.js", Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), b.Bytes()...)
}

func TestReadThemePackage(t *testing.T) {
	valid := writeTestThemeArchive(t, "theme.json", themeTestManifest, tar.TypeReg, false)
	result, err := readThemePackage(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "example-theme" || result.Name != "示例主题" || result.Version != "1.2.3" ||
		result.APIVersion != 1 || result.Surfaces["front"].Light["radius"] != "2rem" || result.Source != "" || result.Release != "" {
		t.Fatalf("unexpected package: %#v", result)
	}

	var bomb bytes.Buffer
	w := gzip.NewWriter(&bomb)
	if _, err := w.Write(make([]byte, themePackageMaxExpanded+1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var extensions bytes.Buffer
	gz := gzip.NewWriter(&extensions)
	tr := tar.NewWriter(gz)
	h := &tar.Header{Name: "theme.json", Mode: 0600, Size: int64(len(themeTestManifest)), Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{"custom.note": "unsupported metadata"}, Format: tar.FormatPAX}
	if err := tr.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tr, themeTestManifest); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 1
	for label, data := range map[string][]byte{
		"traversal":           writeTestThemeArchive(t, "../theme.json", themeTestManifest, tar.TypeReg, false),
		"subdirectory":        writeTestThemeArchive(t, "themes/theme.json", themeTestManifest, tar.TypeReg, false),
		"absolute":            writeTestThemeArchive(t, "/theme.json", themeTestManifest, tar.TypeReg, false),
		"symbolic link":       writeTestThemeArchive(t, "theme.json", "", tar.TypeSymlink, false),
		"second file":         writeTestThemeArchive(t, "theme.json", themeTestManifest, tar.TypeReg, true),
		"oversize manifest":   writeTestThemeArchive(t, "theme.json", strings.Repeat(" ", themePackageMaxManifest+1), tar.TypeReg, false),
		"oversize compressed": make([]byte, themePackageMaxCompressed+1),
		"oversize expansion":  bomb.Bytes(),
		"PAX header":          extensions.Bytes(),
		"second gzip stream":  append(bytes.Clone(valid), valid...),
		"truncated":           valid[:len(valid)-6],
		"checksum":            corrupt,
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := readThemePackage(bytes.NewReader(data)); err == nil {
				t.Fatal("accepted invalid archive")
			}
		})
	}
}

func TestThemePackageManifestStrictFields(t *testing.T) {
	for label, pair := range map[string][2]string{
		"reserved ID":                {"example-theme", "workbench"},
		"invalid ID":                 {"example-theme", "e/evil"},
		"unsupported version":        {`"apiVersion":1`, `"apiVersion":2`},
		"name control":               {"示例主题", `\u202e示例主题`},
		"bad version":                {"1.2.3", "1.2.3-02"},
		"version injection":          {"1.2.3", "1.2.3;"},
		"unsupported surface":        {`"front"`, `"shell"`},
		"unsupported renderer":       {"workbench", "imported-react"},
		"unsupported theme token":    {"background", "background-image"},
		"invalid token case":         {"background", "BACKGROUND"},
		"outside HSL range":          {"360 100% 100%", "360.1 100% 100%"},
		"outside percentage range":   {"360 100% 100%", "360 101% 100%"},
		"negative radius":            {"2rem", "-2rem"},
		"outside radius range":       {"2rem", "2.001rem"},
		"CSS injection":              {"2rem", "1rem; color:red"},
		"token whitespace":           {"2rem", `2rem\n`},
		"duplicate token":            {`"radius":"2rem"`, `"radius":"1rem","radius":"2rem"`},
		"duplicate top-level key":    {`"name":"示例主题"`, `"name":"prior name","name":"示例主题"`},
		"forged source":              {`"name":"示例主题"`, `"name":"示例主题","source":"https://evil.example"`},
		"case-confused required key": {`"apiVersion"`, `"ApiVersion"`},
		"extra executable property":  {`"dark":{}`, `"dark":{},"entry":"loader.js"`},
		"null tokens":                {`"dark":{}`, `"dark":null`},
	} {
		t.Run(label, func(t *testing.T) {
			body := strings.Replace(themeTestManifest, pair[0], pair[1], 1)
			if body == themeTestManifest {
				t.Fatal("malformed fixture unchanged")
			}
			if _, err := decodeThemePackageManifest([]byte(body)); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

type themeTestRoundTripper func(*http.Request) (*http.Response, error)

func (f themeTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func themeTestGitHubResponse(status int, loc string, body []byte) *http.Response {
	h := make(http.Header)
	if loc != "" {
		h.Set("Location", loc)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func themeTestReleaseBody(t *testing.T, release themeGitHubRelease) []byte {
	t.Helper()
	b, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDownloadGitHubThemePackage(t *testing.T) {
	const tag = "v1.2.3"
	const releaseURL = "https://github.com/dev/themes/releases/tag/" + tag
	const sourceURL = "https://github.com/dev/themes"
	const downloadURL = "https://github.com/Dev/Themes/releases/download/" + tag + "/theme.tar.gz"
	const cdnURL = "https://release-assets.githubusercontent.com/github-production-release-asset/12/c22541c3-59c7-4eee-ab80-e01bc76bb16a?example=signed"
	archive := writeTestThemeArchive(t, "theme.json", themeTestManifest, tar.TypeReg, false)
	var release themeGitHubRelease
	if err := json.Unmarshal([]byte(`{"tag_name":"`+tag+`","draft":false,"prerelease":false,"assets":[{"name":"theme.tar.gz","browser_download_url":"`+downloadURL+`","state":"uploaded","size":123}]}`), &release); err != nil {
		t.Fatal(err)
	}
	release.Assets[0].Size = int64(len(archive))
	for _, input := range []string{sourceURL, releaseURL, downloadURL} {
		// A missing digest is accepted for compatibility with older releases.
		if input != sourceURL {
			release.Assets[0].Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
		}
		calls := 0
		httpMock := themeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			if _, ok := r.Context().Deadline(); !ok || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Fatal("expected bounded unauthenticated GitHub requests")
			}
			switch r.URL.String() {
			case "https://api.github.com/repos/dev/themes/releases/latest":
				if input != sourceURL {
					t.Fatalf("wrong release endpoint for %s", input)
				}
				return themeTestGitHubResponse(200, "", themeTestReleaseBody(t, release)), nil
			case "https://api.github.com/repos/dev/themes/releases/tags/" + tag:
				if input == sourceURL {
					t.Fatalf("wrong release endpoint for %s", input)
				}
				return themeTestGitHubResponse(200, "", themeTestReleaseBody(t, release)), nil
			case downloadURL:
				return themeTestGitHubResponse(302, cdnURL, nil), nil
			case cdnURL:
				if r.Header.Get("Referer") != "" {
					t.Fatal("unexpected referrer on signed asset request")
				}
				return themeTestGitHubResponse(200, "", archive), nil
			default:
				t.Fatalf("unexpected GitHub request: %s", r.URL)
				return nil, nil
			}
		})
		p, err := downloadGitHubThemePackageWithTransport(context.Background(), input, httpMock)
		if err != nil || calls != 3 || p.Source != sourceURL || p.Release != releaseURL || p.ID != "example-theme" {
			t.Fatalf("unexpected import: %#v error=%v requests=%d", p, err, calls)
		}
	}

	for label, fixture := range map[string]struct {
		location string
		mutate   func(*themeGitHubRelease)
	}{
		"arbitrary redirect":         {location: "https://evil.example/theme.tar.gz"},
		"private API redirect":       {location: "https://api.github.com/user"},
		"unapproved CDN path":        {location: "https://release-assets.githubusercontent.com/other/theme.tar.gz"},
		"CDN URL with credentials":   {location: "https://a@release-assets.githubusercontent.com/github-production-release-asset/12/c22541c3-59c7-4eee-ab80-e01bc76bb16a"},
		"draft":                      {mutate: func(r *themeGitHubRelease) { r.Draft = true }},
		"prerelease":                 {mutate: func(r *themeGitHubRelease) { r.Prerelease = true }},
		"wrong asset URL":            {mutate: func(r *themeGitHubRelease) { r.Assets[0].URL = strings.Replace(downloadURL, "/Themes/", "/Other/", 1) }},
		"wrong asset name":           {mutate: func(r *themeGitHubRelease) { r.Assets[0].Name = "theme.zip" }},
		"duplicate asset":            {mutate: func(r *themeGitHubRelease) { r.Assets = append(r.Assets, r.Assets[0]) }},
		"asset body size mismatch":   {mutate: func(r *themeGitHubRelease) { r.Assets[0].Size++ }},
		"asset body digest mismatch": {mutate: func(r *themeGitHubRelease) { r.Assets[0].Digest = "sha256:" + strings.Repeat("0", 64) }},
		"unsupported digest":         {mutate: func(r *themeGitHubRelease) { r.Assets[0].Digest = "md5:" + strings.Repeat("0", 32) }},
		"oversized asset":            {mutate: func(r *themeGitHubRelease) { r.Assets[0].Size = themePackageMaxCompressed + 1 }},
	} {
		t.Run(label, func(t *testing.T) {
			u := release
			u.Assets = append(u.Assets[:0:0], u.Assets...)
			if fixture.mutate != nil {
				fixture.mutate(&u)
			}
			mock := themeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case "https://api.github.com/repos/dev/themes/releases/latest":
					return themeTestGitHubResponse(200, "", themeTestReleaseBody(t, u)), nil
				case downloadURL:
					if fixture.location != "" {
						return themeTestGitHubResponse(302, fixture.location, nil), nil
					}
					switch label {
					case "asset body size mismatch", "asset body digest mismatch", "unsupported digest":
						return themeTestGitHubResponse(200, "", archive), nil
					}
				}
				t.Fatalf("request made for rejected target: %s", r.URL)
				return nil, nil
			})
			if _, err := downloadGitHubThemePackageWithTransport(context.Background(), sourceURL, mock); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestParseThemeGitHubSourceRejectsOtherTargets(t *testing.T) {
	for _, s := range []string{
		"http://github.com/dev/themes",
		"https://github.com:443/dev/themes",
		"https://github.com.evil.example/dev/themes",
		"https://github.com./dev/themes",
		"https://github.com@evil.example/dev/themes",
		"https://user@github.com/dev/themes",
		"https://127.0.0.1/dev/themes",
		"https://github.com/dev/themes#",
		"https://github.com/dev/themes?token=123",
		"https://github.com/dev/themes?",
		"https://github.com/dev/themes/releases/edit/v1",
		"https://github.com/dev/themes/releases/download/v1/configs.zip",
		"https://github.com/dev/themes/releases/tag/..",
		"https://github.com/dev/themes/releases/tag/%2f%2e%2e",
		"https://github.com/dev/%74hemes",
		"https://github.com/dev/..",
	} {
		if _, err := parseThemeGitHubSource(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.2", "100.64.0.1", "169.254.169.254", "::1", "fd00::1", "64:ff9b::a00:1", "2002:7f00:1::1"} {
		if validThemeGitHubIP(netip.MustParseAddr(s)) {
			t.Errorf("accepted private or non-routable IP %q", s)
		}
	}
}
