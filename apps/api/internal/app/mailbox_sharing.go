package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type mailboxReadAccess struct {
	Mailbox          *Mailbox
	Owner            bool
	ShareID          string
	Scope            string
	IncludeStarred   bool
	AllowAttachments bool
}

const mailboxShareSelect = `SELECT ms.id,ms.mailbox_id,mb.address,ms.owner_user_id,own.email,own.display_name,
	ms.shared_with_user_id,recipient.email,recipient.display_name,ms.scope,ms.include_starred,ms.allow_attachments,
	ms.version,ms.expires_at,ms.last_accessed_at,ms.revoked_at,ms.left_at,ms.created_at
	FROM mailbox_shares ms
	JOIN mailboxes mb ON mb.id=ms.mailbox_id
	JOIN users own ON own.id=ms.owner_user_id
	JOIN users recipient ON recipient.id=ms.shared_with_user_id`

type mailboxShareRequest struct {
	Scope            string   `json:"scope"`
	FolderIDs        []string `json:"folderIds"`
	LabelIDs         []string `json:"labelIds"`
	IncludeStarred   bool     `json:"includeStarred"`
	AllowAttachments *bool    `json:"allowAttachments"`
	ExpiresInDays    *int     `json:"expiresInDays"`
	ExpiresAt        *string  `json:"expiresAt"`
	Version          int      `json:"version"`
}

