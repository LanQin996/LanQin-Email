package maintenancehost

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostProgressObservesWithoutExtraDurableWrites(t *testing.T) {
	cfg := hostFixture(t)
	s := protocolService(cfg)
	r := hostRequest(cfg, 1)
	uid := cfg.APIUID
	if hostCall(s, "POST", "/v1/requests", encodedRequest(t, r), &uid).Code != 202 || s.update(r.ID, "preparing", "preparing", "") != nil {
		t.Fatal("request setup failed")
	}
	name := filepath.Join(cfg.StateDir, r.ID+".json")
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.progress(r.ID, "preparing", "preparing", "copying"); err != nil {
		t.Fatal(err)
	}
	first := s.records[r.ID].Status
	if first.Progress == nil || first.Progress.Step != "copying" || !ValidProgress(first) {
		t.Fatal("safe observed stage missing")
	}
	if err := s.progress(r.ID, "preparing", "preparing", "copying"); err != nil || s.records[r.ID].Status.Progress.StartedAt != first.Progress.StartedAt {
		t.Fatal("same stage reset its elapsed-time baseline")
	}
	after, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("observations changed the durable request journal")
	}
	if err := s.update(r.ID, "failed", "completed", "preparation_failed"); err != nil {
		t.Fatal(err)
	}
	if s.records[r.ID].Status.Progress.Step != "copying" {
		t.Fatal("failure discarded the last concrete stage")
	}
}

func TestHostProgressRejectsUnknownStagesAndImpossibleTimeRanges(t *testing.T) {
	now := time.Now().UTC()
	base := Status{CreatedAt: now, UpdatedAt: now.Add(time.Minute)}
	for _, tc := range []struct {
		name     string
		progress *Progress
		valid    bool
	}{
		{"legacy", nil, true},
		{"safe", &Progress{Step: "installing", StartedAt: now}, true},
		{"private", &Progress{Step: "/private/synthetic-secret", StartedAt: now}, false},
		{"missing-time", &Progress{Step: "copying"}, false},
		{"before-request", &Progress{Step: "copying", StartedAt: now.Add(-time.Second)}, false},
		{"future", &Progress{Step: "copying", StartedAt: now.Add(time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := base
			status.Progress = tc.progress
			if ValidProgress(status) != tc.valid {
				t.Fatal("incorrect finite progress validation")
			}
		})
	}
}
