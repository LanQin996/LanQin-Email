package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func backupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBackupExecution):
		respondError(w, http.StatusServiceUnavailable, errBackupExecution.Error())
	case errors.Is(err, errBackupQueue):
		respondError(w, http.StatusTooManyRequests, errBackupQueue.Error())
	case errors.Is(err, errBackupKey):
		respondError(w, http.StatusServiceUnavailable, errBackupKey.Error())
	case errors.Is(err, errBackupConflict):
		respondError(w, http.StatusConflict, errBackupConflict.Error())
	case errors.Is(err, errBackupLimit):
		respondError(w, http.StatusRequestEntityTooLarge, errBackupLimit.Error())
	case errors.Is(err, errBackupMissing):
		respondError(w, http.StatusNotFound, errBackupMissing.Error())
	case errors.Is(err, errBackupInvalid), errors.Is(err, errBackupAuth):
		respondError(w, http.StatusBadRequest, err.Error())
	default:
		respondError(w, http.StatusInternalServerError, "backup_store_unavailable")
	}
}

func decodeBackupRequest(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errBackupInvalid // Do not echo invalid fields that may contain credentials.
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errBackupInvalid
	}
	return nil
}

func (a *App) handleBackupDestinations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, raw, err := a.loadBackupDestinationState(r.Context())
	if err != nil {
		backupError(w, err)
		return
	}
	_, keyErr := a.backupCipher()
	response := backupDestinationResponse(state, raw, keyErr == nil)
	registry, _, registryErr := a.backupRegistry(r.Context())
	if registryErr == nil {
		for i, record := range state.Items {
			if success, ok := registry.Success[record.Config.ID]; ok && success.Fingerprint == backupTargetFingerprint(record) {
				stamp := success.At
				response.Items[i].LastSuccessAt = &stamp
			}
		}
	}
	respondJSON(w, http.StatusOK, response)
}

func (a *App) handleCreateBackupDestination(w http.ResponseWriter, r *http.Request) {
	a.mutateBackupDestination(w, r, "create")
}

func (a *App) handleUpdateBackupDestination(w http.ResponseWriter, r *http.Request) {
	a.mutateBackupDestination(w, r, "update")
}

func (a *App) handleDeleteBackupDestination(w http.ResponseWriter, r *http.Request) {
	a.mutateBackupDestination(w, r, "delete")
}

func (a *App) mutateBackupDestination(w http.ResponseWriter, r *http.Request, operation string) {
	w.Header().Set("Cache-Control", "no-store")
	if _, err := a.backupCipher(); err != nil {
		backupError(w, err)
		return
	}
	var req backupDestinationRequest
	if operation == "delete" {
		var body struct {
			Revision string `json:"revision"`
		}
		if err := decodeBackupRequest(r, &body); err != nil {
			backupError(w, err)
			return
		}
		req.Revision = body.Revision
	} else if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	state, raw, err := a.loadBackupDestinationState(r.Context())
	if err != nil {
		backupError(w, err)
		return
	}
	if req.Revision != backupRevision(raw) {
		backupError(w, errBackupConflict)
		return
	}
	id, index := chi.URLParam(r, "id"), -1
	for i, record := range state.Items {
		if record.Config.ID == id {
			index = i
			break
		}
	}
	if operation != "create" && index == -1 {
		backupError(w, errBackupMissing)
		return
	}
	status := http.StatusOK
	if operation == "delete" {
		state.Items = append(state.Items[:index], state.Items[index+1:]...)
	} else {
		var old *backupDestinationRecord
		if operation == "create" {
			id, status = newID("bkd"), http.StatusCreated
		} else {
			old = &state.Items[index]
		}
		record, err := buildBackupDestination(id, req, old)
		if err != nil {
			backupError(w, err)
			return
		}
		if old == nil {
			state.Items = append(state.Items, record)
		} else {
			state.Items[index] = record
		}
	}
	if err := a.saveBackupDestinationState(r.Context(), raw, state); err != nil {
		backupError(w, err)
		return
	}
	// Clients refetch the list and revision after a mutation. Never echo the
	// request, since it contains credentials, or return persisted ciphertext.
	respondJSON(w, status, map[string]bool{"ok": true})
}