func (a *App) handleSearchShareUsers(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(query)) < 2 {
		respondJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	like := "%" + escapeLike(strings.ToLower(query)) + "%"
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,email,display_name FROM users
		WHERE id<>? AND disabled=0 AND (lower(email) LIKE ? ESCAPE '\' OR lower(display_name) LIKE ? ESCAPE '\')
		ORDER BY display_name,email LIMIT 10`, user.ID, like, like)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to search users")
		return
	}
	type shareUser struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	}
	candidates := []shareUser{}
	for rows.Next() {
		var item shareUser
		if err := rows.Scan(&item.ID, &item.Email, &item.DisplayName); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "failed to scan users")
			return
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		respondError(w, http.StatusInternalServerError, "failed to scan users")
		return
	}
	if err := rows.Close(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to close user search")
		return
	}
	items := []shareUser{}
	for _, item := range candidates {
		target, err := a.userByID(r.Context(), item.ID)
		if err == nil && userHasPermission(target, PermissionMailAccess) && userHasPermission(target, PermissionMailRead) {
			items = append(items, item)
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleListMailboxShares(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	args := []any{user.ID}
	where := `ms.owner_user_id=? AND ms.owner_user_id=mb.user_id`
	if mailboxID != "" {
		if _, err := a.mailboxForCurrentUserWithID(r, mailboxID); err != nil {
			respondError(w, http.StatusNotFound, "mailbox not found")
			return
		}
		where += ` AND ms.mailbox_id=?`
		args = append(args, mailboxID)
	}
	a.respondMailboxShareList(w, r, where, args...)
}

func (a *App) handleListReceivedMailboxShares(w http.ResponseWriter, r *http.Request) {
	a.respondMailboxShareList(w, r, `ms.shared_with_user_id=? AND ms.owner_user_id=mb.user_id`, currentUser(r).ID)
}

func (a *App) respondMailboxShareList(w http.ResponseWriter, r *http.Request, where string, args ...any) {
	rows, err := a.db.QueryContext(r.Context(), mailboxShareSelect+` WHERE `+where+` ORDER BY ms.created_at DESC`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox shares")
		return
	}
	items := []MailboxShare{}
	for rows.Next() {
		item, err := scanMailboxShare(rows)
		if err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "failed to scan mailbox shares")
			return
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		respondError(w, http.StatusInternalServerError, "failed to scan mailbox shares")
		return
	}
	if err := rows.Close(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to close mailbox shares")
		return
	}
	for i := range items {
		items[i].Status = mailboxShareStatus(&items[i], a.now().UTC())
		if err := a.loadMailboxShareScopes(r.Context(), &items[i]); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load mailbox share scope")
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateMailboxShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MailboxID        string `json:"mailboxId"`
		SharedWithUserID string `json:"sharedWithUserId"`
		mailboxShareRequest
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, strings.TrimSpace(req.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	target, err := a.userByID(r.Context(), strings.TrimSpace(req.SharedWithUserID))
	if err != nil || target.Disabled || target.ID == currentUser(r).ID || !userHasPermission(target, PermissionMailAccess) || !userHasPermission(target, PermissionMailRead) {
		badRequest(w, errors.New("请选择可使用邮件功能的站内用户"))
		return
	}
	folderIDs, labelIDs, err := a.validateMailboxShareScope(r.Context(), mb.ID, &req.mailboxShareRequest)
	if err != nil {
		badRequest(w, err)
		return
	}
	now := a.now().UTC()
	expiresAt, err := shareExpiration(now, req.ExpiresInDays, req.ExpiresAt, false)
	if err != nil {
		badRequest(w, err)
		return
	}
	allowAttachments := true
	if req.AllowAttachments != nil {
		allowAttachments = *req.AllowAttachments
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create mailbox share")
		return
	}
	defer tx.Rollback()
	stamp := now.Format(time.RFC3339Nano)
	id := newID("shr")
	var existingID string
	var revokedAt, leftAt sql.NullString
	err = tx.QueryRowContext(r.Context(), `SELECT id,revoked_at,left_at FROM mailbox_shares WHERE mailbox_id=? AND shared_with_user_id=?`, mb.ID, target.ID).Scan(&existingID, &revokedAt, &leftAt)
	if err == nil {
		if !revokedAt.Valid && !leftAt.Valid {
			respondError(w, http.StatusConflict, "该邮箱已共享给此用户")
			return
		}
		id = existingID
		if _, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET scope=?,include_starred=?,allow_attachments=?,expires_at=?,last_accessed_at=NULL,
			revoked_at=NULL,left_at=NULL,version=version+1,updated_at=? WHERE id=?`, req.Scope, req.IncludeStarred, allowAttachments, *expiresAt, stamp, id); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to restore mailbox share")
			return
		}
		if _, err = tx.ExecContext(r.Context(), `DELETE FROM mailbox_share_folders WHERE share_id=?`, id); err == nil {
			_, err = tx.ExecContext(r.Context(), `DELETE FROM mailbox_share_labels WHERE share_id=?`, id)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO mailbox_shares(id,mailbox_id,owner_user_id,shared_with_user_id,scope,include_starred,allow_attachments,version,expires_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, mb.ID, currentUser(r).ID, target.ID, req.Scope, req.IncludeStarred, allowAttachments, 1, *expiresAt, stamp, stamp)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create mailbox share")
		return
	}
	if err := saveMailboxShareScopes(r.Context(), tx, id, folderIDs, labelIDs); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save mailbox share scope")
		return
	}
	a.addMailboxShareAudit(r.Context(), tx, id, mb.ID, currentUser(r).ID, "created", map[string]any{"scope": req.Scope})
	a.addNotification(r.Context(), tx, target.ID, "mailbox_share_created", "收到邮箱共享", currentUser(r).Email+" 向你共享了 "+mb.Address, map[string]any{"shareId": id, "mailboxId": mb.ID}, "")
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create mailbox share")
		return
	}
	item, err := a.mailboxShareByID(r.Context(), id, currentUser(r).ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox share")
		return
	}
	respondJSON(w, http.StatusCreated, item)
}

