package maintenancehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/maintenance"
)

func hostFixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS == "linux" {
		// Unix sockaddr paths are bounded independently of Go's descriptive test names.
		var err error
		root, err = os.MkdirTemp("", "lqh-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mkdir := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pin := func(name, content string) maintenance.PathDigest {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(content))
		return maintenance.PathDigest{Path: p, SHA256: hex.EncodeToString(h[:])}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	template := maintenance.Config{
		Version: 1, Mode: "all-in-one", Driver: "sqlite", ProjectName: "synthetic-maintenance",
		DockerPath: executable, ProjectDirectory: mkdir("project"),
		ComposeFiles:    []maintenance.PathDigest{pin("compose.yml", "services: {}\n")},
		EnvironmentFile: pin("runtime.env", "SYNTHETIC=1\n"),
		StateDir:        mkdir("maintenance-state"), Workspace: mkdir("workspace"),
		Targets:         install.Targets{Data: mkdir("data"), Mail: mkdir("mail"), DKIM: mkdir("dkim")},
		Owners:          install.Owners{Data: install.Owner{UID: 0, GID: 0}, Mail: install.Owner{UID: 0, GID: 0}, DKIM: install.Owner{UID: 0, GID: 0}},
		RuntimeDataRoot: "/synthetic/data", RuntimeMailRoot: "/synthetic/mail", RuntimeDKIMRoot: "/synthetic/dkim",
		Staged: filepath.Join(root, "future-stage"), ExpectedReportSHA256: strings.Repeat("a", 64),
	}
	data, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	templatePin := pin("maintenance-template.json", string(data))
	cfg := Config{Version: 1, Socket: filepath.Join(mkdir("socket"), "host.sock"), StateDir: mkdir("host-state"), APIUID: 0,
		Plans: []ApprovedPlan{{ID: "approved-plan", Template: templatePin, Source: Source{Driver: "sqlite", DBPath: filepath.Join(root, "source.sqlite"), RestoreRoot: filepath.Join(root, "restore")}}}}
	encoded, _ := json.Marshal(cfg)
	decoded, err := DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("real fixed maintenance template rejected: %v", err)
	}
	return decoded
}

func hostRequest(cfg Config, n int) Request {
	return Request{ID: fmt.Sprintf("%032x", n), PlanID: cfg.Plans[0].ID, TemplateSHA256: cfg.Plans[0].Template.SHA256,
		WebJob: fmt.Sprintf("bkj_%032x", n), ReportSHA256: strings.Repeat("c", 64), Actor: "usr_synthetic-admin"}
}

func protocolService(cfg Config) *service {
	return &service{cfg: cfg, records: map[string]record{}, wake: make(chan Request, 1)}
}

func hostCall(s *service, method, path, body string, uid *int) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if uid != nil {
		r = r.WithContext(context.WithValue(r.Context(), peerKey{}, *uid))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func encodedRequest(t *testing.T, r Request) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHostProtocolRejectsUntrustedPeersPathsAndUnknownFields(t *testing.T) {
	cfg := hostFixture(t)
	s := protocolService(cfg)
	uid, wrong := cfg.APIUID, cfg.APIUID+1
	body := encodedRequest(t, hostRequest(cfg, 1))
	for _, tc := range []struct {
		name, method, path, body string
		uid                      *int
		code                     int
	}{
		{"missing-peer", "GET", "/v1/plans", "", nil, 403},
		{"wrong-peer", "GET", "/v1/plans", "", &wrong, 403},
		{"query", "GET", "/v1/plans?path=/private", "", &uid, 400},
		{"unknown-path", "POST", "/v1/execute", body, &uid, 404},
		{"unknown-field", "POST", "/v1/requests", strings.TrimSuffix(body, "}") + `,"command":"do-not-execute"}`, &uid, 400},
		{"trailing-json", "POST", "/v1/requests", body + "{}", &uid, 400},
		{"oversize", "POST", "/v1/requests", strings.Repeat(" ", 8193) + body, &uid, 400},
		{"plan-path", "POST", "/v1/requests", strings.Replace(body, `"approved-plan"`, `"../private"`, 1), &uid, 400},
		{"id-path", "POST", "/v1/requests", strings.Replace(body, `"id":"00000000000000000000000000000001"`, `"id":"../private"`, 1), &uid, 400},
		{"unknown-id", "GET", "/v1/requests/" + strings.Repeat("d", 32), "", &uid, 404},
		{"status-path", "GET", "/v1/requests/../private", "", &uid, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := hostCall(s, tc.method, tc.path, tc.body, tc.uid)
			if w.Code != tc.code || len(s.records) != 0 || len(s.wake) != 0 {
				t.Fatalf("code=%d want=%d records=%d wake=%d", w.Code, tc.code, len(s.records), len(s.wake))
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("status response can be cached")
			}
		})
	}
}

