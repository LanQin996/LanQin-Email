package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	recoveryIndexSuffix = ".index.json"
	maxRecoveryIndexes  = 128
	maxRecoveryPages    = 16
	maxRecoveryPageBody = 128 << 10
)

var errRecoveryRemote = errors.New("recovery index publication or discovery could not be verified")

// PublishRecoveryIndex first verifies the entire remote ciphertext, then
// publishes an authenticated discovery descriptor without replacing an object.
// It never deletes objects, changes scopes or returns a provider response body.
func PublishRecoveryIndex(ctx context.Context, d Destination, object, sha string, size int64, at time.Time, key []byte, c Credentials) error {
	return publishRecoveryIndex(ctx, d, object, sha, size, at, key, c, nil)
}

func publishRecoveryIndex(ctx context.Context, d Destination, object, sha string, size int64, at time.Time, key []byte, c Credentials, client *http.Client, guards ...func(context.Context) error) error {
	document, err := SealRecoveryIndex(d, object, sha, size, at, key)
	if err != nil || !validDownloadCredentials(d, c) {
		return errRecoveryRemote
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanup := func() {}
	if client == nil {
		client, cleanup = recoveryIndexClient(d, true)
	}
	defer cleanup()
	if err := retainRemote(ctx, d, object, sha, size, c, false, client); err != nil {
		return recoveryIndexError(ctx)
	}
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, check := range guards {
			if check != nil {
				if err := check(ctx); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var actual []byte
	switch d.Type {
	case "s3":
		actual, err = publishS3RecoveryIndex(ctx, d, object+recoveryIndexSuffix, document, c, client, guard)
	case "webdav":
		actual, err = publishDAVRecoveryIndex(ctx, d, object+recoveryIndexSuffix, document, c, client, guard)
	case "drive":
		actual, err = publishDriveRecoveryIndex(ctx, d, object, document, key, c, client, guard)
	default:
		return errRecoveryRemote
	}
	if err != nil {
		return recoveryIndexError(ctx)
	}
	if bytes.Equal(actual, document) {
		return nil
	}
	// Deterministic sidecars can already exist after a lost response/retry.
	// Authenticate their content rather than overwriting or requiring a newly
	// generated completion timestamp to equal the original receipt.
	previous, err := OpenRecoveryIndex(d, actual, key)
	wanted, wantErr := OpenRecoveryIndex(d, document, key)
	if err != nil || wantErr != nil || previous.DestinationID != wanted.DestinationID || previous.Type != wanted.Type || previous.Location != wanted.Location || previous.ObjectKey != wanted.ObjectKey || previous.SHA256 != wanted.SHA256 || previous.Bytes != wanted.Bytes {
		return errRecoveryRemote
	}
	return nil
}

// DiscoverRecoveryIndexes returns authenticated historical receipts observed in
// the selected location. It does not prove that no newer backup was withheld,
// nor that a receipted ciphertext still exists; Download/Restore decide current
// availability, exact bytes and full archive authentication before recovery.
func DiscoverRecoveryIndexes(ctx context.Context, d Destination, key []byte, c Credentials) ([]RecoveryIndexEntry, error) {
	return discoverRecoveryIndexes(ctx, d, key, c, nil)
}

func discoverRecoveryIndexes(ctx context.Context, d Destination, key []byte, c Credentials, client *http.Client) ([]RecoveryIndexEntry, error) {
	if (Config{Version: 1, Destinations: []Destination{d}}).Validate() != nil || len(key) != 32 || !validDownloadCredentials(d, c) {
		return nil, errRecoveryRemote
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cleanup := func() {}
	if client == nil {
		client, cleanup = recoveryIndexClient(d, false)
	}
	defer cleanup()
	var documents [][]byte
	var err error
	switch d.Type {
	case "s3":
		documents, err = discoverS3RecoveryIndexes(ctx, d, key, c, client)
	case "webdav":
		documents, err = discoverDAVRecoveryIndexes(ctx, d, key, c, client)
	case "drive":
		documents, err = discoverDriveRecoveryIndexes(ctx, d, key, c, client)
	default:
		return nil, errRecoveryRemote
	}
	if err != nil || len(documents) > maxRecoveryIndexes {
		return nil, recoveryIndexError(ctx)
	}
	entries := []RecoveryIndexEntry{}
	seen := map[string]RecoveryIndexEntry{}
	for _, document := range documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := OpenRecoveryIndex(d, document, key)
		if err != nil {
			return nil, errRecoveryRemote
		}
		id := entry.DestinationID + "\x00" + entry.ObjectKey
		if previous, exists := seen[id]; exists {
			if previous.SHA256 != entry.SHA256 || previous.Bytes != entry.Bytes || previous.Location != entry.Location || previous.Type != entry.Type {
				return nil, errRecoveryRemote
			}
			// Repeated Drive publications may create another immutable descriptor.
			// Preserve the earliest authenticated completion without claiming it
			// establishes global freshness or completeness.
			if previous.CompletedAt.Before(entry.CompletedAt) {
				continue
			}
		}
		seen[id] = entry
	}
	for _, entry := range seen {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CompletedAt.Equal(entries[j].CompletedAt) {
			return entries[i].ObjectKey < entries[j].ObjectKey
		}
		return entries[i].CompletedAt.After(entries[j].CompletedAt)
	})
	return entries, nil
}

func recoveryIndexError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errRecoveryRemote
}

func recoveryIndexClient(d Destination, publish bool) (*http.Client, func()) {
	client, closeClient := downloadClient(d)
	methods := map[string]bool{http.MethodGet: true}
	if publish {
		if d.Type == "drive" {
			methods[http.MethodPost] = true
		} else {
			methods[http.MethodPut] = true
		}
	} else if d.Type == "webdav" {
		methods["PROPFIND"] = true
	}
	client.Transport.(*restrictedTransport).methods = methods
	return client, closeClient
}

func recoveryBody(response *http.Response, limit int64) ([]byte, error) {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errRecoveryRemote
	}
	return data, nil
}

func recoveryJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return errRecoveryRemote
	}
	return nil
}

func validRecoveryIndexObject(d Destination, name string) bool {
	return strings.HasSuffix(name, recoveryIndexSuffix) && validDownloadObject(d, strings.TrimSuffix(name, recoveryIndexSuffix))
}

func validRecoveryPageToken(token string) bool {
	return len(token) > 0 && len(token) <= 2048 && strings.IndexFunc(token, unicode.IsControl) < 0
}
