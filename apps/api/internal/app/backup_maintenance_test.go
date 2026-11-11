package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/maintenancehost"
	"lanqin-email-api/internal/backup/storage"
)

type fakeBackupMaintenanceClient struct {
	plans                          []maintenancehost.Plan
	plansErr, submitErr, statusErr error
	submitted                      []maintenancehost.Request
	statuses                       map[string]maintenancehost.Status
	plansCalls, statusCalls        int
	mutateStatus                   func(*maintenancehost.Status)
}

func (client *fakeBackupMaintenanceClient) Plans(ctx context.Context) ([]maintenancehost.Plan, error) {
	client.plansCalls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing bounded host timeout")
	}
	return append([]maintenancehost.Plan{}, client.plans...), client.plansErr
}
func (client *fakeBackupMaintenanceClient) Submit(ctx context.Context, request maintenancehost.Request) (maintenancehost.Status, error) {
	client.submitted = append(client.submitted, request)
	if _, ok := ctx.Deadline(); !ok {
		return maintenancehost.Status{}, errors.New("missing bounded host timeout")
	}
	if previous, ok := client.statuses[request.ID]; ok {
		return previous, client.submitErr
	}
	now := time.Now().UTC()
	status := maintenancehost.Status{Version: 1, ID: request.ID, PlanID: request.PlanID, WebJob: request.WebJob, State: "queued", Phase: "queued", CreatedAt: now, UpdatedAt: now}
	if client.mutateStatus != nil {
		client.mutateStatus(&status)
	}
	client.statuses[request.ID] = status
	return status, client.submitErr
}
func (client *fakeBackupMaintenanceClient) Status(ctx context.Context, id string) (maintenancehost.Status, error) {
	client.statusCalls++
	if _, ok := ctx.Deadline(); !ok {
		return maintenancehost.Status{}, errors.New("missing bounded host timeout")
	}
	if client.statusErr != nil {
		return maintenancehost.Status{}, client.statusErr
	}
	status, ok := client.statuses[id]
	if !ok {
		return maintenancehost.Status{}, maintenancehost.ErrNotFound
	}
	if client.mutateStatus != nil {
		client.mutateStatus(&status)
	}
	return status, nil
}

func maintenanceHTTPFixture(t *testing.T) (*App, *testClient, *fakeBackupMaintenanceClient, backupJob, backupMaintenanceRequest) {
	t.Helper()
	a := newTestApp(t)
	_, restoreReq, ciphertext := restoreJobFixture(t, a)
	job, execution, err := a.admitBackupRestore(t.Context(), "synthetic-web-restore-admin", restoreReq)
	if err != nil || execution == nil {
		t.Fatal("synthetic restore admission", err)
	}
	a.backupDownloadFile = func(_ context.Context, _ storage.Destination, _, sha, out string, _ storage.Options, _ storage.Credentials) (storage.DownloadReceipt, error) {
		if err := os.WriteFile(out, ciphertext, 0o600); err != nil {
			return storage.DownloadReceipt{}, err
		}
		return storage.DownloadReceipt{SHA256: sha, Bytes: int64(len(ciphertext))}, nil
	}
	a.runBackupRestore(t.Context(), *execution)
	job, _, err = a.backupReadJob(t.Context(), job.ID)
	if err != nil || job.Status != "succeeded" || job.Restore == nil || !job.Restore.Ready {
		t.Fatal("actual authenticated restore fixture failed", err)
	}
	a.cfg.BackupMaintenanceSocket = filepath.Join(t.TempDir(), "root-only-maintenance.sock")
	host := &fakeBackupMaintenanceClient{plans: []maintenancehost.Plan{{ID: "approved-synthetic-plan", TemplateSHA256: strings.Repeat("d", 64), Driver: "sqlite", QueueQuarantine: false}}, statuses: map[string]maintenancehost.Status{}}
	a.backupMaintenanceClientFactory = func(socket string) backupMaintenanceClient {
		if socket != a.cfg.BackupMaintenanceSocket {
			t.Fatal("host client accepted unrelated socket")
		}
		return host
	}
	request := backupMaintenanceRequest{SourceJobID: job.ID, PlanID: host.plans[0].ID, TemplateSHA256: host.plans[0].TemplateSHA256, RequestID: "550e8400-e29b-41d4-a716-446655440000", Password: a.cfg.AdminPassword, Confirm: true}
	return a, backupLoggedInAdmin(t, a), host, job, request
}

