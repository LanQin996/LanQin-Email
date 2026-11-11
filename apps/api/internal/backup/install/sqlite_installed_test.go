package install

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup"
)

func TestOfflineSQLiteInstalledValidationBindsPlanAndActualDatabase(t *testing.T) {
	for _, damage := range []string{"database", "mail", "attachment", "dkim", "extra", "date", "committed-data"} {
		t.Run(damage, func(t *testing.T) {
			f := newMaterializeFixture(t)
			p, err := Prepare(t.Context(), f.container, f.workspace, "/data", "/var/mail/vhosts", backup.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			plan, err := os.ReadFile(filepath.Join(p.Path, "data", "restore-install-plan.json"))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(plan)
			sha := hex.EncodeToString(sum[:])
			report, err := os.ReadFile(filepath.Join(p.Path, "data", "restore-source-report.json"))
			if err != nil {
				t.Fatal(err)
			}
			reportSum := sha256.Sum256(report)
			reportSHA := hex.EncodeToString(reportSum[:])
			preparedDB, err := os.ReadFile(filepath.Join(p.Path, "data", "lanqin.db"))
			if err != nil {
				t.Fatal(err)
			}
			dbSum := sha256.Sum256(preparedDB)
			dbSHA := hex.EncodeToString(dbSum[:])
			switcher := newSwitchFixture(t)
			switcher.source = p.Path
			info, err := switcher.apply(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteInstalled(t.Context(), info.Path, switcher.targets, sha, reportSHA, dbSHA); err != nil {
				t.Fatal("valid offline DB/tree rejected", err)
			}
			if err := VerifySQLiteInstalled(t.Context(), info.Path, switcher.targets, strings.Repeat("0", 64), reportSHA, dbSHA); err == nil {
				t.Fatal("wrong plan accepted")
			}
			db := filepath.Join(switcher.targets.Data, "lanqin.db")
			mail := filepath.Join(switcher.targets.Mail, "example.test", "user", "Maildir", "cur", "m.eml")
			switch damage {
			case "database":
				if err := os.WriteFile(db, []byte("synthetic corrupted SQLite"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "mail":
				if err := os.Remove(mail); err != nil {
					t.Fatal(err)
				}
			case "attachment":
				if err := os.WriteFile(filepath.Join(switcher.targets.Data, "attachments", "m", "a_file.txt"), []byte("changed attachment"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "dkim":
				if err := os.WriteFile(filepath.Join(switcher.targets.DKIM, "example.test.key"), []byte("changed key"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(switcher.targets.Data, "unexpected"), []byte("unexpected"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "date":
				if err := os.Chtimes(mail, time.Now(), time.Now()); err != nil {
					t.Fatal(err)
				}
			case "committed-data":
				current, err := sql.Open("sqlite", db)
				if err != nil {
					t.Fatal(err)
				}
				_, err = current.Exec("UPDATE send_queue SET mime_base64='synthetic new committed work' WHERE id='q'")
				current.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifySQLiteInstalled(t.Context(), info.Path, switcher.targets, sha, reportSHA, dbSHA); err == nil {
				t.Fatal("changed actual installation accepted", damage)
			}
			if damage == "committed-data" {
				if err := VerifySQLiteRuntime(t.Context(), info.Path, switcher.targets, sha); err != nil {
					t.Fatal("legitimate committed data was required to equal old snapshot", err)
				}
			}
		})
	}
}
