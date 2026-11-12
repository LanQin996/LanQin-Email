package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const MaxRecoveryIndexBytes = 16384

var errRecoveryIndex = errors.New("recovery index is invalid, unauthenticated or outside the selected destination")

// RecoveryIndexEntry is a separately authenticated discovery receipt, not proof
// that the selected ciphertext still exists or can be decrypted. Its original
// destination ID survives reconfiguration, notably for Drive appProperties.
type RecoveryIndexEntry struct {
	Version       int       `json:"version"`
	DestinationID string    `json:"destinationId"`
	Type          string    `json:"type"`
	Location      string    `json:"location"`
	ObjectKey     string    `json:"objectKey"`
	SHA256        string    `json:"sha256"`
	Bytes         int64     `json:"bytes"`
	CompletedAt   time.Time `json:"completedAt"`
}

type signedRecoveryIndex struct {
	Entry RecoveryIndexEntry `json:"entry"`
	MAC   string             `json:"mac"`
}

// SealRecoveryIndex must only be called after successful ciphertext readback.
// A storage server's untrusted SHA metadata alone must never authorize sealing.
func SealRecoveryIndex(d Destination, object, sha string, size int64, completed time.Time, key []byte) ([]byte, error) {
	location, err := recoveryIndexLocation(d)
	if err != nil || len(key) != sha256.Size {
		return nil, errRecoveryIndex
	}
	digest, err := hex.DecodeString(sha)
	if err != nil || len(digest) != sha256.Size {
		return nil, errRecoveryIndex
	}
	entry := RecoveryIndexEntry{Version: 1, DestinationID: d.ID, Type: d.Type, Location: location, ObjectKey: object, SHA256: hex.EncodeToString(digest), Bytes: size, CompletedAt: completed.UTC()}
	if !validRecoveryIndexEntry(d, entry, location) {
		return nil, errRecoveryIndex
	}
	tag, err := recoveryIndexMAC(entry, key)
	if err != nil {
		return nil, errRecoveryIndex
	}
	document, err := json.Marshal(signedRecoveryIndex{Entry: entry, MAC: hex.EncodeToString(tag)})
	if err != nil || len(document) > MaxRecoveryIndexBytes {
		return nil, errRecoveryIndex
	}
	return document, nil
}

// OpenRecoveryIndex accepts only a bounded, strictly decoded signed receipt
// bound to this physical storage location. The new configuration ID, labels,
// credentials and credential references are intentionally not part of location.
func OpenRecoveryIndex(d Destination, document, key []byte) (RecoveryIndexEntry, error) {
	var empty RecoveryIndexEntry
	if len(document) < 1 || len(document) > MaxRecoveryIndexBytes || len(key) != sha256.Size || !uniqueRecoveryIndexJSON(document) {
		return empty, errRecoveryIndex
	}
	location, err := recoveryIndexLocation(d)
	if err != nil {
		return empty, errRecoveryIndex
	}
	var signed signedRecoveryIndex
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&signed) != nil || decoder.Decode(new(any)) != io.EOF || !validRecoveryIndexEntry(d, signed.Entry, location) {
		return empty, errRecoveryIndex
	}
	tag, err := hex.DecodeString(signed.MAC)
	if err != nil || len(tag) != sha256.Size || hex.EncodeToString(tag) != signed.MAC {
		return empty, errRecoveryIndex
	}
	want, err := recoveryIndexMAC(signed.Entry, key)
	if err != nil || !hmac.Equal(tag, want) {
		return empty, errRecoveryIndex
	}
	signed.Entry.CompletedAt = signed.Entry.CompletedAt.UTC()
	return signed.Entry, nil
}

func validRecoveryIndexEntry(d Destination, entry RecoveryIndexEntry, location string) bool {
	if entry.Version != 1 || !identifierPattern.MatchString(entry.DestinationID) || entry.Type != d.Type || entry.Location != location || !validDownloadObject(d, entry.ObjectKey) || entry.Bytes < 1 || entry.Bytes > MaxUploadBytes || entry.CompletedAt.IsZero() || entry.CompletedAt.Year() < 1 || entry.CompletedAt.Year() > 9999 {
		return false
	}
	digest, err := hex.DecodeString(entry.SHA256)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == entry.SHA256
}

func recoveryIndexMAC(entry RecoveryIndexEntry, key []byte) ([]byte, error) {
	if len(key) != sha256.Size {
		return nil, errRecoveryIndex
	}
	derived := make([]byte, sha256.Size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, nil, []byte("lanqin/backup/recovery-index/v1")), derived); err != nil {
		return nil, errRecoveryIndex
	}
	defer clear(derived)
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, errRecoveryIndex
	}
	mac := hmac.New(sha256.New, derived)
	mac.Write(data)
	return mac.Sum(nil), nil
}

func recoveryIndexLocation(d Destination) (string, error) {
	// Keep the runtime policy attached during validation. In particular, NAS S3
	// must retain its explicit policy and path-style restriction; serializing a
	// destination here and decoding it again would discard that authority.
	if err := (Config{Version: 1, Destinations: []Destination{d}}).Validate(); err != nil {
		return "", errRecoveryIndex
	}
	location := struct {
		Type     string `json:"type"`
		Endpoint string `json:"endpoint,omitempty"`
		Bucket   string `json:"bucket,omitempty"`
		Prefix   string `json:"prefix,omitempty"`
		Folder   string `json:"folder,omitempty"`
	}{Type: d.Type}
	switch d.Type {
	case "s3":
		location.Endpoint = canonicalRecoveryEndpoint(d.S3.Endpoint)
		location.Bucket, location.Prefix = d.S3.Bucket, d.S3.Prefix
	case "webdav":
		location.Endpoint = canonicalRecoveryEndpoint(d.WebDAV.URL)
	case "drive":
		location.Folder = d.Drive.FolderID
	default:
		return "", errRecoveryIndex
	}
	data, err := json.Marshal(location)
	if err != nil {
		return "", errRecoveryIndex
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalRecoveryEndpoint(endpoint string) string {
	// The caller validated a credential-free, unambiguous HTTPS URL above.
	u, _ := url.Parse(endpoint)
	host := strings.ToLower(u.Hostname())
	if address, err := netip.ParseAddr(host); err == nil {
		host = address.String()
	}
	port := endpointPort(u)
	if number, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(number)
	}
	if port != "443" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

// Reject duplicate object keys, including case aliases accepted by Go's struct
// decoder. No second JSON parser gets to choose different authenticated fields.
func uniqueRecoveryIndexJSON(document []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 8 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		if delimiter != '{' && delimiter != '[' {
			return false
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				name, ok := key.(string)
				name = strings.ToLower(name)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
			}
			if !value(depth + 1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