func TestBackupMaintenanceHTTPReadyTaskToApprovedHostAndStatusAfterSourceLost(t *testing.T) {
	a, client, host, source, req := maintenanceHTTPFixture(t)
	var plans struct {
		Available bool                   `json:"available"`
		Items     []maintenancehost.Plan `json:"items"`
	}
	if status := client.do("GET", "/api/admin/backups/maintenance/plans", nil, &plans); status != 200 || !plans.Available || len(plans.Items) != 1 {
		t.Fatal("approved maintenance plans unavailable", status)
	}
	var response maintenancehost.Status
	if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &response); status != 202 {
		t.Fatal("maintenance host request was not accepted", status)
	}
	var actor string
	if err := a.db.QueryRowContext(t.Context(), "SELECT id FROM users WHERE email=?", a.cfg.AdminEmail).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if len(host.submitted) != 1 {
		t.Fatal("host not submitted exactly once")
	}
	request := host.submitted[0]
	if request.Actor != actor || request.WebJob != source.ID || request.ReportSHA256 != source.Restore.ReportSHA256 || request.ID != backupMaintenanceRequestID(actor, req.RequestID) || request.PlanID != req.PlanID || request.TemplateSHA256 != req.TemplateSHA256 || response.ID != request.ID {
		t.Fatal("host intent was not bound to authenticated task,actor and plan")
	}
	encoded := jsonEncode(request) + jsonEncode(response)
	for _, secret := range []string{req.Password, a.cfg.BackupMaintenanceSocket, a.cfg.BackupSecretKey, source.Restore.Directory, source.Restore.ReportSHA256} {
		if strings.Contains(jsonEncode(response), secret) {
			t.Fatal("safe host status leaked private source metadata")
		}
	}
	if strings.Contains(encoded, req.Password) || strings.Contains(encoded, "password") || strings.Contains(encoded, "identity") {
		t.Fatal("reauth credentials reached host protocol")
	}
	// Root's persisted status is separate from data being replaced. A current
	// authenticated superadmin can inspect the known ID after runtime jobs vanish.
	if _, err := a.db.ExecContext(t.Context(), "DELETE FROM system_settings WHERE "+keyColumnSQL(a.cfg.DBDriver)+"=?", backupJobPrefix+source.ID); err != nil {
		t.Fatal(err)
	}
	var statusResponse maintenancehost.Status
	if status := client.do("GET", "/api/admin/backups/maintenance/requests/"+response.ID, nil, &statusResponse); status != 200 || statusResponse.ID != response.ID {
		t.Fatal("host status incorrectly required old source metadata", status)
	}
	if backupMaintenanceRequestID("usr_synthetic-admin", req.RequestID) != "d2eea1e29533371c9b88b0cf6d4529d2" {
		t.Fatal("browser/root deterministic request-ID contract changed")
	}
}

func TestBackupMaintenanceHTTPUnknownSubmitOutcomeCanBeQueriedWithoutResubmission(t *testing.T) {
	a, client, host, _, req := maintenanceHTTPFixture(t)
	host.submitErr = errors.New("root-state-dir/private-socket/token-must-not-echo")
	var body map[string]any
	if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body); status != 503 || body["error"] != "backup_maintenance_unavailable" {
		t.Fatal("unknown helper result was exposed or misreported", status, body)
	}
	var actor string
	if err := a.db.QueryRowContext(t.Context(), "SELECT id FROM users WHERE email=?", a.cfg.AdminEmail).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	id := backupMaintenanceRequestID(actor, req.RequestID)
	var statusResponse maintenancehost.Status
	if status := client.do("GET", "/api/admin/backups/maintenance/requests/"+id, nil, &statusResponse); status != 200 || statusResponse.ID != id || len(host.submitted) != 1 {
		t.Fatal("unknown response required an unsafe second submit", status)
	}
	if status := client.do("GET", "/api/admin/backups/maintenance/requests/"+strings.Repeat("0", 32), nil, &body); status != 404 || body["error"] != "backup_maintenance_not_found" {
		t.Fatal("missing host request fabricated terminal success/failure", status, body)
	}
}