func TestHostProtocolIdempotenceActiveGuardAndDurableStatus(t *testing.T) {
	cfg := hostFixture(t)
	s := protocolService(cfg)
	uid := cfg.APIUID
	req := hostRequest(cfg, 1)
	body := encodedRequest(t, req)
	first := hostCall(s, "POST", "/v1/requests", body, &uid)
	if first.Code != 202 || len(s.records) != 1 || len(s.wake) != 1 {
		t.Fatalf("initial request: %d records=%d wake=%d", first.Code, len(s.records), len(s.wake))
	}
	for i := 0; i < 2; i++ {
		if w := hostCall(s, "POST", "/v1/requests", body, &uid); w.Code != 202 || w.Body.String() != first.Body.String() || len(s.wake) != 1 {
			t.Fatalf("idempotent duplicate changed outcome or enqueued twice: %d", w.Code)
		}
	}
	changed := req
	changed.ReportSHA256 = strings.Repeat("d", 64)
	if w := hostCall(s, "POST", "/v1/requests", encodedRequest(t, changed), &uid); w.Code != 409 {
		t.Fatalf("same ID changed binding accepted: %d", w.Code)
	}
	if w := hostCall(s, "POST", "/v1/requests", encodedRequest(t, hostRequest(cfg, 2)), &uid); w.Code != 409 {
		t.Fatalf("second live operation accepted: %d", w.Code)
	}
	got := hostCall(s, "GET", "/v1/requests/"+req.ID, "", &uid)
	if got.Code != 200 || got.Body.String() != first.Body.String() {
		t.Fatal("status lookup did not retain the queued request")
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, req.ID+".json"))
	var saved record
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Request != req || saved.Status.State != "queued" {
		t.Fatal("accepted response lacked a durable matching record")
	}
	if !strings.Contains(first.Body.String(), `"state":"queued"`) || strings.Contains(first.Body.String(), cfg.StateDir) {
		t.Fatal("unsafe or incorrect public status")
	}
}

func TestHostProtocolReplayAndRecordCapacity(t *testing.T) {
	cfg := hostFixture(t)
	uid := cfg.APIUID
	for _, tc := range []string{"same-source-new-nonce", "capacity", "poisoned"} {
		t.Run(tc, func(t *testing.T) {
			s := protocolService(cfg)
			r := hostRequest(cfg, 500)
			switch tc {
			case "same-source-new-nonce":
				old := hostRequest(cfg, 1)
				old.WebJob = r.WebJob
				s.records[old.ID] = record{Request: old, Status: Status{State: "succeeded"}}
			case "capacity":
				for i := 1; i <= 128; i++ {
					old := hostRequest(cfg, i)
					s.records[old.ID] = record{Request: old, Status: Status{State: "succeeded"}}
				}
			case "poisoned":
				s.poisoned = true
			}
			if w := hostCall(s, "POST", "/v1/requests", encodedRequest(t, r), &uid); w.Code != 409 || len(s.wake) != 0 {
				t.Fatalf("guard failed: code=%d wake=%d", w.Code, len(s.wake))
			}
		})
	}
}

