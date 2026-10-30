package install

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"lanqin-email-api/internal/backup/online"
)

// Targets must be existing, stopped-service directories, not mount points.
type Targets struct{ Data, Mail, DKIM string }
type Owner struct{ UID, GID int }
type Owners struct{ Data, Mail, DKIM Owner }

const (
	switchActivationName  = ".lanqin-restore-not-activated"
	switchActivationValue = "prepared-online-restore-v1\n"
)

type JournalInfo struct {
	Path            string
	TransactionID   string
	RollbackTargets Targets
}

var (
	ErrMaintenanceRequired = errors.New("restore requires explicit maintenance confirmation")
	ErrSwitchPlatform      = errors.New("restore switching requires Linux")
	ErrSwitchUnsafe        = errors.New("unsafe restore switch path or state")
)

type switchRoot struct {
	Name, Target, OldToken, NewToken string
}

type switchJournal struct {
	Version int
	Txn     string
	Roots   []switchRoot
	Events  []string `json:"-"`
}

type switchOps struct {
	owner   func(string, Owner) error
	syncDir func(string) error
	rename  func(string, string) error
	// Tests inject deterministic failures without exposing a production bypass.
	beforeStep func(string) error
}

func productionSwitchOps() switchOps {
	return switchOps{
		owner: func(path string, owner Owner) error { return os.Chown(path, owner.UID, owner.GID) },
		syncDir: func(path string) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Sync()
		},
		rename: renameSwitchNoReplace,
	}
}

// Apply only renames local directories. It never stops services, contacts Docker,
// imports a database, or deletes rollback backups. The caller must stop ALL
// writers first. The three separate renames are not a global atomic operation.
// Owners are mandatory numeric identities of the destination service accounts;
// ownership and permissions from the backup are never replayed.
func Apply(ctx context.Context, preparedPath string, targets Targets, owners Owners, journalDir string, confirmMaintenance bool) (JournalInfo, error) {
	if runtime.GOOS != "linux" {
		return JournalInfo{}, ErrSwitchPlatform
	}
	if !confirmMaintenance {
		return JournalInfo{}, ErrMaintenanceRequired
	}
	if _, err := validateSwitchActivationFlag(filepath.Join(preparedPath, "data"), true); err != nil {
		return JournalInfo{}, err
	}
	return applyWithOps(ctx, preparedPath, targets, owners, journalDir, confirmMaintenance, productionSwitchOps())
}

// Rollback requires the operator to supply all three destination roots again.
// Replaced new trees are quarantined, never deleted. A journal is not authority
// to access an arbitrary path: roots, transaction and derived names are checked.
func Rollback(ctx context.Context, journalFile string, targets Targets, confirmMaintenance bool) error {
	if runtime.GOOS != "linux" {
		return ErrSwitchPlatform
	}
	return rollbackWithOps(ctx, journalFile, targets, confirmMaintenance, productionSwitchOps())
}