func TestBackupMaintenanceHTTPSourcePlanAndInputBoundaries(t *testing.T) {
	for _, scenario := range []string{"wrong-password", "unconfirmed", "source-upload", "source-not-ready", "source-canceled", "source-failed", "source-no-report", "source-no-manifest", "offline-scope", "wrong-version", "driver-mismatch", "template-changed", "plan-missing", "unknown-path", "private-key", "arbitrary-command", "bad-nonce", "bad-plan"} {
		t.Run(scenario, func(t *testing.T) {
			a, client, host, source, req := maintenanceHTTPFixture(t)
			payload := map[string]any{}
			if json.Unmarshal([]byte(jsonEncode(req)), &payload) != nil {
				t.Fatal("payload fixture")
			}
			sourceChanged := false
			switch scenario {
			case "wrong-password":
				payload["password"] = "synthetic-wrong-password"
			case "unconfirmed":
				payload["confirm"] = false
			case "source-upload":
				source.Kind = "upload"
				sourceChanged = true
			case "source-not-ready":
				source.Restore.Ready = false
				sourceChanged = true
			case "source-canceled":
				source.CancelRequested = true
				sourceChanged = true
			case "source-failed":
				source.Status = "failed"
				sourceChanged = true
			case "source-no-report":
				source.Restore.ReportSHA256 = ""
				sourceChanged = true
			case "source-no-manifest":
				source.Restore.Manifest = nil
				sourceChanged = true
			case "offline-scope":
				source.Restore.Manifest.Scope = "offline-data"
				sourceChanged = true
			case "wrong-version":
				source.Restore.Manifest.Version = 999
				sourceChanged = true
			case "driver-mismatch":
				host.plans[0].Driver = "postgres"
			case "template-changed":
				host.plans[0].TemplateSHA256 = strings.Repeat("e", 64)
			case "plan-missing":
				host.plans = []maintenancehost.Plan{}
			case "unknown-path":
				payload["workspace"] = "/root/private-do-not-echo"
			case "private-key":
				payload["identity"] = "AGE-SECRET-KEY-do-not-echo"
			case "arbitrary-command":
				payload["command"] = "docker rm -f unrelated-do-not-echo"
			case "bad-nonce":
				payload["requestId"] = "bad nonce do-not-echo"
			case "bad-plan":
				payload["planId"] = "../private-do-not-echo"
			}
			if sourceChanged {
				_, raw, err := a.backupReadJob(t.Context(), source.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := a.backupCAS(t.Context(), a.db, backupJobPrefix+source.ID, raw, source); err != nil {
					t.Fatal(err)
				}
			}
			var body map[string]any
			status := client.do("POST", "/api/admin/backups/maintenance/requests", payload, &body)
			if status == 202 || len(host.submitted) != 0 || strings.Contains(jsonEncode(body), "do-not-echo") || strings.Contains(jsonEncode(body), req.Password) {
				t.Fatal("invalid maintenance intent dispatched or leaked", status, body)
			}
		})
	}
}

func TestBackupMaintenanceSocketConfigAndStatusSanitization(t *testing.T) {
	t.Setenv("LANQIN_BACKUP_MAINTENANCE_SOCKET", "")
	if cfg := LoadConfig(); cfg.BackupMaintenanceSocket != "" {
		t.Fatal("maintenance helper enabled by default")
	}
	socket := filepath.Join(t.TempDir(), "fixed-helper.sock")
	t.Setenv("LANQIN_BACKUP_MAINTENANCE_SOCKET", socket)
	if cfg := LoadConfig(); cfg.BackupMaintenanceSocket != socket {
		t.Fatal("explicit helper socket not loaded")
	}
	a, client, host, _, _ := maintenanceHTTPFixture(t)
	for _, configured := range []string{"", "relative-helper.sock", "https://external.example.invalid/root"} {
		a.cfg.BackupMaintenanceSocket = configured
		before := host.plansCalls
		var body map[string]any
		if status := client.do("GET", "/api/admin/backups/maintenance/plans", nil, &body); status != 200 || body["available"] != false || host.plansCalls != before {
			t.Fatal("missing/nonlocal helper config triggered host access")
		}
	}
	a.cfg.BackupMaintenanceSocket = socket
	id := strings.Repeat("a", 32)
	now := time.Now().UTC()
	host.statuses[id] = maintenancehost.Status{Version: 1, ID: id, PlanID: host.plans[0].ID, WebJob: "bkj_" + strings.Repeat("a", 32), State: "needsRecovery", Phase: "interrupted", ErrorCode: "interrupted", CreatedAt: now, UpdatedAt: now}
	var statusResponse maintenancehost.Status
	if status := client.do("GET", "/api/admin/backups/maintenance/requests/"+id, nil, &statusResponse); status != 200 || statusResponse.State != "needsRecovery" {
		t.Fatal("safe root recovery status unavailable", status)
	}
	host.mutateStatus = func(status *maintenancehost.Status) { status.ErrorCode = "/root/private-environment-do-not-echo" }
	var body map[string]any
	if status := client.do("GET", "/api/admin/backups/maintenance/requests/"+id, nil, &body); status != 503 || body["error"] != "backup_maintenance_unavailable" || strings.Contains(jsonEncode(body), "do-not-echo") {
		t.Fatal("status error text escaped whitelist", status, body)
	}
}

func TestBackupMaintenanceHTTPUnavailableAndUnsafeHelperResponsesAreFailClosed(t *testing.T) {
	for _, scenario := range []string{"not-configured", "unavailable", "unknown-error", "bad-plan-id", "duplicate-plan", "private-template", "bad-status-phase", "bad-status-error", "wrong-status-id", "wrong-status-job", "wrong-status-plan"} {
		t.Run(scenario, func(t *testing.T) {
			a, client, host, _, req := maintenanceHTTPFixture(t)
			switch scenario {
			case "not-configured":
				a.cfg.BackupMaintenanceSocket = ""
			case "unavailable":
				host.plansErr = maintenancehost.ErrUnavailable
			case "unknown-error":
				host.plansErr = errors.New("/root/private-helper-socket-do-not-echo")
			case "bad-plan-id":
				host.plans[0].ID = "/root/private-plan-do-not-echo"
			case "duplicate-plan":
				host.plans = append(host.plans, host.plans[0])
			case "private-template":
				host.plans[0].TemplateSHA256 = "AGE-SECRET-KEY-do-not-echo"
			default:
				host.mutateStatus = func(status *maintenancehost.Status) {
					switch scenario {
					case "bad-status-phase":
						status.Phase = "/root/private-phase-do-not-echo"
					case "bad-status-error":
						status.ErrorCode = "private-token-do-not-echo"
					case "wrong-status-id":
						status.ID = strings.Repeat("0", 32)
					case "wrong-status-job":
						status.WebJob = "bkj_" + strings.Repeat("0", 32)
					case "wrong-status-plan":
						status.PlanID = "another-plan"
					}
				}
			}
			var plans struct {
				Available bool                   `json:"available"`
				Items     []maintenancehost.Plan `json:"items"`
			}
			if status := client.do("GET", "/api/admin/backups/maintenance/plans", nil, &plans); status != 200 {
				t.Fatal("plans unavailable response", status)
			}
			if host.mutateStatus == nil && (plans.Available || len(plans.Items) != 0) {
				t.Fatal("untrusted/unavailable plans were presented as available")
			}
			var body map[string]any
			if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body); status != 503 || body["error"] != "backup_maintenance_unavailable" || strings.Contains(jsonEncode(body), "do-not-echo") {
				t.Fatal("unsafe helper response escaped fail-closed adapter", status, body)
			}
		})
	}
}