func (a *App) handleUpdateMailboxShare(w http.ResponseWriter, r *http.Request) {
	var req mailboxShareRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	share, err := a.mailboxShareByID(r.Context(), id, user.ID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox share not found")
		return
	}
	if req.Version < 1 {
		badRequest(w, errors.New("version is required"))
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, share.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	folderIDs, labelIDs, err := a.validateMailboxShareScope(r.Context(), mb.ID, &req)
	if err != nil {
		badRequest(w, err)
		return
	}
	now := a.now().UTC()
	expiresAt, err := shareExpiration(now, req.ExpiresInDays, req.ExpiresAt, true)
	if err != nil {
		badRequest(w, err)
		return
	}
	allowAttachments := share.AllowAttachments
	if req.AllowAttachments != nil {
		allowAttachments = *req.AllowAttachments
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share")
		return
	}
	defer tx.Rollback()
	stamp := now.Format(time.RFC3339Nano)
	var result sql.Result
	if expiresAt == nil {
		result, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET scope=?,include_starred=?,allow_attachments=?,revoked_at=NULL,left_at=NULL,version=version+1,updated_at=?
			WHERE id=? AND owner_user_id=? AND mailbox_id=? AND version=?`, req.Scope, req.IncludeStarred, allowAttachments, stamp, id, user.ID, mb.ID, req.Version)
	} else {
		result, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET scope=?,include_starred=?,allow_attachments=?,expires_at=?,revoked_at=NULL,left_at=NULL,version=version+1,updated_at=?
			WHERE id=? AND owner_user_id=? AND mailbox_id=? AND version=?`, req.Scope, req.IncludeStarred, allowAttachments, *expiresAt, stamp, id, user.ID, mb.ID, req.Version)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var exists int
		_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM mailbox_shares WHERE id=? AND owner_user_id=?`, id, user.ID).Scan(&exists)
		if exists > 0 {
			respondError(w, http.StatusConflict, "共享设置已被其他页面修改，请刷新后重试")
		} else {
			respondError(w, http.StatusNotFound, "mailbox share not found")
		}
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM mailbox_share_folders WHERE share_id=?`, id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share folders")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM mailbox_share_labels WHERE share_id=?`, id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share labels")
		return
	}
	if err := saveMailboxShareScopes(r.Context(), tx, id, folderIDs, labelIDs); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share scope")
		return
	}
	event := "updated"
	if share.Status == "expired" || share.Status == "revoked" || share.Status == "left" {
		event = "renewed"
	}
	a.addMailboxShareAudit(r.Context(), tx, id, mb.ID, user.ID, event, map[string]any{"scope": req.Scope, "allowAttachments": allowAttachments})
	a.addNotification(r.Context(), tx, share.SharedWithUserID, "mailbox_share_updated", "邮箱共享已更新", user.Email+" 更新了 "+mb.Address+" 的共享设置", map[string]any{"shareId": id, "mailboxId": mb.ID}, "")
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share")
		return
	}
	item, err := a.mailboxShareByID(r.Context(), id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox share")
		return
	}
	respondJSON(w, http.StatusOK, item)
}