func applyWithOps(ctx context.Context, preparedPath string, targets Targets, owners Owners, journalDir string, confirmed bool, ops switchOps) (info JournalInfo, result error) {
	if !confirmed {
		return info, ErrMaintenanceRequired
	}
	if err := ctx.Err(); err != nil {
		return info, err
	}
	ownerList := []Owner{owners.Data, owners.Mail, owners.DKIM}
	for _, owner := range ownerList {
		if owner.UID < 0 || owner.GID < 0 {
			return info, ErrSwitchUnsafe
		}
	}
	source, err := switchRealDirectory(preparedPath)
	if err != nil {
		return info, err
	}
	activationPresent, err := validateSwitchActivationFlag(filepath.Join(source, "data"), false)
	if err != nil {
		return info, err
	}
	journalDir, err = switchRealDirectory(journalDir)
	if err != nil {
		return info, err
	}
	if runtime.GOOS == "linux" {
		for _, dir := range []string{source, journalDir} {
			identity, err := pinnedInfo(dir, true)
			if err != nil || !materializePrivateInfo(identity) {
				return info, ErrSwitchUnsafe
			}
		}
	}
	roots, err := validateTargets(targets, false)
	if err != nil {
		return info, err
	}
	allPaths := append(append([]string{}, roots...), source, journalDir)
	if !disjointPaths(allPaths) {
		return info, ErrSwitchUnsafe
	}
	originals := make(map[string]os.FileInfo, len(roots))
	for _, root := range roots {
		identity, err := pinnedInfo(root, true)
		if err != nil {
			return info, err
		}
		originals[root] = identity
	}
	txn, err := randomSwitchID()
	if err != nil {
		return info, err
	}
	j := switchJournal{Version: 1, Txn: txn}
	for i, name := range []string{"data", "mail", "dkim"} {
		oldToken, err := randomSwitchID()
		if err != nil {
			return info, err
		}
		newToken, err := randomSwitchID()
		if err != nil {
			return info, err
		}
		j.Roots = append(j.Roots, switchRoot{name, roots[i], oldToken, newToken})
		if err := requireAbsent(filepath.Join(roots[i], ".lanqin-restore-incomplete")); err != nil {
			return info, err
		}
		if i == 0 {
			if err := requireAbsent(filepath.Join(roots[i], switchActivationName)); err != nil {
				return info, err
			}
		}
		for _, kind := range []string{"new", "rollback", "failed"} {
			if err := requireAbsent(switchSibling(roots[i], kind, txn)); err != nil {
				return info, err
			}
		}
	}
	// An identity obtained from an open handle is frozen now (including Windows
	// file IDs), rather than a path Lstat whose identity may be loaded lazily.
	clones := make(map[string]os.FileInfo)
	started := false
	defer func() {
		if result == nil {
			return
		}
		if started {
			// Cancellation must not prevent restoring an already displaced old tree.
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := rollbackWithOps(rollbackCtx, info.Path, targets, true, ops); err != nil {
				result = errors.Join(result, fmt.Errorf("automatic rollback incomplete; retain journal %s: %w", info.Path, err))
			}
			return
		}
		for path, identity := range clones {
			current, err := pinnedInfo(path, true)
			if err == nil && os.SameFile(identity, current) {
				// Only roots created exclusively by this call are eligible.
				if err := os.RemoveAll(path); err != nil {
					result = errors.Join(result, err)
				}
			}
		}
	}()
	remainingBytes, remainingEntries := int64(10<<30), 100000
	for i, root := range j.Roots {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		clone := switchSibling(root.Target, "new", txn)
		if err := os.Mkdir(clone, 0o700); err != nil {
			return info, err
		}
		identity, err := pinnedInfo(clone, true)
		if err != nil {
			return info, err
		}
		clones[clone] = identity
		if err := clonePreparedTree(ctx, filepath.Join(source, root.Name), clone, ownerList[i], &remainingBytes, &remainingEntries, root.Name == "data", ops); err != nil {
			return info, err
		}
		if root.Name == "data" {
			present, err := validateSwitchActivationFlag(clone, activationPresent)
			if err != nil || present != activationPresent {
				return info, ErrSwitchUnsafe
			}
		}
		if err := createSwitchMarker(clone, txn, "new", root.NewToken, ops); err != nil {
			return info, err
		}
		if err := ownSwitchMetadata(filepath.Join(clone, ".lanqin-restore-"+txn), ownerList[i], ops); err != nil {
			return info, err
		}
		if root.Name == "data" {
			if err := createSwitchSentinel(clone, txn, ops); err != nil {
				return info, err
			}
			if err := ownSwitchMetadata(filepath.Join(clone, ".lanqin-restore-incomplete"), ownerList[i], ops); err != nil {
				return info, err
			}
		}
		if err := ops.syncDir(filepath.Dir(clone)); err != nil {
			return info, err
		}
	}
	info = JournalInfo{filepath.Join(journalDir, "restore-"+txn+".journal"), txn, Targets{roots[0], roots[1], roots[2]}}
	if err := createSwitchJournal(info.Path, j, ops); err != nil {
		return info, err
	}
	started = true
	for _, root := range j.Roots {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		current, err := pinnedInfo(root.Target, true)
		if err != nil || !os.SameFile(originals[root.Target], current) {
			return info, ErrSwitchUnsafe
		}
		if err := appendSwitchEvent(info.Path, "mark-old:"+root.Name); err != nil {
			return info, err
		}
		if err := createSwitchMarker(root.Target, txn, "old", root.OldToken, ops); err != nil {
			return info, err
		}
		if err := switchRename(ctx, info.Path, "save-old:"+root.Name, root.Target, switchSibling(root.Target, "rollback", txn), j, root, "old", ops); err != nil {
			return info, err
		}
		if err := switchRename(ctx, info.Path, "publish-new:"+root.Name, switchSibling(root.Target, "new", txn), root.Target, j, root, "new", ops); err != nil {
			return info, err
		}
	}
	if err := appendSwitchEvent(info.Path, "committed"); err != nil {
		return info, err
	}
	// Keep the transaction sentinel until activation guard verification/removal
	// has succeeded durably. Any cleanup failure rolls back and fails closed.
	if activationPresent {
		if err := removeSwitchGuard(roots[0], switchActivationName, switchActivationValue, ops); err != nil {
			return info, err
		}
	}
	if err := removeSwitchSentinel(roots[0], txn, ops); err != nil {
		return info, err
	}
	return info, nil
}