func TestBackupMaintenanceHTTPFreshTwoFactorAndRateLimits(t *testing.T) {
	for _, scenario := range []string{"two-factor", "rate-limit"} {
		t.Run(scenario, func(t *testing.T) {
			a, client, host, _, req := maintenanceHTTPFixture(t)
			var body map[string]any
			if scenario == "rate-limit" {
				req.Password = "synthetic-wrong-password"
				for i := 0; i < 6; i++ {
					status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body)
					if i < 4 && status != 403 || i >= 4 && status != 429 {
						t.Fatal("maintenance reauth not bounded", i, status)
					}
				}
				if len(host.submitted) != 0 {
					t.Fatal("failed authentication reached helper")
				}
				return
			}
			secret, err := newTOTPSecret()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.ExecContext(t.Context(), "UPDATE users SET two_factor_secret=?,two_factor_enabled=1 WHERE email=?", secret, a.cfg.AdminEmail); err != nil {
				t.Fatal(err)
			}
			if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body); status != 403 {
				t.Fatal("maintenance reauth bypassed enabled TOTP", status)
			}
			req.TwoFactorCode, err = generateTOTP(secret, a.now())
			if err != nil {
				t.Fatal(err)
			}
			if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body); status != 202 {
				t.Fatal("fresh reauth rejected", status)
			}
			if status := client.do("POST", "/api/admin/backups/maintenance/requests", req, &body); status != 403 || len(host.submitted) != 1 {
				t.Fatal("TOTP replay dispatched another host operation", status)
			}
		})
	}
}

