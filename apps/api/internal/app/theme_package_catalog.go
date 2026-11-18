package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

const (
	themePackageSettingKey    = "appearance.theme-packages.v1"
	maxInstalledThemePackages = 24
	// The lowest supported storage limit is MySQL TEXT (65,535 bytes).
	// Reserve 8 KiB above the installed-revisions budget for two live snapshots.
	maxThemePackageStoreBytes   = 60 * 1024
	maxThemePackageRecordsBytes = 52 * 1024
	themePackageUpdateTries     = 8
	themePackageFront           = "front"
	themePackageAdmin           = "admin"
)

type themePackageRecord struct {
	Current  themePackage  `json:"current"`
	Previous *themePackage `json:"previous,omitempty"`
}

type activeThemePackageRevision struct {
	ID         string                 `json:"id"`
	Version    string                 `json:"version"`
	Appearance themePackageAppearance `json:"appearance"`
}

type themePackageCatalog struct {
	Packages map[string]themePackageRecord         `json:"packages"`
	Active   map[string]activeThemePackageRevision `json:"active"`
}

type themePackageListItem struct {
	themePackage
	PreviousVersion string `json:"previousVersion,omitempty"`
}

type themePackageActiveIDs struct {
	Front string `json:"front"`
	Admin string `json:"admin"`
}

type themePackageActiveVersions struct {
	Front string `json:"front,omitempty"`
	Admin string `json:"admin,omitempty"`
}

type themePackageCatalogResponse struct {
	Packages       []themePackageListItem     `json:"packages"`
	Active         themePackageActiveIDs      `json:"active"`
	ActiveVersions themePackageActiveVersions `json:"activeVersions"`
}

type themePackageCatalogError struct {
	status  int
	message string
}

func (e *themePackageCatalogError) Error() string { return e.message }

var (
	errThemePackageAlreadyInstalled = &themePackageCatalogError{http.StatusConflict, "Theme package ID is already installed"}
	errThemePackageBadReplacement   = &themePackageCatalogError{http.StatusConflict, "Update must match the installed GitHub source and theme package ID"}
	errThemePackageExistingVersion  = &themePackageCatalogError{http.StatusConflict, "Theme package version is already installed"}
	errThemePackageActive           = &themePackageCatalogError{http.StatusConflict, "Disable the theme package on each active surface before removing it"}
	errThemePackageNoPrevious       = &themePackageCatalogError{http.StatusConflict, "No previous theme package version is installed"}
	errThemePackageStorageLimit     = &themePackageCatalogError{http.StatusConflict, "Theme package catalog is full"}
	errThemePackageConflict         = &themePackageCatalogError{http.StatusConflict, "Theme package catalog changed concurrently; please retry"}
	errThemePackageNotFound         = &themePackageCatalogError{http.StatusNotFound, "Theme package is not installed"}
	errThemePackagePreviewChanged   = &themePackageCatalogError{http.StatusConflict, "Theme package version changed; refresh before activating"}
	errThemePackageSurface          = &themePackageCatalogError{http.StatusBadRequest, "Theme package does not support this surface"}
)

func validThemePackageSurface(surface string) bool {
	return surface == themePackageFront || surface == themePackageAdmin
}

func (a *App) readThemePackageCatalog(ctx context.Context) (themePackageCatalog, string, bool, error) {
	key := keyColumnSQL(a.cfg.DBDriver)
	var raw string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE "+key+"=?", themePackageSettingKey).Scan(&raw)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return themePackageCatalog{}, "", false, err
	}
	state := themePackageCatalog{
		Packages: make(map[string]themePackageRecord),
		Active:   make(map[string]activeThemePackageRevision),
	}
	if exists {
		if len(raw) > maxThemePackageStoreBytes {
			return themePackageCatalog{}, "", false, errors.New("stored theme package catalog exceeds limit")
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return themePackageCatalog{}, "", false, fmt.Errorf("decode stored theme package catalog: %w", err)
		}
		if state.Packages == nil || state.Active == nil || len(state.Packages) > maxInstalledThemePackages || len(state.Active) > 2 {
			return themePackageCatalog{}, "", false, errors.New("invalid stored theme package catalog")
		}
	}
	return state, raw, exists, nil
}

func (a *App) readActiveThemePackage(ctx context.Context, surface string) *themePackageAppearance {
	state, _, _, err := a.readThemePackageCatalog(ctx)
	if err != nil {
		return nil
	}
	if revision, ok := state.Active[surface]; ok && validStoredThemePackageAppearance(revision.Appearance) {
		return &revision.Appearance
	}
	return nil
}

// Imported data is validated before it can be stored. Fail closed at runtime
// too if a manually restored or edited settings row contains unsafe tokens.
func validStoredThemePackageAppearance(value themePackageAppearance) bool {
	if value.Layout != "classic" && value.Layout != "workbench" {
		return false
	}
	for _, group := range []map[string]string{value.Light, value.Dark} {
		if group == nil {
			return false
		}
		for key, token := range group {
			if !validThemePackageToken(key, token) {
				return false
			}
		}
	}
	return true
}

