package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type switchFixture struct {
	source, journal string
	targets         Targets
	owners          Owners
	ops             switchOps
}

// Windows runs real temp-directory copy/rename/recovery operations, but owner
// mapping and directory fsync are explicitly simulated. This is NOT evidence
// that production Linux ownership or crash durability has been exercised.
func newSwitchFixture(t *testing.T) switchFixture {
	t.Helper()
	base := t.TempDir()
	f := switchFixture{
		source: filepath.Join(base, "prepared"), journal: filepath.Join(base, "journals"),
		targets: Targets{filepath.Join(base, "live-data"), filepath.Join(base, "live-mail"), filepath.Join(base, "live-dkim")},
		owners:  Owners{Owner{101, 102}, Owner{201, 202}, Owner{301, 302}},
		ops:     switchOps{owner: func(string, Owner) error { return nil }, syncDir: func(string) error { return nil }, rename: os.Rename},
	}
	for _, path := range []string{f.source, f.journal, f.targets.Data, f.targets.Mail, f.targets.DKIM} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"data", "mail", "dkim"} {
		p := filepath.Join(f.source, name, "nested")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		mustSwitchWrite(t, filepath.Join(p, "payload"), "new-"+name)
		mustSwitchWrite(t, []string{f.targets.Data, f.targets.Mail, f.targets.DKIM}[i]+string(filepath.Separator)+"old", "old-"+name)
	}
	return f
}

func mustSwitchWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSwitchContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s: got %q, %v; want %q", path, got, err, want)
	}
}

func (f switchFixture) apply(ctx context.Context) (JournalInfo, error) {
	return applyWithOps(ctx, f.source, f.targets, f.owners, f.journal, true, f.ops)
}

func assertOldSwitchRoots(t *testing.T, f switchFixture) {
	t.Helper()
	for i, name := range []string{"data", "mail", "dkim"} {
		assertSwitchContent(t, filepath.Join([]string{f.targets.Data, f.targets.Mail, f.targets.DKIM}[i], "old"), "old-"+name)
	}
}

