package maintenancehost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type record struct {
	Request          Request `json:"request"`
	Status           Status  `json:"status"`
	InterruptedPhase string  `json:"interruptedPhase,omitempty"`
}
type peerKey struct{}
type service struct {
	cfg      Config
	execute  Executor
	mu       sync.Mutex
	records  map[string]record
	active   bool
	poisoned bool
	wake     chan Request
	handlers sync.WaitGroup
	closing  bool
}

// Serve is Linux/root-only. The socket directory is deployment-owned; access
// to its world-connectable socket is restricted by kernel peer credentials.
// Root config and state are never writable by the API UID.
func Serve(ctx context.Context, cfg Config, executor Executor) error {
	if !rootHost() || executor == nil {
		return ErrUnavailable
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return ErrUnavailable
	}
	cfg, err = DecodeConfig(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "host.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return ErrUnavailable
	}
	defer lock.Close()
	if !trustedPath(lock.Name(), false, true) || lockFile(lock) != nil {
		return ErrConflict
	}
	defer unlockFile(lock)
	s := &service{cfg: cfg, execute: executor, records: map[string]record{}, wake: make(chan Request, 1)}
	if s.load() != nil {
		return ErrUnavailable
	}
	if info, err := os.Lstat(cfg.Socket); err == nil {
		// Only a stale socket owned by root may be removed after the daemon lock.
		if info.Mode()&os.ModeSocket == 0 || !socketOwned(info) {
			return ErrUnavailable
		}
		if conn, e := net.DialTimeout("unix", cfg.Socket, time.Second); e == nil {
			conn.Close()
			return ErrConflict
		}
		if os.Remove(cfg.Socket) != nil {
			return ErrUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	listener, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return ErrUnavailable
	}
	defer listener.Close()
	if os.Chmod(cfg.Socket, 0o666) != nil {
		return ErrUnavailable
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		uid, _ := peerUID(c)
		return context.WithValue(ctx, peerKey{}, uid)
	}}
	workerContext, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); s.worker(workerContext) }()
	go func() { <-workerContext.Done(); _ = server.Close() }()
	err = server.Serve(listener)
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	_ = server.Close()
	s.handlers.Wait()
	stopWorker()
	// Keep the host lock until all copy/import/service callbacks have returned.
	// A restarted daemon must not race an old executor's final durable status.
	<-workerDone
	if err == http.ErrServerClosed || ctx.Err() != nil {
		return nil
	}
	return ErrUnavailable
}

func (s *service) load() error {
	dir, err := os.Open(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(258)
	if err != nil && err != io.EOF {
		return ErrUnavailable
	}
	if len(entries) > 257 {
		return ErrUnavailable
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !hexID.MatchString(id) {
			return ErrUnavailable
		}
		data, err := readTrusted(filepath.Join(s.cfg.StateDir, entry.Name()), 64<<10)
		if err != nil {
			return err
		}
		var r record
		if decode(strings.NewReader(string(data)), &r, 64<<10) != nil || !validRecord(r) || r.Request.ID != id {
			return ErrUnavailable
		}
		s.records[id] = r
		if r.Status.State == "queued" || r.Status.State == "preparing" || r.Status.State == "maintaining" {
			r.InterruptedPhase = r.Status.Phase
			r.Status.State, r.Status.Phase, r.Status.ErrorCode = "needsRecovery", "interrupted", "interrupted"
			r.Status.UpdatedAt = time.Now().UTC()
			if s.persist(r) != nil {
				return ErrUnavailable
			}
			s.records[id] = r
		}
		if r.Status.State == "needsRecovery" {
			s.active = true
		}
	}
	return nil
}

func (s *service) persist(r record) error {
	if !validRecord(r) {
		return ErrUnavailable
	}
	data, err := json.Marshal(r)
	if err != nil {
		return ErrUnavailable
	}
	file, err := os.CreateTemp(s.cfg.StateDir, ".host-record-")
	if err != nil {
		return ErrUnavailable
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if _, err = file.Write(append(data, '\n')); err != nil || file.Sync() != nil || file.Close() != nil {
		return ErrUnavailable
	}
	// This is a helper-owned status record, not an arbitrary publication target.
	if os.Rename(name, filepath.Join(s.cfg.StateDir, r.Request.ID+".json")) != nil || syncDir(s.cfg.StateDir) != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		http.Error(w, "unavailable", 503)
		return
	}
	s.handlers.Add(1)
	s.mu.Unlock()
	defer s.handlers.Done()
	w.Header().Set("Cache-Control", "no-store")
	uid, ok := r.Context().Value(peerKey{}).(int)
	if !ok || uid != s.cfg.APIUID {
		http.Error(w, "unavailable", http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "unavailable", 400)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/plans" {
		items := []Plan{}
		for _, p := range s.cfg.Plans {
			cfg, err := LoadTemplate(p)
			if err != nil {
				continue
			}
			items = append(items, Plan{p.ID, p.Template.SHA256, cfg.Driver, cfg.Preservation != nil && cfg.Preservation.QuarantineQueues})
		}
		hostJSON(w, 200, struct {
			Items []Plan `json:"items"`
		}{items})
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/requests/") {
		id := strings.TrimPrefix(r.URL.Path, "/v1/requests/")
		s.mu.Lock()
		record, ok := s.records[id]
		s.mu.Unlock()
		if !hexID.MatchString(id) || !ok {
			http.Error(w, "not found", 404)
			return
		}
		hostJSON(w, 200, record.Status)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/requests" {
		http.Error(w, "not found", 404)
		return
	}
	var req Request
	if decode(r.Body, &req, 8192) != nil || !ValidRequest(req) {
		http.Error(w, "unavailable", 400)
		return
	}
	var plan *ApprovedPlan
	for _, candidate := range s.cfg.Plans {
		if candidate.ID == req.PlanID && candidate.Template.SHA256 == req.TemplateSHA256 {
			v := candidate
			plan = &v
			break
		}
	}
	if plan == nil {
		http.Error(w, "conflict", 409)
		return
	}
	if _, err := LoadTemplate(*plan); err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[req.ID]; ok {
		if existing.Request != req {
			http.Error(w, "conflict", 409)
		} else {
			hostJSON(w, 202, existing.Status)
		}
		return
	}
	if s.active || s.poisoned || len(s.records) >= 128 {
		http.Error(w, "conflict", 409)
		return
	}
	// A different nonce cannot replay the same verified recovery with this plan.
	for _, existing := range s.records {
		if existing.Request.PlanID == req.PlanID && existing.Request.WebJob == req.WebJob {
			http.Error(w, "conflict", 409)
			return
		}
	}
	now := time.Now().UTC()
	rec := record{Request: req, Status: Status{Version: 1, ID: req.ID, PlanID: req.PlanID, WebJob: req.WebJob, State: "queued", Phase: "queued", CreatedAt: now, UpdatedAt: now}}
	if s.persist(rec) != nil {
		s.poisoned = true
		http.Error(w, "unavailable", 503)
		return
	}
	s.records[req.ID] = rec
	s.active = true
	s.wake <- req
	hostJSON(w, 202, rec.Status)
}

func hostJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *service) update(id, state, phase, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok || s.poisoned {
		return ErrUnavailable
	}
	rec.Status.State, rec.Status.Phase, rec.Status.ErrorCode = state, phase, code
	rec.Status.UpdatedAt = time.Now().UTC()
	step := phase
	switch phase {
	case "preparing":
		step = "checking-source"
	case "maintenance":
		step = "preflight"
	}
	if phase == "completed" && state != "succeeded" && rec.Status.Progress != nil {
		step = rec.Status.Progress.Step
	}
	if rec.Status.Progress == nil || rec.Status.Progress.Step != step {
		rec.Status.Progress = &Progress{Step: step, StartedAt: rec.Status.UpdatedAt}
	}
	if s.persist(rec) != nil {
		s.poisoned = true
		return ErrUnavailable
	}
	s.records[id] = rec
	return nil
}

