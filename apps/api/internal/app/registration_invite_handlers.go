package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var errRegistrationInviteInvalid = errors.New("registration invite is invalid or exhausted")

// errRegistrationInviteGroupUnavailable means the code is valid but the permission
// group it grants no longer exists, so honouring it would silently give the user
// less than the code promised.
var errRegistrationInviteGroupUnavailable = errors.New("registration invite permission group is unavailable")

type RegistrationInvite struct {
	ID                 string                   `json:"id"`
	Code               string                   `json:"code"`
	MaxUses            int                      `json:"maxUses"`
	UsedCount          int                      `json:"usedCount"`
	RemainingUses      int                      `json:"remainingUses"`
	PermissionGroupIDs []string                 `json:"permissionGroupIds"`
	PermissionGroups   []PermissionGroupSummary `json:"permissionGroups"`
	CreatedByEmail     string                   `json:"createdByEmail,omitempty"`
	CreatedAt          time.Time                `json:"createdAt"`
}

func (a *App) handleListRegistrationInvites(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT i.id,i.code,i.max_uses,i.used_count,i.permission_group_ids_json,COALESCE(u.email,''),i.created_at
		FROM registration_invites i LEFT JOIN users u ON u.id=i.created_by
		ORDER BY i.created_at DESC`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载邀请码")
		return
	}
	defer rows.Close()
	items := []RegistrationInvite{}
	for rows.Next() {
		var item RegistrationInvite
		var createdAt, groupIDsJSON string
		if err := rows.Scan(&item.ID, &item.Code, &item.MaxUses, &item.UsedCount, &groupIDsJSON, &item.CreatedByEmail, &createdAt); err != nil {
			respondError(w, http.StatusInternalServerError, "无法加载邀请码")
			return
		}
		item.RemainingUses = item.MaxUses - item.UsedCount
		item.PermissionGroupIDs = jsonDecodeSlice(groupIDsJSON)
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载邀请码")
		return
	}
	if err := rows.Close(); err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载邀请码")
		return
	}
	names := a.permissionGroupNamesByID(r.Context())
	for i := range items {
		items[i].PermissionGroups = make([]PermissionGroupSummary, 0, len(items[i].PermissionGroupIDs))
		for _, id := range items[i].PermissionGroupIDs {
			name := names[id]
			if name == "" {
				name = id + "（已删除）"
			}
			items[i].PermissionGroups = append(items[i].PermissionGroups, PermissionGroupSummary{ID: id, Name: name})
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) permissionGroupNamesByID(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := a.db.QueryContext(ctx, `SELECT id,name FROM permission_groups`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

func (a *App) handleCreateRegistrationInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code               string   `json:"code"`
		MaxUses            int      `json:"maxUses"`
		PermissionGroupIDs []string `json:"permissionGroupIds"`
	}
	if err := decodeJSONWithLimit(r, &req, 8<<10); err != nil {
		badRequest(w, err)
		return
	}
	if req.MaxUses < 1 || req.MaxUses > 1_000_000 {
		badRequest(w, errors.New("可用次数必须在 1 到 1000000 之间"))
		return
	}
	groupIDs := cleanIDList(req.PermissionGroupIDs)
	// isAssignablePermissionGroupID already excludes pg_super_admin, so an invite
	// can never hand out super administrator rights.
	if err := a.validateAssignableGroupIDsTx(r.Context(), nil, groupIDs); err != nil {
		badRequest(w, err)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		code = "INV-" + strings.ToUpper(randomToken()[:12])
	}
	code, err := normalizeRegistrationInviteCode(code)
	if err != nil {
		badRequest(w, err)
		return
	}
	now := a.now().UTC()
	item := RegistrationInvite{
		ID:                 newID("inv"),
		Code:               code,
		MaxUses:            req.MaxUses,
		RemainingUses:      req.MaxUses,
		PermissionGroupIDs: groupIDs,
		CreatedByEmail:     currentUser(r).Email,
		CreatedAt:          now,
	}
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO registration_invites(id,code,max_uses,used_count,permission_group_ids_json,created_by,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, item.ID, item.Code, item.MaxUses, 0, jsonEncode(groupIDs), currentUser(r).ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		if isUniqueViolation(err) {
			respondError(w, http.StatusConflict, "该邀请码已存在")
			return
		}
		respondError(w, http.StatusInternalServerError, "创建邀请码失败")
		return
	}
	names := a.permissionGroupNamesByID(r.Context())
	for _, id := range groupIDs {
		item.PermissionGroups = append(item.PermissionGroups, PermissionGroupSummary{ID: id, Name: names[id]})
	}
	respondJSON(w, http.StatusCreated, item)
}

func (a *App) handleDeleteRegistrationInvite(w http.ResponseWriter, r *http.Request) {
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM registration_invites WHERE id=?`, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "删除邀请码失败")
		return
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		respondError(w, http.StatusNotFound, "邀请码不存在")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func normalizeRegistrationInviteCode(value string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(value))
	if len(code) < 4 || len(code) > 64 {
		return "", errors.New("邀请码长度必须在 4 到 64 个字符之间")
	}
	for _, char := range code {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return "", errors.New("邀请码只能包含字母、数字、短横线和下划线")
	}
	return code, nil
}

func (a *App) validateRegistrationInvite(ctx context.Context, code string) error {
	normalized, err := normalizeRegistrationInviteCode(code)
	if err != nil {
		return errRegistrationInviteInvalid
	}
	var available int
	if err := a.db.QueryRowContext(ctx, `SELECT 1 FROM registration_invites WHERE code=? AND used_count<max_uses`, normalized).Scan(&available); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errRegistrationInviteInvalid
		}
		return err
	}
	return nil
}

// consumeRegistrationInviteTx atomically spends one use of the code and returns
// the permission groups it grants.
//
// The conditional UPDATE plus RowsAffected check is what prevents concurrent
// registrations from over-spending a code; the group IDs are read inside the same
// transaction so they cannot change between the two steps.
func (a *App) consumeRegistrationInviteTx(ctx context.Context, tx *sql.Tx, code string) ([]string, error) {
	normalized, err := normalizeRegistrationInviteCode(code)
	if err != nil {
		return nil, errRegistrationInviteInvalid
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE registration_invites SET used_count=used_count+1,updated_at=?
		WHERE code=? AND used_count<max_uses`, now, normalized)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, errRegistrationInviteInvalid
	}
	var groupIDsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT permission_group_ids_json FROM registration_invites WHERE code=?`, normalized).Scan(&groupIDsJSON); err != nil {
		return nil, err
	}
	groupIDs := cleanIDList(jsonDecodeSlice(groupIDsJSON))
	// Re-validate: a group bound at creation time may have been deleted since.
	// Failing loudly is deliberate — silently downgrading the user to the default
	// group would leave them believing they hold rights they do not.
	if err := a.validateAssignableGroupIDsTx(ctx, tx, groupIDs); err != nil {
		return nil, fmt.Errorf("%w: %v", errRegistrationInviteGroupUnavailable, err)
	}
	return groupIDs, nil
}
