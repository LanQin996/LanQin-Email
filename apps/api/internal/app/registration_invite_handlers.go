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

var errRegistrationInviteInvalid = errors.New("registration invite is invalid or exhausted")

type RegistrationInvite struct {
	ID             string    `json:"id"`
	Code           string    `json:"code"`
	MaxUses        int       `json:"maxUses"`
	UsedCount      int       `json:"usedCount"`
	RemainingUses  int       `json:"remainingUses"`
	CreatedByEmail string    `json:"createdByEmail,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (a *App) handleListRegistrationInvites(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT i.id,i.code,i.max_uses,i.used_count,COALESCE(u.email,''),i.created_at
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
		var createdAt string
		if err := rows.Scan(&item.ID, &item.Code, &item.MaxUses, &item.UsedCount, &item.CreatedByEmail, &createdAt); err != nil {
			respondError(w, http.StatusInternalServerError, "无法加载邀请码")
			return
		}
		item.RemainingUses = item.MaxUses - item.UsedCount
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "无法加载邀请码")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateRegistrationInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code    string `json:"code"`
		MaxUses int    `json:"maxUses"`
	}
	if err := decodeJSONWithLimit(r, &req, 8<<10); err != nil {
		badRequest(w, err)
		return
	}
	if req.MaxUses < 1 || req.MaxUses > 1_000_000 {
		badRequest(w, errors.New("可用次数必须在 1 到 1000000 之间"))
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
		ID:             newID("inv"),
		Code:           code,
		MaxUses:        req.MaxUses,
		RemainingUses:  req.MaxUses,
		CreatedByEmail: currentUser(r).Email,
		CreatedAt:      now,
	}
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO registration_invites(id,code,max_uses,used_count,created_by,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)`, item.ID, item.Code, item.MaxUses, 0, currentUser(r).ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		if isUniqueViolation(err) {
			respondError(w, http.StatusConflict, "该邀请码已存在")
			return
		}
		respondError(w, http.StatusInternalServerError, "创建邀请码失败")
		return
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

func (a *App) consumeRegistrationInviteTx(ctx context.Context, tx *sql.Tx, code string) error {
	normalized, err := normalizeRegistrationInviteCode(code)
	if err != nil {
		return errRegistrationInviteInvalid
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE registration_invites SET used_count=used_count+1,updated_at=?
		WHERE code=? AND used_count<max_uses`, now, normalized)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errRegistrationInviteInvalid
	}
	return nil
}
