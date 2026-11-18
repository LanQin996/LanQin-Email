package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	siteTitleSettingKey = "siteTitle"
	defaultSiteTitle    = "LanQin Email"
)

func normalizeSiteTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return defaultSiteTitle, nil
	}
	if utf8.RuneCountInString(title) > 80 {
		return "", errors.New("siteTitle must be 80 characters or fewer")
	}
	return title, nil
}

func systemSettingUpsertSQL(driver string) string {
	key := keyColumnSQL(driver)
	return upsertSQL(driver, `INSERT INTO system_settings(`+key+`,value,updated_at) VALUES(?,?,?)`, `(`+key+`)`,
		`value=excluded.value,updated_at=excluded.updated_at`,
		`value=VALUES(value),updated_at=VALUES(updated_at)`)
}

// Read the title separately from the mutable general-settings snapshot so a
// concurrent general settings update cannot put a stale brand back in memory.
func (a *App) siteTitle(ctx context.Context) string {
	key := keyColumnSQL(a.cfg.DBDriver)
	var title string
	if err := a.db.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE "+key+"=?", siteTitleSettingKey).Scan(&title); err == nil {
		return title
	}
	if a.siteTitleFallback != "" {
		return a.siteTitleFallback
	}
	return defaultSiteTitle
}

func (a *App) handleUpdateSiteBranding(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SiteTitle *string `json:"siteTitle"`
	}
	if err := decodeJSONWithLimit(r, &request, 1024); err != nil {
		badRequest(w, err)
		return
	}
	if request.SiteTitle == nil {
		badRequest(w, errors.New("siteTitle is required"))
		return
	}
	title, err := normalizeSiteTitle(*request.SiteTitle)
	if err != nil {
		badRequest(w, err)
		return
	}
	_, err = a.db.ExecContext(r.Context(), systemSettingUpsertSQL(a.cfg.DBDriver), siteTitleSettingKey, title, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save branding settings")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"siteTitle": title})
}
