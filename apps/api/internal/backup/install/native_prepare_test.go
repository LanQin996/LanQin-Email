package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/nativedb"
	"lanqin-email-api/internal/backup/online"
)

func newNativeMaterializeFixture(t *testing.T) (materializeFixture, string) {
	t.Helper()
	base := t.TempDir()
	f := materializeFixture{container: filepath.Join(base, "restored"), stage: filepath.Join(base, "restored", "snapshot"), workspace: filepath.Join(base, "work")}
	for _, directory := range []string{f.workspace, filepath.Join(f.stage, "data", "attachments"), filepath.Join(f.stage, "mail"), filepath.Join(f.stage, "dkim")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustSwitchWrite(t, filepath.Join(f.container, "READY"), "Synthetic authenticated native recovery\n")
	var native bytes.Buffer
	writer, err := nativedb.NewWriter(&native, nativedb.Header{Version: 1, Driver: "postgres", Family: "postgres:16", SchemaVersion: 12, Tables: []nativedb.Table{{Name: "synthetic", Columns: []nativedb.Column{{Name: "value", Type: "text"}}}}}, nativedb.DefaultLimits())
	if err != nil || writer.Close() != nil {
		t.Fatal("cannot create native fixture", err)
	}
	f.report.Version, f.report.Scope, f.report.Complete = 2, "postgres-reference-closure", true
	f.report.Summary = json.RawMessage(`{}`)
	f.report.Sources.Data, f.report.Sources.Mail = "/old/data", "/old/mail"
	date := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, item := range []struct{ kind, name, content string }{
		{"database", nativedb.Filename, native.String()},
		{"message", "mail/synthetic.eml", "Subject: Synthetic\r\n\r\nPayload\r\n"},
		{"attachment", "data/attachments/synthetic.bin", "synthetic attachment"},
		{"dkim", "dkim/synthetic.key", "synthetic-key-fixture"},
	} {
		mustSwitchWrite(t, filepath.Join(f.stage, filepath.FromSlash(item.name)), item.content)
		digest := sha256.Sum256([]byte(item.content))
		record := reportFile{Kind: item.kind, CollectedFile: online.CollectedFile{Destination: item.name, Bytes: int64(len(item.content)), SHA256: hex.EncodeToString(digest[:])}}
		if item.kind == "message" || item.kind == "attachment" {
			record.MessageID = "synthetic-message"
		}
		if item.kind == "message" {
			record.InternalDate = &date
		}
		if item.kind == "attachment" {
			record.AttachmentID = "synthetic-attachment"
		}
		f.report.Files = append(f.report.Files, record)
	}
	f.writeReport(t)
	content, err := os.ReadFile(filepath.Join(f.stage, reportName))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return f, hex.EncodeToString(digest[:])
}

func TestNativePreparationAuthenticatesBeforeDatabaseMutation(t *testing.T) {
	for _, damage := range []string{"receipt", "payload", "extra", "missing", "driver", "canceled"} {
		t.Run(damage, func(t *testing.T) {
			f, receipt := newNativeMaterializeFixture(t)
			driver := "postgres"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch damage {
			case "receipt":
				receipt = strings.Repeat("0", 64)
			case "payload":
				mustSwitchWrite(t, filepath.Join(f.stage, "mail", "synthetic.eml"), "modified payload")
			case "extra":
				mustSwitchWrite(t, filepath.Join(f.stage, "data", "extra"), "unexpected")
			case "missing":
				if err := os.Remove(filepath.Join(f.stage, "dkim", "synthetic.key")); err != nil {
					t.Fatal(err)
				}
			case "driver":
				driver = "mysql"
			case "canceled":
				cancel()
			}
			called := false
			p, err := PrepareNativeFiles(ctx, f.container, f.workspace, driver, receipt, backup.DefaultLimits(), func(context.Context, string, []byte) (json.RawMessage, error) {
				called = true
				return json.RawMessage(`{}`), nil
			})
			if err == nil {
				p.Close()
				t.Fatal("damaged input accepted")
			}
			if called {
				t.Fatal("database callback called before authentication")
			}
			if entries, err := os.ReadDir(f.workspace); err != nil || len(entries) != 0 {
				t.Fatal("failed preparation retained plaintext", err)
			}
		})
	}
}

func TestNativePreparationPreservesDateAndSourceAndCleansFailure(t *testing.T) {
	f, receipt := newNativeMaterializeFixture(t)
	failed := errors.New("synthetic preparation failure")
	if _, err := PrepareNativeFiles(t.Context(), f.container, f.workspace, "postgres", receipt, backup.DefaultLimits(), func(context.Context, string, []byte) (json.RawMessage, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatal("callback failure lost", err)
	}
	if entries, err := os.ReadDir(f.workspace); err != nil || len(entries) != 0 {
		t.Fatal("failed callback retained private copy", err)
	}
	plan := json.RawMessage(`{"version":1,"operation":"synthetic"}`)
	p, err := PrepareNativeFiles(t.Context(), f.container, f.workspace, "postgres", receipt, backup.DefaultLimits(), func(_ context.Context, snapshot string, report []byte) (json.RawMessage, error) {
		if snapshot == f.stage || !strings.HasPrefix(snapshot, f.workspace+string(filepath.Separator)) {
			t.Fatal("database preparation used original source")
		}
		if _, err := os.Stat(filepath.Join(snapshot, filepath.FromSlash(nativedb.Filename))); err != nil {
			t.Fatal("callback cannot verify copied native dump", err)
		}
		return plan, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := os.Stat(filepath.Join(p.Path, filepath.FromSlash(nativedb.Filename))); !os.IsNotExist(err) {
		t.Fatal("old native dump shipped live")
	}
	if _, err := os.Stat(filepath.Join(f.stage, filepath.FromSlash(nativedb.Filename))); err != nil {
		t.Fatal("source dump removed", err)
	}
	info, err := os.Stat(filepath.Join(p.Path, "mail", "synthetic.eml"))
	if err != nil || !info.ModTime().Equal(*f.report.Files[1].InternalDate) {
		t.Fatal("INTERNALDATE lost", err)
	}
	for _, name := range []string{"INSTALL-PLAN.json", "data/restore-install-plan.json"} {
		content, err := os.ReadFile(filepath.Join(p.Path, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(content, plan) {
			t.Fatal("plan not retained", err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Path, preparedMarkerName)); err != nil {
		t.Fatal("preparation missing startup guard", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(f.workspace); err != nil || len(entries) != 0 {
		t.Fatal("private copy cleanup failed", err)
	}
}

func TestNativeInstalledRejectsTamperingAndIncompleteJournal(t *testing.T) {
	for _, damage := range []string{"", "payload", "extra", "missing", "plan", "report", "marker", "date", "journal", "targets"} {
		t.Run(damage, func(t *testing.T) {
			f, receipt := newNativeMaterializeFixture(t)
			plan := json.RawMessage(`{"version":1,"operation":"synthetic"}`)
			p, err := PrepareNativeFiles(t.Context(), f.container, f.workspace, "postgres", receipt, backup.DefaultLimits(), func(context.Context, string, []byte) (json.RawMessage, error) { return plan, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			switcher := newSwitchFixture(t)
			switcher.source = p.Path
			info, err := switcher.apply(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(plan)
			planSHA := hex.EncodeToString(digest[:])
			if _, err := VerifyNativeInstalled(t.Context(), info.Path, switcher.targets, "postgres", receipt, planSHA, backup.DefaultLimits()); err != nil {
				t.Fatal("valid native installation rejected", err)
			}
			switch damage {
			case "payload":
				mustSwitchWrite(t, filepath.Join(switcher.targets.Mail, "synthetic.eml"), "tampered")
			case "extra":
				mustSwitchWrite(t, filepath.Join(switcher.targets.Data, "unexpected"), "extra")
			case "missing":
				if err := os.Remove(filepath.Join(switcher.targets.DKIM, "synthetic.key")); err != nil {
					t.Fatal(err)
				}
			case "plan":
				mustSwitchWrite(t, filepath.Join(switcher.targets.Data, "restore-install-plan.json"), `{}`)
			case "report":
				mustSwitchWrite(t, filepath.Join(switcher.targets.Data, "restore-source-report.json"), `{}`)
			case "marker":
				mustSwitchWrite(t, filepath.Join(switcher.targets.Data, ".lanqin-restore-"+info.TransactionID), "incorrect marker")
			case "date":
				if err := os.Chtimes(filepath.Join(switcher.targets.Mail, "synthetic.eml"), time.Now(), time.Now()); err != nil {
					t.Fatal(err)
				}
			case "journal":
				file, err := os.OpenFile(info.Path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("\"interrupted\"\n")
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "targets":
				switcher.targets.Mail = switcher.targets.DKIM
			}
			_, err = VerifyNativeInstalled(t.Context(), info.Path, switcher.targets, "postgres", receipt, planSHA, backup.DefaultLimits())
			if (err != nil) != (damage != "") {
				t.Fatal("incorrect native verification outcome", damage, err)
			}
		})
	}
}
