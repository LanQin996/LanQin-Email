package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func recoveryIndexKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func recoveryIndexDate() time.Time {
	return time.Date(2026, 11, 3, 4, 5, 6, 123, time.FixedZone("fixture", 8*60*60))
}

func TestRecoveryIndexRoundTripBindsLocationAcrossNewConfigurationID(t *testing.T) {
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			d, _, object := downloadDestination(kind)
			key := recoveryIndexKey()
			sha := strings.Repeat("AB", 32)
			document, err := SealRecoveryIndex(d, object, sha, 12345, recoveryIndexDate(), key)
			if err != nil || len(document) > MaxRecoveryIndexBytes {
				t.Fatal("cannot seal recovery index", err)
			}
			newConfig := d
			newConfig.ID, newConfig.Name, newConfig.Disabled = "new_instance_destination", "new display label", true
			if d.S3 != nil {
				settings := *d.S3
				settings.AccessKeyIDEnv, settings.SecretAccessKeyEnv, settings.SessionTokenEnv = "DIFFERENT_KEY", "DIFFERENT_SECRET", "DIFFERENT_SESSION"
				settings.Endpoint = strings.Replace(settings.Endpoint, "https://s3.", "https://S3.", 1) + ":443/"
				newConfig.S3 = &settings
			}
			if d.WebDAV != nil {
				settings := *d.WebDAV
				settings.UsernameEnv, settings.PasswordEnv = "DIFFERENT_USER", "DIFFERENT_PASSWORD"
				settings.URL = strings.Replace(settings.URL, "https://dav.example.invalid", "https://DAV.EXAMPLE.INVALID:443", 1) + "/"
				newConfig.WebDAV = &settings
			}
			if d.Drive != nil {
				settings := *d.Drive
				settings.AccessTokenEnv = "DIFFERENT_ACCESS_TOKEN"
				newConfig.Drive = &settings
			}
			entry, err := OpenRecoveryIndex(newConfig, document, key)
			if err != nil || entry.Version != 1 || entry.DestinationID != d.ID || entry.Type != kind || entry.ObjectKey != object || entry.SHA256 != strings.ToLower(sha) || entry.Bytes != 12345 || !entry.CompletedAt.Equal(recoveryIndexDate()) || entry.CompletedAt.Location() != time.UTC {
				t.Fatal("reconfigured destination lost authenticated receipt", entry, err)
			}
			if kind == "drive" && entry.DestinationID == newConfig.ID {
				t.Fatal("Drive original identity replaced by new ID")
			}
			originalIdentity := newConfig
			originalIdentity.ID = entry.DestinationID
			resealed, err := SealRecoveryIndex(originalIdentity, entry.ObjectKey, entry.SHA256, entry.Bytes, entry.CompletedAt, key)
			if err != nil {
				t.Fatal(err)
			}
			again, err := OpenRecoveryIndex(newConfig, resealed, key)
			if err != nil || again != entry {
				t.Fatal("receipt is not stable for canonical resealing", err)
			}
			location, err := hex.DecodeString(entry.Location)
			if err != nil || len(location) != sha256.Size {
				t.Fatal("location not a SHA256 binding")
			}
			for _, private := range []string{string(key), d.Name, "DIFFERENT_SECRET", "DIFFERENT_PASSWORD", "DIFFERENT_ACCESS_TOKEN", "DIFFERENT_SESSION"} {
				// The original display label may equal the explicit public ID in
				// fixtures; IDs are intentionally recorded and not a secret.
				if private != d.ID && bytes.Contains(document, []byte(private)) {
					t.Fatal("discovery metadata exposed unrelated secret/configuration")
				}
			}
			if d.S3 != nil && bytes.Contains(document, []byte(d.S3.Endpoint)) || d.WebDAV != nil && bytes.Contains(document, []byte(d.WebDAV.URL)) || d.Drive != nil && bytes.Contains(document, []byte(d.Drive.FolderID)) {
				t.Fatal("location metadata exposed raw endpoint/folder instead of digest")
			}
		})
	}
}

func TestRecoveryIndexRejectsCrossLocationAndUnauthenticatedChanges(t *testing.T) {
	for _, kind := range []string{"s3", "webdav", "drive"} {
		t.Run(kind, func(t *testing.T) {
			d, _, object := downloadDestination(kind)
			document, err := SealRecoveryIndex(d, object, strings.Repeat("a", 64), 123, recoveryIndexDate(), recoveryIndexKey())
			if err != nil {
				t.Fatal(err)
			}
			other := d
			if d.S3 != nil {
				settings := *d.S3
				settings.Prefix += "-other"
				other.S3 = &settings
			}
			if d.WebDAV != nil {
				settings := *d.WebDAV
				settings.URL += "-other"
				other.WebDAV = &settings
			}
			if d.Drive != nil {
				settings := *d.Drive
				settings.FolderID += "-other"
				other.Drive = &settings
			}
			if _, err := OpenRecoveryIndex(other, document, recoveryIndexKey()); err == nil {
				t.Fatal("receipt crossed physical location")
			}
			if _, err := OpenRecoveryIndex(d, document, bytes.Repeat([]byte{0x43}, 32)); err == nil {
				t.Fatal("wrong independent index key authenticated")
			}
			for _, mutation := range []func(*RecoveryIndexEntry){
				func(e *RecoveryIndexEntry) { e.SHA256 = strings.Repeat("b", 64) },
				func(e *RecoveryIndexEntry) { e.Bytes++ },
				func(e *RecoveryIndexEntry) { e.DestinationID = "forged_original_id" },
				func(e *RecoveryIndexEntry) { e.CompletedAt = e.CompletedAt.Add(time.Hour) },
			} {
				var signed signedRecoveryIndex
				if err := json.Unmarshal(document, &signed); err != nil {
					t.Fatal(err)
				}
				mutation(&signed.Entry)
				forged, _ := json.Marshal(signed)
				if _, err := OpenRecoveryIndex(d, forged, recoveryIndexKey()); err == nil {
					t.Fatal("cloud metadata change became trusted receipt")
				}
			}
		})
	}
}

