package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

func (a *App) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Users           int64 `json:"users"`
		ActiveUsers     int64 `json:"activeUsers"`
		Domains         int64 `json:"domains"`
		Mailboxes       int64 `json:"mailboxes"`
		ActiveMailboxes int64 `json:"activeMailboxes"`
		Aliases         int64 `json:"aliases"`
		Messages        int64 `json:"messages"`
		UnreadMessages  int64 `json:"unreadMessages"`
		StorageBytes    int64 `json:"storageBytes"`
	}
	queries := []struct {
		q    string
		dest *int64
	}{
		{`SELECT COUNT(*) FROM users`, &out.Users},
		{`SELECT COUNT(*) FROM users WHERE disabled=0`, &out.ActiveUsers},
		{`SELECT COUNT(*) FROM domains`, &out.Domains},
		{`SELECT COUNT(*) FROM mailboxes`, &out.Mailboxes},
		{`SELECT COUNT(*) FROM mailboxes WHERE status='active'`, &out.ActiveMailboxes},
		{`SELECT COUNT(*) FROM aliases`, &out.Aliases},
		{`SELECT COUNT(*) FROM messages`, &out.Messages},
		{`SELECT COUNT(*) FROM messages WHERE is_read=0`, &out.UnreadMessages},
		{`SELECT COALESCE(SUM(size_bytes),0) FROM messages`, &out.StorageBytes},
	}
	for _, item := range queries {
		if err := a.db.QueryRowContext(r.Context(), item.q).Scan(item.dest); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load overview")
			return
		}
	}
	respondJSON(w, http.StatusOK, out)
}

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT u.id,u.email,u.display_name,u.role,u.disabled,u.two_factor_enabled,u.created_at,COUNT(mb.id),COALESCE(GROUP_CONCAT(mb.address), '')
		FROM users u LEFT JOIN mailboxes mb ON mb.user_id=u.id
		GROUP BY u.id,u.email,u.display_name,u.role,u.disabled,u.two_factor_enabled,u.created_at
		ORDER BY u.created_at DESC`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	defer rows.Close()
	items := []AdminUser{}
	for rows.Next() {
		var item AdminUser
		var disabled, twoFactorEnabled int
		var created, mailboxCSV string
		if err := rows.Scan(&item.ID, &item.Email, &item.DisplayName, &item.Role, &disabled, &twoFactorEnabled, &created, &item.MailboxCount, &mailboxCSV); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan users")
			return
		}
		item.Disabled = intBool(disabled)
		item.TwoFactorEnabled = intBool(twoFactorEnabled)
		item.CreatedAt = parseTime(created)
		item.Mailboxes = splitCSV(mailboxCSV)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	if err := rows.Close(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	for i := range items {
		if err := a.attachUserAuthorization(r.Context(), &items[i].User); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load user permissions")
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email              string   `json:"email"`
		DisplayName        string   `json:"displayName"`
		Role               string   `json:"role"`
		Password           string   `json:"password"`
		Disabled           bool     `json:"disabled"`
		PermissionGroupIDs []string `json:"permissionGroupIds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	actor := currentUser(r)
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		badRequest(w, errors.New("invalid email"))
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = email
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		badRequest(w, errors.New("invalid role"))
		return
	}
	if role == "admin" && (actor == nil || actor.Role != "admin") {
		respondError(w, http.StatusForbidden, "only administrators can create administrator users")
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("password must be at least 8 characters"))
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	id := newID("usr")
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, id, email, displayName, role, string(passwordHash), boolInt(req.Disabled), now, now); err != nil {
		badRequest(w, err)
		return
	}
	permissionGroupIDs := req.PermissionGroupIDs
	if role == "admin" {
		permissionGroupIDs = nil
	}
	if err := a.setUserPermissionGroups(r.Context(), tx, id, permissionGroupIDs, actor); err != nil {
		badRequest(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	user, err := a.adminUserByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	respondJSON(w, http.StatusCreated, user)
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current := currentUser(r)
	var req struct {
		DisplayName        string    `json:"displayName"`
		Role               string    `json:"role"`
		Disabled           *bool     `json:"disabled"`
		PermissionGroupIDs *[]string `json:"permissionGroupIds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		badRequest(w, errors.New("displayName is required"))
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		badRequest(w, errors.New("invalid role"))
		return
	}
	existing, err := a.userByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	if current == nil || (current.Role != "admin" && (existing.Role == "admin" || role == "admin")) {
		respondError(w, http.StatusForbidden, "only administrators can modify administrator users")
		return
	}
	disabled := existing.Disabled
	if req.Disabled != nil {
		disabled = *req.Disabled
	}
	if a.isDefaultAdminUser(existing) && (role != "admin" || disabled) {
		badRequest(w, errors.New("default administrator must remain an active super administrator"))
		return
	}
	if err := a.ensureAdminRemains(r.Context(), id, role, disabled); err != nil {
		badRequest(w, err)
		return
	}
	shouldUpdatePermissionGroups := role == "admin" || existing.Role == "admin" || req.PermissionGroupIDs != nil
	var permissionGroupIDs []string
	if role == "user" {
		if req.PermissionGroupIDs != nil {
			permissionGroupIDs = *req.PermissionGroupIDs
		} else if existing.Role == "user" {
			for _, groupID := range existing.PermissionGroupIDs {
				if isAssignablePermissionGroupID(groupID) {
					permissionGroupIDs = append(permissionGroupIDs, groupID)
				}
			}
		}
	}
	if current != nil && current.ID == id {
		next := *existing
		next.Role = role
		next.Disabled = disabled
		if shouldUpdatePermissionGroups {
			if role == "admin" {
				next.Permissions = allPermissionKeys()
			} else {
				permissions, err := a.effectivePermissionsForUserGroups(r.Context(), nil, permissionGroupIDs)
				if err != nil {
					badRequest(w, err)
					return
				}
				next.Permissions = permissions
			}
		}
		if next.Disabled || !userHasAdminAccess(&next) {
			badRequest(w, errors.New("cannot remove your own admin access"))
			return
		}
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `UPDATE users SET display_name=?, role=?, disabled=?, updated_at=? WHERE id=?`,
		displayName, role, boolInt(disabled), a.now().UTC().Format(time.RFC3339Nano), id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	if shouldUpdatePermissionGroups {
		if err := a.setUserPermissionGroups(r.Context(), tx, id, permissionGroupIDs, current); err != nil {
			badRequest(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	user, err := a.adminUserByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	respondJSON(w, http.StatusOK, user)
}

func (a *App) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if target, err := a.userByID(r.Context(), id); err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	} else if target.Role == "admin" {
		current := currentUser(r)
		if current == nil || current.Role != "admin" {
			respondError(w, http.StatusForbidden, "only administrators can reset administrator passwords")
			return
		}
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("password must be at least 8 characters"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, string(hash), now, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE mailboxes SET password_hash=?, updated_at=? WHERE user_id=?`, string(hash), now, id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox passwords")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save password")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current := currentUser(r)
	if current != nil && current.ID == id {
		badRequest(w, errors.New("cannot delete your own user"))
		return
	}
	if target, err := a.userByID(r.Context(), id); err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	} else if a.isDefaultAdminUser(target) {
		badRequest(w, errors.New("default administrator cannot be deleted"))
		return
	} else if target.Role == "admin" && (current == nil || current.Role != "admin") {
		respondError(w, http.StatusForbidden, "only administrators can delete administrator users")
		return
	}
	if err := a.ensureAdminRemains(r.Context(), id, "user", true); err != nil {
		badRequest(w, err)
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleListDomains(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,status,dkim_selector,dkim_public_key,dns_status,dns_checked_at,created_at FROM domains ORDER BY name`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list domains")
		return
	}
	defer rows.Close()
	items := []Domain{}
	for rows.Next() {
		var d Domain
		var checked sql.NullString
		var created string
		if err := rows.Scan(&d.ID, &d.Name, &d.Status, &d.DKIMSelector, &d.DKIMPublicKey, &d.DNSStatus, &checked, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan domains")
			return
		}
		d.DNSCheckedAt = nullableTime(checked)
		d.CreatedAt = parseTime(created)
		items = append(items, d)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	id, err := a.createDomainTx(r.Context(), nil, req.Name)
	if err != nil {
		badRequest(w, err)
		return
	}
	d, err := a.domainByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load domain")
		return
	}
	respondJSON(w, http.StatusCreated, d)
}

func (a *App) handleUpdateDomain(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "active" && status != "disabled" {
		badRequest(w, errors.New("invalid status"))
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE domains SET status=?, updated_at=? WHERE id=?`,
		status, a.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update domain")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	d, err := a.domainByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load domain")
		return
	}
	respondJSON(w, http.StatusOK, d)
}

