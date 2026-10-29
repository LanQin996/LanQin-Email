package online

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticDKIM(t *testing.T) (string, string, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(private), private
}

func dkimSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE domains(name TEXT, status TEXT, dkim_selector TEXT, dkim_public_key TEXT, dkim_private_key TEXT)`); err != nil {
		t.Fatal(err)
	}
}

func TestDKIMCollectedFromFrozenOnlineDatabaseNotRotatedLiveState(t *testing.T) {
	db, live := liveSQLite(t)
	dkimSchema(t, db)
	public, private, pemBytes := syntheticDKIM(t)
	if _, err := db.Exec(`INSERT INTO domains VALUES('mail.example.test','active','',?,?),('inactive.example.test','disabled','bad/selector','','')`, public, private); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := SnapshotSQLite(t.Context(), live, snapshot, 8<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE domains SET dkim_public_key='rotated',dkim_private_key='rotated' WHERE status='active'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	count, err := CollectDKIM(t.Context(), snapshot, root, 10)
	if err != nil || count != 1 {
		t.Fatal("frozen keys not collected", count, err)
	}
	key, err := os.ReadFile(filepath.Join(dir, "mail.example.test.lanqin.key"))
	if err != nil || !bytes.Equal(key, pemBytes) {
		t.Fatal("collector used live rotation / wrong key", err)
	}
	// Existing output is never overwritten or removed by a second attempt.
	if _, err := CollectDKIM(t.Context(), snapshot, root, 10); err == nil {
		t.Fatal("collector overwrote an existing key")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "mail.example.test.lanqin.key"))
	if !bytes.Equal(after, pemBytes) {
		t.Fatal("existing key changed during failed attempt")
	}
}

func TestDKIMCollectionRejectsUnsafeNamesMismatchAndPartialResults(t *testing.T) {
	pub, priv, _ := syntheticDKIM(t)
	otherPub, _, _ := syntheticDKIM(t)
	for _, scenario := range []string{"domain-traversal", "selector-traversal", "public-mismatch", "invalid-private", "duplicate-name", "count-limit"} {
		t.Run(scenario, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "snapshot.db")
			db, err := sql.Open("sqlite", name)
			if err != nil {
				t.Fatal(err)
			}
			dkimSchema(t, db)
			domain, selector, public, private, limit := "z.example.test", "lanqin", pub, priv, 10
			switch scenario {
			case "domain-traversal":
				domain = "z/../../escape"
			case "selector-traversal":
				selector = "../escape"
			case "public-mismatch":
				public = otherPub
			case "invalid-private":
				private = "SYNTHETIC-SECRET-DO-NOT-LOG"
			case "duplicate-name":
				domain = "A.EXAMPLE.TEST"
			case "count-limit":
				limit = 1
			}
			if _, err := db.Exec(`INSERT INTO domains VALUES('a.example.test','active','lanqin',?,?),(?,'active',?,?,?)`, pub, priv, domain, selector, public, private); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			_, err = CollectDKIM(t.Context(), name, root, limit)
			if err == nil || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("unsafe material accepted or secret leaked")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed collection left a partially complete key set", err)
			}
		})
	}
}

func TestDKIMCollectionRefusesActiveWALDatabase(t *testing.T) {
	db, live := liveSQLite(t)
	dkimSchema(t, db)
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := CollectDKIM(t.Context(), live, root, 10); err == nil {
		t.Fatal("collector inspected an active WAL as immutable")
	}
}
