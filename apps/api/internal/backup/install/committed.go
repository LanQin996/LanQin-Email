package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

func CheckTargets(targets Targets) (Targets, error) {
	roots, err := validateTargets(targets, false)
	if err != nil {
		return Targets{}, err
	}
	return Targets{roots[0], roots[1], roots[2]}, nil
}

// VerifyPlanTransaction binds a database plan to the journal's unique NEW data
// tree, whether live, waiting to publish, or quarantined by rollback. Derived
// siblings only; no journal-provided arbitrary path is treated as authority.
func VerifyPlanTransaction(ctx context.Context, journal string, targets Targets, expectedPlanSHA256 string) (JournalInfo, error) {
	want, err := hex.DecodeString(expectedPlanSHA256)
	if err != nil || len(want) != sha256.Size {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	j, file, err := readSwitchJournal(journal)
	if err != nil {
		return JournalInfo{}, err
	}
	roots, err := validateTargets(targets, true)
	if err != nil || len(j.Roots) != 3 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	for i, root := range j.Roots {
		if root.Name != []string{"data", "mail", "dkim"}[i] || root.Target != roots[i] || !switchIDPattern.MatchString(root.OldToken) || !switchIDPattern.MatchString(root.NewToken) {
			return JournalInfo{}, ErrSwitchUnsafe
		}
	}
	if !disjointPaths(append(append([]string{}, roots...), filepath.Dir(file))) {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	data := j.Roots[0]
	found := 0
	for _, directory := range []string{data.Target, switchSibling(data.Target, "new", j.Txn), switchSibling(data.Target, "failed", j.Txn)} {
		if ctx.Err() != nil {
			return JournalInfo{}, ctx.Err()
		}
		state, err := switchMarkerState(directory, j.Txn, data)
		if err != nil {
			return JournalInfo{}, err
		}
		if state != "new" {
			continue
		}
		found++
		name := filepath.Join(directory, "restore-install-plan.json")
		info, err := pinnedInfo(name, false)
		if err != nil || info.Size() > 32<<10 {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		handle, err := os.Open(name)
		if err != nil {
			return JournalInfo{}, err
		}
		opened, statErr := handle.Stat()
		hash := sha256.New()
		n, copyErr := io.Copy(hash, io.LimitReader(handle, (32<<10)+1))
		closeErr := handle.Close()
		if statErr != nil || copyErr != nil || closeErr != nil || !os.SameFile(info, opened) || n != info.Size() || hex.EncodeToString(hash.Sum(nil)) != expectedPlanSHA256 {
			return JournalInfo{}, ErrSwitchUnsafe
		}
	}
	if found != 1 {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	return JournalInfo{Path: file, TransactionID: j.Txn, RollbackTargets: targets}, nil
}

// VerifyCommitted checks a specific journal against explicit operator-provided
// roots and all original/new identity markers. It never creates, moves or
// deletes a path, and refuses partial or already rolled-back transactions.
func VerifyCommitted(ctx context.Context, journal string, targets Targets) (JournalInfo, error) {
	j, path, err := readSwitchJournal(journal)
	if err != nil {
		return JournalInfo{}, err
	}
	roots, err := validateTargets(targets, false)
	if err != nil {
		return JournalInfo{}, err
	}
	if len(j.Roots) != 3 || len(j.Events) == 0 || j.Events[len(j.Events)-1] != "committed" || !disjointPaths(append(append([]string{}, roots...), filepath.Dir(path))) {
		return JournalInfo{}, ErrSwitchUnsafe
	}
	for i, record := range j.Roots {
		if ctx.Err() != nil {
			return JournalInfo{}, ctx.Err()
		}
		if record.Name != []string{"data", "mail", "dkim"}[i] || record.Target != roots[i] || !switchIDPattern.MatchString(record.OldToken) || !switchIDPattern.MatchString(record.NewToken) {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		if state, err := switchMarkerState(roots[i], j.Txn, record); err != nil || state != "new" {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		if state, err := switchMarkerState(switchSibling(roots[i], "rollback", j.Txn), j.Txn, record); err != nil || state != "old" {
			return JournalInfo{}, ErrSwitchUnsafe
		}
		if err := requireAbsent(switchSibling(roots[i], "failed", j.Txn)); err != nil {
			return JournalInfo{}, err
		}
		if err := requireAbsent(switchSibling(roots[i], "new", j.Txn)); err != nil {
			return JournalInfo{}, err
		}
	}
	for _, name := range []string{switchActivationName, ".lanqin-restore-incomplete", "backup-online.json"} {
		if requireAbsent(filepath.Join(roots[0], name)) != nil {
			return JournalInfo{}, ErrSwitchUnsafe
		}
	}
	return JournalInfo{Path: path, TransactionID: j.Txn, RollbackTargets: targets}, nil
}