func rollbackWithOps(ctx context.Context, journalFile string, targets Targets, confirmed bool, ops switchOps) error {
	if !confirmed {
		return ErrMaintenanceRequired
	}
	j, journalFile, err := readSwitchJournal(journalFile)
	if err != nil {
		return err
	}
	roots, err := validateTargets(targets, true)
	if err != nil {
		return err
	}
	if len(j.Roots) != 3 || !disjointPaths(append(append([]string{}, roots...), filepath.Dir(journalFile))) {
		return ErrSwitchUnsafe
	}
	for i, root := range j.Roots {
		if root.Name != []string{"data", "mail", "dkim"}[i] || root.Target != roots[i] || !switchIDPattern.MatchString(root.OldToken) || !switchIDPattern.MatchString(root.NewToken) {
			return ErrSwitchUnsafe
		}
	}
	type rollbackAction struct {
		root       switchRoot
		quarantine bool
	}
	var actions []rollbackAction
	// Validate every root before moving any root, so a corrupted third marker
	// cannot cause an otherwise avoidable partial rollback of the first two.
	for i := len(j.Roots) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		root := j.Roots[i]
		backup := switchSibling(root.Target, "rollback", j.Txn)
		oldState, err := switchMarkerState(backup, j.Txn, root)
		if err != nil {
			return err
		}
		targetState, err := switchMarkerState(root.Target, j.Txn, root)
		if err != nil {
			return err
		}
		if oldState == "absent" {
			// Not displaced, or a previous rollback already restored this root.
			if targetState != "old" && targetState != "unmarked" {
				return ErrSwitchUnsafe
			}
			if targetState == "unmarked" && switchRootWasMoved(j, root.Name) {
				return ErrSwitchUnsafe
			}
			// A preceding process may have crashed after restoring the old
			// name but before syncing its parent. Repeated recovery must finish
			// that durability boundary, not merely recognize the marker.
			if targetState == "old" {
				if err := ops.syncDir(filepath.Dir(root.Target)); err != nil {
					return err
				}
			}
			continue
		}
		if oldState != "old" || (targetState != "new" && targetState != "absent") {
			return ErrSwitchUnsafe
		}
		if targetState == "new" {
			if err := requireAbsent(switchSibling(root.Target, "failed", j.Txn)); err != nil {
				return err
			}
		}
		actions = append(actions, rollbackAction{root, targetState == "new"})
	}
	for _, action := range actions {
		root := action.root
		backup := switchSibling(root.Target, "rollback", j.Txn)
		if action.quarantine {
			if err := switchRename(ctx, journalFile, "quarantine-new:"+root.Name, root.Target, switchSibling(root.Target, "failed", j.Txn), j, root, "new", ops); err != nil {
				return err
			}
		}
		if err := switchRename(ctx, journalFile, "restore-old:"+root.Name, backup, root.Target, j, root, "old", ops); err != nil {
			return err
		}
	}
	return appendSwitchEvent(journalFile, "rolled-back")
}

