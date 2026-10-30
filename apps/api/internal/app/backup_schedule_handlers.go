package app

import (
	"github.com/go-chi/chi/v5"
	"net/http"
)

func (a *App) handleBackupSchedules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, raw, err := a.loadBackupSchedules(r.Context())
	if err != nil {
		backupError(w, err)
		return
	}
	items := []backupScheduleView{}
	for _, p := range state.Items {
		items = append(items, backupScheduleView{DestinationID: p.DestinationID, Enabled: p.Enabled, Time: p.Time, Timezone: p.Timezone, NextRunAt: p.NextRunAt, LastJobID: p.LastJobID, LastErrorCode: p.LastErrorCode})
	}
	respondJSON(w, http.StatusOK, map[string]any{"revision": backupRevision(raw), "items": items})
}

func (a *App) handleSaveBackupSchedule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req backupScheduleRequest
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	if err := a.saveBackupSchedule(r.Context(), chi.URLParam(r, "id"), req); err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
