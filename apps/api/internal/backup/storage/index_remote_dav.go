package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"
)

func davRecoveryURL(d Destination, object string) string {
	u, _ := endpointURLWithPolicy(d.WebDAV.URL, d.NetworkPolicy)
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + object
	return u.String()
}

func readDAVRecoveryIndex(ctx context.Context, d Destination, object string, c Credentials, client *http.Client) ([]byte, error) {
	if !validRecoveryIndexObject(d, object) {
		return nil, errRecoveryRemote
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, davRecoveryURL(d, object), nil)
	if err != nil {
		return nil, errRecoveryRemote
	}
	request.SetBasicAuth(c.Username, c.Password)
	response, err := client.Do(request)
	if err != nil {
		return nil, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	return recoveryBody(response, MaxRecoveryIndexBytes)
}

func publishDAVRecoveryIndex(ctx context.Context, d Destination, object string, document []byte, c Credentials, client *http.Client, guard func() error) ([]byte, error) {
	if !validRecoveryIndexObject(d, object) {
		return nil, errRecoveryRemote
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, davRecoveryURL(d, object), bytes.NewReader(document))
	if err != nil {
		return nil, errRecoveryRemote
	}
	request.Header.Set("If-None-Match", "*")
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(c.Username, c.Password)
	if err := guard(); err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errRecoveryRemote
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusPreconditionFailed {
		return nil, errRecoveryRemote
	}
	return readDAVRecoveryIndex(ctx, d, object, c, client)
}

type recoveryDAVStatus struct {
	Statuses []string `xml:"DAV: status"`
	Property struct {
		Type struct {
			Collections []struct{} `xml:"DAV: collection"`
		} `xml:"DAV: resourcetype"`
	} `xml:"DAV: prop"`
}
type recoveryDAVResponse struct {
	Hrefs    []string            `xml:"DAV: href"`
	Statuses []string            `xml:"DAV: status"`
	Props    []recoveryDAVStatus `xml:"DAV: propstat"`
}

func strictRecoveryDAVXML(data []byte) ([]recoveryDAVResponse, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, tokens := 0, 0
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errRecoveryRemote
		}
		tokens++
		if tokens > 16384 {
			return nil, errRecoveryRemote
		}
		switch value := token.(type) {
		case xml.Directive:
			return nil, errRecoveryRemote
		case xml.StartElement:
			if depth == 0 {
				if rootSeen {
					return nil, errRecoveryRemote
				}
				rootSeen = true
			}
			depth++
			if depth > 32 {
				return nil, errRecoveryRemote
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return nil, errRecoveryRemote
			}
		case xml.ProcInst:
			if rootSeen || value.Target != "xml" {
				return nil, errRecoveryRemote
			}
		}
	}
	var value struct {
		XMLName   xml.Name              `xml:"DAV: multistatus"`
		Responses []recoveryDAVResponse `xml:"DAV: response"`
	}
	if xml.Unmarshal(data, &value) != nil || value.XMLName.Space != "DAV:" || value.XMLName.Local != "multistatus" || len(value.Responses) > maxRecoveryIndexes*maxRecoveryPages+1 {
		return nil, errRecoveryRemote
	}
	return value.Responses, nil
}

// Interpret only the selected collection and direct children. Encoded, nested,
// foreign-origin and dot-segment hrefs never become authenticated GET targets.
func recoveryDAVChild(base *url.URL, href string) (string, bool, error) {
	if href == "" || len(href) > 4096 || strings.ContainsAny(href, "\\%") || strings.IndexFunc(href, unicode.IsControl) >= 0 {
		return "", false, errRecoveryRemote
	}
	u, err := url.Parse(href)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(href, "#") || u.RawPath != "" {
		return "", false, errRecoveryRemote
	}
	if u.Path != "" && u.Path != "/" && path.Clean(strings.TrimSuffix(u.Path, "/")) != strings.TrimSuffix(u.Path, "/") {
		return "", false, errRecoveryRemote
	}
	resolved := base.ResolveReference(u)
	if resolved.Scheme != "https" || !strings.EqualFold(resolved.Host, base.Host) {
		return "", false, errRecoveryRemote
	}
	parent := strings.TrimSuffix(base.Path, "/")
	if resolved.Path == parent || resolved.Path == parent+"/" {
		return "", true, nil
	}
	if !strings.HasPrefix(resolved.Path, parent+"/") {
		return "", false, errRecoveryRemote
	}
	name := strings.TrimPrefix(resolved.Path, parent+"/")
	if name == "" || strings.Contains(strings.TrimSuffix(name, "/"), "/") {
		return "", false, errRecoveryRemote
	}
	return name, false, nil
}

func discoverDAVRecoveryIndexes(ctx context.Context, d Destination, key []byte, c Credentials, client *http.Client) ([][]byte, error) {
	base, _ := endpointURLWithPolicy(d.WebDAV.URL, d.NetworkPolicy)
	base.Path = strings.TrimSuffix(base.Path, "/") + "/"
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", base.String(), strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`))
	if err != nil {
		return nil, errRecoveryRemote
	}
	request.Header.Set("Depth", "1")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	request.SetBasicAuth(c.Username, c.Password)
	response, err := client.Do(request)
	if err != nil {
		return nil, errRecoveryRemote
	}
	if response.StatusCode != http.StatusMultiStatus {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	data, err := recoveryBody(response, maxRecoveryPageBody)
	if err != nil {
		return nil, err
	}
	responses, err := strictRecoveryDAVXML(data)
	if err != nil {
		return nil, err
	}
	names := []string{}
	seen := map[string]bool{}
	for _, item := range responses {
		if len(item.Hrefs) != 1 {
			return nil, errRecoveryRemote
		}
		name, self, err := recoveryDAVChild(base, item.Hrefs[0])
		if err != nil {
			return nil, err
		}
		if self {
			continue
		}
		if seen[name] {
			return nil, errRecoveryRemote
		}
		seen[name] = true
		if !strings.HasSuffix(name, recoveryIndexSuffix) {
			continue
		}
		if !validRecoveryIndexObject(d, name) || len(item.Statuses) > 0 {
			return nil, errRecoveryRemote
		}
		success := 0
		for _, prop := range item.Props {
			if len(prop.Statuses) != 1 {
				return nil, errRecoveryRemote
			}
			parts := strings.Fields(prop.Statuses[0])
			if len(parts) < 2 || parts[1] != "200" {
				continue
			}
			if len(prop.Property.Type.Collections) > 0 {
				return nil, errRecoveryRemote
			}
			success++
		}
		if success != 1 {
			return nil, errRecoveryRemote
		}
		names = append(names, name)
		if len(names) > maxRecoveryIndexes {
			return nil, errRecoveryRemote
		}
	}
	documents := make([][]byte, 0, len(names))
	for _, name := range names {
		document, err := readDAVRecoveryIndex(ctx, d, name, c, client)
		if err != nil {
			return nil, err
		}
		entry, err := OpenRecoveryIndex(d, document, key)
		if err != nil || entry.ObjectKey+recoveryIndexSuffix != name {
			return nil, errRecoveryRemote
		}
		documents = append(documents, document)
	}
	return documents, nil
}
