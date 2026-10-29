package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"lanqin-email-api/internal/backup/catalog"
)

func (a *App) handleBackupArtifacts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	items := []catalog.Entry{}
	if a.cfg.BackupArtifactDir == "" {
		respondJSON(w, http.StatusOK, map[string]any{"available": false, "items": items, "maxBytes": backupMaxBytes})
		return
	}
	key, err := a.backupMasterKey()
	if err != nil {
		backupError(w, err)
		return
	}
	items, err = catalog.List(r.Context(), a.cfg.BackupArtifactDir, key, backupMaxBytes)
	if err != nil {
		backupError(w, errBackupExecution)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"available": true, "items": items, "maxBytes": backupMaxBytes})
}

func (a *App) handleCreateBackupUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		ArtifactID           string   `json:"artifactId"`
		DestinationIDs       []string `json:"destinationIds"`
		RequestID            string   `json:"requestId"`
		DestinationsRevision string   `json:"destinationsRevision"`
	}
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	if req.DestinationsRevision == "" {
		backupError(w, errBackupInvalid)
		return
	}
	job, err := a.enqueueBackupUpload(r.Context(), currentUser(r).ID, req.RequestID, req.ArtifactID, req.DestinationIDs, req.DestinationsRevision)
	if err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, backupJobResponse(job))
}

func (a *App) handleGetBackupUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	job, _, err := a.backupReadJob(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		backupError(w, err)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+job.ID+`.json"`)
	}
	respondJSON(w, http.StatusOK, backupJobResponse(job))
}

func (a *App) handleCancelBackupUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := a.cancelBackupUpload(r.Context(), chi.URLParam(r, "id")); err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

type backupJobCursor struct {
	Time string `json:"time"`
	Key  string `json:"key"`
}

func (a *App) handleListBackupUploads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	column := keyColumnSQL(a.cfg.DBDriver)
	query := `SELECT ` + column + `,value,updated_at FROM system_settings WHERE ` + column + ` LIKE ?`
	args := []any{backupJobPrefix + "%"}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if len(raw) > 256 {
			backupError(w, errBackupInvalid)
			return
		}
		var cursor backupJobCursor
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || !strings.HasPrefix(cursor.Key, backupJobPrefix) || !backupJobIDPattern.MatchString(strings.TrimPrefix(cursor.Key, backupJobPrefix)) {
			backupError(w, errBackupInvalid)
			return
		}
		stamp, err := time.Parse(time.RFC3339Nano, cursor.Time)
		if err != nil || stamp.UTC().Format(backupSQLTime) != cursor.Time {
			backupError(w, errBackupInvalid)
			return
		}
		query += ` AND (updated_at<? OR (updated_at=? AND ` + column + `<?))`
		args = append(args, cursor.Time, cursor.Time, cursor.Key)
	}
	query += ` ORDER BY updated_at DESC,` + column + ` DESC LIMIT 21`
	rows, err := a.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		backupError(w, err)
		return
	}
	type row struct{ key, raw, stamp string }
	saved := []row{}
	for rows.Next() {
		var v row
		if err := rows.Scan(&v.key, &v.raw, &v.stamp); err != nil {
			rows.Close()
			backupError(w, err)
			return
		}
		saved = append(saved, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		backupError(w, err)
		return
	}
	items := []backupJobView{}
	next := ""
	for i, v := range saved {
		if i == 20 {
			last := saved[19]
			data, _ := json.Marshal(backupJobCursor{Time: last.stamp, Key: last.key})
			next = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		var job backupJob
		if err := a.backupOpen(v.key, v.raw, &job); err != nil {
			backupError(w, err)
			return
		}
		if job.Version != 1 || v.key != backupJobPrefix+job.ID || !backupJobIDPattern.MatchString(job.ID) {
			backupError(w, errBackupInvalid)
			return
		}
		items = append(items, backupJobResponse(job))
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}
