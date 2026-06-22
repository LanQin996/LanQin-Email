package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	PermissionMailAccess = "mail.access"

	PermissionMailRead        = "mail.messages.read"
	PermissionMailSend        = "mail.messages.send"
	PermissionMailDrafts      = "mail.messages.drafts"
	PermissionMailSchedule    = "mail.messages.schedule"
	PermissionMailOrganize    = "mail.messages.organize"
	PermissionMailLabels      = "mail.labels.manage"
	PermissionMailAttachments = "mail.attachments.download"

	PermissionMailContacts   = "mail.contacts.manage"
	PermissionMailSignatures = "mail.signatures.manage"
	PermissionMailRules      = "mail.rules.manage"
	PermissionMailBlocked    = "mail.blocked_senders.manage"
	PermissionMailStats      = "mail.stats.view"
	PermissionMailboxApply   = "mail.mailboxes.apply"

	PermissionAdminOverview = "admin.overview.view"

	PermissionUsersView          = "admin.users.view"
	PermissionUsersCreate        = "admin.users.create"
	PermissionUsersUpdate        = "admin.users.update"
	PermissionUsersDelete        = "admin.users.delete"
	PermissionUsersResetPassword = "admin.users.reset_password"

	PermissionGroupsView   = "admin.permission_groups.view"
	PermissionGroupsCreate = "admin.permission_groups.create"
	PermissionGroupsUpdate = "admin.permission_groups.update"
	PermissionGroupsDelete = "admin.permission_groups.delete"

	PermissionDomainsView   = "admin.domains.view"
	PermissionDomainsCreate = "admin.domains.create"
	PermissionDomainsUpdate = "admin.domains.update"
	PermissionDomainsDelete = "admin.domains.delete"

	PermissionDNSView  = "admin.dns.view"
	PermissionDNSCheck = "admin.dns.check"

	PermissionMailboxesView   = "admin.mailboxes.view"
	PermissionMailboxesCreate = "admin.mailboxes.create"
	PermissionMailboxesUpdate = "admin.mailboxes.update"
	PermissionMailboxesDelete = "admin.mailboxes.delete"

	PermissionAliasesView   = "admin.aliases.view"
	PermissionAliasesCreate = "admin.aliases.create"
	PermissionAliasesUpdate = "admin.aliases.update"
	PermissionAliasesDelete = "admin.aliases.delete"

	PermissionMessagesView       = "admin.messages.view"
	PermissionMessagesRead       = "admin.messages.read"
	PermissionMessagesAttachment = "admin.messages.attachments"

	PermissionSettingsView     = "admin.settings.view"
	PermissionSettingsUpdate   = "admin.settings.update"
	PermissionSettingsTestSMTP = "admin.settings.test_smtp"

	PermissionTemplatesView   = "admin.templates.view"
	PermissionTemplatesUpdate = "admin.templates.update"
	PermissionTemplatesReset  = "admin.templates.reset"

	PermissionUsersManage     = PermissionUsersUpdate
	PermissionGroupsManage    = PermissionGroupsUpdate
	PermissionDomainsManage   = PermissionDomainsUpdate
	PermissionDNSManage       = PermissionDNSCheck
	PermissionMailboxesManage = PermissionMailboxesUpdate
	PermissionAliasesManage   = PermissionAliasesUpdate
	PermissionSystemSettings  = PermissionSettingsUpdate
)

const regularUserPermissionMigrationKey = "permission_groups.regular_mail_permissions_v1"

const (
	PermissionGroupSuperAdmin      = "pg_super_admin"
	PermissionGroupRegular         = "pg_regular_user"
	PermissionGroupUserAdmin       = "pg_user_admin"
	PermissionGroupPermissionAdmin = "pg_permission_group_admin"
	PermissionGroupDomainAdmin     = "pg_domain_admin"
	PermissionGroupDNSAdmin        = "pg_dns_admin"
	PermissionGroupMailboxAdmin    = "pg_mailbox_admin"
	PermissionGroupAliasAdmin      = "pg_alias_admin"
	PermissionGroupMessageAudit    = "pg_message_audit"
	PermissionGroupSystemAdmin     = "pg_system_admin"
)

