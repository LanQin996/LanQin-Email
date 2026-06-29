package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const defaultAPITokenTTL = 90 * 24 * time.Hour

func (a *App) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,last_used_at,expires_at,disabled,created_at,updated_at
		FROM api_tokens WHERE user_id=? ORDER BY created_at DESC`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list api tokens")
		return
	}
	defer rows.Close()
	items := []APIToken{}
	for rows.Next() {
		item, err := scanAPIToken(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan api tokens")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list api tokens")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		Name      string `json:"name"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		badRequest(w, errors.New("name is required"))
		return
	}
	if len([]rune(name)) > 80 {
		badRequest(w, errors.New("name cannot exceed 80 characters"))
		return
	}
	expiresAt, err := parseOptionalFutureTime(req.ExpiresAt, a.now().UTC())
	if err != nil {
		badRequest(w, err)
		return
	}
	if expiresAt == nil {
		defaultExpiry := a.now().UTC().Add(defaultAPITokenTTL)
		expiresAt = &defaultExpiry
	}
	id := newID("apt")
	token := "lq_" + randomToken()
	now := a.now().UTC().Format(time.RFC3339Nano)
	var expiresValue any
	if expiresAt != nil {
		expiresValue = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO api_tokens(id,user_id,name,token_hash,expires_at,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, id, user.ID, name, hashToken(token), expiresValue, 0, now, now); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create api token")
		return
	}
	item, err := a.apiTokenByID(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load api token")
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"token": token, "item": item})
}

func (a *App) handleUpdateAPIToken(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		respondError(w, http.StatusNotFound, "api token not found")
		return
	}
	var req struct {
		Name      *string `json:"name"`
		ExpiresAt *string `json:"expiresAt"`
		Disabled  *bool   `json:"disabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	current, err := a.apiTokenByID(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusNotFound, "api token not found")
		return
	}
	name := current.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			badRequest(w, errors.New("name is required"))
			return
		}
		if len([]rune(name)) > 80 {
			badRequest(w, errors.New("name cannot exceed 80 characters"))
			return
		}
	}
	var expiresValue any
	if current.ExpiresAt != nil {
		expiresValue = current.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if req.ExpiresAt != nil {
		if strings.TrimSpace(*req.ExpiresAt) == "" {
			badRequest(w, errors.New("expiresAt must be an RFC3339 timestamp"))
			return
		}
		expiresAt, err := parseOptionalFutureTime(*req.ExpiresAt, a.now().UTC())
		if err != nil {
			badRequest(w, err)
			return
		}
		expiresValue = nil
		if expiresAt != nil {
			expiresValue = expiresAt.UTC().Format(time.RFC3339Nano)
		}
	}
	disabled := current.Disabled
	if req.Disabled != nil {
		disabled = *req.Disabled
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE api_tokens SET name=?,expires_at=?,disabled=?,updated_at=? WHERE id=? AND user_id=?`,
		name, expiresValue, boolInt(disabled), a.now().UTC().Format(time.RFC3339Nano), id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update api token")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "api token not found")
		return
	}
	item, err := a.apiTokenByID(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load api token")
		return
	}
	respondJSON(w, http.StatusOK, item)
}

func (a *App) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM api_tokens WHERE id=? AND user_id=?`, chi.URLParam(r, "id"), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete api token")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "api token not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiTokenByID(ctx context.Context, userID, id string) (APIToken, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,name,last_used_at,expires_at,disabled,created_at,updated_at
		FROM api_tokens WHERE id=? AND user_id=?`, id, userID)
	return scanAPIToken(row)
}

type apiTokenScanner interface{ Scan(dest ...any) error }

func scanAPIToken(row apiTokenScanner) (APIToken, error) {
	var item APIToken
	var lastUsed, expires sql.NullString
	var disabled int
	var created, updated string
	if err := row.Scan(&item.ID, &item.Name, &lastUsed, &expires, &disabled, &created, &updated); err != nil {
		return item, err
	}
	item.LastUsedAt = nullableTime(lastUsed)
	item.ExpiresAt = nullableTime(expires)
	item.Disabled = intBool(disabled)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func parseOptionalFutureTime(value string, now time.Time) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, errors.New("expiresAt must be an RFC3339 timestamp")
	}
	t = t.UTC()
	if !t.After(now) {
		return nil, errors.New("expiresAt must be in the future")
	}
	return &t, nil
}
