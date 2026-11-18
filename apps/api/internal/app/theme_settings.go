package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"time"
)

// The web build owns the catalogue. A theme ID is a configuration value, never
// a path, URL, or template input. Unknown IDs render as Classic in the client.
const (
	uiThemeSettingKey = "appearance.theme.v1"
	defaultUIThemeID  = "classic"
)

var uiThemeIDPattern = regexp.MustCompile("^[a-z][a-z0-9]*(-[a-z0-9]+)*$")

type uiThemeSettings struct {
	ThemeID string `json:"themeId"`
}

func validUIThemeID(id string) bool {
	return len(id) <= 48 && uiThemeIDPattern.MatchString(id)
}

func (a *App) readUIThemeID(ctx context.Context) (string, error) {
	key := keyColumnSQL(a.cfg.DBDriver)
	var id string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE "+key+"=?", uiThemeSettingKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !validUIThemeID(id)) {
		return defaultUIThemeID, nil
	}
	return id, err
}

func (a *App) handleGetUITheme(w http.ResponseWriter, r *http.Request) {
	id, err := a.readUIThemeID(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载主题设置")
		return
	}
	respondJSON(w, http.StatusOK, uiThemeSettings{ThemeID: id})
}

func (a *App) handleUpdateUITheme(w http.ResponseWriter, r *http.Request) {
	var settings uiThemeSettings
	if err := decodeJSONWithLimit(r, &settings, 1024); err != nil || !validUIThemeID(settings.ThemeID) {
		respondError(w, http.StatusBadRequest, "无效的主题 ID")
		return
	}
	key := keyColumnSQL(a.cfg.DBDriver)
	query := upsertSQL(a.cfg.DBDriver, `INSERT INTO system_settings(`+key+`,value,updated_at) VALUES(?,?,?)`, `(`+key+`)`,
		`value=excluded.value,updated_at=excluded.updated_at`,
		`value=VALUES(value),updated_at=VALUES(updated_at)`)
	if _, err := a.db.ExecContext(r.Context(), query, uiThemeSettingKey, settings.ThemeID, a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		respondError(w, http.StatusInternalServerError, "无法保存主题设置")
		return
	}
	respondJSON(w, http.StatusOK, settings)
}