func (a *App) modifyThemePackageCatalog(ctx context.Context, mutate func(*themePackageCatalog) error) (themePackageCatalog, error) {
	driver := a.cfg.DBDriver
	key := keyColumnSQL(driver)
	insert := insertIgnoreSQL(driver, "INSERT INTO system_settings("+key+",value,updated_at) VALUES(?,?,?)", "("+key+")")
	compare := "value=?"
	if sqlDialect(driver) == databaseDriverMySQL {
		// Catalog strings can vary only in case or accents; the row's collation
		// must not make two distinct revisions compare equal during CAS.
		compare = "CAST(value AS BINARY)=CAST(? AS BINARY)"
	}
	update := "UPDATE system_settings SET value=?,updated_at=? WHERE " + key + "=? AND " + compare

	for range themePackageUpdateTries {
		state, previous, exists, err := a.readThemePackageCatalog(ctx)
		if err != nil {
			return themePackageCatalog{}, err
		}
		if err := mutate(&state); err != nil {
			return themePackageCatalog{}, err
		}
		data, err := json.Marshal(state)
		if err != nil {
			return themePackageCatalog{}, err
		}
		if len(data) > maxThemePackageStoreBytes {
			return themePackageCatalog{}, errThemePackageStorageLimit
		}
		if exists && string(data) == previous {
			return state, nil
		}
		now := a.now().UTC().Format(time.RFC3339Nano)
		var result sql.Result
		if exists {
			result, err = a.db.ExecContext(ctx, update, string(data), now, themePackageSettingKey, previous)
		} else {
			result, err = a.db.ExecContext(ctx, insert, themePackageSettingKey, string(data), now)
		}
		if err != nil {
			return themePackageCatalog{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return themePackageCatalog{}, err
		}
		if count == 1 {
			return state, nil
		}
	}
	return themePackageCatalog{}, errThemePackageConflict
}

func (state *themePackageCatalog) install(pkg themePackage, replacementSource string) error {
	record, exists := state.Packages[pkg.ID]
	if replacementSource == "" {
		if exists {
			return errThemePackageAlreadyInstalled
		}
		if len(state.Packages) >= maxInstalledThemePackages {
			return errThemePackageStorageLimit
		}
		state.Packages[pkg.ID] = themePackageRecord{Current: pkg}
	} else {
		if !exists || record.Current.Source != replacementSource || pkg.Source != replacementSource {
			return errThemePackageBadReplacement
		}
		if record.Current.Version == pkg.Version || (record.Previous != nil && record.Previous.Version == pkg.Version) {
			return errThemePackageExistingVersion
		}
		for _, revision := range state.Active {
			if revision.ID == pkg.ID && revision.Version == pkg.Version {
				return errThemePackageExistingVersion
			}
		}
		state.Packages[pkg.ID] = themePackageRecord{Current: pkg, Previous: &record.Current}
	}
	data, err := json.Marshal(state.Packages)
	if err != nil {
		return err
	}
	if len(data) > maxThemePackageRecordsBytes {
		return errThemePackageStorageLimit
	}
	return nil
}

func (state *themePackageCatalog) activate(surface, id, version string) error {
	if id == defaultUIThemeID {
		delete(state.Active, surface)
		return nil
	}
	record, ok := state.Packages[id]
	if !ok {
		return errThemePackageNotFound
	}
	if record.Current.Version != version {
		return errThemePackagePreviewChanged
	}
	appearance, ok := record.Current.Surfaces[surface]
	if !ok || !validStoredThemePackageAppearance(appearance) {
		return errThemePackageSurface
	}
	state.Active[surface] = activeThemePackageRevision{ID: id, Version: record.Current.Version, Appearance: appearance}
	return nil
}

func (state *themePackageCatalog) rollback(id, current, previous string) error {
	record, exists := state.Packages[id]
	if !exists {
		return errThemePackageNotFound
	}
	if record.Previous == nil {
		return errThemePackageNoPrevious
	}
	if record.Current.Version != current || record.Previous.Version != previous {
		return errThemePackageConflict
	}
	state.Packages[id] = themePackageRecord{Current: *record.Previous, Previous: &record.Current}
	return nil
}

func (state *themePackageCatalog) remove(id string) error {
	if _, exists := state.Packages[id]; !exists {
		return errThemePackageNotFound
	}
	for _, revision := range state.Active {
		if revision.ID == id {
			return errThemePackageActive
		}
	}
	delete(state.Packages, id)
	return nil
}

func (state themePackageCatalog) response() themePackageCatalogResponse {
	out := themePackageCatalogResponse{
		Packages: make([]themePackageListItem, 0, len(state.Packages)),
		Active:   themePackageActiveIDs{Front: defaultUIThemeID, Admin: defaultUIThemeID},
	}
	for _, record := range state.Packages {
		item := themePackageListItem{themePackage: record.Current}
		if record.Previous != nil {
			item.PreviousVersion = record.Previous.Version
		}
		out.Packages = append(out.Packages, item)
	}
	slices.SortFunc(out.Packages, func(a, b themePackageListItem) int {
		if a.ID == b.ID {
			return 0
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	if front, ok := state.Active[themePackageFront]; ok {
		out.Active.Front, out.ActiveVersions.Front = front.ID, front.Version
	}
	if admin, ok := state.Active[themePackageAdmin]; ok {
		out.Active.Admin, out.ActiveVersions.Admin = admin.ID, admin.Version
	}
	return out
}
