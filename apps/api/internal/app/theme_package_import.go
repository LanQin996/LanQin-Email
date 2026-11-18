package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	themePackageFilename      = "theme.tar.gz"
	themePackageMaxCompressed = 1 << 20
	themePackageMaxExpanded   = 2 << 20
	themePackageMaxManifest   = 64 << 10
)

type themePackage struct {
	ID         string                            `json:"id"`
	Name       string                            `json:"name"`
	Version    string                            `json:"version"`
	APIVersion int                               `json:"apiVersion"`
	Surfaces   map[string]themePackageAppearance `json:"surfaces"`
	Source     string                            `json:"source,omitempty"`
	Release    string                            `json:"release,omitempty"`
}

type themePackageAppearance struct {
	Layout string            `json:"layout"`
	Light  map[string]string `json:"light"`
	Dark   map[string]string `json:"dark"`
}

var (
	themePackageVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	themePackageColor   = regexp.MustCompile(`^([0-9]{1,3}(?:\.[0-9]{1,3})?) ([0-9]{1,3}(?:\.[0-9]{1,3})?)% ([0-9]{1,3}(?:\.[0-9]{1,3})?)%$`)
	themePackageRadius  = regexp.MustCompile(`^([0-9]{1,3}(?:\.[0-9]{1,3})?)rem$`)
)

// Keep synchronized with apps/web/src/themes/contract.ts.
var themePackageColorTokens = strings.Fields(`
	background foreground card card-foreground popover popover-foreground primary primary-foreground
	secondary secondary-foreground muted muted-foreground accent accent-foreground destructive
	destructive-foreground border input ring sidebar-background sidebar-foreground sidebar-primary
	sidebar-primary-foreground sidebar-accent sidebar-accent-foreground sidebar-border sidebar-ring
`)

func readThemePackage(input io.Reader) (themePackage, error) {
	compressed, err := readThemePackageBytes(input, themePackageMaxCompressed)
	if err != nil {
		return themePackage{}, fmt.Errorf("invalid theme archive: %w", err)
	}
	source := bytes.NewReader(compressed)
	gz, err := gzip.NewReader(source)
	if err != nil {
		return themePackage{}, errors.New("theme archive must be a gzip stream")
	}
	gz.Multistream(false)
	defer gz.Close()
	expanded, err := readThemePackageBytes(gz, themePackageMaxExpanded)
	if err != nil || source.Len() != 0 {
		return themePackage{}, errors.New("theme archive must contain one bounded gzip stream")
	}

	const blockSize = 512
	// Check the first raw member too: archive/tar otherwise consumes some
	// extension headers without returning them as entries.
	if len(expanded) < 3*blockSize || len(expanded)%blockSize != 0 ||
		(expanded[156] != tar.TypeReg && expanded[156] != tar.TypeRegA) {
		return themePackage{}, errors.New("theme archive must contain only a regular theme.json file")
	}
	tr := tar.NewReader(bytes.NewReader(expanded))
	h, err := tr.Next()
	if err != nil || h.Typeflag != tar.TypeReg || h.Name != "theme.json" || h.Linkname != "" ||
		h.Size <= 0 || h.Size > themePackageMaxManifest {
		return themePackage{}, errors.New("theme archive must contain only a regular theme.json file")
	}
	end := blockSize + int(h.Size)
	// Only zero padding and the two required TAR end blocks may follow.
	tarEnd := (end + blockSize - 1) / blockSize * blockSize
	if len(expanded) < tarEnd+2*blockSize || bytes.Count(expanded[end:], []byte{0}) != len(expanded)-end {
		return themePackage{}, errors.New("theme archive contains trailing data or has an invalid end marker")
	}
	return decodeThemePackageManifest(expanded[blockSize:end])
}

func readThemePackageBytes(input io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(input, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("size limit exceeded")
	}
	return b, nil
}