func TestRecoveryIndexRejectsMalformedJSONFieldsAndInvalidSignedEntries(t *testing.T) {
	d, _, object := downloadDestination("s3")
	document, err := SealRecoveryIndex(d, object, strings.Repeat("a", 64), 123, recoveryIndexDate(), recoveryIndexKey())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{
		nil, document[:len(document)-1], append(append([]byte{}, document...), []byte("{}")...),
		bytes.Replace(document, []byte(`"entry":`), []byte(`"unknown":1,"entry":`), 1),
		bytes.Replace(document, []byte(`"version":1`), []byte(`"version":1,"unexpected":0`), 1),
		bytes.Replace(document, []byte(`"bytes":123`), []byte(`"bytes":123,"bytes":124`), 1),
		bytes.Replace(document, []byte(`"bytes":123`), []byte(`"bytes":123,"BYTES":123`), 1),
		bytes.Replace(document, []byte(`"mac":"`), []byte(`"mac":"0`), 1),
		bytes.Repeat([]byte(" "), MaxRecoveryIndexBytes+1),
	} {
		if _, err := OpenRecoveryIndex(d, invalid, recoveryIndexKey()); err == nil {
			t.Fatal("malformed index accepted")
		}
	}
	var signed signedRecoveryIndex
	if err := json.Unmarshal(document, &signed); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RecoveryIndexEntry){
		func(e *RecoveryIndexEntry) { e.Version = 2 }, func(e *RecoveryIndexEntry) { e.DestinationID = "../other" }, func(e *RecoveryIndexEntry) { e.Type = "webdav" },
		func(e *RecoveryIndexEntry) { e.Location = strings.Repeat("0", 64) }, func(e *RecoveryIndexEntry) { e.ObjectKey = "../secret.age" },
		func(e *RecoveryIndexEntry) { e.SHA256 = strings.Repeat("A", 64) }, func(e *RecoveryIndexEntry) { e.SHA256 = "not-sha256" },
		func(e *RecoveryIndexEntry) { e.Bytes = 0 }, func(e *RecoveryIndexEntry) { e.Bytes = MaxUploadBytes + 1 }, func(e *RecoveryIndexEntry) { e.CompletedAt = time.Time{} },
	} {
		entry := signed.Entry
		mutate(&entry)
		tag, err := recoveryIndexMAC(entry, recoveryIndexKey())
		if err != nil {
			t.Fatal(err)
		}
		bad, _ := json.Marshal(signedRecoveryIndex{Entry: entry, MAC: hex.EncodeToString(tag)})
		if _, err := OpenRecoveryIndex(d, bad, recoveryIndexKey()); err == nil {
			t.Fatal("invalid signed record accepted")
		}
	}
	for _, key := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		if _, err := SealRecoveryIndex(d, object, strings.Repeat("a", 64), 123, recoveryIndexDate(), key); err == nil {
			t.Fatal("invalid seal key length")
		}
		if _, err := OpenRecoveryIndex(d, document, key); err == nil {
			t.Fatal("invalid open key length")
		}
	}
	if _, err := SealRecoveryIndex(d, object, "synthetic-secret-misplaced-hash", 123, recoveryIndexDate(), recoveryIndexKey()); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("bad secret/hash accepted or echoed")
	}
	// A raw shared-key HMAC is not this independently HKDF-derived purpose.
	entryJSON, _ := json.Marshal(signed.Entry)
	wrong := hmac.New(sha256.New, recoveryIndexKey())
	wrong.Write(entryJSON)
	bad, _ := json.Marshal(signedRecoveryIndex{Entry: signed.Entry, MAC: hex.EncodeToString(wrong.Sum(nil))})
	if _, err := OpenRecoveryIndex(d, bad, recoveryIndexKey()); err == nil {
		t.Fatal("index accepted different cryptographic purpose")
	}
}

func TestRecoveryIndexNASPolicyDoesNotDisappearOrPermitVirtualHost(t *testing.T) {
	policy := testNetworkPolicy(t)
	d := s3Destination("old_nas")
	d.NetworkPolicy, d.S3.Endpoint, d.S3.PathStyle = policy, "https://192.168.1.10:8443", true
	object := d.S3.Prefix + "/" + downloadName
	document, err := SealRecoveryIndex(d, object, strings.Repeat("a", 64), 123, recoveryIndexDate(), recoveryIndexKey())
	if err != nil {
		t.Fatal("valid NAS index rejected", err)
	}
	d.ID = "new_nas"
	if _, err := OpenRecoveryIndex(d, document, recoveryIndexKey()); err != nil {
		t.Fatal("runtime NAS authority dropped", err)
	}
	d.NetworkPolicy = nil
	if _, err := OpenRecoveryIndex(d, document, recoveryIndexKey()); err == nil {
		t.Fatal("NAS policy omission bypassed validation")
	}
	d.NetworkPolicy, d.S3.PathStyle = policy, false
	if _, err := OpenRecoveryIndex(d, document, recoveryIndexKey()); err == nil {
		t.Fatal("NAS virtual-host constraint bypassed")
	}
}
