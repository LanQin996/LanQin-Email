package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/handoff"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/storage"
)

func maintenanceSourceFixture(t *testing.T) (*App, backupJob, string) {
	t.Helper()
	a := newTestApp(t)
	_, req, cipher := restoreJobFixture(t, a)
	job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-admin", req)
	if err != nil || execution == nil {
		t.Fatal(err)
	}
	a.backupDownloadFile = func(_ context.Context, _ storage.Destination, _, sha, out string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
		if err := os.WriteFile(out, cipher, 0o600); err != nil {
			return storage.DownloadReceipt{}, err
		}
		return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(cipher))}, nil
	}
	a.runBackupRestore(t.Context(), *execution)
	stored, _, err := a.backupReadJob(t.Context(), job.ID)
	if err != nil || stored.Restore == nil || !stored.Restore.Ready {
		t.Fatal("ready fixture", err)
	}
	var actor string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE role='admin' AND disabled=0 LIMIT 1`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	return a, stored, actor
}

func TestResolveWebMaintenanceSourceCurrentActorAndIndependentSQLiteCopy(t *testing.T) {
	a, job, actor := maintenanceSourceFixture(t)
	cfg := Config{DBDriver: "sqlite", DBPath: a.cfg.DBPath}
	resolve := func() (WebMaintenanceSource, error) {
		return ResolveWebMaintenanceSource(t.Context(), cfg, a.cfg.BackupRestoreDir, "", a.cfg.BackupSecretKey, job.ID, actor, job.Restore.ReportSHA256)
	}
	source, err := resolve()
	if err != nil || source.ReportSHA256 != job.Restore.ReportSHA256 || source.SourceSHA256 != job.Artifact.SHA256 || source.Driver != "sqlite" {
		t.Fatal("resolve", err)
	}
	if _, err := a.db.Exec(`UPDATE users SET role='user' WHERE id=?`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil {
		t.Fatal("demoted actor authorized")
	}
	if _, err := a.db.Exec(`UPDATE users SET role='admin',disabled=1 WHERE id=?`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil {
		t.Fatal("disabled actor authorized")
	}
	if _, err := a.db.Exec(`UPDATE users SET disabled=0 WHERE id=?`, actor); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	if runtime.GOOS == "windows" {
		uid = 0
	}
	// Go's SQLite-created files may use 0644; real backup.Restore files must be
	// private. Assert/set the synthetic source to the contract before copying.
	if err := filepath.WalkDir(source.Directory, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "installer-copy")
	limits := backup.Limits{MaxBytes: 1 << 30, MaxEntries: 100000}
	if err := handoff.Copy(t.Context(), source.Directory, out, uid, source.ReportSHA256, source.Driver, limits); err != nil {
		t.Fatal("SQLite handoff", err)
	}
	work := t.TempDir()
	os.Chmod(work, 0o700)
	prepared, err := install.Prepare(t.Context(), out, work, "/data", "/var/mail/vhosts", limits, source.ReportSHA256)
	if err != nil {
		t.Fatal("root-owned copy not installable", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	// Copy must not promote changed data even if the authenticated task and
	// plaintext report still match; report-only resolution is not final trust.
	payload := filepath.Join(source.Directory, "snapshot/data/lanqin.db")
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(payload, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err != nil {
		t.Fatal("report-only resolver unexpectedly checks full payload", err)
	}
	if err := handoff.Copy(t.Context(), source.Directory, filepath.Join(t.TempDir(), "bad-copy"), uid, source.ReportSHA256, "sqlite", limits); err == nil {
		t.Fatal("tampered SQLite copied")
	}
}

func TestResolveWebMaintenanceSourceRejectsUnboundInputsAndNeverMigrates(t *testing.T) {
	a, job, actor := maintenanceSourceFixture(t)
	cfg := Config{DBDriver: "sqlite", DBPath: a.cfg.DBPath}
	for _, change := range []string{"actor", "report", "master", "root", "driver", "cancelled", "not-ready", "scope"} {
		t.Run(change, func(t *testing.T) {
			selected := cfg
			root, key, id, who, report := a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey, job.ID, actor, job.Restore.ReportSHA256
			original, raw, err := a.backupReadJob(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			modified := original
			modified.Restore = new(backupRestoreState)
			*modified.Restore = *original.Restore
			m := *original.Restore.Manifest
			modified.Restore.Manifest = &m
			switch change {
			case "actor":
				who = "missing-admin"
			case "report":
				report = strings.Repeat("0", 64)
			case "master":
				key = "wrong-master"
			case "root":
				root = t.TempDir()
			case "driver":
				selected.DBDriver = "postgres"
			case "cancelled":
				modified.CancelRequested = true
			case "not-ready":
				modified.Restore.Ready = false
			case "scope":
				modified.Restore.Manifest.Scope = "offline-data"
			}
			if change == "cancelled" || change == "not-ready" || change == "scope" {
				if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+job.ID, raw, modified); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, current, err := a.backupReadJob(t.Context(), job.ID)
					if err == nil {
						if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+job.ID, current, original); err != nil {
							t.Fatal(err)
						}
					}
				}()
			}
			_, err = ResolveWebMaintenanceSource(t.Context(), selected, root, "", key, id, who, report)
			if err == nil {
				t.Fatal("unbound source accepted")
			}
			for _, secret := range []string{a.cfg.DBPath, a.cfg.BackupRestoreDir, a.cfg.BackupSecretKey} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatal("source error leaked configuration")
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "no-schema.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE untouched(value TEXT);INSERT INTO untouched VALUES('synthetic')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(before)
	_, err = ResolveWebMaintenanceSource(t.Context(), Config{DBDriver: "sqlite", DBPath: path}, a.cfg.BackupRestoreDir, "", a.cfg.BackupSecretKey, job.ID, actor, job.Restore.ReportSHA256)
	if err == nil {
		t.Fatal("missing schema was migrated/accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.Sum256(after)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(actual[:]) {
		t.Fatal("readonly resolver modified source DB")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("resolver created SQLite sidecar")
		}
	}
}

func TestImportVerifiedNativeStageRejectsBeforeDatabaseConnection(t *testing.T) {
	stage := t.TempDir()
	workspace := t.TempDir()
	for _, cfg := range []Config{{DBDriver: "sqlite"}, {DBDriver: "postgres", DBDSN: "host=127.0.0.1 port=1 dbname=synthetic user=fixture sslmode=disable"}, {DBDriver: "mysql", DBDSN: "fixture@tcp(127.0.0.1:1)/synthetic"}} {
		if _, err := ImportVerifiedNativeStage(t.Context(), cfg, stage, strings.Repeat("a", 64), strings.Repeat("b", 64), workspace, backup.Limits{MaxBytes: 1 << 30, MaxEntries: 100000}); err == nil {
			t.Fatal("unverified stage imported")
		}
	}
}

// Called by the real native contract only when an additional isolated EMPTY
// host target has explicitly been provisioned, independently of the CLI target.
func assertNativeMaintenanceSourceImport(t *testing.T, a *App, jobID, recordedRoot string) {
	t.Helper()
	targetDSN := os.Getenv("LANQIN_TEST_NATIVE_" + strings.ToUpper(a.cfg.DBDriver) + "_HOST_DSN")
	if targetDSN == "" {
		t.Log("isolated HOST target DSN absent; host native source/import positive path not exercised")
		return
	}
	if targetDSN == a.cfg.DBDSN {
		t.Fatal("host import must use separate empty target")
	}
	var actor string
	if err := a.db.QueryRow(`SELECT id FROM users WHERE role='admin' AND disabled=0 LIMIT 1`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	job, _, err := a.backupReadJob(t.Context(), jobID)
	if err != nil || job.Restore == nil {
		t.Fatal(err)
	}
	resolve := func() (WebMaintenanceSource, error) {
		return ResolveWebMaintenanceSource(t.Context(), Config{DBDriver: a.cfg.DBDriver, DBDSN: a.cfg.DBDSN}, a.cfg.BackupRestoreDir, recordedRoot, a.cfg.BackupSecretKey, jobID, actor, job.Restore.ReportSHA256)
	}
	source, err := resolve()
	if err != nil || source.SourceSHA256 != job.Artifact.SHA256 || source.Driver != a.cfg.DBDriver {
		t.Fatal("native host authenticated source", err)
	}
	for _, mutation := range []string{"role='user'", "role='admin',disabled=1"} {
		if _, err := a.db.Exec(`UPDATE users SET `+mutation+` WHERE id=?`, actor); err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(); err == nil {
			t.Fatal("native host source authorized demoted/disabled actor")
		}
	}
	if _, err := a.db.Exec(`UPDATE users SET role='admin',disabled=0 WHERE id=?`, actor); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	if runtime.GOOS == "windows" {
		uid = 0
	}
	if err := filepath.WalkDir(source.Directory, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	os.Chmod(parent, 0o700)
	staged := filepath.Join(parent, "host-ready")
	limits := backup.Limits{MaxBytes: 1 << 30, MaxEntries: 100000}
	if err := handoff.Copy(t.Context(), source.Directory, staged, uid, source.ReportSHA256, source.Driver, limits); err != nil {
		t.Fatal("native host independent handoff", err)
	}
	workspace := t.TempDir()
	os.Chmod(workspace, 0o700)
	target := Config{DBDriver: a.cfg.DBDriver, DBDSN: targetDSN}
	// Changed copied payload must fail before creating any target table.
	payload := filepath.Join(staged, "snapshot/data/lanqin-native.jsonl")
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload, append(append([]byte(nil), data...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportVerifiedNativeStage(t.Context(), target, staged, source.ReportSHA256, source.SourceSHA256, workspace, limits); err == nil {
		t.Fatal("host import accepted copied payload tamper")
	}
	probe, err := openNativeBackupDatabase(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	where := "table_schema=DATABASE()"
	if target.DBDriver == "postgres" {
		where = "table_schema=current_schema()"
	}
	var count int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE ` + where + ` AND table_type='BASE TABLE'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("tampered host import changed empty target", count, err)
	}
	if err := os.WriteFile(payload, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ImportVerifiedNativeStage(t.Context(), target, staged, source.ReportSHA256, source.SourceSHA256, workspace, limits)
	if err != nil || result.Rows < 1 || result.Activated || result.Driver != target.DBDriver || result.ReportSHA256 != source.ReportSHA256 || result.SourceSHA256 != source.SourceSHA256 {
		t.Fatal("native host import result", err)
	}
	if checkNativeRestoreActivation(t.Context(), probe, target.DBDriver) == nil {
		t.Fatal("host imported DB activated")
	}
	if _, err := ImportVerifiedNativeStage(t.Context(), target, staged, source.ReportSHA256, source.SourceSHA256, workspace, limits); err == nil {
		t.Fatal("host import overwrote nonempty target")
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatal("host import retained temporary plaintext", err)
	}
	if original, err := os.ReadFile(filepath.Join(source.Directory, "snapshot/data/lanqin-native.jsonl")); err != nil || !bytes.Equal(original, data) {
		t.Fatal("host import changed API source")
	}
	t.Log("Native maintenance source current-role check, independent handoff, full copied-payload validation and guarded EMPTY host database import passed; no service control")
}
