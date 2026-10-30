package online

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"unicode"
)

// CollectDKIM materializes active DKIM keys from the CLOSED database produced by
// SnapshotSQLite, not the independently refreshed live rspamd directory. output
// must be a caller-owned private staging root. On failure, only files created by
// this call are removed; callers must never publish an incomplete staging tree.
// The private keys are never returned or included in errors.
func CollectDKIM(ctx context.Context, snapshot string, output *os.Root, maxKeys int) (int, error) {
	if maxKeys < 1 || maxKeys > 100000 {
		return 0, errors.New("invalid DKIM collection limits")
	}
	return CollectDKIMLimited(ctx, snapshot, output, maxKeys, int64(maxKeys)*32768)
}

// CollectDKIMLimited also bounds plaintext key bytes as part of a whole-snapshot
// budget. Callers may pass zero keys/bytes only when there are no active keys.
func CollectDKIMLimited(ctx context.Context, snapshot string, output *os.Root, maxKeys int, maxBytes int64) (int, error) {
	if output == nil || maxKeys < 0 || maxKeys > 100000 || maxBytes < 0 {
		return 0, errors.New("invalid DKIM collection limits")
	}
	info, err := os.Lstat(snapshot)
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("DKIM collection requires a prepared database")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(snapshot + suffix); !errors.Is(err, os.ErrNotExist) {
			return 0, errors.New("DKIM collection requires a closed standalone database")
		}
	}
	uri, err := sqliteURI(snapshot, "mode=ro&immutable=1&_pragma=trusted_schema(OFF)")
	if err != nil {
		return 0, errors.New("cannot resolve DKIM snapshot")
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return 0, errors.New("cannot read DKIM snapshot")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT name,dkim_selector,dkim_public_key,dkim_private_key FROM domains WHERE status='active' ORDER BY name`)
	if err != nil {
		return 0, errors.New("cannot read DKIM snapshot")
	}
	defer rows.Close()
	written := []string{}
	complete := false
	defer func() {
		if !complete {
			for _, name := range written {
				output.Remove(name)
			}
		}
	}()
	seen := map[string]bool{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if len(written) >= maxKeys {
			return 0, errors.New("DKIM snapshot exceeds key count limit")
		}
		var domain, selector, public, private string
		if err := rows.Scan(&domain, &selector, &public, &private); err != nil {
			return 0, errors.New("cannot read DKIM record")
		}
		if selector == "" {
			selector = "lanqin" // Same default as deploy/rspamd/sync-dkim.sh.
		}
		if !dkimComponent(domain, true) || !dkimComponent(selector, false) {
			return 0, errors.New("DKIM snapshot contains an unsafe key name")
		}
		name := domain + "." + selector + ".key"
		if seen[strings.ToLower(name)] {
			return 0, errors.New("DKIM snapshot contains duplicate key names")
		}
		seen[strings.ToLower(name)] = true
		key, err := matchingDKIMKey(public, private)
		if err != nil {
			return 0, err
		}
		if int64(len(key)) > maxBytes {
			return 0, errors.New("DKIM snapshot exceeds byte limit")
		}
		maxBytes -= int64(len(key))
		file, err := output.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, errors.New("cannot create DKIM snapshot without replacement")
		}
		written = append(written, name)
		_, writeErr := file.Write(key)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return 0, errors.New("cannot persist DKIM snapshot")
		}
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return 0, errors.New("DKIM snapshot collection did not complete")
	}
	complete = true
	return len(written), nil
}

func dkimComponent(value string, domain bool) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && (domain || r != '_') {
				return false
			}
		}
	}
	return true
}

func matchingDKIMKey(public, private string) ([]byte, error) {
	invalid := errors.New("DKIM snapshot key is invalid or does not match its public key")
	if len(private) > 32768 || len(public) > 16384 {
		return nil, invalid
	}
	key, err := base64.StdEncoding.Strict().DecodeString(private)
	if err != nil {
		return nil, invalid
	}
	block, rest := pem.Decode(key)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, invalid
	}
	if !bytes.Equal(bytes.TrimSpace(key), bytes.TrimSpace(pem.EncodeToMemory(block))) {
		return nil, invalid
	}
	parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil || parsed.Validate() != nil {
		return nil, invalid
	}
	pubDER, err := base64.StdEncoding.Strict().DecodeString(public)
	if err != nil {
		return nil, invalid
	}
	pub, err := x509.ParsePKIXPublicKey(pubDER)
	if err != nil {
		return nil, invalid
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok || !parsed.PublicKey.Equal(rsaPub) {
		return nil, invalid
	}
	return key, nil
}