func (a *App) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var count int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailboxes WHERE domain_id=?`, id).Scan(&count); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check domain")
		return
	}
	if count > 0 {
		badRequest(w, errors.New("domain still has mailboxes"))
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM domains WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete domain")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleListMailboxes(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT mb.id,mb.user_id,u.email,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at
		FROM mailboxes mb JOIN users u ON u.id=mb.user_id ORDER BY mb.address`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list mailboxes")
		return
	}
	defer rows.Close()
	items := []Mailbox{}
	for rows.Next() {
		var m Mailbox
		var created string
		if err := rows.Scan(&m.ID, &m.UserID, &m.UserEmail, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan mailboxes")
			return
		}
		m.CreatedAt = parseTime(created)
		items = append(items, m)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateMailbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DomainID    string `json:"domainId"`
		LocalPart   string `json:"localPart"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		QuotaMB     int    `json:"quotaMb"`
		Role        string `json:"role"`
		OwnerEmail  string `json:"ownerEmail"`
		UserID      string `json:"userId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := requireString("domainId", req.DomainID); err != nil {
		badRequest(w, err)
		return
	}
	if err := requireString("localPart", req.LocalPart); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Password) < 8 {
		badRequest(w, errors.New("password must be at least 8 characters"))
		return
	}
	role := req.Role
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		badRequest(w, errors.New("invalid role"))
		return
	}
	if role == "admin" {
		current := currentUser(r)
		if current == nil || current.Role != "admin" {
			respondError(w, http.StatusForbidden, "only administrators can create administrator users")
			return
		}
	}

	domain, err := a.domainByID(r.Context(), req.DomainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	local := normalizeLocalPart(req.LocalPart)
	address := local + "@" + domain.Name

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	now := a.now().UTC().Format(time.RFC3339Nano)
	userID := strings.TrimSpace(req.UserID)
	displayName := req.DisplayName
	if displayName == "" {
		displayName = address
	}
	if userID != "" {
		var disabled int
		if err := tx.QueryRowContext(r.Context(), `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				respondError(w, http.StatusNotFound, "owner user not found")
			} else {
				respondError(w, http.StatusInternalServerError, "failed to load owner user")
			}
			return
		}
		if intBool(disabled) {
			badRequest(w, errors.New("owner user is disabled"))
			return
		}
	} else {
		ownerEmail := normalizeEmail(req.OwnerEmail)
		if ownerEmail == "" {
			ownerEmail = address
		}
		if !strings.Contains(ownerEmail, "@") {
			badRequest(w, errors.New("invalid owner email"))
			return
		}
		err = tx.QueryRowContext(r.Context(), `SELECT id FROM users WHERE email=? AND disabled=0`, ownerEmail).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			passwordHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "failed to hash password")
				return
			}
			userID = newID("usr")
			ownerDisplayName := displayName
			if !strings.EqualFold(ownerEmail, address) {
				ownerDisplayName = ownerEmail
			}
			_, err = tx.ExecContext(r.Context(), `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?)`, userID, ownerEmail, ownerDisplayName, role, string(passwordHash), 0, now, now)
			if err != nil {
				badRequest(w, err)
				return
			}
		} else if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to load owner user")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to prepare owner user")
		return
	}

	mailboxID, err := a.createMailbox(r.Context(), userID, req.DomainID, local, displayName, req.Password, req.QuotaMB, "active")
	if err != nil {
		badRequest(w, err)
		return
	}
	m, err := a.mailboxByID(r.Context(), mailboxID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	respondJSON(w, http.StatusCreated, m)
}

func (a *App) handleUpdateMailbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		DisplayName string `json:"displayName"`
		QuotaMB     int    `json:"quotaMb"`
		Status      string `json:"status"`
		UserID      string `json:"userId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		badRequest(w, errors.New("displayName is required"))
		return
	}
	if req.QuotaMB <= 0 {
		req.QuotaMB = 1024
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		badRequest(w, errors.New("invalid status"))
		return
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		badRequest(w, errors.New("userId is required"))
		return
	}
	var disabled int
	if err := a.db.QueryRowContext(r.Context(), `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "owner user not found")
		} else {
			respondError(w, http.StatusInternalServerError, "failed to load owner user")
		}
		return
	}
	if intBool(disabled) {
		badRequest(w, errors.New("owner user is disabled"))
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE mailboxes SET user_id=?,display_name=?,quota_mb=?,status=?,updated_at=? WHERE id=?`,
		userID, displayName, req.QuotaMB, status, a.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mailbox")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	m, err := a.mailboxByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	respondJSON(w, http.StatusOK, m)
}

func (a *App) handleDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current := currentUser(r)
	var owner string
	if err := a.db.QueryRowContext(r.Context(), `SELECT user_id FROM mailboxes WHERE id=?`, id).Scan(&owner); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	var count int
	if current != nil && owner == current.ID {
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailboxes WHERE user_id=?`, owner).Scan(&count); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to check mailbox")
			return
		}
		if count <= 1 {
			badRequest(w, errors.New("cannot delete your last mailbox"))
			return
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id FROM messages WHERE mailbox_id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox messages")
		return
	}
	messageIDs := []string{}
	for rows.Next() {
		var messageID string
		if rows.Scan(&messageID) == nil {
			messageIDs = append(messageIDs, messageID)
		}
	}
	rows.Close()
	for _, messageID := range messageIDs {
		a.deleteMessage(r.Context(), messageID)
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM mailboxes WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete mailbox")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleListAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,domain_id,source,destination,enabled,created_at FROM aliases ORDER BY source`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list aliases")
		return
	}
	defer rows.Close()
	items := []Alias{}
	for rows.Next() {
		var item Alias
		var enabled int
		var created string
		if err := rows.Scan(&item.ID, &item.DomainID, &item.Source, &item.Destination, &enabled, &created); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan aliases")
			return
		}
		item.Enabled = intBool(enabled)
		item.CreatedAt = parseTime(created)
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleAdminMessages(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	if offset < 0 {
		offset = 0
	}
	limit := 50

	where := []string{"1=1"}
	args := []any{}
	if mailboxID == "unregistered" {
		where = append(where, "m.mailbox_id IS NULL")
	} else if mailboxID != "" && mailboxID != "all" {
		where = append(where, "m.mailbox_id=?")
		args = append(args, mailboxID)
	}
	if folder != "" && folder != "all" {
		if strings.EqualFold(folder, "Unregistered") {
			where = append(where, "m.mailbox_id IS NULL")
		} else {
			where = append(where, "lower(f.name)=lower(?)")
			args = append(args, folder)
		}
	}
	if q != "" {
		where = append(where, "(m.subject LIKE ? OR m.from_addr LIKE ? OR m.from_name LIKE ? OR m.to_addrs LIKE ? OR m.recipient_addr LIKE ? OR m.snippet LIKE ? OR m.body_text LIKE ? OR mb.address LIKE ? OR u.email LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like, like, like, like, like)
	}
	args = append(args, limit+1, offset)

	rows, err := a.db.QueryContext(r.Context(), `SELECT m.id,COALESCE(m.mailbox_id,''),COALESCE(mb.address,''),COALESCE(u.email,''),COALESCE(m.recipient_addr,''),COALESCE(m.folder_id,''),COALESCE(f.name,'Unregistered'),m.message_uid,m.imap_uid,m.imap_modseq,m.message_id,m.subject,m.from_addr,COALESCE(m.from_name,''),m.to_addrs,m.cc_addrs,m.bcc_addrs,m.sent_at,m.received_at,m.snippet,m.is_read,m.is_starred,m.has_attachments,m.size_bytes
		FROM messages m
		LEFT JOIN folders f ON f.id=m.folder_id
		LEFT JOIN mailboxes mb ON mb.id=m.mailbox_id
		LEFT JOIN users u ON u.id=mb.user_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY m.received_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	defer rows.Close()
	items := []MailMessage{}
	for rows.Next() {
		msg, err := scanAdminMessageSummary(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan messages")
			return
		}
		items = append(items, msg)
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.Itoa(offset + limit)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (a *App) handleAdminMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	msg, err := a.messageByID(r.Context(), id, true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	if err := a.db.QueryRowContext(r.Context(), `SELECT COALESCE(mb.address,''),COALESCE(u.email,''),COALESCE(m.recipient_addr,'')
		FROM messages m
		LEFT JOIN mailboxes mb ON mb.id=m.mailbox_id
		LEFT JOIN users u ON u.id=mb.user_id
		WHERE m.id=?`, id).Scan(&msg.MailboxAddress, &msg.OwnerEmail, &msg.RecipientAddr); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load message owner")
		return
	}
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleAdminSendAudit(w http.ResponseWriter, r *http.Request) {
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	messageID := strings.TrimSpace(r.URL.Query().Get("messageId"))
	event := strings.TrimSpace(r.URL.Query().Get("event"))
	from, err := adminAuditTimeParam(r.URL.Query().Get("from"), false)
	if err != nil {
		badRequest(w, err)
		return
	}
	to, err := adminAuditTimeParam(r.URL.Query().Get("to"), true)
	if err != nil {
		badRequest(w, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	if offset < 0 {
		offset = 0
	}
	limit := 50

	where := []string{"1=1"}
	args := []any{}
	if mailboxID != "" && mailboxID != "all" {
		where = append(where, "sae.mailbox_id=?")
		args = append(args, mailboxID)
	}
	if messageID != "" {
		where = append(where, "(sq.message_id=? OR m.message_id=? OR sae.sent_message_id=?)")
		args = append(args, messageID, messageID, messageID)
	}
	if event != "" && event != "all" {
		if !isSendAuditEvent(event) {
			badRequest(w, errors.New("invalid event"))
			return
		}
		where = append(where, "sae.event=?")
		args = append(args, event)
	}
	if from != "" {
		where = append(where, "sae.created_at>=?")
		args = append(args, from)
	}
	if to != "" {
		where = append(where, "sae.created_at<=?")
		args = append(args, to)
	}
	args = append(args, limit+1, offset)

	rows, err := a.db.QueryContext(r.Context(), `SELECT sae.id,sae.queue_id,sae.mailbox_id,COALESCE(mb.address,''),sae.sent_message_id,COALESCE(sq.message_id,m.message_id,''),sae.source,sae.event,sae.status,sae.mail_from,sae.header_from,sae.recipients_json,sae.error,sae.created_at
		FROM send_audit_events sae
		LEFT JOIN mailboxes mb ON mb.id=sae.mailbox_id
		LEFT JOIN send_queue sq ON sq.id=sae.queue_id
		LEFT JOIN messages m ON m.id=sae.sent_message_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY sae.created_at DESC, sae.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load send audit")
		return
	}
	defer rows.Close()
	items := []SendAuditEvent{}
	for rows.Next() {
		var item SendAuditEvent
		var recipientsJSON, createdAt string
		if err := rows.Scan(&item.ID, &item.QueueID, &item.MailboxID, &item.MailboxAddress, &item.SentMessageID, &item.MessageID, &item.Source, &item.Event, &item.Status, &item.MailFrom, &item.HeaderFrom, &recipientsJSON, &item.Error, &createdAt); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan send audit")
			return
		}
		item.Recipients = jsonDecodeSlice(recipientsJSON)
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load send audit")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.Itoa(offset + limit)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func adminAuditTimeParam(value string, endOfDay bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", errors.New("invalid time filter")
}

func isSendAuditEvent(event string) bool {
	switch event {
	case sendAuditAccepted, sendAuditQueued, sendAuditRetry, sendAuditDelivered, sendAuditFailed, sendAuditCanceled:
		return true
	default:
		return false
	}
}

func (a *App) handleCreateAlias(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DomainID    string `json:"domainId"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	domain, err := a.domainByID(r.Context(), req.DomainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	source := normalizeEmail(req.Source)
	if !strings.Contains(source, "@") {
		source = normalizeLocalPart(source) + "@" + domain.Name
	}
	destination := normalizeEmail(req.Destination)
	if source == "" || destination == "" || !strings.Contains(destination, "@") {
		badRequest(w, errors.New("invalid alias"))
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id := newID("als")
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO aliases(id,domain_id,source,destination,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		id, req.DomainID, source, destination, boolInt(enabled), now, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, Alias{ID: id, DomainID: req.DomainID, Source: source, Destination: destination, Enabled: enabled, CreatedAt: parseTime(now)})
}

func (a *App) handleUpdateAlias(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	var domainID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT domain_id FROM aliases WHERE id=?`, id).Scan(&domainID); err != nil {
		respondError(w, http.StatusNotFound, "alias not found")
		return
	}
	domain, err := a.domainByID(r.Context(), domainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	source := normalizeEmail(req.Source)
	if !strings.Contains(source, "@") {
		source = normalizeLocalPart(source) + "@" + domain.Name
	}
	destination := normalizeEmail(req.Destination)
	if source == "" || destination == "" || !strings.Contains(destination, "@") {
		badRequest(w, errors.New("invalid alias"))
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE aliases SET source=?,destination=?,enabled=?,updated_at=? WHERE id=?`,
		source, destination, boolInt(enabled), a.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		badRequest(w, err)
		return
	}
	respondJSON(w, http.StatusOK, Alias{ID: id, DomainID: domainID, Source: source, Destination: destination, Enabled: enabled, CreatedAt: a.now().UTC()})
}

func (a *App) handleDeleteAlias(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM aliases WHERE id=?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete alias")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "alias not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) domainByID(ctx context.Context, id string) (*Domain, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,name,status,dkim_selector,dkim_public_key,dns_status,dns_checked_at,created_at FROM domains WHERE id=?`, id)
	var d Domain
	var checked sql.NullString
	var created string
	if err := row.Scan(&d.ID, &d.Name, &d.Status, &d.DKIMSelector, &d.DKIMPublicKey, &d.DNSStatus, &checked, &created); err != nil {
		return nil, err
	}
	d.DNSCheckedAt = nullableTime(checked)
	d.CreatedAt = parseTime(created)
	return &d, nil
}

func (a *App) adminUserByID(ctx context.Context, id string) (*AdminUser, error) {
	row := a.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.display_name,u.role,u.disabled,u.two_factor_enabled,u.created_at,COUNT(mb.id),COALESCE(GROUP_CONCAT(mb.address), '')
		FROM users u LEFT JOIN mailboxes mb ON mb.user_id=u.id
		WHERE u.id=?
		GROUP BY u.id,u.email,u.display_name,u.role,u.disabled,u.two_factor_enabled,u.created_at`, id)
	var item AdminUser
	var disabled, twoFactorEnabled int
	var created, mailboxCSV string
	if err := row.Scan(&item.ID, &item.Email, &item.DisplayName, &item.Role, &disabled, &twoFactorEnabled, &created, &item.MailboxCount, &mailboxCSV); err != nil {
		return nil, err
	}
	item.Disabled = intBool(disabled)
	item.TwoFactorEnabled = intBool(twoFactorEnabled)
	item.CreatedAt = parseTime(created)
	item.Mailboxes = splitCSV(mailboxCSV)
	if err := a.attachUserAuthorization(ctx, &item.User); err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *App) ensureAdminRemains(ctx context.Context, targetID, nextRole string, nextDisabled bool) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,role,disabled FROM users`)
	if err != nil {
		return err
	}
	defer rows.Close()
	admins := 0
	for rows.Next() {
		var id, role string
		var disabled int
		if err := rows.Scan(&id, &role, &disabled); err != nil {
			return err
		}
		if id == targetID {
			role = nextRole
			disabled = boolInt(nextDisabled)
		}
		if role == "admin" && disabled == 0 {
			admins++
		}
	}
	if admins == 0 {
		return errors.New("at least one active admin is required")
	}
	return nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (a *App) mailboxByID(ctx context.Context, id string) (*Mailbox, error) {
	row := a.db.QueryRowContext(ctx, `SELECT mb.id,mb.user_id,u.email,mb.domain_id,mb.local_part,mb.address,mb.display_name,mb.quota_mb,mb.status,mb.created_at
		FROM mailboxes mb JOIN users u ON u.id=mb.user_id WHERE mb.id=?`, id)
	var m Mailbox
	var created string
	if err := row.Scan(&m.ID, &m.UserID, &m.UserEmail, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	return &m, nil
}

func (a *App) mailboxForUser(ctx context.Context, userID string) (*Mailbox, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,domain_id,local_part,address,display_name,quota_mb,status,created_at FROM mailboxes WHERE user_id=? AND status='active' ORDER BY created_at LIMIT 1`, userID)
	var m Mailbox
	var created string
	if err := row.Scan(&m.ID, &m.UserID, &m.DomainID, &m.LocalPart, &m.Address, &m.DisplayName, &m.QuotaMB, &m.Status, &created); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	return &m, nil
}

func (a *App) ensureFolder(ctx context.Context, mailboxID, folder string) (string, error) {
	var id string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM folders WHERE mailbox_id=? AND lower(name)=lower(?)`, mailboxID, folder).Scan(&id); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	role := strings.ToLower(folder)
	id = newID("fld")
	sortOrder := 0
	if !isSystemFolderName(folder) {
		var err error
		sortOrder, err = a.nextCustomFolderSortOrder(ctx, mailboxID)
		if err != nil {
			return "", err
		}
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO folders(id,mailbox_id,name,role,sort_order,uid_validity,uid_next,highest_modseq,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, mailboxID, folder, role, sortOrder, a.newUIDValidity(), 1, 1, a.now().UTC().Format(time.RFC3339Nano))
	return id, err
}
