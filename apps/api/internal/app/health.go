package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const healthCheckTimeout = 3 * time.Second

type appHealthState struct {
	schemaInitialized atomic.Bool
	workersExpected   atomic.Int64
	workersRunning    atomic.Int64
}

type healthCheckResponse struct {
	OK     bool              `json:"ok"`
	Time   time.Time         `json:"time"`
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func (a *App) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, healthCheckResponse{
		OK:     true,
		Time:   a.now().UTC(),
		Status: "alive",
	})
}

func (a *App) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	checks := map[string]string{
		"database": "ready",
		"schema":   "ready",
		"storage":  "ready",
		"workers":  "ready",
	}
	ready := true
	if err := a.checkDatabaseReady(ctx); err != nil {
		checks["database"] = "unavailable"
		ready = false
	}
	if err := a.checkSchemaReady(ctx); err != nil {
		checks["schema"] = "unavailable"
		ready = false
	}
	if err := a.checkStorageReady(); err != nil {
		checks["storage"] = "unavailable"
		ready = false
	}
	if !a.workersReady() {
		checks["workers"] = "unavailable"
		ready = false
	}

	statusCode := http.StatusOK
	status := "ready"
	if !ready {
		statusCode = http.StatusServiceUnavailable
		status = "not_ready"
	}
	respondJSON(w, statusCode, healthCheckResponse{
		OK:     ready,
		Time:   a.now().UTC(),
		Status: status,
		Checks: checks,
	})
}

func (a *App) checkDatabaseReady(ctx context.Context) error {
	if a == nil || a.db == nil {
		return context.Canceled
	}
	return a.db.PingContext(ctx)
}

func (a *App) checkSchemaReady(ctx context.Context) error {
	if a == nil || a.db == nil || a.health == nil || !a.health.schemaInitialized.Load() {
		return context.Canceled
	}
	for _, table := range []string{"users", "messages", "send_queue", "system_settings", "telegram_notification_outbox"} {
		rows, err := a.db.QueryContext(ctx, "SELECT 1 FROM "+table+" WHERE 1=0")
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if a.cfg.DBDriver == databaseDriverSQLite {
		return nil
	}
	var count, version int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return err
	}
	if count != externalSchemaVersion || version != externalSchemaVersion {
		return context.Canceled
	}
	return nil
}

func (a *App) checkStorageReady() error {
	attachmentsDir := filepath.Join(a.cfg.DataDir, "attachments")
	if err := checkDirectoryWritable(attachmentsDir); err != nil {
		return err
	}
	maildirRoot := strings.TrimSpace(a.cfg.MaildirRoot)
	if maildirRoot == "" {
		return nil
	}
	dir, err := os.Open(maildirRoot)
	if err != nil {
		return err
	}
	defer dir.Close()
	_, err = dir.Readdirnames(1)
	if err == io.EOF {
		return nil
	}
	return err
}

func checkDirectoryWritable(dir string) error {
	file, err := os.CreateTemp(dir, ".lanqin-health-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func (a *App) workersReady() bool {
	if a == nil || a.health == nil {
		return false
	}
	expected := a.health.workersExpected.Load()
	return expected > 0 && a.health.workersRunning.Load() == expected
}
