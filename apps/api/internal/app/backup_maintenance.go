package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"lanqin-email-api/internal/backup/maintenancehost"
)

type backupMaintenanceClient interface {
	Plans(context.Context) ([]maintenancehost.Plan, error)
	Submit(context.Context, maintenancehost.Request) (maintenancehost.Status, error)
	Status(context.Context, string) (maintenancehost.Status, error)
}

type backupMaintenanceRequest struct {
	SourceJobID    string `json:"sourceJobId"`
	PlanID         string `json:"planId"`
	TemplateSHA256 string `json:"templateSHA256"`
	RequestID      string `json:"requestId"`
	Password       string `json:"password"`
	TwoFactorCode  string `json:"twoFactorCode,omitempty"`
	Confirm        bool   `json:"confirm"`
}

var backupMaintenancePlanPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var backupMaintenanceHostIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (a *App) backupMaintenanceClient() (backupMaintenanceClient, error) {
	socket := a.cfg.BackupMaintenanceSocket
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 4096 || strings.IndexFunc(socket, unicode.IsControl) >= 0 {
		return nil, maintenancehost.ErrUnavailable
	}
	if a.backupMaintenanceClientFactory != nil {
		client := a.backupMaintenanceClientFactory(socket)
		if client == nil {
			return nil, maintenancehost.ErrUnavailable
		}
		return client, nil
	}
	return maintenancehost.NewClient(socket), nil
}

func validBackupMaintenancePlan(plan maintenancehost.Plan) bool {
	digest, err := hex.DecodeString(plan.TemplateSHA256)
	return backupMaintenancePlanPattern.MatchString(plan.ID) && err == nil && len(digest) == 32 && plan.TemplateSHA256 == strings.ToLower(plan.TemplateSHA256) && (plan.Driver == "sqlite" || plan.Driver == "mysql" || plan.Driver == "postgres")
}

func validBackupMaintenancePlans(plans []maintenancehost.Plan) bool {
	if len(plans) > 64 {
		return false
	}
	seen := map[string]bool{}
	for _, plan := range plans {
		if !validBackupMaintenancePlan(plan) || seen[plan.ID] {
			return false
		}
		seen[plan.ID] = true
	}
	return true
}

func (a *App) handleBackupMaintenancePlans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	items := []maintenancehost.Plan{}
	client, err := a.backupMaintenanceClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		items, err = client.Plans(ctx)
		cancel()
	}
	if err != nil || !validBackupMaintenancePlans(items) {
		respondJSON(w, http.StatusOK, map[string]any{"available": false, "items": []maintenancehost.Plan{}})
		return
	}
	if items == nil {
		items = []maintenancehost.Plan{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"available": true, "items": items})
}

func backupMaintenanceRequestID(actor, nonce string) string {
	digest := sha256.Sum256([]byte(actor + ":" + nonce))
	return hex.EncodeToString(digest[:16])
}

func (a *App) handleCreateBackupMaintenanceRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req backupMaintenanceRequest
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	digest, digestErr := hex.DecodeString(req.TemplateSHA256)
	if !req.Confirm || !backupJobIDPattern.MatchString(req.SourceJobID) || !backupMaintenancePlanPattern.MatchString(req.PlanID) || !backupRequestPattern.MatchString(req.RequestID) || digestErr != nil || len(digest) != 32 || req.TemplateSHA256 != strings.ToLower(req.TemplateSHA256) {
		backupError(w, errBackupInvalid)
		return
	}
	user := currentUser(r)
	if retry, err := a.checkLoginRateLimit(r.Context(), user.Email, r.RemoteAddr); err != nil {
		backupError(w, err)
		return
	} else if retry > 0 {
		respondLoginRateLimited(w, retry)
		return
	}
	if len(req.Password) > maxPasswordBytes || len(req.TwoFactorCode) > 6 || a.verifyLinuxDoReauthentication(r.Context(), user.ID, req.Password, req.TwoFactorCode) != nil {
		retry, err := a.recordLoginFailure(r.Context(), user.Email, r.RemoteAddr)
		if err != nil {
			backupError(w, err)
		} else if retry > 0 {
			respondLoginRateLimited(w, retry)
		} else {
			respondError(w, http.StatusForbidden, "backup_restore_reauthentication_failed")
		}
		return
	}
	req.Password, req.TwoFactorCode = "", ""
	job, _, err := a.backupReadJob(r.Context(), req.SourceJobID)
	if err != nil || job.Kind != "restore" || job.Status != "succeeded" || job.CancelRequested || job.Restore == nil || !job.Restore.Ready || job.Restore.Manifest == nil {
		backupError(w, errBackupRestoreSource)
		return
	}
	manifest := job.Restore.Manifest
	if manifest.Driver == "sqlite" && (manifest.Scope != "online-data" || manifest.Version != 1) || manifest.Driver != "sqlite" && (manifest.Scope != "online-native-data" || manifest.Version != 2 || manifest.Driver != "postgres" && manifest.Driver != "mysql") {
		backupError(w, errBackupRestoreSource)
		return
	}
	report, digestErr := hex.DecodeString(job.Restore.ReportSHA256)
	if digestErr != nil || len(report) != 32 || job.Restore.ReportSHA256 != strings.ToLower(job.Restore.ReportSHA256) {
		backupError(w, errBackupRestoreSource)
		return
	}
	client, err := a.backupMaintenanceClient()
	if err != nil {
		backupMaintenanceError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	plans, err := client.Plans(ctx)
	if err != nil || !validBackupMaintenancePlans(plans) {
		backupMaintenanceError(w, maintenancehost.ErrUnavailable)
		return
	}
	var selected *maintenancehost.Plan
	for _, plan := range plans {
		if plan.ID == req.PlanID {
			copy := plan
			selected = &copy
			break
		}
	}
	if selected == nil {
		backupMaintenanceError(w, maintenancehost.ErrNotFound)
		return
	}
	if selected.TemplateSHA256 != req.TemplateSHA256 {
		backupMaintenanceError(w, maintenancehost.ErrConflict)
		return
	}
	if selected.Driver != job.Restore.Manifest.Driver {
		backupError(w, errBackupRestoreSource)
		return
	}
	request := maintenancehost.Request{ID: backupMaintenanceRequestID(user.ID, req.RequestID), PlanID: req.PlanID, TemplateSHA256: req.TemplateSHA256, WebJob: job.ID, ReportSHA256: job.Restore.ReportSHA256, Actor: user.ID}
	status, err := client.Submit(ctx, request)
	if err != nil {
		backupMaintenanceError(w, err)
		return
	}
	if !validBackupMaintenanceStatus(status) || status.ID != request.ID || status.PlanID != request.PlanID || status.WebJob != request.WebJob {
		backupMaintenanceError(w, maintenancehost.ErrUnavailable)
		return
	}
	respondJSON(w, http.StatusAccepted, status)
}

func (a *App) handleBackupMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := chi.URLParam(r, "id")
	if !backupMaintenanceHostIDPattern.MatchString(id) {
		backupError(w, errBackupInvalid)
		return
	}
	client, err := a.backupMaintenanceClient()
	if err != nil {
		backupMaintenanceError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status, err := client.Status(ctx, id)
	if err != nil {
		backupMaintenanceError(w, err)
		return
	}
	if !validBackupMaintenanceStatus(status) || status.ID != id {
		backupMaintenanceError(w, maintenancehost.ErrUnavailable)
		return
	}
	respondJSON(w, http.StatusOK, status)
}

func validBackupMaintenanceStatus(status maintenancehost.Status) bool {
	if status.Version != 1 || !backupMaintenanceHostIDPattern.MatchString(status.ID) || !backupMaintenancePlanPattern.MatchString(status.PlanID) || !backupJobIDPattern.MatchString(status.WebJob) || status.CreatedAt.IsZero() || status.UpdatedAt.IsZero() || status.UpdatedAt.Before(status.CreatedAt) {
		return false
	}
	switch status.State {
	case "queued", "preparing", "maintaining", "succeeded", "failed", "needsRecovery":
	default:
		return false
	}
	switch status.Phase {
	case "queued", "preparing", "importing", "maintenance", "completed", "interrupted":
	default:
		return false
	}
	switch status.ErrorCode {
	case "", "unavailable", "source_unavailable", "preparation_failed", "import_failed", "maintenance_failed", "interrupted":
	default:
		return false
	}
	return true
}

func backupMaintenanceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, maintenancehost.ErrConflict):
		respondError(w, http.StatusConflict, "backup_maintenance_conflict")
	case errors.Is(err, maintenancehost.ErrNotFound):
		respondError(w, http.StatusNotFound, "backup_maintenance_not_found")
	default:
		respondError(w, http.StatusServiceUnavailable, "backup_maintenance_unavailable")
	}
}
