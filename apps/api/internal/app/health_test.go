package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthEndpointsReadyAndCompatible(t *testing.T) {
	a := newTestApp(t)
	router := a.Router()

	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, res.Code, res.Body.String())
		}
		var payload healthCheckResponse
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		if !payload.OK {
			t.Fatalf("%s unexpectedly unhealthy: %+v", path, payload)
		}
		if path != "/livez" {
			for name, status := range payload.Checks {
				if status != "ready" {
					t.Fatalf("%s check %s=%s", path, name, status)
				}
			}
		}
	}
}

func TestReadinessReportsSchemaAndWorkerFailures(t *testing.T) {
	a := newTestApp(t)
	router := a.Router()

	a.health.schemaInitialized.Store(false)
	assertHealthCheckUnavailable(t, router, "/readyz", "schema")
	a.health.schemaInitialized.Store(true)
	if _, err := a.db.Exec(`DROP TABLE telegram_notification_outbox`); err != nil {
		t.Fatal(err)
	}
	assertHealthCheckUnavailable(t, router, "/readyz", "schema")

	stopTestWorkers(a)
	assertHealthCheckUnavailable(t, router, "/readyz", "workers")

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("liveness must not depend on workers: status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestReadinessReportsDatabaseFailureWithoutDetails(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	a.Router().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	var payload healthCheckResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Checks["database"] != "unavailable" {
		t.Fatalf("unexpected database check: %+v", payload.Checks)
	}
	if strings.Contains(strings.ToLower(res.Body.String()), "database is closed") {
		t.Fatalf("readiness response leaked database error: %s", res.Body.String())
	}
}

func TestReadinessReportsStorageFailureWithoutLeakingPath(t *testing.T) {
	a := newTestApp(t)
	secretPath := filepath.Join(t.TempDir(), "sensitive-storage-name", "missing")
	a.cfg.DataDir = secretPath

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	res := httptest.NewRecorder()
	a.Router().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), secretPath) || strings.Contains(res.Body.String(), "sensitive-storage-name") {
		t.Fatalf("readiness response leaked storage path: %s", res.Body.String())
	}
	var payload healthCheckResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Checks["storage"] != "unavailable" {
		t.Fatalf("unexpected storage check: %+v", payload.Checks)
	}
}

func assertHealthCheckUnavailable(t *testing.T, handler http.Handler, path, check string) {
	t.Helper()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s status=%d body=%s", path, res.Code, res.Body.String())
	}
	var payload healthCheckResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Status != "not_ready" || payload.Checks[check] != "unavailable" {
		t.Fatalf("unexpected health response: %+v", payload)
	}
}
