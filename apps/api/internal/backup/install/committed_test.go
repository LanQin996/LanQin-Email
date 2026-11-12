package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedJournalAndNativePlanRemainBoundAcrossRollback(t *testing.T) {
	f := newSwitchFixture(t)
	plan := []byte(`{"version":1,"operation":"synthetic-operation"}`)
	if err := os.WriteFile(filepath.Join(f.source, "data", "restore-install-plan.json"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(plan)
	expected := hex.EncodeToString(digest[:])
	info, err := f.apply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCommitted(t.Context(), info.Path, f.targets); err != nil {
		t.Fatal("completed switch not verifiable", err)
	}
	if _, err := VerifyPlanTransaction(t.Context(), info.Path, f.targets, expected); err != nil {
		t.Fatal("new plan not bound to journal", err)
	}
	if _, err := VerifyPlanTransaction(t.Context(), info.Path, f.targets, hex.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("wrong plan accepted")
	}
	if err := rollbackWithOps(context.Background(), info.Path, f.targets, true, f.ops); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCommitted(t.Context(), info.Path, f.targets); err == nil {
		t.Fatal("rolled-back directories advertised as committed")
	}
	if _, err := VerifyPlanTransaction(t.Context(), info.Path, f.targets, expected); err != nil {
		t.Fatal("quarantined new plan not available for repeat rollback", err)
	}
	failed := filepath.Join(filepath.Dir(f.targets.Data), filepath.Base(f.targets.Data)+".failed-"+info.TransactionID, "restore-install-plan.json")
	if err := os.WriteFile(failed, []byte("modified plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPlanTransaction(t.Context(), info.Path, f.targets, expected); err == nil {
		t.Fatal("modified quarantined plan accepted")
	}
}
