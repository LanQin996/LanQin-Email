package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

type mailboxPushRequest struct {
	ID, MailboxID, MailboxAddress, FromUserID, FromEmail, ToUserID, ToEmail, Status string
	ExpiresAt, CreatedAt                                                            time.Time
	Version                                                                         int
}

func (a *App) handleCreateMailboxPush(w http.ResponseWriter, r *http.Request) {
	var in struct{ MailboxID, ToUserID string }
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, strings.TrimSpace(in.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	u := currentUser(r)
	if strings.EqualFold(mb.Address, u.Email) {
		badRequest(w, errors.New("登录主邮箱不能转移"))
		return
	}
	target, err := a.userByID(r.Context(), strings.TrimSpace(in.ToUserID))
	if err != nil || target.Disabled || target.ID == u.ID || !userHasPermission(target, PermissionMailAccess) {
		badRequest(w, errors.New("目标用户不可用"))
		return
	}
	var pending int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailbox_push_requests WHERE mailbox_id=? AND status='pending'`, mb.ID).Scan(&pending); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check pending request")
		return
	}
	if pending > 0 {
		respondError(w, http.StatusConflict, "该邮箱已有待处理转移请求")
		return
	}
	now := a.now().UTC()
	exp := now.Add(24 * time.Hour)
	id := newID("push")
	stamp := now.Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO mailbox_push_requests(id,mailbox_id,from_user_id,to_user_id,status,expires_at,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, mb.ID, u.ID, target.ID, "pending", exp.Format(time.RFC3339Nano), 1, stamp, stamp)
	if err != nil {
		respondError(w, http.StatusConflict, "该邮箱已有待处理转移请求")
		return
	}
	a.addNotification(r.Context(), a.db, target.ID, "mailbox_push_requested", "收到邮箱转移请求", u.Email+" 请求将 "+mb.Address+" 转移给你", map[string]any{"requestId": id, "mailboxId": mb.ID}, "push:"+id)
	respondJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "pending", "expiresAt": exp})
}

func (a *App) handleListMailboxPush(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id,p.mailbox_id,m.address,p.from_user_id,fu.email,p.to_user_id,tu.email,p.status,p.expires_at,p.created_at,p.version FROM mailbox_push_requests p JOIN mailboxes m ON m.id=p.mailbox_id JOIN users fu ON fu.id=p.from_user_id JOIN users tu ON tu.id=p.to_user_id WHERE p.from_user_id=? OR p.to_user_id=? ORDER BY p.created_at DESC`, currentUser(r).ID, currentUser(r).ID)
	if err != nil {
		respondError(w, 500, "failed to load push requests")
		return
	}
	defer rows.Close()
	items := []mailboxPushRequest{}
	for rows.Next() {
		var x mailboxPushRequest
		var ex, cr string
		if err := rows.Scan(&x.ID, &x.MailboxID, &x.MailboxAddress, &x.FromUserID, &x.FromEmail, &x.ToUserID, &x.ToEmail, &x.Status, &ex, &cr, &x.Version); err != nil {
			respondError(w, 500, "failed to scan push requests")
			return
		}
		x.ExpiresAt = parseTime(ex)
		x.CreatedAt = parseTime(cr)
		if x.Status == "pending" && x.ExpiresAt.Before(a.now().UTC()) {
			_, _ = a.db.ExecContext(r.Context(), `UPDATE mailbox_push_requests SET status='expired',updated_at=?,resolved_at=? WHERE id=? AND status='pending'`, a.now().UTC().Format(time.RFC3339Nano), a.now().UTC().Format(time.RFC3339Nano), x.ID)
			x.Status = "expired"
		}
		items = append(items, x)
	}
	respondJSON(w, 200, map[string]any{"items": items})
}

func (a *App) resolveMailboxPush(w http.ResponseWriter, r *http.Request, action string) {
	id := chi.URLParam(r, "id")
	u := currentUser(r)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, 500, "failed to resolve request")
		return
	}
	defer tx.Rollback()
	var mailboxID, fromID, toID, status, expires string
	var version int
	err = tx.QueryRowContext(r.Context(), `SELECT mailbox_id,from_user_id,to_user_id,status,expires_at,version FROM mailbox_push_requests WHERE id=?`, id).Scan(&mailboxID, &fromID, &toID, &status, &expires, &version)
	if err != nil {
		respondError(w, 404, "request not found")
		return
	}
	if status != "pending" || parseTime(expires).Before(a.now().UTC()) {
		respondError(w, 409, "request is no longer pending")
		return
	}
	if action == "cancelled" && u.ID != fromID || action != "cancelled" && u.ID != toID {
		respondError(w, 403, "forbidden")
		return
	}
	stamp := a.now().UTC().Format(time.RFC3339Nano)
	if action != "accept" {
		if _, err = tx.ExecContext(r.Context(), `UPDATE mailbox_push_requests SET status=?,resolved_at=?,resolved_by=?,updated_at=?,version=version+1 WHERE id=? AND status='pending'`, action, stamp, u.ID, stamp, id); err != nil {
			respondError(w, 500, "failed to update request")
			return
		}
		if err = tx.Commit(); err != nil {
			respondError(w, 500, "failed to commit")
			return
		}
		respondJSON(w, 200, map[string]any{"status": action})
		return
	}
	var owner string
	var address string
	if err = tx.QueryRowContext(r.Context(), `SELECT user_id,address FROM mailboxes WHERE id=? AND status='active'`, mailboxID).Scan(&owner, &address); err != nil || owner != fromID {
		respondError(w, 409, "mailbox ownership changed")
		return
	}
	raw := make([]byte, 18)
	_, _ = rand.Read(raw)
	hash, _ := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(raw)), bcrypt.DefaultCost)
	if _, err = tx.ExecContext(r.Context(), `UPDATE mailboxes SET user_id=?,password_hash=?,updated_at=? WHERE id=? AND user_id=?`, toID, string(hash), stamp, mailboxID, fromID); err != nil {
		respondError(w, 500, "failed to transfer mailbox")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET revoked_at=?,version=version+1,updated_at=? WHERE mailbox_id=? AND revoked_at IS NULL AND left_at IS NULL`, stamp, stamp, mailboxID); err != nil {
		respondError(w, 500, "failed to revoke shares")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE mailbox_push_requests SET status='accepted',resolved_at=?,resolved_by=?,updated_at=?,version=version+1 WHERE id=? AND status='pending'`, stamp, u.ID, stamp, id); err != nil {
		respondError(w, 500, "failed to finalize request")
		return
	}
	a.addNotification(r.Context(), tx, fromID, "mailbox_push_accepted", "邮箱转移已完成", address+" 已转移给 "+u.Email, map[string]any{"mailboxId": mailboxID}, "")
	if err = tx.Commit(); err != nil {
		respondError(w, 500, "failed to commit transfer")
		return
	}
	respondJSON(w, 200, map[string]any{"status": "accepted"})
}
