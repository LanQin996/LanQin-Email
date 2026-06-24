package app

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxMaildirRecentErrors = 10

type maildirSyncCounts struct {
	FilesScanned     int      `json:"filesScanned"`
	Imported         int      `json:"imported"`
	Backfilled       int      `json:"backfilled"`
	Cleaned          int      `json:"cleaned"`
	FileErrors       int      `json:"fileErrors"`
	fileErrorDetails []string `json:"-"`
}

func (c maildirSyncCounts) total() int {
	return c.Imported + c.Backfilled + c.Cleaned
}

type maildirSyncRun struct {
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
	DurationMs int64             `json:"durationMs"`
	Status     string            `json:"status"`
	Error      string            `json:"error,omitempty"`
	Counts     maildirSyncCounts `json:"counts"`
}

type maildirSyncHealthResponse struct {
	Configured    bool              `json:"configured"`
	Enabled       bool              `json:"enabled"`
	Root          string            `json:"root"`
	ScanSeconds   int               `json:"scanSeconds"`
	WorkerStarted bool              `json:"workerStarted"`
	Running       bool              `json:"running"`
	LastRun       *maildirSyncRun   `json:"lastRun,omitempty"`
	LastError     string            `json:"lastError,omitempty"`
	NextRunAt     *time.Time        `json:"nextRunAt,omitempty"`
	RecentErrors  []string          `json:"recentErrors"`
	Summary       maildirSyncCounts `json:"summary"`
}

type maildirSyncHealthTracker struct {
	mu            sync.Mutex
	workerStarted bool
	running       bool
	current       *maildirSyncRun
	lastRun       *maildirSyncRun
	lastError     string
	nextRunAt     *time.Time
	recentErrors  []string
	summary       maildirSyncCounts
}

func newMaildirSyncHealthTracker() *maildirSyncHealthTracker {
	return &maildirSyncHealthTracker{}
}

func (h *maildirSyncHealthTracker) markWorkerStarted(nextRunAt *time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workerStarted = true
	h.nextRunAt = cloneTimePtr(nextRunAt)
}

func (h *maildirSyncHealthTracker) markWorkerStopped() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workerStarted = false
	h.nextRunAt = nil
}

func (h *maildirSyncHealthTracker) markRunStarted(startedAt time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	run := &maildirSyncRun{StartedAt: startedAt.UTC(), Status: "running"}
	h.running = true
	h.current = run
	h.lastRun = cloneMaildirSyncRun(run)
}

func (h *maildirSyncHealthTracker) markRunFinished(finishedAt time.Time, counts maildirSyncCounts, err error, nextRunAt *time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	run := h.current
	if run == nil {
		run = &maildirSyncRun{StartedAt: finishedAt.UTC()}
	}
	finished := finishedAt.UTC()
	run.FinishedAt = &finished
	run.DurationMs = finished.Sub(run.StartedAt).Milliseconds()
	run.Counts = counts
	run.Status = "success"
	run.Error = ""
	if err != nil {
		run.Status = "error"
		run.Error = err.Error()
		h.lastError = run.Error
		h.pushRecentError(run.Error)
	} else if counts.FileErrors > 0 {
		run.Status = "partial"
		if len(counts.fileErrorDetails) > 0 {
			run.Error = counts.fileErrorDetails[0]
			h.lastError = run.Error
		}
		for _, detail := range counts.fileErrorDetails {
			h.pushRecentError(detail)
		}
	} else {
		h.lastError = ""
	}
	h.summary.FilesScanned += counts.FilesScanned
	h.summary.Imported += counts.Imported
	h.summary.Backfilled += counts.Backfilled
	h.summary.Cleaned += counts.Cleaned
	h.summary.FileErrors += counts.FileErrors
	h.running = false
	h.current = nil
	h.lastRun = cloneMaildirSyncRun(run)
	h.nextRunAt = cloneTimePtr(nextRunAt)
}

func (h *maildirSyncHealthTracker) snapshot(cfg Config) maildirSyncHealthResponse {
	root := strings.TrimSpace(cfg.MaildirRoot)
	scanSeconds := cfg.MaildirScanSeconds
	if scanSeconds <= 0 {
		scanSeconds = 30
	}
	out := maildirSyncHealthResponse{
		Configured:  root != "",
		Enabled:     root != "",
		Root:        root,
		ScanSeconds: scanSeconds,
	}
	if h == nil {
		return out
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out.WorkerStarted = h.workerStarted
	out.Running = h.running
	out.LastRun = cloneMaildirSyncRun(h.lastRun)
	out.LastError = h.lastError
	out.NextRunAt = cloneTimePtr(h.nextRunAt)
	out.RecentErrors = append([]string(nil), h.recentErrors...)
	out.Summary = h.summary
	return out
}

func (h *maildirSyncHealthTracker) pushRecentError(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	h.recentErrors = append([]string{value}, h.recentErrors...)
	if len(h.recentErrors) > maxMaildirRecentErrors {
		h.recentErrors = h.recentErrors[:maxMaildirRecentErrors]
	}
}

func cloneMaildirSyncRun(in *maildirSyncRun) *maildirSyncRun {
	if in == nil {
		return nil
	}
	out := *in
	out.FinishedAt = cloneTimePtr(in.FinishedAt)
	return &out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := in.UTC()
	return &out
}

func (a *App) handleMaildirSyncHealth(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, a.maildirHealth.snapshot(a.cfg))
}
