package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func writeThemePackageCatalogError(w http.ResponseWriter, err error) {
	var failure *themePackageCatalogError
	if errors.As(err, &failure) {
		respondError(w, failure.status, failure.message)
		return
	}
	respondError(w, http.StatusInternalServerError, "Could not load or save theme package catalog")
}

func (a *App) handleGetThemePackages(w http.ResponseWriter, r *http.Request) {
	state, _, _, err := a.readThemePackageCatalog(r.Context())
	if err != nil {
		writeThemePackageCatalogError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	respondJSON(w, http.StatusOK, state.response())
}

func (a *App) handleThemePackageMutation(w http.ResponseWriter, r *http.Request, mutate func(*themePackageCatalog) error) {
	state, err := a.modifyThemePackageCatalog(r.Context(), mutate)
	if err != nil {
		writeThemePackageCatalogError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, state.response())
}

func (a *App) handleUploadThemePackage(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if !requireThemePackageMediaType(w, r, "application/gzip") {
		return
	}
	if r.ContentLength > themePackageMaxCompressed {
		respondError(w, http.StatusRequestEntityTooLarge, "Theme package archive exceeds 1 MiB")
		return
	}
	archive := http.MaxBytesReader(w, r.Body, themePackageMaxCompressed)
	pkg, err := readThemePackage(archive)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondError(w, http.StatusRequestEntityTooLarge, "Theme package archive exceeds 1 MiB")
		} else {
			respondError(w, http.StatusBadRequest, "Invalid theme package archive or manifest")
		}
		return
	}
	pkg.Source = ""
	a.handleThemePackageMutation(w, r, func(state *themePackageCatalog) error {
		return state.install(pkg, "")
	})
}

func requireThemePackageMediaType(w http.ResponseWriter, r *http.Request, expected string) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != expected {
		// Never accept browser-simple content types for authenticated mutations;
		// the browser must preflight a cross-origin JSON/gzip request.
		respondError(w, http.StatusUnsupportedMediaType, "Content-Type must be "+expected)
		return false
	}
	return true
}

func decodeThemePackageRequest(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2048))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}

type themePackageGitHubRequest struct {
	URL             string `json:"url"`
	ID              string `json:"id,omitempty"`
	ExpectedVersion string `json:"expectedVersion,omitempty"`
}

func (req themePackageGitHubRequest) checkReplacement(state *themePackageCatalog) error {
	record, ok := state.Packages[req.ID]
	if !ok || record.Current.Source == "" || req.URL != record.Current.Source {
		return errThemePackageBadReplacement
	}
	if req.ExpectedVersion != "" && req.ExpectedVersion != record.Current.Version {
		return errThemePackageConflict
	}
	return nil
}

func (a *App) handleGitHubThemePackage(w http.ResponseWriter, r *http.Request) {
	if !requireThemePackageMediaType(w, r, "application/json") {
		return
	}
	qs := r.URL.Query()
	isReplace := qs.Get("replace") == "true"
	if replacements := qs["replace"]; len(replacements) > 1 || (len(replacements) == 1 && !isReplace && replacements[0] != "false") {
		respondError(w, http.StatusBadRequest, "Invalid theme package replacement flag")
		return
	}
	var req themePackageGitHubRequest
	if err := decodeThemePackageRequest(r, &req); err != nil || req.URL == "" ||
		(isReplace && !validUIThemeID(req.ID)) ||
		(!isReplace && (req.ID != "" || req.ExpectedVersion != "")) ||
		(req.ExpectedVersion != "" && !validThemePackageVersion(req.ExpectedVersion)) {
		respondError(w, http.StatusBadRequest, "Invalid GitHub theme package request")
		return
	}

	if _, err := parseThemeGitHubSource(req.URL); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid GitHub theme package source")
		return
	}
	// A replacement request must have named a matching local package and
	// source before doing any network work. Recheck after download in the CAS.
	if isReplace {
		state, _, _, err := a.readThemePackageCatalog(r.Context())
		if err == nil {
			err = req.checkReplacement(&state)
		}
		if err != nil {
			writeThemePackageCatalogError(w, err)
			return
		}
	}
	pkg, err := downloadGitHubThemePackage(r.Context(), req.URL)
	if err != nil {
		respondError(w, http.StatusBadGateway, "Could not import a valid GitHub theme package release")
		return
	}
	if isReplace && pkg.ID != req.ID {
		writeThemePackageCatalogError(w, errThemePackageBadReplacement)
		return
	}
	a.handleThemePackageMutation(w, r, func(state *themePackageCatalog) error {
		if isReplace {
			if err := req.checkReplacement(state); err != nil {
				return err
			}
			return state.install(pkg, req.URL)
		}
		return state.install(pkg, "")
	})
}

func (a *App) handleActivateThemePackage(w http.ResponseWriter, r *http.Request) {
	if !requireThemePackageMediaType(w, r, "application/json") {
		return
	}
	var req struct {
		Surface string `json:"surface"`
		ID      string `json:"id"`
		Version string `json:"version,omitempty"`
	}
	if err := decodeThemePackageRequest(r, &req); err != nil || !validThemePackageSurface(req.Surface) ||
		!validUIThemeID(req.ID) || (req.ID == defaultUIThemeID && req.Version != "") ||
		(req.ID != defaultUIThemeID && !validThemePackageVersion(req.Version)) {
		respondError(w, http.StatusBadRequest, "Invalid theme package activation request")
		return
	}
	a.handleThemePackageMutation(w, r, func(state *themePackageCatalog) error {
		return state.activate(req.Surface, req.ID, req.Version)
	})
}

func (a *App) handleRollbackThemePackage(w http.ResponseWriter, r *http.Request) {
	if !requireThemePackageMediaType(w, r, "application/json") {
		return
	}
	var req struct {
		ID              string `json:"id"`
		ExpectedVersion string `json:"expectedVersion"`
		PreviousVersion string `json:"previousVersion"`
	}
	if err := decodeThemePackageRequest(r, &req); err != nil || !validUIThemeID(req.ID) || req.ID == defaultUIThemeID ||
		!validThemePackageVersion(req.ExpectedVersion) || !validThemePackageVersion(req.PreviousVersion) {
		respondError(w, http.StatusBadRequest, "Invalid theme package rollback request")
		return
	}
	a.handleThemePackageMutation(w, r, func(state *themePackageCatalog) error {
		return state.rollback(req.ID, req.ExpectedVersion, req.PreviousVersion)
	})
}

func (a *App) handleRemoveThemePackage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validUIThemeID(id) || id == defaultUIThemeID {
		respondError(w, http.StatusBadRequest, "Invalid theme package ID")
		return
	}
	a.handleThemePackageMutation(w, r, func(state *themePackageCatalog) error {
		return state.remove(id)
	})
}