func (a *App) handleDeleteMailboxShare(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	share, err := a.mailboxShareByID(r.Context(), id, user.ID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox share not found")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke mailbox share")
		return
	}
	defer tx.Rollback()
	stamp := a.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET revoked_at=?,version=version+1,updated_at=? WHERE id=? AND owner_user_id=? AND revoked_at IS NULL`, stamp, stamp, id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke mailbox share")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		respondError(w, http.StatusConflict, "共享已撤销")
		return
	}
	a.addMailboxShareAudit(r.Context(), tx, id, share.MailboxID, user.ID, "revoked", nil)
	a.addNotification(r.Context(), tx, share.SharedWithUserID, "mailbox_share_revoked", "邮箱共享已撤销", user.Email+" 撤销了 "+share.MailboxAddress+" 的共享", map[string]any{"shareId": id, "mailboxId": share.MailboxID}, "")
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to revoke mailbox share")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleLeaveMailboxShare(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	share, err := a.mailboxShareForRecipient(r.Context(), id, user.ID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox share not found")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to leave mailbox share")
		return
	}
	defer tx.Rollback()
	stamp := a.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET left_at=?,version=version+1,updated_at=? WHERE id=? AND shared_with_user_id=? AND left_at IS NULL`, stamp, stamp, id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to leave mailbox share")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		respondError(w, http.StatusConflict, "已退出该共享")
		return
	}
	a.addMailboxShareAudit(r.Context(), tx, id, share.MailboxID, user.ID, "left", nil)
	a.addNotification(r.Context(), tx, share.OwnerUserID, "mailbox_share_left", "接收人已退出共享", user.Email+" 已退出 "+share.MailboxAddress+" 的共享", map[string]any{"shareId": id, "mailboxId": share.MailboxID}, "")
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to leave mailbox share")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMailboxShareAudit(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, err := a.mailboxShareByID(r.Context(), id, currentUser(r).ID); err != nil {
		respondError(w, http.StatusNotFound, "mailbox share not found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT e.id,e.share_id,u.email,e.event,e.details_json,e.created_at
		FROM mailbox_share_audit_events e JOIN users u ON u.id=e.actor_user_id WHERE e.share_id=? ORDER BY e.created_at DESC`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox share audit")
		return
	}
	defer rows.Close()
	items := []MailboxShareAuditEvent{}
	for rows.Next() {
		var item MailboxShareAuditEvent
		var details, created string
		if err := rows.Scan(&item.ID, &item.ShareID, &item.ActorEmail, &item.Event, &details, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan mailbox share audit")
			return
		}
		item.Details = map[string]any{}
		_ = json.Unmarshal([]byte(details), &item.Details)
		item.CreatedAt = parseTime(created)
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	a.generateExpiringShareNotifications(r.Context(), currentUser(r).ID)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,type,title,body,data_json,read_at,created_at FROM user_notifications WHERE user_id=? ORDER BY created_at DESC LIMIT 100`, currentUser(r).ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load notifications")
		return
	}
	defer rows.Close()
	items := []UserNotification{}
	for rows.Next() {
		var item UserNotification
		var data, created string
		var readAt sql.NullString
		if err := rows.Scan(&item.ID, &item.Type, &item.Title, &item.Body, &data, &readAt, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan notifications")
			return
		}
		item.Data = map[string]any{}
		_ = json.Unmarshal([]byte(data), &item.Data)
		item.CreatedAt = parseTime(created)
		if readAt.Valid {
			value := parseTime(readAt.String)
			item.ReadAt = &value
		}
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	result, err := a.db.ExecContext(r.Context(), `UPDATE user_notifications SET read_at=? WHERE id=? AND user_id=?`, a.now().UTC().Format(time.RFC3339Nano), strings.TrimSpace(chi.URLParam(r, "id")), currentUser(r).ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read notification")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "notification not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) mailboxReadAccessForRequest(r *http.Request) (*mailboxReadAccess, error) {
	return a.mailboxReadAccessWithID(r, r.URL.Query().Get("mailboxId"))
}

func (a *App) mailboxReadAccessWithID(r *http.Request, mailboxID string) (*mailboxReadAccess, error) {
	user := currentUser(r)
	if user == nil {
		return nil, errors.New("no user")
	}
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID == "" {
		mb, err := a.mailboxForUser(r.Context(), user.ID)
		if err != nil {
			return nil, err
		}
		mb.Access = "owner"
		return &mailboxReadAccess{Mailbox: mb, Owner: true, Scope: "all", AllowAttachments: true}, nil
	}
	if mb, err := a.mailboxForCurrentUserWithID(r, mailboxID); err == nil {
		mb.Access = "owner"
		return &mailboxReadAccess{Mailbox: mb, Owner: true, Scope: "all", AllowAttachments: true}, nil
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT mb.id,mb.user_id,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at,
		ms.id,ms.scope,ms.include_starred,ms.allow_attachments,u.email
		FROM mailbox_shares ms JOIN mailboxes mb ON mb.id=ms.mailbox_id JOIN domains d ON d.id=mb.domain_id JOIN users u ON u.id=mb.user_id
		WHERE ms.mailbox_id=? AND ms.shared_with_user_id=? AND ms.owner_user_id=mb.user_id
		AND ms.revoked_at IS NULL AND ms.left_at IS NULL AND (ms.expires_at IS NULL OR ms.expires_at>?) AND mb.status='active' AND d.status='active'`, mailboxID, user.ID, a.now().UTC().Format(time.RFC3339Nano))
	var mb Mailbox
	var created, shareID, scope, sharedBy string
	var includeStarred, allowAttachments bool
	if err := row.Scan(&mb.ID, &mb.UserID, &mb.DomainID, &mb.LocalPart, &mb.Address, &mb.DisplayName, &mb.QuotaMB, &mb.Status, &created,
		&shareID, &scope, &includeStarred, &allowAttachments, &sharedBy); err != nil {
		return nil, err
	}
	mb.CreatedAt = parseTime(created)
	mb.Access = "read"
	mb.SharedBy = sharedBy
	mb.ShareScope = scope
	mb.ShareIncludesStarred = includeStarred
	mb.ShareAllowsAttachments = allowAttachments
	a.recordMailboxShareAccess(r.Context(), shareID, mb.ID, user.ID)
	return &mailboxReadAccess{Mailbox: &mb, ShareID: shareID, Scope: scope, IncludeStarred: includeStarred, AllowAttachments: allowAttachments}, nil
}

func (a *App) mailboxShareCanReadFolder(ctx context.Context, access *mailboxReadAccess, folderID string) bool {
	if access == nil {
		return false
	}
	if access.Owner || access.Scope == "all" {
		return true
	}
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM mailbox_share_folders WHERE share_id=? AND folder_id=?`, access.ShareID, folderID).Scan(&count)
	return count > 0
}

func (a *App) mailboxShareCanReadLabel(ctx context.Context, access *mailboxReadAccess, labelID string) bool {
	if access == nil {
		return false
	}
	if access.Owner || access.Scope == "all" {
		return true
	}
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM mailbox_share_labels WHERE share_id=? AND label_id=?`, access.ShareID, labelID).Scan(&count)
	return count > 0
}

func (a *App) mailboxShareCanReadMessage(ctx context.Context, access *mailboxReadAccess, messageID string) bool {
	if access == nil {
		return false
	}
	if access.Owner || access.Scope == "all" {
		var count int
		_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages WHERE id=? AND mailbox_id=?`, messageID, access.Mailbox.ID).Scan(&count)
		return count > 0
	}
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages m
		WHERE m.id=? AND m.mailbox_id=? AND (
			EXISTS (SELECT 1 FROM mailbox_share_folders msf WHERE msf.share_id=? AND msf.folder_id=m.folder_id)
			OR EXISTS (SELECT 1 FROM message_labels ml JOIN mailbox_share_labels msl ON msl.label_id=ml.label_id WHERE ml.message_id=m.id AND msl.share_id=?)
			OR (?=1 AND m.is_starred=1)
		)`, messageID, access.Mailbox.ID, access.ShareID, access.ShareID, access.IncludeStarred).Scan(&count)
	return count > 0
}

func (a *App) loadMessageForReadRequest(r *http.Request, id string, includeBody bool) (*MailMessage, error) {
	var mailboxID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT COALESCE(mailbox_id,'') FROM messages WHERE id=?`, id).Scan(&mailboxID); err != nil || mailboxID == "" {
		return nil, sql.ErrNoRows
	}
	access, err := a.mailboxReadAccessWithID(r, mailboxID)
	if err != nil || !a.mailboxShareCanReadMessage(r.Context(), access, id) {
		return nil, sql.ErrNoRows
	}
	msg, err := a.messageByID(r.Context(), id, includeBody)
	if err != nil {
		return nil, err
	}
	if !access.Owner && access.Scope == "custom" {
		visible := msg.Labels[:0]
		for _, label := range msg.Labels {
			if a.mailboxShareCanReadLabel(r.Context(), access, label.ID) {
				visible = append(visible, label)
			}
		}
		msg.Labels = visible
	}
	return msg, nil
}

