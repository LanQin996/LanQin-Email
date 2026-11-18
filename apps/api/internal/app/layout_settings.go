package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	uiLayoutSettingKey = "appearance.layouts.v1"
	defaultUILayoutID  = "classic"
	layoutUpdateTries  = 8
)

var errUILayoutConflict = errors.New("site layout settings changed concurrently")

type uiLayoutSettings struct {
	FrontLayoutID string `json:"frontLayoutId"`
	AdminLayoutID string `json:"adminLayoutId"`
}

// Empty fields in this internal patch mean "keep the current value". HTTP
// validation rejects clients that explicitly supply null or an empty string.
func decodeUILayoutPatch(r *http.Request) (uiLayoutSettings, error) {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1024))
	dec.DisallowUnknownFields()
	var fields struct {
		Front json.RawMessage `json:"frontLayoutId"`
		Admin json.RawMessage `json:"adminLayoutId"`
	}
	var patch uiLayoutSettings
	if err := dec.Decode(&fields); err != nil {
		return patch, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return patch, errors.New("expected a single JSON object")
	}
	if len(fields.Front) == 0 && len(fields.Admin) == 0 {
		return patch, errors.New("at least one layout field is required")
	}
	for _, entry := range []struct {
		raw json.RawMessage
		id  *string
	}{
		{fields.Front, &patch.FrontLayoutID},
		{fields.Admin, &patch.AdminLayoutID},
	} {
		if len(entry.raw) > 0 && (json.Unmarshal(entry.raw, entry.id) != nil || !validUIThemeID(*entry.id)) {
			return patch, errors.New("invalid layout ID")
		}
	}
	return patch, nil
}

func decodeStoredUILayouts(raw string) uiLayoutSettings {
	var settings uiLayoutSettings
	if json.Unmarshal([]byte(raw), &settings) != nil {
		settings = uiLayoutSettings{}
	}
	if !validUIThemeID(settings.FrontLayoutID) {
		settings.FrontLayoutID = defaultUILayoutID
	}
	if !validUIThemeID(settings.AdminLayoutID) {
		settings.AdminLayoutID = defaultUILayoutID
	}
	return settings
}

func (a *App) readUILayoutRow(ctx context.Context) (raw string, exists bool, err error) {
	key := keyColumnSQL(a.cfg.DBDriver)
	err = a.db.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE "+key+"=?", uiLayoutSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return raw, err == nil, err
}

func (a *App) readUILayouts(ctx context.Context) (uiLayoutSettings, error) {
	raw, _, err := a.readUILayoutRow(ctx)
	return decodeStoredUILayouts(raw), err
}

func (a *App) updateUILayouts(ctx context.Context, patch uiLayoutSettings) (uiLayoutSettings, error) {
	key := keyColumnSQL(a.cfg.DBDriver)
	// Insert-if-absent lets the first write race safely on a fresh install;
	// after that, compare-and-swap prevents stale disjoint patches overwriting
	// the other value. The two selections always live in one atomic row.
	insert := insertIgnoreSQL(a.cfg.DBDriver, "INSERT INTO system_settings("+key+",value,updated_at) VALUES(?,?,?)", "("+key+")")
	update := "UPDATE system_settings SET value=?,updated_at=? WHERE " + key + "=? AND value=?"

	for range layoutUpdateTries {
		previous, exists, err := a.readUILayoutRow(ctx)
		if err != nil {
			return uiLayoutSettings{}, err
		}
		next := decodeStoredUILayouts(previous)
		if patch.FrontLayoutID != "" {
			next.FrontLayoutID = patch.FrontLayoutID
		}
		if patch.AdminLayoutID != "" {
			next.AdminLayoutID = patch.AdminLayoutID
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return uiLayoutSettings{}, err
		}
		if exists && previous == string(raw) {
			return next, nil
		}
		now := a.now().UTC().Format(time.RFC3339Nano)
		var result sql.Result
		if exists {
			result, err = a.db.ExecContext(ctx, update, string(raw), now, uiLayoutSettingKey, previous)
		} else {
			result, err = a.db.ExecContext(ctx, insert, uiLayoutSettingKey, string(raw), now)
		}
		if err != nil {
			return uiLayoutSettings{}, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return uiLayoutSettings{}, err
		}
		if affected == 1 {
			return next, nil
		}
	}
	return uiLayoutSettings{}, errUILayoutConflict
}

func (a *App) handleGetUILayouts(w http.ResponseWriter, r *http.Request) {
	settings, err := a.readUILayouts(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载界面模板设置")
		return
	}
	respondJSON(w, http.StatusOK, settings)
}

func (a *App) handleGetAdminUILayout(w http.ResponseWriter, r *http.Request) {
	settings, err := a.readUILayouts(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载界面模板设置")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	respondJSON(w, http.StatusOK, struct {
		AdminLayoutID string `json:"adminLayoutId"`
	}{settings.AdminLayoutID})
}

func (a *App) handleUpdateUILayouts(w http.ResponseWriter, r *http.Request) {
	patch, err := decodeUILayoutPatch(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "无效的界面模板设置")
		return
	}
	settings, err := a.updateUILayouts(r.Context(), patch)
	if errors.Is(err, errUILayoutConflict) {
		respondError(w, http.StatusConflict, "界面模板设置已更新，请重试")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法保存界面模板设置")
		return
	}
	respondJSON(w, http.StatusOK, settings)
}