func (s *service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.wake:
			var plan ApprovedPlan
			for _, p := range s.cfg.Plans {
				if p.ID == req.PlanID {
					plan = p
					break
				}
			}
			if s.update(req.ID, "preparing", "preparing", "") != nil {
				continue
			}
			phase := func(value string) error {
				switch value {
				case "preparing", "importing":
					return s.update(req.ID, "preparing", value, "")
				case "maintenance":
					return s.update(req.ID, "maintaining", value, "")
				case "checking-source", "copying", "checking-copy":
					return s.progress(req.ID, "preparing", "preparing", value)
				case "preflight", "stopping", "installing", "validating", "committing", "starting", "health-check", "rolling-back", "recovering-original":
					return s.progress(req.ID, "maintaining", "maintenance", value)
				default:
					return ErrUnavailable
				}
			}
			outcome, err := s.execute(ctx, req, plan, phase)
			if outcome.State != "succeeded" && outcome.State != "failed" && outcome.State != "needsRecovery" {
				outcome = Outcome{"needsRecovery", "interrupted"}
			}
			if err != nil && outcome.State == "succeeded" {
				outcome = Outcome{"needsRecovery", "interrupted"}
			}
			if outcome.ErrorCode != "" && outcome.ErrorCode != "source_unavailable" && outcome.ErrorCode != "preparation_failed" && outcome.ErrorCode != "import_failed" && outcome.ErrorCode != "maintenance_failed" && outcome.ErrorCode != "interrupted" {
				outcome.ErrorCode = "unavailable"
			}
			if s.update(req.ID, outcome.State, "completed", outcome.ErrorCode) != nil {
				continue
			}
			s.mu.Lock()
			s.active = outcome.State == "needsRecovery"
			s.mu.Unlock()
		}
	}
}

func (s *service) progress(id, state, phase, step string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok || s.poisoned {
		return ErrUnavailable
	}
	rec.Status.State, rec.Status.Phase = state, phase
	rec.Status.UpdatedAt = time.Now().UTC()
	if rec.Status.Progress == nil || rec.Status.Progress.Step != step {
		rec.Status.Progress = &Progress{Step: step, StartedAt: rec.Status.UpdatedAt}
	}
	// Stage observations remain in memory. Only the existing coarse state
	// transitions persist records; progress cannot add transaction IO or alter
	// the outcome of a service-control or data-switch operation.
	s.records[id] = rec
	return nil
}

// RequestFingerprint is useful for audit receipts without recording credentials.
func RequestFingerprint(r Request) string {
	data, _ := json.Marshal(r)
	v := sha256.Sum256(data)
	return hex.EncodeToString(v[:])
}