func (a *App) mailboxShareByID(ctx context.Context, id, ownerUserID string) (*MailboxShare, error) {
	row := a.db.QueryRowContext(ctx, mailboxShareSelect+` WHERE ms.id=? AND ms.owner_user_id=? AND ms.owner_user_id=mb.user_id`, id, ownerUserID)
	item, err := scanMailboxShare(row)
	if err != nil {
		return nil, err
	}
	item.Status = mailboxShareStatus(item, a.now().UTC())
	if err := a.loadMailboxShareScopes(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

func (a *App) mailboxShareForRecipient(ctx context.Context, id, recipientUserID string) (*MailboxShare, error) {
	row := a.db.QueryRowContext(ctx, mailboxShareSelect+` WHERE ms.id=? AND ms.shared_with_user_id=? AND ms.owner_user_id=mb.user_id`, id, recipientUserID)
	item, err := scanMailboxShare(row)
	if err != nil {
		return nil, err
	}
	item.Status = mailboxShareStatus(item, a.now().UTC())
	return item, nil
}

type rowScanner interface{ Scan(...any) error }

func scanMailboxShare(row rowScanner) (*MailboxShare, error) {
	var item MailboxShare
	var expiresAt, lastAccessedAt, revokedAt, leftAt sql.NullString
	var createdAt string
	if err := row.Scan(&item.ID, &item.MailboxID, &item.MailboxAddress, &item.OwnerUserID, &item.OwnerEmail, &item.OwnerName,
		&item.SharedWithUserID, &item.SharedWithEmail, &item.SharedWithName, &item.Scope, &item.IncludeStarred, &item.AllowAttachments,
		&item.Version, &expiresAt, &lastAccessedAt, &revokedAt, &leftAt, &createdAt); err != nil {
		return nil, err
	}
	item.CreatedAt = parseTime(createdAt)
	assignNullableTime(&item.ExpiresAt, expiresAt)
	assignNullableTime(&item.LastAccessedAt, lastAccessedAt)
	assignNullableTime(&item.RevokedAt, revokedAt)
	assignNullableTime(&item.LeftAt, leftAt)
	return &item, nil
}

func assignNullableTime(target **time.Time, value sql.NullString) {
	if value.Valid && strings.TrimSpace(value.String) != "" {
		parsed := parseTime(value.String)
		*target = &parsed
	}
}

func mailboxShareStatus(item *MailboxShare, now time.Time) string {
	if item.RevokedAt != nil {
		return "revoked"
	}
	if item.LeftAt != nil {
		return "left"
	}
	if item.ExpiresAt == nil {
		return "active"
	}
	if !item.ExpiresAt.After(now) {
		return "expired"
	}
	if !item.ExpiresAt.After(now.Add(7 * 24 * time.Hour)) {
		return "expiring"
	}
	return "active"
}

func (a *App) loadMailboxShareScopes(ctx context.Context, item *MailboxShare) error {
	item.FolderIDs, item.FolderNames = []string{}, []string{}
	item.LabelIDs, item.LabelNames = []string{}, []string{}
	rows, err := a.db.QueryContext(ctx, `SELECT f.id,f.name FROM mailbox_share_folders msf JOIN folders f ON f.id=msf.folder_id WHERE msf.share_id=? ORDER BY f.sort_order,f.name`, item.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		item.FolderIDs = append(item.FolderIDs, id)
		item.FolderNames = append(item.FolderNames, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = a.db.QueryContext(ctx, `SELECT l.id,l.name FROM mailbox_share_labels msl JOIN mail_labels l ON l.id=msl.label_id WHERE msl.share_id=? ORDER BY l.name`, item.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		item.LabelIDs = append(item.LabelIDs, id)
		item.LabelNames = append(item.LabelNames, name)
	}
	return rows.Err()
}

func (a *App) validateMailboxShareScope(ctx context.Context, mailboxID string, req *mailboxShareRequest) ([]string, []string, error) {
	req.Scope = strings.ToLower(strings.TrimSpace(req.Scope))
	if req.Scope != "all" && req.Scope != "custom" {
		return nil, nil, errors.New("invalid share scope")
	}
	folderIDs, labelIDs := uniqueNonEmpty(req.FolderIDs), uniqueNonEmpty(req.LabelIDs)
	if len(folderIDs) > 100 || len(labelIDs) > 100 {
		return nil, nil, errors.New("共享范围过大")
	}
	if req.Scope == "custom" && len(folderIDs) == 0 && len(labelIDs) == 0 && !req.IncludeStarred {
		return nil, nil, errors.New("请至少选择一个文件夹、标签或星标邮件")
	}
	if req.Scope == "all" {
		folderIDs, labelIDs, req.IncludeStarred = nil, nil, false
	}
	for _, id := range folderIDs {
		if !a.folderBelongsToMailbox(ctx, id, mailboxID) {
			return nil, nil, errors.New("共享文件夹无效")
		}
	}
	for _, id := range labelIDs {
		if !a.labelBelongsToMailbox(ctx, id, mailboxID) {
			return nil, nil, errors.New("共享标签无效")
		}
	}
	return folderIDs, labelIDs, nil
}

func shareExpiration(now time.Time, days *int, raw *string, keepAllowed bool) (*any, error) {
	if days != nil && raw != nil {
		return nil, errors.New("只能选择一种有效期")
	}
	if days == nil && raw == nil {
		if keepAllowed {
			return nil, nil
		}
		var permanent any
		return &permanent, nil
	}
	if days != nil {
		if *days != 0 && *days != 7 && *days != 30 && *days != 90 {
			return nil, errors.New("invalid expiration")
		}
		var value any
		if *days > 0 {
			value = now.Add(time.Duration(*days) * 24 * time.Hour).Format(time.RFC3339Nano)
		}
		return &value, nil
	}
	value := strings.TrimSpace(*raw)
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339Nano, value)
	}
	if err != nil || !parsed.After(now) {
		return nil, errors.New("自定义到期时间必须晚于当前时间")
	}
	var result any = parsed.UTC().Format(time.RFC3339Nano)
	return &result, nil
}

func saveMailboxShareScopes(ctx context.Context, tx *sql.Tx, shareID string, folderIDs, labelIDs []string) error {
	for _, folderID := range folderIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_share_folders(share_id,folder_id) VALUES(?,?)`, shareID, folderID); err != nil {
			return err
		}
	}
	for _, labelID := range labelIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_share_labels(share_id,label_id) VALUES(?,?)`, shareID, labelID); err != nil {
			return err
		}
	}
	return nil
}