func clonePreparedTree(ctx context.Context, source, output string, owner Owner, bytesLeft *int64, entriesLeft *int, allowActivation bool, ops switchOps) error {
	if _, err := switchRealDirectory(source); err != nil {
		return err
	}
	in, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenRoot(output)
	if err != nil {
		return err
	}
	defer out.Close()
	var files []online.FileMapping
	times := make(map[string]time.Time)
	var dirs []string
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		*entriesLeft--
		if *entriesLeft < 0 {
			return online.ErrFileLimit
		}
		rel, err := filepath.Rel(source, path)
		allowedGuard := allowActivation && rel == switchActivationName
		if err != nil || len(rel) > 4096 || strings.Count(filepath.ToSlash(rel), "/") > 64 || (strings.HasPrefix(entry.Name(), ".lanqin-restore-") && !allowedGuard) {
			return ErrSwitchUnsafe
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrSwitchUnsafe
		}
		if info.IsDir() {
			if rel != "." {
				if err := out.Mkdir(rel, 0o700); err != nil {
					return err
				}
			}
			dirs = append(dirs, rel)
			return nil
		}
		// Pin via File.Stat, not the directory enumeration's lazy FileInfo.
		file, err := in.Open(rel)
		if err != nil {
			return err
		}
		pinned, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(info, pinned) || !pinned.Mode().IsRegular() || pinned.Size() < 0 || pinned.Size() > *bytesLeft {
			return ErrSwitchUnsafe
		}
		*bytesLeft -= pinned.Size()
		name := filepath.ToSlash(rel)
		files = append(files, online.FileMapping{Source: name, Destination: name, ExpectedInfo: pinned})
		times[rel] = pinned.ModTime()
		return nil
	})
	if err != nil {
		return err
	}
	// CopyFiles hashes and independently re-reads pinned source bytes, rejects
	// links and enforces context/entry/byte limits before returning.
	if _, err := online.CopyFiles(ctx, in, out, files, online.FileLimits{MaxBytes: 10 << 30, MaxEntries: 100000}); err != nil {
		return err
	}
	for _, file := range files {
		path := filepath.Join(output, filepath.FromSlash(file.Destination))
		if err := ops.owner(path, owner); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		mtime := times[filepath.FromSlash(file.Destination)]
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			return errors.Join(syncErr, closeErr)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		path := filepath.Join(output, dirs[i])
		if err := ops.owner(path, owner); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
		if err := ops.syncDir(path); err != nil {
			return err
		}
	}
	return nil
}

var switchIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func randomSwitchID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

func switchSibling(target, kind, txn string) string {
	return filepath.Join(filepath.Dir(target), filepath.Base(target)+"."+kind+"-"+txn)
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrSwitchUnsafe
}

