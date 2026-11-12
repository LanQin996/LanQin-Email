package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordArchiveRoundTripAndAuthentication(t *testing.T) {
	const password = "  synthetic recovery password  "
	source := snapshotFixture(t)
	var encrypted bytes.Buffer
	want, err := WritePassword(t.Context(), source, &encrypted, password, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	data := encrypted.Bytes()
	if bytes.Contains(data, []byte(password)) || bytes.Contains(data, []byte("synthetic.eml")) {
		t.Fatal("password archive exposed secrets or file names")
	}
	hash := sha256.Sum256(data)
	receipt := hex.EncodeToString(hash[:])
	out := filepath.Join(t.TempDir(), "recovered")
	got, err := RestorePassword(t.Context(), bytes.NewReader(data), out, password, receipt, DefaultLimits())
	if err != nil || want != got {
		t.Fatal("password archive round trip failed", err)
	}
	if content, err := os.ReadFile(filepath.Join(out, "snapshot", "mail", "synthetic.eml")); err != nil || string(content) != "Synthetic private message" {
		t.Fatal("password recovery payload changed", err)
	}
	tampered := bytes.Clone(data)
	tampered[len(tampered)-1] ^= 1
	for _, tc := range []struct {
		name, password, receipt string
		data                    []byte
	}{
		{"wrong-password", "other synthetic password", receipt, data},
		{"trimmed-password", strings.TrimSpace(password), receipt, data},
		{"tampered", password, receipt, tampered},
		{"untrusted-receipt", password, strings.Repeat("0", 64), data},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "bad-recovery")
			if _, err := RestorePassword(t.Context(), bytes.NewReader(tc.data), destination, tc.password, tc.receipt, DefaultLimits()); err == nil {
				t.Fatal("unauthenticated password backup published")
			}
			if _, err := os.Stat(filepath.Join(destination, "READY")); !os.IsNotExist(err) {
				t.Fatal("invalid password archive advertised ready")
			}
		})
	}
	identity := identityFixture(t)
	if _, err := Restore(t.Context(), bytes.NewReader(data), filepath.Join(t.TempDir(), "wrong-mode"), identity.String(), receipt, DefaultLimits()); err == nil {
		t.Fatal("password archive accepted private-key recovery")
	}
}

func TestPasswordArchiveRejectsExcessiveScryptWork(t *testing.T) {
	// An untrusted provider can alter the unauthenticated stanza. Reject before
	// allocating memory for any factor above the writer's fixed age default 18.
	header := "age-encryption.org/v1\n-> scrypt AAAAAAAAAAAAAAAAAAAAAA 19\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n--- AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"
	digest := sha256.Sum256([]byte(header))
	if _, err := RestorePassword(t.Context(), strings.NewReader(header), filepath.Join(t.TempDir(), "bad-work-factor"), "synthetic password", hex.EncodeToString(digest[:]), DefaultLimits()); err == nil {
		t.Fatal("excessive scrypt work factor accepted")
	}
}