func decodeThemePackageManifest(contents []byte) (themePackage, error) {
	if !utf8.Valid(contents) {
		return themePackage{}, errors.New("theme.json must be UTF-8")
	}
	fields, err := readThemePackageObject(contents)
	if err != nil || len(fields) != 5 {
		return themePackage{}, errors.New("theme.json must contain id, name, version, apiVersion and surfaces")
	}
	var p themePackage
	if json.Unmarshal(fields["id"], &p.ID) != nil || !validUIThemeID(p.ID) || (p.ID == "classic" || p.ID == "workbench" || p.ID == "paper") {
		return themePackage{}, errors.New("invalid or reserved theme ID")
	}
	if json.Unmarshal(fields["name"], &p.Name) != nil || p.Name == "" || len(p.Name) > 120 ||
		strings.TrimSpace(p.Name) != p.Name || strings.ContainsRune(p.Name, unicode.ReplacementChar) ||
		strings.ContainsFunc(p.Name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return themePackage{}, errors.New("invalid theme name")
	}
	if json.Unmarshal(fields["version"], &p.Version) != nil || !validThemePackageVersion(p.Version) {
		return themePackage{}, errors.New("theme version must use semantic versioning")
	}
	if json.Unmarshal(fields["apiVersion"], &p.APIVersion) != nil || p.APIVersion != 1 {
		return themePackage{}, errors.New("unsupported theme API version")
	}
	surfaces, err := readThemePackageObject(fields["surfaces"])
	if err != nil || len(surfaces) == 0 {
		return themePackage{}, errors.New("theme must declare at least one surface")
	}
	p.Surfaces = make(map[string]themePackageAppearance, len(surfaces))
	for surface, raw := range surfaces {
		if surface != "front" && surface != "admin" {
			return themePackage{}, errors.New("unsupported theme surface")
		}
		appearance, err := readThemePackageObject(raw)
		if err != nil || len(appearance) != 3 {
			return themePackage{}, errors.New("theme appearance must contain layout, light and dark")
		}
		var a themePackageAppearance
		if json.Unmarshal(appearance["layout"], &a.Layout) != nil || (a.Layout != "classic" && a.Layout != "workbench") {
			return themePackage{}, errors.New("unsupported theme layout")
		}
		a.Light, err = readThemePackageTokens(appearance["light"])
		if err == nil {
			a.Dark, err = readThemePackageTokens(appearance["dark"])
		}
		if err != nil {
			return themePackage{}, err
		}
		p.Surfaces[surface] = a
	}
	return p, nil
}

func validThemePackageVersion(s string) bool {
	match := themePackageVersion.FindStringSubmatch(s)
	if len(s) > 80 || match == nil {
		return false
	}
	for id := range strings.SplitSeq(match[4], ".") {
		if len(id) > 1 && id[0] == '0' && strings.Trim(id, "0123456789") == "" {
			return false
		}
	}
	return true
}

func readThemePackageObject(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	keys := make(map[string]json.RawMessage)
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok || keys[key] != nil {
			return nil, errors.New("invalid or duplicate JSON property")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		keys[key] = value
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, errors.New("invalid JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON object")
	}
	return keys, nil
}

func readThemePackageTokens(raw []byte) (map[string]string, error) {
	fields, err := readThemePackageObject(raw)
	if err != nil {
		return nil, errors.New("theme tokens must be an object")
	}
	tokens := make(map[string]string, len(fields))
	for k, v := range fields {
		var value string
		if json.Unmarshal(v, &value) != nil || !validThemePackageToken(k, value) {
			return nil, errors.New("invalid theme token name or value")
		}
		tokens[k] = value
	}
	return tokens, nil
}

func validThemePackageToken(key, value string) bool {
	if key == "radius" {
		match := themePackageRadius.FindStringSubmatch(value)
		return match != nil && themePackageNumberAtMost(match[1], 2)
	}
	for _, name := range themePackageColorTokens {
		if name == key {
			match := themePackageColor.FindStringSubmatch(value)
			return match != nil && themePackageNumberAtMost(match[1], 360) &&
				themePackageNumberAtMost(match[2], 100) && themePackageNumberAtMost(match[3], 100)
		}
	}
	return false
}

func themePackageNumberAtMost(s string, limit float64) bool {
	n, err := strconv.ParseFloat(s, 64)
	return err == nil && n <= limit
}