func TestBackupMaintenanceRoutesRequireSuperAdminSessionNotSettingsOrAPIToken(t *testing.T) {
	_, client, host, _, req := maintenanceHTTPFixture(t)
	group := createAssignableGroup(t, client, "synthetic maintenance settings operator", []string{PermissionSettingsView, PermissionSettingsUpdate})
	var operator AdminUser
	if status := client.do("POST", "/api/admin/users", map[string]any{"email": "maintenance-operator@example.test", "displayName": "Synthetic maintenance operator", "role": "user", "password": "SyntheticMaintenance123!", "permissionGroupIds": []string{group.ID}}, &operator); status != 201 {
		t.Fatal("operator fixture", status)
	}
	settings := &testClient{t: t, server: client.server}
	var body map[string]any
	if status := settings.do("POST", "/api/auth/login", map[string]string{"email": operator.Email, "password": "SyntheticMaintenance123!"}, &body); status != 200 {
		t.Fatal("operator login", status)
	}
	token := createTestAPIToken(t, client, "synthetic maintenance token")
	apiToken := &testClient{t: t, server: client.server, bearer: token}
	anonymous := &testClient{t: t, server: client.server}
	for _, route := range []struct{ method, path string }{{"GET", "/api/admin/backups/maintenance/plans"}, {"POST", "/api/admin/backups/maintenance/requests"}, {"GET", "/api/admin/backups/maintenance/requests/" + strings.Repeat("0", 32)}} {
		if status := settings.do(route.method, route.path, req, &body); status != 403 {
			t.Fatal("settings group escalated to host maintenance", status)
		}
		for _, denied := range []*testClient{apiToken, anonymous} {
			if status := denied.do(route.method, route.path, req, &body); status != 401 && status != 403 {
				t.Fatal("non-session accessed host maintenance", status)
			}
		}
	}
	if len(host.submitted) != 0 || host.plansCalls != 0 || host.statusCalls != 0 {
		t.Fatal("unauthorized route contacted host helper")
	}
}