func TestSwitchSuccessAndRollback(t *testing.T) {
	f := newSwitchFixture(t)
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	mail := filepath.Join(f.source, "mail", "nested", "payload")
	if err := os.Chtimes(mail, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var owners []Owner
	f.ops.owner = func(_ string, o Owner) error { owners = append(owners, o); return nil }
	info, err := f.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Path == "" || !switchIDPattern.MatchString(info.TransactionID) {
		t.Fatalf("bad journal: %+v", info)
	}
	for i, name := range []string{"data", "mail", "dkim"} {
		target := []string{f.targets.Data, f.targets.Mail, f.targets.DKIM}[i]
		assertSwitchContent(t, filepath.Join(target, "nested", "payload"), "new-"+name)
		assertSwitchContent(t, filepath.Join(switchSibling(target, "rollback", info.TransactionID), "old"), "old-"+name)
		if runtime.GOOS == "linux" {
			stat, _ := os.Stat(target)
			file, _ := os.Stat(filepath.Join(target, "nested", "payload"))
			if stat.Mode().Perm() != 0o700 || file.Mode().Perm() != 0o600 {
				t.Fatal("unsafe modes")
			}
		}
	}
	stat, err := os.Stat(filepath.Join(f.targets.Mail, "nested", "payload"))
	if err != nil || !stat.ModTime().Equal(stamp) {
		t.Fatalf("mtime not preserved: %v %v", stat, err)
	}
	if len(owners) != 13 {
		t.Fatalf("owner calls: %v", owners)
	}
	for i, o := range owners {
		want := f.owners.Data
		if i >= 9 {
			want = f.owners.DKIM
		} else if i >= 5 {
			want = f.owners.Mail
		}
		if o != want {
			t.Fatalf("incorrect ownership: %v", owners)
		}
	}
	if err := requireAbsent(filepath.Join(f.targets.Data, ".lanqin-restore-incomplete")); err != nil {
		t.Fatal(err)
	}
	if err := rollbackWithOps(context.Background(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
	for _, target := range []string{f.targets.Data, f.targets.Mail, f.targets.DKIM} {
		if _, err := os.Stat(switchSibling(target, "failed", info.TransactionID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := rollbackWithOps(context.Background(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatalf("repeat rollback: %v", err)
	}
}

func TestSwitchSecondPublishFailureRollsBack(t *testing.T) {
	f := newSwitchFixture(t)
	injected := errors.New("injected second publish failure")
	f.ops.beforeStep = func(event string) error {
		if event == "publish-new:mail" {
			data, err := os.ReadFile(filepath.Join(f.targets.Data, ".lanqin-restore-incomplete"))
			if err != nil || !switchIDPattern.MatchString(strings.TrimSpace(string(data))) {
				t.Fatalf("partial switch lacks startup sentinel: %q %v", data, err)
			}
			return injected
		}
		return nil
	}
	info, err := f.apply(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("got %v", err)
	}
	assertOldSwitchRoots(t, f)
	if err := requireAbsent(filepath.Join(f.targets.Data, ".lanqin-restore-incomplete")); err != nil {
		t.Fatal("rollback left active sentinel on original data root", err)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(switchSibling(f.targets.Data, "failed", info.TransactionID)); err != nil {
		t.Fatal("new data was not preserved", err)
	}
}

func TestSwitchCrashBetweenRenames(t *testing.T) {
	f := newSwitchFixture(t)
	info, err := f.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after old -> rollback, before new -> target. The durable
	// markers, not potentially stale journal progress events, drive recovery.
	newPath := switchSibling(f.targets.Mail, "new", info.TransactionID)
	if err := os.Rename(f.targets.Mail, newPath); err != nil {
		t.Fatal(err)
	}
	if err := rollbackWithOps(context.Background(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
	assertSwitchContent(t, filepath.Join(newPath, "nested", "payload"), "new-mail")
}

func TestSwitchJournalAndMarkerTamperingRejected(t *testing.T) {
	for _, which := range []string{"journal-target", "explicit-target", "transaction", "marker"} {
		t.Run(which, func(t *testing.T) {
			f := newSwitchFixture(t)
			info, err := f.apply(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			targets := f.targets
			j, _, err := readSwitchJournal(info.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "journal-target":
				j.Roots[0].Target = f.source
			case "explicit-target":
				targets.Data = f.source
			case "transaction":
				j.Txn = "../../escape"
			case "marker":
				mustSwitchWrite(t, filepath.Join(switchSibling(f.targets.Data, "rollback", j.Txn), ".lanqin-restore-"+j.Txn), "old\nforged\n")
			}
			if which == "journal-target" || which == "transaction" {
				data, err := json.Marshal(j)
				if err != nil {
					t.Fatal(err)
				}
				mustSwitchWrite(t, info.Path, string(data)+"\n")
			}
			if err := rollbackWithOps(context.Background(), info.Path, targets, true, f.ops); err == nil {
				t.Fatal("accepted tampered state")
			}
			// Preflight must reject before touching even the otherwise valid DKIM.
			assertSwitchContent(t, filepath.Join(f.targets.DKIM, "nested", "payload"), "new-dkim")
		})
	}
}

func TestSwitchUnsafePathsAndConfirmation(t *testing.T) {
	f := newSwitchFixture(t)
	if _, err := applyWithOps(context.Background(), f.source, f.targets, f.owners, f.journal, false, f.ops); !errors.Is(err, ErrMaintenanceRequired) {
		t.Fatal(err)
	}
	nested := f.targets
	nested.Mail = filepath.Join(nested.Data, "child")
	if err := os.Mkdir(nested.Mail, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := applyWithOps(context.Background(), f.source, nested, f.owners, f.journal, true, f.ops); err == nil {
		t.Fatal("accepted nested roots")
	}
	badOwners := f.owners
	badOwners.Mail.UID = -1
	if _, err := applyWithOps(context.Background(), f.source, f.targets, badOwners, f.journal, true, f.ops); err == nil {
		t.Fatal("accepted implicit owner")
	}
	if _, err := applyWithOps(context.Background(), f.source, f.targets, f.owners, f.source, true, f.ops); err == nil {
		t.Fatal("accepted nested journal")
	}
	mustSwitchWrite(t, filepath.Join(f.targets.Data, ".lanqin-restore-incomplete"), "unrelated maintenance state\n")
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("accepted preexisting startup sentinel")
	}
	assertOldSwitchRoots(t, f)
}

func TestSwitchSymlinkRejected(t *testing.T) {
	f := newSwitchFixture(t)
	link := filepath.Join(f.source, "mail", "linked")
	if err := os.Symlink(f.targets.Data, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("accepted source symlink")
	}
	assertOldSwitchRoots(t, f)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(f.source), "*.new-*"))
	if len(matches) != 0 {
		t.Fatalf("failed clone leaked: %v", matches)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	targetLink := filepath.Join(filepath.Dir(f.source), "target-link")
	if err := os.Symlink(f.targets.Data, targetLink); err != nil {
		t.Fatal(err)
	}
	bad := f.targets
	bad.Data = targetLink
	if _, err := applyWithOps(context.Background(), f.source, bad, f.owners, f.journal, true, f.ops); err == nil {
		t.Fatal("accepted target symlink")
	}
}

func TestSwitchPreservesExistingSibling(t *testing.T) {
	f := newSwitchFixture(t)
	var preexisting string
	f.ops.beforeStep = func(event string) error {
		if event == "save-old:data" {
			matches, _ := filepath.Glob(filepath.Join(f.journal, "restore-*.journal"))
			j, _, err := readSwitchJournal(matches[0])
			if err != nil {
				return err
			}
			preexisting = switchSibling(f.targets.Data, "rollback", j.Txn)
			if err := os.Mkdir(preexisting, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(preexisting, "untouched"), []byte("keep"), 0o600)
		}
		return nil
	}
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("overwrote existing sibling")
	}
	assertOldSwitchRoots(t, f)
	assertSwitchContent(t, filepath.Join(preexisting, "untouched"), "keep")
}

func TestSwitchCancellationUsesIndependentRollbackContext(t *testing.T) {
	f := newSwitchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.ops.beforeStep = func(event string) error {
		if event == "publish-new:mail" {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if _, err := f.apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
	if _, err := f.apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSwitchPublicEntryRejectsUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("production Linux platform is supported")
	}
	if _, err := Apply(context.Background(), "", Targets{}, Owners{}, "", true); !errors.Is(err, ErrSwitchPlatform) {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), "", Targets{}, true); !errors.Is(err, ErrSwitchPlatform) {
		t.Fatal(err)
	}
}

func TestSwitchCopyFailureNeverTouchesTargets(t *testing.T) {
	f := newSwitchFixture(t)
	f.ops.owner = func(string, Owner) error { return errors.New("ownership rejected") }
	if _, err := f.apply(context.Background()); err == nil || !strings.Contains(err.Error(), "ownership rejected") {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(f.source), "*.new-*"))
	if len(matches) != 0 {
		t.Fatalf("owned clone leaked: %v", matches)
	}
}

func TestSwitchLinuxRealOwnershipAndDirectorySync(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires production Linux ownership and directory fsync")
	}
	f := newSwitchFixture(t)
	owner := Owner{os.Getuid(), os.Getgid()}
	f.owners = Owners{owner, owner, owner}
	f.ops = productionSwitchOps()
	if _, err := Apply(context.Background(), f.source, f.targets, f.owners, f.journal, true); err == nil {
		t.Fatal("public Apply accepted missing prepared activation guard")
	}
	assertOldSwitchRoots(t, f)
	mustSwitchWrite(t, filepath.Join(f.source, "data", switchActivationName), switchActivationValue)
	info, err := Apply(context.Background(), f.source, f.targets, f.owners, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), info.Path, f.targets, true); err != nil {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
}

func TestSwitchTornFinalJournalRecordCanBeRecovered(t *testing.T) {
	f := newSwitchFixture(t)
	info, err := f.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`"quarantine-ne`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rollbackWithOps(context.Background(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatal(err)
	}
	assertOldSwitchRoots(t, f)
	if _, _, err := readSwitchJournal(info.Path); err != nil {
		t.Fatalf("repaired journal not readable: %v", err)
	}
}

func TestSwitchCleanupDoesNotDeleteReplacedClone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows pins the open clone directory against rename; replacement fixture requires Linux")
	}
	f := newSwitchFixture(t)
	var replaced string
	f.ops.owner = func(path string, _ Owner) error {
		clone := filepath.Dir(filepath.Dir(path))
		if !strings.Contains(filepath.Base(clone), ".new-") {
			t.Fatalf("unexpected owner path: %s", path)
		}
		if err := os.Rename(clone, clone+"-retained"); err != nil {
			return err
		}
		if err := os.Mkdir(clone, 0o700); err != nil {
			return err
		}
		replaced = filepath.Join(clone, "unrelated")
		if err := os.WriteFile(replaced, []byte("preserve"), 0o600); err != nil {
			return err
		}
		return errors.New("clone replaced")
	}
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("accepted replaced clone")
	}
	assertSwitchContent(t, replaced, "preserve")
	assertOldSwitchRoots(t, f)
}

func TestSwitchLinuxMountPointRejected(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux mount table")
	}
	f := newSwitchFixture(t)
	f.targets.Data = "/proc"
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("accepted mount point")
	}
}

func TestSwitchLinuxNoReplacePreservesEvenEmptyDestination(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux RENAME_NOREPLACE")
	}
	base := t.TempDir()
	from, to := filepath.Join(base, "from"), filepath.Join(base, "existing")
	for _, path := range []string{from, to} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before, err := pinnedInfo(to, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := renameSwitchNoReplace(from, to); err == nil {
		t.Fatal("overwrote preexisting empty directory")
	}
	after, err := pinnedInfo(to, true)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("destination identity changed", err)
	}
	if _, err := os.Stat(from); err != nil {
		t.Fatal("source moved despite existing destination", err)
	}
}

func TestSwitchPreparedActivationGuard(t *testing.T) {
	for _, failPublish := range []bool{false, true} {
		name := "committed"
		if failPublish {
			name = "publish-failed"
		}
		t.Run(name, func(t *testing.T) {
			f := newSwitchFixture(t)
			sourceGuard := filepath.Join(f.source, "data", switchActivationName)
			mustSwitchWrite(t, sourceGuard, switchActivationValue)
			if failPublish {
				f.ops.beforeStep = func(event string) error {
					if event == "publish-new:mail" {
						assertSwitchContent(t, filepath.Join(f.targets.Data, switchActivationName), switchActivationValue)
						return errors.New("activation fixture publish failure")
					}
					return nil
				}
			}
			info, err := f.apply(context.Background())
			if failPublish {
				if err == nil {
					t.Fatal("expected switch failure")
				}
				assertOldSwitchRoots(t, f)
				failedData := switchSibling(f.targets.Data, "failed", info.TransactionID)
				assertSwitchContent(t, filepath.Join(failedData, switchActivationName), switchActivationValue)
				assertSwitchContent(t, filepath.Join(failedData, ".lanqin-restore-incomplete"), info.TransactionID+"\n")
			} else if err != nil {
				t.Fatal(err)
			}
			if err := requireAbsent(filepath.Join(f.targets.Data, switchActivationName)); err != nil {
				t.Fatal("activation guard remains on active/original target", err)
			}
			if err := requireAbsent(filepath.Join(f.targets.Data, ".lanqin-restore-incomplete")); err != nil {
				t.Fatal("transaction sentinel remains on active/original target", err)
			}
			assertSwitchContent(t, sourceGuard, switchActivationValue)
		})
	}
}

func TestSwitchActivationCleanupMismatchRollsBack(t *testing.T) {
	for _, mutation := range []string{"wrong-content", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSwitchFixture(t)
			mustSwitchWrite(t, filepath.Join(f.source, "data", switchActivationName), switchActivationValue)
			f.ops.beforeStep = func(event string) error {
				if event == "publish-new:dkim" {
					guard := filepath.Join(f.targets.Data, switchActivationName)
					if mutation == "missing" {
						return os.Remove(guard)
					}
					return os.WriteFile(guard, []byte("unrelated state\n"), 0o600)
				}
				return nil
			}
			info, err := f.apply(context.Background())
			if err == nil {
				t.Fatal("activation guard mismatch accepted")
			}
			assertOldSwitchRoots(t, f)
			failedData := switchSibling(f.targets.Data, "failed", info.TransactionID)
			assertSwitchContent(t, filepath.Join(failedData, ".lanqin-restore-incomplete"), info.TransactionID+"\n")
			if mutation == "wrong-content" {
				assertSwitchContent(t, filepath.Join(failedData, switchActivationName), "unrelated state\n")
			}
		})
	}
}

func TestSwitchActivationPreflight(t *testing.T) {
	f := newSwitchFixture(t)
	if _, err := validateSwitchActivationFlag(filepath.Join(f.source, "data"), true); err == nil {
		t.Fatal("production-required guard can be missing")
	}
	guard := filepath.Join(f.source, "data", switchActivationName)
	mustSwitchWrite(t, guard, "invalid activation state\n")
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("invalid prepared activation guard accepted")
	}
	mustSwitchWrite(t, guard, switchActivationValue)
	mustSwitchWrite(t, filepath.Join(f.targets.Data, switchActivationName), switchActivationValue)
	if _, err := f.apply(context.Background()); err == nil {
		t.Fatal("old target activation guard accepted")
	}
	assertOldSwitchRoots(t, f)
	assertSwitchContent(t, filepath.Join(f.targets.Data, switchActivationName), switchActivationValue)
}