func pinnedInfo(path string, directory bool) (os.FileInfo, error) {
	lstat, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if lstat.Mode()&os.ModeSymlink != 0 || (directory && !lstat.IsDir()) || (!directory && !lstat.Mode().IsRegular()) {
		return nil, ErrSwitchUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(lstat, info) {
		return nil, ErrSwitchUnsafe
	}
	return info, nil
}

// switchRealDirectory rejects traversal and symlinks in every existing component.
func switchRealDirectory(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", ErrSwitchUnsafe
	}
	for current := path; ; current = filepath.Dir(current) {
		if _, err := pinnedInfo(current, true); err != nil {
			return "", err
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !sameSwitchPath(canonical, path) {
		return "", ErrSwitchUnsafe
	}
	return canonical, nil
}

func sameSwitchPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func disjointPaths(paths []string) bool {
	for i, a := range paths {
		for _, b := range paths[i+1:] {
			if switchWithin(a, b) || switchWithin(b, a) {
				return false
			}
		}
	}
	return true
}

func switchWithin(parent, child string) bool {
	if runtime.GOOS == "windows" {
		parent, child = strings.ToLower(parent), strings.ToLower(child)
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func validateTargets(targets Targets, allowMissing bool) ([]string, error) {
	roots := []string{targets.Data, targets.Mail, targets.DKIM}
	for i, path := range roots {
		if path == "" || filepath.Clean(path) != path || !filepath.IsAbs(path) || filepath.Dir(path) == path {
			return nil, ErrSwitchUnsafe
		}
		parent, err := switchRealDirectory(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		if runtime.GOOS == "linux" {
			identity, err := pinnedInfo(parent, true)
			if err != nil || !switchControlledParent(identity) {
				return nil, ErrSwitchUnsafe
			}
		}
		canonical := filepath.Join(parent, filepath.Base(path))
		if !sameSwitchPath(path, canonical) {
			return nil, ErrSwitchUnsafe
		}
		_, err = pinnedInfo(canonical, true)
		if err != nil && !(allowMissing && errors.Is(err, fs.ErrNotExist)) {
			return nil, err
		}
		if runtime.GOOS == "linux" {
			mount, err := switchMountPoint(canonical)
			if err != nil || mount {
				return nil, ErrSwitchUnsafe
			}
		}
		roots[i] = canonical
	}
	if !disjointPaths(roots) {
		return nil, ErrSwitchUnsafe
	}
	return roots, nil
}

func switchMountPoint(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		mount := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if mount == path {
			return true, nil
		}
	}
	return false, nil
}

func createSwitchMarker(root, txn, role, token string, ops switchOps) error {
	if _, err := switchRealDirectory(root); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, ".lanqin-restore-"+txn), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, role+"\n"+token+"\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return ops.syncDir(root)
}

func createSwitchSentinel(root, txn string, ops switchOps) error {
	f, err := os.OpenFile(filepath.Join(root, ".lanqin-restore-incomplete"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, txn+"\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return ops.syncDir(root)
}

func ownSwitchMetadata(path string, owner Owner, ops switchOps) error {
	if err := ops.owner(path, owner); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

func removeSwitchSentinel(root, txn string, ops switchOps) error {
	return removeSwitchGuard(root, ".lanqin-restore-incomplete", txn+"\n", ops)
}

func validateSwitchActivationFlag(root string, required bool) (bool, error) {
	if _, err := switchRealDirectory(root); err != nil {
		return false, err
	}
	path := filepath.Join(root, switchActivationName)
	identity, err := pinnedInfo(path, false)
	if err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 128))
	current, statErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil || statErr != nil || closeErr != nil || string(data) != switchActivationValue || !os.SameFile(identity, current) {
		return false, ErrSwitchUnsafe
	}
	return true, nil
}

func removeSwitchGuard(root, name, value string, ops switchOps) error {
	path := filepath.Join(root, name)
	identity, err := pinnedInfo(path, false)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 128))
	current, statErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil || statErr != nil || closeErr != nil || string(data) != value || !os.SameFile(identity, current) {
		return ErrSwitchUnsafe
	}
	current, err = pinnedInfo(path, false)
	if err != nil || !os.SameFile(identity, current) {
		return ErrSwitchUnsafe
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return ops.syncDir(root)
}

func switchMarkerState(root, txn string, record switchRoot) (string, error) {
	if _, err := pinnedInfo(root, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent", nil
		}
		return "", err
	}
	marker := filepath.Join(root, ".lanqin-restore-"+txn)
	identity, err := pinnedInfo(marker, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "unmarked", nil
		}
		return "", err
	}
	f, err := os.Open(marker)
	if err != nil {
		return "", err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(identity, current) {
		return "", ErrSwitchUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", err
	}
	if string(data) == "old\n"+record.OldToken+"\n" {
		return "old", nil
	}
	if string(data) == "new\n"+record.NewToken+"\n" {
		return "new", nil
	}
	return "", ErrSwitchUnsafe
}

func createSwitchJournal(path string, j switchJournal, ops switchOps) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(f).Encode(j)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return ops.syncDir(filepath.Dir(path))
}

func readSwitchJournal(path string) (switchJournal, string, error) {
	var j switchJournal
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return j, "", ErrSwitchUnsafe
	}
	parent, err := switchRealDirectory(filepath.Dir(path))
	if err != nil {
		return j, "", err
	}
	if runtime.GOOS == "linux" {
		identity, err := pinnedInfo(parent, true)
		if err != nil || !materializePrivateInfo(identity) {
			return j, "", ErrSwitchUnsafe
		}
	}
	path = filepath.Join(parent, filepath.Base(path))
	identity, err := pinnedInfo(path, false)
	if err != nil {
		return j, "", err
	}
	if runtime.GOOS == "linux" && !materializePrivateInfo(identity) {
		return j, "", ErrSwitchUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return j, "", err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(identity, current) {
		return j, "", ErrSwitchUnsafe
	}
	if current.Size() < 1 || current.Size() > 1<<20 {
		return j, "", ErrSwitchUnsafe
	}
	scanner := bufio.NewScanner(io.LimitReader(f, 1<<20))
	// A crash can tear only the final append, before its fsync/rename. Ignore
	// that unterminated record; filesystem markers remain authoritative.
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	if !scanner.Scan() {
		return j, "", ErrSwitchUnsafe
	}
	decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&j); err != nil || j.Version != 1 || !switchIDPattern.MatchString(j.Txn) || filepath.Base(path) != "restore-"+j.Txn+".journal" {
		return j, "", ErrSwitchUnsafe
	}
	if decoder.Decode(new(any)) != io.EOF {
		return j, "", ErrSwitchUnsafe
	}
	for scanner.Scan() {
		var event string
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || !validSwitchEvent(event) {
			return j, "", ErrSwitchUnsafe
		}
		j.Events = append(j.Events, event)
	}
	if scanner.Err() != nil {
		return j, "", ErrSwitchUnsafe
	}
	return j, path, nil
}

