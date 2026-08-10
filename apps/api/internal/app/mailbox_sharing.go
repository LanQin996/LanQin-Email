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

type mailboxReadAccess struct {
	Mailbox        *Mailbox
	Owner          bool
	ShareID        string
	Scope          string
	IncludeStarred bool
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
	rows, err := a.db.QueryContext(r.Context(), `SELECT ms.id,ms.mailbox_id,mb.address,ms.shared_with_user_id,u.email,u.display_name,
		ms.scope,ms.include_starred,ms.expires_at,ms.created_at
		FROM mailbox_shares ms JOIN mailboxes mb ON mb.id=ms.mailbox_id JOIN users u ON u.id=ms.shared_with_user_id
		WHERE `+where+` ORDER BY ms.created_at DESC`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox shares")
		return
	}
	items := []MailboxShare{}
	for rows.Next() {
		var item MailboxShare
		var includeStarred bool
		var expiresAt sql.NullString
		var createdAt string
		if err := rows.Scan(&item.ID, &item.MailboxID, &item.MailboxAddress, &item.SharedWithUserID, &item.SharedWithEmail, &item.SharedWithName,
			&item.Scope, &includeStarred, &expiresAt, &createdAt); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "failed to scan mailbox shares")
			return
		}
		item.IncludeStarred = includeStarred
		item.CreatedAt = parseTime(createdAt)
		if expiresAt.Valid && strings.TrimSpace(expiresAt.String) != "" {
			expires := parseTime(expiresAt.String)
			item.ExpiresAt = &expires
		}
		items = append(items, item)
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
		if err := a.loadMailboxShareScopes(r.Context(), &items[i]); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load mailbox share scope")
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateMailboxShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MailboxID        string   `json:"mailboxId"`
		SharedWithUserID string   `json:"sharedWithUserId"`
		Scope            string   `json:"scope"`
		FolderIDs        []string `json:"folderIds"`
		LabelIDs         []string `json:"labelIds"`
		IncludeStarred   bool     `json:"includeStarred"`
		ExpiresInDays    int      `json:"expiresInDays"`
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
	targetID := strings.TrimSpace(req.SharedWithUserID)
	target, err := a.userByID(r.Context(), targetID)
	if err != nil || target.Disabled || target.ID == currentUser(r).ID || !userHasPermission(target, PermissionMailAccess) || !userHasPermission(target, PermissionMailRead) {
		badRequest(w, errors.New("请选择可使用邮件功能的站内用户"))
		return
	}
	req.Scope = strings.ToLower(strings.TrimSpace(req.Scope))
	if req.Scope != "all" && req.Scope != "custom" {
		badRequest(w, errors.New("invalid share scope"))
		return
	}
	folderIDs := uniqueNonEmpty(req.FolderIDs)
	labelIDs := uniqueNonEmpty(req.LabelIDs)
	if len(folderIDs) > 100 || len(labelIDs) > 100 {
		badRequest(w, errors.New("共享范围过大"))
		return
	}
	if req.Scope == "custom" && len(folderIDs) == 0 && len(labelIDs) == 0 && !req.IncludeStarred {
		badRequest(w, errors.New("请至少选择一个文件夹、标签或星标邮件"))
		return
	}
	if req.Scope == "all" {
		folderIDs, labelIDs, req.IncludeStarred = nil, nil, false
	}
	for _, id := range folderIDs {
		if !a.folderBelongsToMailbox(r.Context(), id, mb.ID) {
			badRequest(w, errors.New("共享文件夹无效"))
			return
		}
	}
	for _, id := range labelIDs {
		if !a.labelBelongsToMailbox(r.Context(), id, mb.ID) {
			badRequest(w, errors.New("共享标签无效"))
			return
		}
	}
	if req.ExpiresInDays != 0 && req.ExpiresInDays != 7 && req.ExpiresInDays != 30 && req.ExpiresInDays != 90 {
		badRequest(w, errors.New("invalid expiration"))
		return
	}
	now := a.now().UTC()
	var expiresAt any
	if req.ExpiresInDays > 0 {
		expiresAt = now.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).Format(time.RFC3339Nano)
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create mailbox share")
		return
	}
	defer tx.Rollback()
	id := newID("shr")
	stamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO mailbox_shares(id,mailbox_id,owner_user_id,shared_with_user_id,scope,include_starred,expires_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, id, mb.ID, currentUser(r).ID, target.ID, req.Scope, req.IncludeStarred, expiresAt, stamp, stamp); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			respondError(w, http.StatusConflict, "该邮箱已共享给此用户")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create mailbox share")
		return
	}
	for _, folderID := range folderIDs {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO mailbox_share_folders(share_id,folder_id) VALUES(?,?)`, id, folderID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save mailbox share folders")
			return
		}
	}
	for _, labelID := range labelIDs {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO mailbox_share_labels(share_id,label_id) VALUES(?,?)`, id, labelID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save mailbox share labels")
			return
		}
	}
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
	var req struct {
		Scope          string   `json:"scope"`
		FolderIDs      []string `json:"folderIds"`
		LabelIDs       []string `json:"labelIds"`
		IncludeStarred bool     `json:"includeStarred"`
		ExpiresInDays  *int     `json:"expiresInDays"`
	}
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
	mb, err := a.mailboxForCurrentUserWithID(r, share.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	req.Scope = strings.ToLower(strings.TrimSpace(req.Scope))
	if req.Scope != "all" && req.Scope != "custom" {
		badRequest(w, errors.New("invalid share scope"))
		return
	}
	folderIDs := uniqueNonEmpty(req.FolderIDs)
	labelIDs := uniqueNonEmpty(req.LabelIDs)
	if len(folderIDs) > 100 || len(labelIDs) > 100 {
		badRequest(w, errors.New("共享范围过大"))
		return
	}
	if req.Scope == "custom" && len(folderIDs) == 0 && len(labelIDs) == 0 && !req.IncludeStarred {
		badRequest(w, errors.New("请至少选择一个文件夹、标签或星标邮件"))
		return
	}
	if req.Scope == "all" {
		folderIDs, labelIDs, req.IncludeStarred = nil, nil, false
	}
	for _, folderID := range folderIDs {
		if !a.folderBelongsToMailbox(r.Context(), folderID, mb.ID) {
			badRequest(w, errors.New("共享文件夹无效"))
			return
		}
	}
	for _, labelID := range labelIDs {
		if !a.labelBelongsToMailbox(r.Context(), labelID, mb.ID) {
			badRequest(w, errors.New("共享标签无效"))
			return
		}
	}
	if req.ExpiresInDays != nil && *req.ExpiresInDays != 0 && *req.ExpiresInDays != 7 && *req.ExpiresInDays != 30 && *req.ExpiresInDays != 90 {
		badRequest(w, errors.New("invalid expiration"))
		return
	}

	now := a.now().UTC()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share")
		return
	}
	defer tx.Rollback()
	stamp := now.Format(time.RFC3339Nano)
	var result sql.Result
	if req.ExpiresInDays == nil {
		result, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET scope=?,include_starred=?,updated_at=?
			WHERE id=? AND owner_user_id=? AND mailbox_id=?`, req.Scope, req.IncludeStarred, stamp, id, user.ID, mb.ID)
	} else {
		var expiresAt any
		if *req.ExpiresInDays > 0 {
			expiresAt = now.Add(time.Duration(*req.ExpiresInDays) * 24 * time.Hour).Format(time.RFC3339Nano)
		}
		result, err = tx.ExecContext(r.Context(), `UPDATE mailbox_shares SET scope=?,include_starred=?,expires_at=?,updated_at=?
			WHERE id=? AND owner_user_id=? AND mailbox_id=?`, req.Scope, req.IncludeStarred, expiresAt, stamp, id, user.ID, mb.ID)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox share")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox share not found")
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
	for _, folderID := range folderIDs {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO mailbox_share_folders(share_id,folder_id) VALUES(?,?)`, id, folderID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update mailbox share folders")
			return
		}
	}
	for _, labelID := range labelIDs {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO mailbox_share_labels(share_id,label_id) VALUES(?,?)`, id, labelID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update mailbox share labels")
			return
		}
	}
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
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM mailbox_shares
		WHERE id=? AND owner_user_id=? AND EXISTS (SELECT 1 FROM mailboxes mb WHERE mb.id=mailbox_shares.mailbox_id AND mb.user_id=?)`, id, user.ID, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete mailbox share")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox share not found")
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
		return &mailboxReadAccess{Mailbox: mb, Owner: true, Scope: "all"}, nil
	}
	if mb, err := a.mailboxForCurrentUserWithID(r, mailboxID); err == nil {
		mb.Access = "owner"
		return &mailboxReadAccess{Mailbox: mb, Owner: true, Scope: "all"}, nil
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT mb.id,mb.user_id,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at,
		ms.id,ms.scope,ms.include_starred,u.email
		FROM mailbox_shares ms JOIN mailboxes mb ON mb.id=ms.mailbox_id JOIN domains d ON d.id=mb.domain_id JOIN users u ON u.id=mb.user_id
		WHERE ms.mailbox_id=? AND ms.shared_with_user_id=? AND ms.owner_user_id=mb.user_id
		AND (ms.expires_at IS NULL OR ms.expires_at>?) AND mb.status='active' AND d.status='active'`, mailboxID, user.ID, a.now().UTC().Format(time.RFC3339Nano))
	var mb Mailbox
	var created, shareID, scope, sharedBy string
	var includeStarred bool
	if err := row.Scan(&mb.ID, &mb.UserID, &mb.DomainID, &mb.LocalPart, &mb.Address, &mb.DisplayName, &mb.QuotaMB, &mb.Status, &created,
		&shareID, &scope, &includeStarred, &sharedBy); err != nil {
		return nil, err
	}
	mb.CreatedAt = parseTime(created)
	mb.Access = "read"
	mb.SharedBy = sharedBy
	mb.ShareScope = scope
	mb.ShareIncludesStarred = includeStarred
	return &mailboxReadAccess{Mailbox: &mb, ShareID: shareID, Scope: scope, IncludeStarred: includeStarred}, nil
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
	row := a.db.QueryRowContext(ctx, `SELECT ms.id,ms.mailbox_id,mb.address,ms.shared_with_user_id,u.email,u.display_name,
		ms.scope,ms.include_starred,ms.expires_at,ms.created_at
		FROM mailbox_shares ms JOIN mailboxes mb ON mb.id=ms.mailbox_id JOIN users u ON u.id=ms.shared_with_user_id
		WHERE ms.id=? AND ms.owner_user_id=? AND ms.owner_user_id=mb.user_id`, id, ownerUserID)
	var item MailboxShare
	var expiresAt sql.NullString
	var createdAt string
	if err := row.Scan(&item.ID, &item.MailboxID, &item.MailboxAddress, &item.SharedWithUserID, &item.SharedWithEmail, &item.SharedWithName,
		&item.Scope, &item.IncludeStarred, &expiresAt, &createdAt); err != nil {
		return nil, err
	}
	item.CreatedAt = parseTime(createdAt)
	if expiresAt.Valid && strings.TrimSpace(expiresAt.String) != "" {
		expires := parseTime(expiresAt.String)
		item.ExpiresAt = &expires
	}
	if err := a.loadMailboxShareScopes(ctx, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *App) loadMailboxShareScopes(ctx context.Context, item *MailboxShare) error {
	item.FolderIDs = []string{}
	item.LabelIDs = []string{}
	rows, err := a.db.QueryContext(ctx, `SELECT folder_id FROM mailbox_share_folders WHERE share_id=? ORDER BY folder_id`, item.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		item.FolderIDs = append(item.FolderIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = a.db.QueryContext(ctx, `SELECT label_id FROM mailbox_share_labels WHERE share_id=? ORDER BY label_id`, item.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		item.LabelIDs = append(item.LabelIDs, id)
	}
	return rows.Err()
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
