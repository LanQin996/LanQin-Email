package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/nativedb"
)

func TestNativeVersionedEncryptedArchivePreservesPayloadAndDriver(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"data", "mail", "dkim"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			family := driver + ":16"
			if driver == "mysql" {
				family = "mysql:8.4"
			}
			file, err := os.Create(filepath.Join(root, filepath.FromSlash(nativedb.Filename)))
			if err != nil {
				t.Fatal(err)
			}
			writer, err := nativedb.NewWriter(file, nativedb.Header{Version: 1, Driver: driver, Family: family, SchemaVersion: 12, Tables: []nativedb.Table{{Name: "synthetic", Columns: []nativedb.Column{{Name: "value", Type: "text"}}}}}, nativedb.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if writer.Row("synthetic", []nativedb.Cell{{Kind: "text", Data: "not SQL: '); DROP TABLE synthetic;--"}}) != nil || writer.Close() != nil || file.Close() != nil {
				t.Fatal("cannot prepare typed native fixture")
			}
			marker, _ := json.Marshal(map[string]any{"version": 2, "scope": driver + "-reference-closure", "complete": true, "summary": map[string]int{"messages": 0}, "sources": map[string]string{"data": "/synthetic-data", "mail": "/synthetic-mail"}, "files": []any{}})
			if err := os.WriteFile(filepath.Join(root, "data", "backup-online.json"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "mail", "sample.eml"), []byte("synthetic message"), 0o600); err != nil {
				t.Fatal(err)
			}
			identity, _ := age.GenerateX25519Identity()
			var encrypted bytes.Buffer
			manifest, err := WriteNative(t.Context(), root, &encrypted, identity.Recipient().String(), driver, DefaultLimits())
			if err != nil || manifest.Version != 2 || manifest.Driver != driver {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encrypted.Bytes())
			receipt := hex.EncodeToString(digest[:])
			out := filepath.Join(t.TempDir(), "recovered")
			got, err := Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), out, identity.String(), receipt, DefaultLimits())
			if err != nil || got.Scope != "online-native-data" || got.Driver != driver {
				t.Fatal("native archive rejected", err)
			}
			if payload, err := os.ReadFile(filepath.Join(out, "snapshot", "mail", "sample.eml")); err != nil || string(payload) != "synthetic message" {
				t.Fatal("native payload changed", err)
			}
			report, err := StagedOnlineReportSHA256(t.Context(), out, driver)
			if err != nil || len(report) != 64 {
				t.Fatal(err)
			}
			const password = "synthetic native recovery password"
			var passwordArchive bytes.Buffer
			passwordManifest, err := WriteNativePassword(t.Context(), root, &passwordArchive, password, driver, DefaultLimits())
			if err != nil || passwordManifest.Scope != "online-native-data" || passwordManifest.Driver != driver {
				t.Fatal("native password writer changed database scope", err)
			}
			passwordDigest := sha256.Sum256(passwordArchive.Bytes())
			passwordOut := filepath.Join(t.TempDir(), "password-recovered")
			passwordRecovered, err := RestorePassword(t.Context(), bytes.NewReader(passwordArchive.Bytes()), passwordOut, password, hex.EncodeToString(passwordDigest[:]), DefaultLimits())
			if err != nil || passwordRecovered != passwordManifest {
				t.Fatal("native password archive rejected", err)
			}
			if passwordReport, err := StagedOnlineReportSHA256(t.Context(), passwordOut, driver); err != nil || passwordReport != report {
				t.Fatal("native password recovery changed authenticated report", err)
			}
			originalPayload, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nativedb.Filename)))
			if err != nil {
				t.Fatal(err)
			}
			passwordPayload, err := os.ReadFile(filepath.Join(passwordOut, "snapshot", filepath.FromSlash(nativedb.Filename)))
			if err != nil || !bytes.Equal(originalPayload, passwordPayload) {
				t.Fatal("native password recovery changed typed database payload", err)
			}
			other := "mysql"
			if driver == other {
				other = "postgres"
			}
			if _, err := StagedOnlineReportSHA256(t.Context(), out, other); err == nil {
				t.Fatal("driver-mismatched completion marker accepted")
			}
			if _, err := Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), filepath.Join(t.TempDir(), "bad-hash"), identity.String(), strings.Repeat("0", 64), DefaultLimits()); err == nil {
				t.Fatal("untrusted native ciphertext receipt accepted")
			}
			wrong, _ := age.GenerateX25519Identity()
			if _, err := Restore(t.Context(), bytes.NewReader(encrypted.Bytes()), filepath.Join(t.TempDir(), "bad-key"), wrong.String(), receipt, DefaultLimits()); err == nil {
				t.Fatal("wrong native identity accepted")
			}
			// SQLite writers/readers cannot silently advertise a native payload.
			if _, err := WriteOnline(t.Context(), root, &bytes.Buffer{}, identity.Recipient().String(), DefaultLimits()); err == nil {
				t.Fatal("native archive mislabeled as SQLite")
			}
		})
	}
}