// addMailboxShareAudit records who touched a shared mailbox. It cannot return an
// error without changing every caller, so a failure is at least logged: silently
// losing an audit row would leave cross-account access untraceable.
func (a *App) addMailboxShareAudit(ctx context.Context, executor externalSchemaExecutor, shareID, mailboxID, actorUserID, event string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	encoded, _ := json.Marshal(details)
	if _, err := executor.ExecContext(ctx, `INSERT INTO mailbox_share_audit_events(id,share_id,mailbox_id,actor_user_id,event,details_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		newID("sha"), shareID, mailboxID, actorUserID, event, string(encoded), a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		a.log.Warn("failed to record mailbox share audit event", "error", err, "share_id", shareID, "event", event)
	}
}

func (a *App) addNotification(ctx context.Context, executor externalSchemaExecutor, userID, kind, title, body string, data map[string]any, dedupeKey string) {
	if data == nil {
		data = map[string]any{}
	}
	encoded, _ := json.Marshal(data)
	var dedupe any
	if dedupeKey != "" {
		dedupe = dedupeKey
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO user_notifications(id,user_id,type,title,body,data_json,dedupe_key,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		newID("ntf"), userID, kind, title, body, string(encoded), dedupe, a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		// Duplicate dedupe_key collisions are expected and benign.
		if !isUniqueViolation(err) {
			a.log.Warn("failed to record user notification", "error", err, "user_id", userID, "type", kind)
		}
	}
}

func (a *App) recordMailboxShareAccess(ctx context.Context, shareID, mailboxID, actorUserID string) {
	now := a.now().UTC()
	result, err := a.db.ExecContext(ctx, `UPDATE mailbox_shares SET last_accessed_at=? WHERE id=? AND (last_accessed_at IS NULL OR last_accessed_at<?)`,
		now.Format(time.RFC3339Nano), shareID, now.Add(-time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		a.log.Warn("failed to record mailbox share access", "error", err, "share_id", shareID)
		return
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		a.addMailboxShareAudit(ctx, a.db, shareID, mailboxID, actorUserID, "accessed", nil)
	}
}

func (a *App) generateExpiringShareNotifications(ctx context.Context, userID string) {
	now := a.now().UTC()
	rows, err := a.db.QueryContext(ctx, `SELECT ms.id,ms.mailbox_id,mb.address,ms.owner_user_id,ms.shared_with_user_id,ms.expires_at
		FROM mailbox_shares ms JOIN mailboxes mb ON mb.id=ms.mailbox_id
		WHERE (ms.owner_user_id=? OR ms.shared_with_user_id=?) AND ms.revoked_at IS NULL AND ms.left_at IS NULL
		AND ms.expires_at>? AND ms.expires_at<=?`, userID, userID, now.Format(time.RFC3339Nano), now.Add(7*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	type expiring struct{ id, mailboxID, address, ownerID, recipientID, expiresAt string }
	items := []expiring{}
	for rows.Next() {
		var item expiring
		if rows.Scan(&item.id, &item.mailboxID, &item.address, &item.ownerID, &item.recipientID, &item.expiresAt) == nil {
			items = append(items, item)
		}
	}
	rows.Close()
	for _, item := range items {
		a.addNotification(ctx, a.db, userID, "mailbox_share_expiring", "邮箱共享即将到期", item.address+" 的共享将在 7 天内到期", map[string]any{"shareId": item.id, "mailboxId": item.mailboxID, "expiresAt": item.expiresAt}, "share-expiring:"+item.id+":"+item.expiresAt+":"+userID)
	}
}

func (a *App) folderBelongsToMailbox(ctx context.Context, folderID, mailboxID string) bool {
	var count int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM folders WHERE id=? AND mailbox_id=?`, folderID, mailboxID).Scan(&count)
	return count > 0
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}