func validSwitchEvent(event string) bool {
	if event == "committed" || event == "rolled-back" {
		return true
	}
	for _, prefix := range []string{"mark-old:", "save-old:", "publish-new:", "quarantine-new:", "restore-old:"} {
		for _, name := range []string{"data", "mail", "dkim"} {
			if event == prefix+name {
				return true
			}
		}
	}
	return false
}

func switchRootWasMoved(j switchJournal, name string) bool {
	for _, event := range j.Events {
		if event == "save-old:"+name || event == "publish-new:"+name || event == "restore-old:"+name || event == "committed" {
			return true
		}
	}
	return false
}

func appendSwitchEvent(path, event string) error {
	identity, err := pinnedInfo(path, false)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	if err != nil || !os.SameFile(identity, current) {
		f.Close()
		return ErrSwitchUnsafe
	}
	if current.Size() < 1 || current.Size() > 1<<20 {
		f.Close()
		return ErrSwitchUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil || len(data) == 0 {
		f.Close()
		return ErrSwitchUnsafe
	}
	if data[len(data)-1] != '\n' {
		end := bytes.LastIndexByte(data, '\n') + 1
		if end == 0 {
			f.Close()
			return ErrSwitchUnsafe
		}
		if err := f.Truncate(int64(end)); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	writeErr := json.NewEncoder(f).Encode(event)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func switchRename(ctx context.Context, journal, event, from, to string, j switchJournal, root switchRoot, role string, ops switchOps) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := appendSwitchEvent(journal, event); err != nil {
		return err
	}
	if ops.beforeStep != nil {
		if err := ops.beforeStep(event); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := switchRealDirectory(filepath.Dir(from)); err != nil {
		return err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return ErrSwitchUnsafe
	}
	identity, err := pinnedInfo(from, true)
	if err != nil {
		return err
	}
	if state, err := switchMarkerState(from, j.Txn, root); err != nil || state != role {
		return ErrSwitchUnsafe
	}
	if err := requireAbsent(to); err != nil {
		return err
	}
	if err := ops.rename(from, to); err != nil {
		return err
	}
	current, err := pinnedInfo(to, true)
	if err != nil || !os.SameFile(identity, current) {
		return ErrSwitchUnsafe
	}
	return ops.syncDir(filepath.Dir(to))
}