func TestHostRestartQuarantinesInterruptedRecordsWithoutAutomaticExecution(t *testing.T) {
	for _, state := range []string{"queued", "preparing", "maintaining"} {
		t.Run(state, func(t *testing.T) {
			cfg := hostFixture(t)
			r := hostRequest(cfg, 1)
			before := protocolService(cfg)
			phase := state
			if state == "maintaining" {
				phase = "maintenance"
			}
			rec := record{Request: r, Status: Status{Version: 1, ID: r.ID, PlanID: r.PlanID, WebJob: r.WebJob, State: state, Phase: phase, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}
			if err := before.persist(rec); err != nil {
				t.Fatal(err)
			}
			after := protocolService(cfg)
			if err := after.load(); err != nil {
				t.Fatal(err)
			}
			got := after.records[r.ID].Status
			if got.State != "needsRecovery" || got.Phase != "interrupted" || got.ErrorCode != "interrupted" || !after.active || len(after.wake) != 0 {
				t.Fatalf("restart replayed or trusted an incomplete operation: %+v", got)
			}
			var calls atomic.Int32
			after.execute = func(context.Context, Request, ApprovedPlan, func(string) error) (Outcome, error) {
				calls.Add(1)
				return Outcome{State: "succeeded"}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			after.worker(ctx)
			if calls.Load() != 0 {
				t.Fatal("restart invoked executor automatically")
			}
			reloaded := protocolService(cfg)
			if reloaded.load() != nil || reloaded.records[r.ID].Status.State != "needsRecovery" {
				t.Fatal("recovery-required state was not persisted")
			}
		})
	}
}

func TestHostWorkerPublishesFiniteStatesAndSanitizesErrors(t *testing.T) {
	for _, tc := range []struct{ name, state, code, wantState, wantCode string }{
		{"success", "succeeded", "", "succeeded", ""},
		{"safe-failure", "failed", "source_unavailable", "failed", "source_unavailable"},
		{"private-error", "failed", "/private/path/password", "failed", "unavailable"},
		{"unknown-outcome", "unexpected", "", "needsRecovery", "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hostFixture(t)
			s := protocolService(cfg)
			r := hostRequest(cfg, 1)
			uid := cfg.APIUID
			if hostCall(s, "POST", "/v1/requests", encodedRequest(t, r), &uid).Code != http.StatusAccepted {
				t.Fatal("request was not queued")
			}
			var calls atomic.Int32
			s.execute = func(_ context.Context, got Request, _ ApprovedPlan, phase func(string) error) (Outcome, error) {
				calls.Add(1)
				if got != r {
					return Outcome{}, ErrUnavailable
				}
				for _, p := range []string{"preparing", "importing", "maintenance"} {
					if phase(p) != nil {
						return Outcome{}, ErrUnavailable
					}
				}
				if phase("/private/unapproved") == nil {
					return Outcome{}, ErrUnavailable
				}
				return Outcome{State: tc.state, ErrorCode: tc.code}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { s.worker(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				s.mu.Lock()
				got := s.records[r.ID].Status
				s.mu.Unlock()
				if got.Phase == "completed" {
					if got.State != tc.wantState || got.ErrorCode != tc.wantCode || calls.Load() != 1 {
						t.Fatalf("unsafe outcome: %+v calls=%d", got, calls.Load())
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker did not finish: %+v", got)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestHostConfigRequiresExplicitAPIUIDAndRejectsUnknownFields(t *testing.T) {
	cfg := hostFixture(t)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if json.Unmarshal(data, &object) != nil {
		t.Fatal("invalid fixture")
	}
	for _, tc := range []string{"missing", "null", "negative", "out-of-range", "unknown-field", "socket-path"} {
		t.Run(tc, func(t *testing.T) {
			copy := map[string]any{}
			for key, value := range object {
				copy[key] = value
			}
			switch tc {
			case "missing":
				delete(copy, "apiUid")
			case "null":
				copy["apiUid"] = nil
			case "negative":
				copy["apiUid"] = -1
			case "out-of-range":
				copy["apiUid"] = int64(2147483648)
			case "unknown-field":
				copy["command"] = "/private/do-not-execute"
			case "socket-path":
				copy["socket"] = "../private/socket"
			}
			encoded, _ := json.Marshal(copy)
			if _, err := DecodeConfig(bytes.NewReader(encoded)); err == nil {
				t.Fatalf("unsafe host config accepted: %s", tc)
			}
		})
	}
}

func TestHostColdLoadRejectsInvalidRecordsBeforeAcceptingOperations(t *testing.T) {
	for _, tc := range []string{"plan-binding", "job-binding", "unknown-state", "wrong-phase", "private-error", "success-with-error", "zero-created", "backwards-updated", "unknown-json", "wrong-file-id"} {
		t.Run(tc, func(t *testing.T) {
			cfg := hostFixture(t)
			r := hostRequest(cfg, 1)
			now := time.Now().UTC()
			rec := record{Request: r, Status: Status{Version: 1, ID: r.ID, PlanID: r.PlanID, WebJob: r.WebJob, State: "succeeded", Phase: "completed", CreatedAt: now, UpdatedAt: now}}
			switch tc {
			case "plan-binding":
				rec.Status.PlanID = "foreign-plan"
			case "job-binding":
				rec.Status.WebJob = hostRequest(cfg, 2).WebJob
			case "unknown-state":
				rec.Status.State = "private-unapproved-state"
			case "wrong-phase":
				rec.Status.Phase = "maintenance"
			case "private-error":
				rec.Status.State, rec.Status.ErrorCode = "failed", "/private/error"
			case "success-with-error":
				rec.Status.ErrorCode = "interrupted"
			case "zero-created":
				rec.Status.CreatedAt = time.Time{}
			case "backwards-updated":
				rec.Status.UpdatedAt = now.Add(-time.Second)
			}
			data, _ := json.Marshal(rec)
			if tc == "unknown-json" {
				data = []byte(strings.TrimSuffix(string(data), "}") + `,"command":"do-not-execute"}`)
			}
			name := r.ID
			if tc == "wrong-file-id" {
				name = hostRequest(cfg, 2).ID
			}
			if err := os.WriteFile(filepath.Join(cfg.StateDir, name+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			s := protocolService(cfg)
			if err := s.load(); err == nil || len(s.wake) != 0 {
				t.Fatalf("unsafe cold record trusted: %s err=%v", tc, err)
			}
		})
	}
}