var legacyDefaultPermissionGroupIDs = []string{
	"pg_permission_manager",
	"pg_user_manager",
	"pg_system_operator",
	"pg_mail_operator",
	PermissionGroupUserAdmin,
	PermissionGroupPermissionAdmin,
	PermissionGroupDomainAdmin,
	PermissionGroupDNSAdmin,
	PermissionGroupMailboxAdmin,
	PermissionGroupAliasAdmin,
	PermissionGroupMessageAudit,
	PermissionGroupSystemAdmin,
}

type PermissionInfo struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

type PermissionGroupSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PermissionGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Permissions []string  `json:"permissions"`
	System      bool      `json:"system"`
	UserCount   int       `json:"userCount"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

var legacyPermissionExpansions = map[string][]string{
	"mail":                    regularUserDefaultPermissions(),
	"mail.messages":           {PermissionMailAccess, PermissionMailRead, PermissionMailSend, PermissionMailDrafts, PermissionMailSchedule, PermissionMailOrganize, PermissionMailAttachments},
	"mail.labels":             {PermissionMailAccess, PermissionMailRead, PermissionMailLabels},
	"mail.contacts":           {PermissionMailContacts},
	"mail.signatures":         {PermissionMailSignatures},
	"mail.rules":              {PermissionMailRules, PermissionMailBlocked},
	"mail.mailboxes":          {PermissionMailboxApply},
	"admin.overview":          {PermissionAdminOverview},
	"admin.users":             {PermissionUsersView, PermissionUsersCreate, PermissionUsersUpdate, PermissionUsersDelete, PermissionUsersResetPassword, PermissionGroupsView},
	"admin.permission_groups": {PermissionGroupsView, PermissionGroupsCreate, PermissionGroupsUpdate, PermissionGroupsDelete},
	"admin.domains":           {PermissionDomainsView, PermissionDomainsCreate, PermissionDomainsUpdate, PermissionDomainsDelete},
	"admin.dns":               {PermissionDomainsView, PermissionDNSView, PermissionDNSCheck},
	"admin.mailboxes":         {PermissionUsersView, PermissionDomainsView, PermissionMailboxesView, PermissionMailboxesCreate, PermissionMailboxesUpdate, PermissionMailboxesDelete},
	"admin.aliases":           {PermissionDomainsView, PermissionAliasesView, PermissionAliasesCreate, PermissionAliasesUpdate, PermissionAliasesDelete},
	"admin.messages":          {PermissionMailboxesView, PermissionMessagesView, PermissionMessagesRead, PermissionMessagesAttachment},
	"admin.settings":          {PermissionDomainsView, PermissionSettingsView, PermissionSettingsUpdate, PermissionSettingsTestSMTP, PermissionTemplatesView, PermissionTemplatesUpdate, PermissionTemplatesReset},
}

var permissionCatalogItems = []PermissionInfo{
	{Key: PermissionMailAccess, Label: "访问邮箱前台", Description: "进入邮箱前台并查看本人邮箱列表。", Category: "邮箱前台"},
	{Key: PermissionMailRead, Label: "查看本人邮件", Description: "查看本人文件夹、邮件列表、星标邮件和邮件正文。", Category: "邮箱前台"},
	{Key: PermissionMailSend, Label: "发送邮件", Description: "使用本人邮箱发送、回复和转发邮件。", Category: "邮箱前台"},
	{Key: PermissionMailDrafts, Label: "管理草稿", Description: "保存、编辑和删除本人草稿。", Category: "邮箱前台"},
	{Key: PermissionMailSchedule, Label: "定时发送", Description: "创建、查看和取消本人定时发送任务。", Category: "邮箱前台"},
	{Key: PermissionMailOrganize, Label: "整理邮件", Description: "标记已读、星标、移动、归档、删除和清理本人邮件。", Category: "邮箱前台"},
	{Key: PermissionMailLabels, Label: "管理邮件标签", Description: "创建标签，并为本人邮件添加或移除标签。", Category: "邮箱前台"},
	{Key: PermissionMailAttachments, Label: "下载附件", Description: "下载本人邮件中的附件。", Category: "邮箱前台"},
	{Key: PermissionMailContacts, Label: "管理联系人", Description: "查看、新增和删除本人的联系人。", Category: "个人中心"},
	{Key: PermissionMailSignatures, Label: "管理签名", Description: "查看、新增、修改和删除本人的邮件签名。", Category: "个人中心"},
	{Key: PermissionMailRules, Label: "管理收件规则", Description: "查看、新增和删除本人的收件规则。", Category: "个人中心"},
	{Key: PermissionMailBlocked, Label: "管理拦截名单", Description: "查看、新增和删除本人的发件人拦截规则。", Category: "个人中心"},
	{Key: PermissionMailStats, Label: "查看邮箱统计", Description: "查看本人邮箱统计和清理概览。", Category: "个人中心"},
	{Key: PermissionMailboxApply, Label: "自助申请邮箱", Description: "在开放申请时为本人申请邮箱账号。", Category: "个人中心"},

	{Key: PermissionAdminOverview, Label: "查看概览", Description: "查看后台统计和首次配置检查。", Category: "概览"},

	{Key: PermissionUsersView, Label: "查看用户", Description: "查看用户列表、状态和绑定邮箱。", Category: "用户"},
	{Key: PermissionUsersCreate, Label: "创建用户", Description: "创建普通用户并分配权限组。", Category: "用户"},
	{Key: PermissionUsersUpdate, Label: "编辑用户", Description: "修改用户显示名称、状态和权限组。", Category: "用户"},
	{Key: PermissionUsersDelete, Label: "删除用户", Description: "删除非受保护用户。", Category: "用户"},
	{Key: PermissionUsersResetPassword, Label: "重置用户密码", Description: "为用户重置登录密码。", Category: "用户"},

	{Key: PermissionGroupsView, Label: "查看权限组", Description: "查看权限组、权限目录和使用人数。", Category: "权限组"},
	{Key: PermissionGroupsCreate, Label: "创建权限组", Description: "创建自定义权限组。", Category: "权限组"},
	{Key: PermissionGroupsUpdate, Label: "编辑权限组", Description: "修改自定义权限组名称、说明和权限。", Category: "权限组"},
	{Key: PermissionGroupsDelete, Label: "删除权限组", Description: "删除未被用户使用的自定义权限组。", Category: "权限组"},

	{Key: PermissionDomainsView, Label: "查看域名", Description: "查看邮件域名和 DKIM 配置。", Category: "域名"},
	{Key: PermissionDomainsCreate, Label: "添加域名", Description: "添加新的邮件域名。", Category: "域名"},
	{Key: PermissionDomainsUpdate, Label: "启停域名", Description: "启用或停用邮件域名。", Category: "域名"},
	{Key: PermissionDomainsDelete, Label: "删除域名", Description: "删除未被邮箱使用的域名。", Category: "域名"},

	{Key: PermissionDNSView, Label: "查看 DNS", Description: "查看域名需要配置的 DNS 记录。", Category: "DNS"},
	{Key: PermissionDNSCheck, Label: "执行 DNS 检测", Description: "触发 MX、SPF、DKIM、DMARC 检测。", Category: "DNS"},

	{Key: PermissionMailboxesView, Label: "查看邮箱账号", Description: "查看邮箱账号列表和归属用户。", Category: "邮箱账号"},
	{Key: PermissionMailboxesCreate, Label: "创建邮箱账号", Description: "创建邮箱账号并准备归属用户。", Category: "邮箱账号"},
	{Key: PermissionMailboxesUpdate, Label: "编辑邮箱账号", Description: "修改邮箱归属、显示名、配额和状态。", Category: "邮箱账号"},
	{Key: PermissionMailboxesDelete, Label: "删除邮箱账号", Description: "删除邮箱账号及关联邮件文件。", Category: "邮箱账号"},

	{Key: PermissionAliasesView, Label: "查看别名转发", Description: "查看别名转发规则。", Category: "别名转发"},
	{Key: PermissionAliasesCreate, Label: "创建别名转发", Description: "创建新的别名转发。", Category: "别名转发"},
	{Key: PermissionAliasesUpdate, Label: "编辑别名转发", Description: "修改别名转发来源、目标和启用状态。", Category: "别名转发"},
	{Key: PermissionAliasesDelete, Label: "删除别名转发", Description: "删除别名转发规则。", Category: "别名转发"},

	{Key: PermissionMessagesView, Label: "查看邮件列表", Description: "查看全局邮件列表和搜索结果。", Category: "邮件审计"},
	{Key: PermissionMessagesRead, Label: "查看邮件正文", Description: "查看任意邮箱及未注册收件人的邮件正文。", Category: "邮件审计"},
	{Key: PermissionMessagesAttachment, Label: "下载邮件附件", Description: "下载全局邮件中的附件。", Category: "邮件审计"},

	{Key: PermissionSettingsView, Label: "查看系统设置", Description: "查看系统、SMTP、安全和邮件设置。", Category: "系统设置"},
	{Key: PermissionSettingsUpdate, Label: "修改系统设置", Description: "保存系统、SMTP、安全和邮件设置。", Category: "系统设置"},
	{Key: PermissionSettingsTestSMTP, Label: "测试 SMTP", Description: "发送 SMTP 测试邮件。", Category: "系统设置"},
	{Key: PermissionTemplatesView, Label: "查看邮件模板", Description: "查看系统邮件模板。", Category: "邮件模板"},
	{Key: PermissionTemplatesUpdate, Label: "编辑邮件模板", Description: "修改系统邮件模板内容。", Category: "邮件模板"},
	{Key: PermissionTemplatesReset, Label: "恢复邮件模板", Description: "将系统邮件模板恢复默认。", Category: "邮件模板"},
}

func permissionCatalog() []PermissionInfo {
	out := make([]PermissionInfo, len(permissionCatalogItems))
	copy(out, permissionCatalogItems)
	return out
}

func allPermissionKeys() []string {
	out := make([]string, 0, len(permissionCatalogItems))
	for _, item := range permissionCatalogItems {
		out = append(out, item.Key)
	}
	return out
}

func adminPermissionKeys() []string {
	out := make([]string, 0, len(permissionCatalogItems))
	for _, item := range permissionCatalogItems {
		if strings.HasPrefix(item.Key, "admin.") {
			out = append(out, item.Key)
		}
	}
	return out
}

func permissionSet() map[string]bool {
	out := map[string]bool{}
	for _, item := range permissionCatalogItems {
		out[item.Key] = true
	}
	return out
}

func addPermissionToSet(item string, seen map[string]bool) bool {
	item = strings.TrimSpace(item)
	if item == "" {
		return true
	}
	if permissionSet()[item] {
		seen[item] = true
		return true
	}
	if expanded, ok := legacyPermissionExpansions[item]; ok {
		for _, permission := range expanded {
			seen[permission] = true
		}
		return true
	}
	return false
}

func normalizePermissionList(items []string) ([]string, error) {
	seen := map[string]bool{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if !addPermissionToSet(item, seen) {
			return nil, fmt.Errorf("invalid permission: %s", item)
		}
	}
	out := make([]string, 0, len(seen))
	for _, item := range allPermissionKeys() {
		if seen[item] {
			out = append(out, item)
		}
	}
	return out, nil
}

func decodeStoredPermissions(value string) []string {
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, item := range raw {
		addPermissionToSet(item, seen)
	}
	out := make([]string, 0, len(seen))
	for _, item := range allPermissionKeys() {
		if seen[item] {
			out = append(out, item)
		}
	}
	return out
}

func encodePermissions(items []string) string {
	normalized, err := normalizePermissionList(items)
	if err != nil {
		normalized = nil
	}
	data, _ := json.Marshal(normalized)
	return string(data)
}

func defaultPermissionGroups() []PermissionGroup {
	return []PermissionGroup{
		{
			ID:          PermissionGroupSuperAdmin,
			Name:        "超级管理员",
			Description: "拥有全部后台权限，由用户身份决定，不通过权限组分配。",
			Permissions: allPermissionKeys(),
			System:      true,
		},
		{
			ID:          PermissionGroupRegular,
			Name:        "普通用户",
			Description: "仅可使用自己的邮箱功能，不包含后台权限。",
			Permissions: regularUserDefaultPermissions(),
			System:      true,
		},
	}
}

func regularUserDefaultPermissions() []string {
	return []string{
		PermissionMailAccess,
		PermissionMailRead,
		PermissionMailSend,
		PermissionMailDrafts,
		PermissionMailSchedule,
		PermissionMailOrganize,
		PermissionMailLabels,
		PermissionMailAttachments,
		PermissionMailContacts,
		PermissionMailSignatures,
		PermissionMailRules,
		PermissionMailBlocked,
		PermissionMailStats,
		PermissionMailboxApply,
	}
}

func fixedPermissionGroupIDs() map[string]bool {
	out := map[string]bool{}
	for _, group := range defaultPermissionGroups() {
		out[group.ID] = true
	}
	return out
}

func assignablePermissionGroupIDs() map[string]bool {
	out := fixedPermissionGroupIDs()
	delete(out, PermissionGroupSuperAdmin)
	delete(out, PermissionGroupRegular)
	return out
}

func isAssignablePermissionGroupID(groupID string) bool {
	return groupID != "" && groupID != PermissionGroupSuperAdmin && groupID != PermissionGroupRegular
}

func permissionGroupOrder() map[string]int {
	out := map[string]int{}
	for index, group := range defaultPermissionGroups() {
		out[group.ID] = index
	}
	return out
}

func permissionGroupNames() map[string]string {
	out := map[string]string{}
	for _, group := range defaultPermissionGroups() {
		out[group.ID] = group.Name
	}
	return out
}

func (a *App) ensureDefaultPermissionGroups(ctx context.Context) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, item := range defaultPermissionGroups() {
		if _, err := a.db.ExecContext(ctx, `UPDATE permission_groups SET name=name || ' (' || id || ')' WHERE name=? AND id<>?`, item.Name, item.ID); err != nil {
			return err
		}
		query := `INSERT INTO permission_groups(id,name,description,permissions_json,system,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description, permissions_json=excluded.permissions_json, system=excluded.system, updated_at=excluded.updated_at`
		if item.ID == PermissionGroupRegular {
			query = `INSERT INTO permission_groups(id,name,description,permissions_json,system,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?)
				ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description, system=excluded.system, updated_at=excluded.updated_at`
		}
		if _, err := a.db.ExecContext(ctx, query, item.ID, item.Name, item.Description, encodePermissions(item.Permissions), boolInt(item.System), now, now); err != nil {
			return err
		}
	}
	if err := a.cleanupLegacyDefaultPermissionGroups(ctx); err != nil {
		return err
	}
	if err := a.ensureRegularUserMailPermissions(ctx); err != nil {
		return err
	}
	return nil
}

func (a *App) ensureRegularUserMailPermissions(ctx context.Context) error {
	var migrated string
	err := a.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key=?`, regularUserPermissionMigrationKey).Scan(&migrated)
	if err == nil && migrated == "1" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var raw string
	if err := a.db.QueryRowContext(ctx, `SELECT permissions_json FROM permission_groups WHERE id=?`, PermissionGroupRegular).Scan(&raw); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, permission := range decodeStoredPermissions(raw) {
		seen[permission] = true
	}
	changed := false
	for _, permission := range regularUserDefaultPermissions() {
		if !seen[permission] {
			seen[permission] = true
			changed = true
		}
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if changed {
		permissions := make([]string, 0, len(seen))
		for _, permission := range allPermissionKeys() {
			if seen[permission] {
				permissions = append(permissions, permission)
			}
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE permission_groups SET permissions_json=?, updated_at=? WHERE id=?`, encodePermissions(permissions), now, PermissionGroupRegular); err != nil {
			return err
		}
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, regularUserPermissionMigrationKey, "1", now)
	return err
}

func (a *App) cleanupLegacyDefaultPermissionGroups(ctx context.Context) error {
	for _, groupID := range legacyDefaultPermissionGroupIDs {
		var userCount int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_permission_groups WHERE group_id=?`, groupID).Scan(&userCount); err != nil {
			return err
		}
		if userCount == 0 {
			if _, err := a.db.ExecContext(ctx, `DELETE FROM permission_groups WHERE id=? AND system=1`, groupID); err != nil {
				return err
			}
			continue
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE permission_groups SET system=0, updated_at=? WHERE id=? AND system=1`, a.now().UTC().Format(time.RFC3339Nano), groupID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) attachUserAuthorization(ctx context.Context, u *User) error {
	if u == nil {
		return nil
	}
	permissions, err := a.permissionsForUser(ctx, u.ID, u.Role)
	if err != nil {
		return err
	}
	groupIDs, groups, err := a.permissionGroupsForUser(ctx, u.ID, u.Role)
	if err != nil {
		return err
	}
	u.Permissions = permissions
	u.PermissionGroupIDs = groupIDs
	u.PermissionGroups = groups
	u.Protected = a.isDefaultAdminUser(u)
	return nil
}

func (a *App) permissionsForUser(ctx context.Context, userID, role string) ([]string, error) {
	if role == "admin" {
		return allPermissionKeys(), nil
	}
	seen := map[string]bool{}
	if err := a.addRegularGroupPermissions(ctx, nil, seen); err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT pg.id,pg.permissions_json
		FROM permission_groups pg
		JOIN user_permission_groups upg ON upg.group_id=pg.id
		WHERE upg.user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID, raw string
		if err := rows.Scan(&groupID, &raw); err != nil {
			return nil, err
		}
		if !isAssignablePermissionGroupID(groupID) {
			continue
		}
		for _, permission := range decodeStoredPermissions(raw) {
			seen[permission] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for _, item := range allPermissionKeys() {
		if seen[item] {
			out = append(out, item)
		}
	}
	return out, nil
}

func (a *App) addRegularGroupPermissions(ctx context.Context, tx *sql.Tx, seen map[string]bool) error {
	var raw string
	query := `SELECT permissions_json FROM permission_groups WHERE id=?`
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, query, PermissionGroupRegular).Scan(&raw)
	} else {
		err = a.db.QueryRowContext(ctx, query, PermissionGroupRegular).Scan(&raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, permission := range decodeStoredPermissions(raw) {
		seen[permission] = true
	}
	return nil
}

func (a *App) effectivePermissionsForUserGroups(ctx context.Context, tx *sql.Tx, groupIDs []string) ([]string, error) {
	seen := map[string]bool{}
	if err := a.addRegularGroupPermissions(ctx, tx, seen); err != nil {
		return nil, err
	}
	groupPermissions, err := a.permissionsForGroupIDs(ctx, tx, groupIDs)
	if err != nil {
		return nil, err
	}
	for _, permission := range groupPermissions {
		seen[permission] = true
	}
	out := make([]string, 0, len(seen))
	for _, permission := range allPermissionKeys() {
		if seen[permission] {
			out = append(out, permission)
		}
	}
	return out, nil
}

func (a *App) permissionGroupsForUser(ctx context.Context, userID, role string) ([]string, []PermissionGroupSummary, error) {
	if role == "admin" {
		group := PermissionGroupSummary{ID: PermissionGroupSuperAdmin, Name: "超级管理员"}
		return []string{group.ID}, []PermissionGroupSummary{group}, nil
	}
	ids := []string{PermissionGroupRegular}
	groups := []PermissionGroupSummary{{ID: PermissionGroupRegular, Name: "普通用户"}}
	if err := a.db.QueryRowContext(ctx, `SELECT name FROM permission_groups WHERE id=?`, PermissionGroupRegular).Scan(&groups[0].Name); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT pg.id,pg.name
		FROM permission_groups pg
		JOIN user_permission_groups upg ON upg.group_id=pg.id
		WHERE upg.user_id=?`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	order := permissionGroupOrder()
	for rows.Next() {
		var group PermissionGroupSummary
		if err := rows.Scan(&group.ID, &group.Name); err != nil {
			return nil, nil, err
		}
		if !isAssignablePermissionGroupID(group.ID) {
			continue
		}
		ids = append(ids, group.ID)
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(groups, func(i, j int) bool {
		left, leftOK := order[groups[i].ID]
		right, rightOK := order[groups[j].ID]
		if leftOK && rightOK {
			return left < right
		}
		if leftOK != rightOK {
			return leftOK
		}
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	sort.SliceStable(ids, func(i, j int) bool {
		left, leftOK := order[ids[i]]
		right, rightOK := order[ids[j]]
		if leftOK && rightOK {
			return left < right
		}
		if leftOK != rightOK {
			return leftOK
		}
		return ids[i] < ids[j]
	})
	return ids, groups, nil
}

func userHasPermission(user *User, permission string) bool {
	if user == nil {
		return false
	}
	if user.Role == "admin" {
		return true
	}
	for _, item := range user.Permissions {
		if item == permission {
			return true
		}
	}
	return false
}

func userHasAnyPermission(user *User, permissions ...string) bool {
	if user == nil {
		return false
	}
	if user.Role == "admin" {
		return true
	}
	for _, permission := range permissions {
		if userHasPermission(user, permission) {
			return true
		}
	}
	return false
}

func userHasAdminAccess(user *User) bool {
	return userHasAnyPermission(user, adminPermissionKeys()...)
}

func (a *App) requireAdminAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userHasAdminAccess(currentUser(r)) {
			respondError(w, http.StatusForbidden, "admin permission required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) requirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !userHasPermission(currentUser(r), permission) {
				respondError(w, http.StatusForbidden, "permission required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *App) requireAnyPermission(permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !userHasAnyPermission(currentUser(r), permissions...) {
				respondError(w, http.StatusForbidden, "permission required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func actorCanGrantPermissions(actor *User, permissions []string) bool {
	if actor == nil {
		return false
	}
	if actor.Role == "admin" {
		return true
	}
	for _, permission := range permissions {
		if !userHasPermission(actor, permission) {
			return false
		}
	}
	return true
}

func (a *App) permissionsForGroupIDs(ctx context.Context, tx *sql.Tx, groupIDs []string) ([]string, error) {
	seen := map[string]bool{}
	for _, groupID := range cleanIDList(groupIDs) {
		if !isAssignablePermissionGroupID(groupID) {
			return nil, fmt.Errorf("permission group not assignable: %s", groupID)
		}
		var raw string
		query := `SELECT permissions_json FROM permission_groups WHERE id=?`
		var err error
		if tx != nil {
			err = tx.QueryRowContext(ctx, query, groupID).Scan(&raw)
		} else {
			err = a.db.QueryRowContext(ctx, query, groupID).Scan(&raw)
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("permission group not found: %s", groupID)
			}
			return nil, err
		}
		for _, permission := range decodeStoredPermissions(raw) {
			seen[permission] = true
		}
	}
	out := make([]string, 0, len(seen))
	for _, permission := range allPermissionKeys() {
		if seen[permission] {
			out = append(out, permission)
		}
	}
	return out, nil
}

func (a *App) setUserPermissionGroups(ctx context.Context, tx *sql.Tx, userID string, groupIDs []string, actor *User) error {
	groupIDs = cleanIDList(groupIDs)
	for _, groupID := range groupIDs {
		if !isAssignablePermissionGroupID(groupID) {
			return fmt.Errorf("permission group not assignable: %s", groupID)
		}
	}
	groupPermissions, err := a.effectivePermissionsForUserGroups(ctx, tx, groupIDs)
	if err != nil {
		return err
	}
	if !actorCanGrantPermissions(actor, groupPermissions) {
		return errors.New("cannot assign permissions you do not hold")
	}
	exec := func(query string, args ...any) error {
		var err error
		if tx != nil {
			_, err = tx.ExecContext(ctx, query, args...)
		} else {
			_, err = a.db.ExecContext(ctx, query, args...)
		}
		return err
	}
	if err := exec(`DELETE FROM user_permission_groups WHERE user_id=?`, userID); err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, groupID := range groupIDs {
		if err := exec(`INSERT INTO user_permission_groups(user_id,group_id,created_at) VALUES(?,?,?)`, userID, groupID, now); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) permissionGroupByID(ctx context.Context, id string) (*PermissionGroup, error) {
	row := a.db.QueryRowContext(ctx, `SELECT pg.id,pg.name,pg.description,pg.permissions_json,pg.system,pg.created_at,pg.updated_at,COUNT(upg.user_id)
		FROM permission_groups pg
		LEFT JOIN user_permission_groups upg ON upg.group_id=pg.id
		WHERE pg.id=?
		GROUP BY pg.id,pg.name,pg.description,pg.permissions_json,pg.system,pg.created_at,pg.updated_at`, id)
	var group PermissionGroup
	var raw, created, updated string
	var system int
	if err := row.Scan(&group.ID, &group.Name, &group.Description, &raw, &system, &created, &updated, &group.UserCount); err != nil {
		return nil, err
	}
	group.Permissions = decodeStoredPermissions(raw)
	group.System = intBool(system)
	group.CreatedAt = parseTime(created)
	group.UpdatedAt = parseTime(updated)
	return &group, nil
}

func (a *App) isDefaultAdminUser(u *User) bool {
	if u == nil {
		return false
	}
	adminEmail := normalizeEmail(a.cfg.AdminEmail)
	return adminEmail != "" && strings.EqualFold(normalizeEmail(u.Email), adminEmail)
}

func sortPermissionGroups(items []PermissionGroup) {
	order := permissionGroupOrder()
	sort.SliceStable(items, func(i, j int) bool {
		left, leftOK := order[items[i].ID]
		right, rightOK := order[items[j].ID]
		if leftOK && rightOK {
			return left < right
		}
		if leftOK != rightOK {
			return leftOK
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}
