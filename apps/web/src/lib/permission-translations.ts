import type { PermissionInfo, PermissionKey } from "./api-types"
import { uiText } from "./language"

export const permissionMessages: Record<PermissionKey, Omit<PermissionInfo, "key">> = {
  "mail.access": {
    label: "访问邮箱前台",
    description: "进入邮箱前台并查看本人邮箱列表。",
    category: "邮箱前台",
  },
  "mail.messages.read": {
    label: "查看本人邮件",
    description: "查看本人文件夹、邮件列表、星标邮件和邮件正文。",
    category: "邮箱前台",
  },
  "mail.messages.send": {
    label: "发送邮件",
    description: "使用本人邮箱发送、回复和转发邮件。",
    category: "邮箱前台",
  },
  "mail.messages.drafts": {
    label: "管理草稿",
    description: "保存、编辑和删除本人草稿。",
    category: "邮箱前台",
  },
  "mail.messages.schedule": {
    label: "定时发送",
    description: "创建、查看和取消本人定时发送任务。",
    category: "邮箱前台",
  },
  "mail.messages.organize": {
    label: "整理邮件",
    description: "标记已读、星标、移动、归档、删除和清理本人邮件。",
    category: "邮箱前台",
  },
  "mail.labels.manage": {
    label: "管理邮件标签",
    description: "创建标签，并为本人邮件添加或移除标签。",
    category: "邮箱前台",
  },
  "mail.attachments.download": {
    label: "下载附件",
    description: "下载本人邮件中的附件。",
    category: "邮箱前台",
  },
  "mail.contacts.manage": {
    label: "管理联系人",
    description: "查看、新增和删除本人的联系人。",
    category: "个人中心",
  },
  "mail.signatures.manage": {
    label: "管理签名",
    description: "查看、新增、修改和删除本人的邮件签名。",
    category: "个人中心",
  },
  "mail.rules.manage": {
    label: "管理收件规则",
    description: "查看、新增和删除本人的收件规则。",
    category: "个人中心",
  },
  "mail.blocked_senders.manage": {
    label: "管理拦截名单",
    description: "查看、新增和删除本人的发件人拦截规则。",
    category: "个人中心",
  },
  "mail.stats.view": {
    label: "查看邮箱统计",
    description: "查看本人邮箱统计和清理概览。",
    category: "个人中心",
  },
  "mail.mailboxes.apply": {
    label: "自助申请邮箱",
    description: "在开放申请时为本人申请邮箱账号。",
    category: "个人中心",
  },
  "admin.overview.view": {
    label: "查看概览",
    description: "查看后台统计和首次配置检查。",
    category: "概览",
  },
  "admin.users.view": {
    label: "查看用户",
    description: "查看用户列表、状态和绑定邮箱。",
    category: "用户",
  },
  "admin.users.create": {
    label: "创建用户",
    description: "创建普通用户并分配权限组。",
    category: "用户",
  },
  "admin.users.update": {
    label: "编辑用户",
    description: "修改用户显示名称、状态和权限组。",
    category: "用户",
  },
  "admin.users.delete": {
    label: "删除用户",
    description: "删除非受保护用户。",
    category: "用户",
  },
  "admin.users.reset_password": {
    label: "重置用户密码",
    description: "为用户重置登录密码。",
    category: "用户",
  },
  "admin.permission_groups.view": {
    label: "查看权限组",
    description: "查看权限组、权限目录和使用人数。",
    category: "权限组",
  },
  "admin.permission_groups.create": {
    label: "创建权限组",
    description: "创建自定义权限组。",
    category: "权限组",
  },
  "admin.permission_groups.update": {
    label: "编辑权限组",
    description: "修改自定义权限组名称、说明和权限。",
    category: "权限组",
  },
  "admin.permission_groups.delete": {
    label: "删除权限组",
    description: "删除未被用户使用的自定义权限组。",
    category: "权限组",
  },
  "admin.domains.view": {
    label: "查看域名",
    description: "查看邮件域名和 DKIM 配置。",
    category: "域名",
  },
  "admin.domains.create": {
    label: "添加域名",
    description: "添加新的邮件域名。",
    category: "域名",
  },
  "admin.domains.update": {
    label: "启停域名",
    description: "启用或停用邮件域名。",
    category: "域名",
  },
  "admin.domains.delete": {
    label: "删除域名",
    description: "删除未被邮箱使用的域名。",
    category: "域名",
  },
  "admin.dns.view": {
    label: "查看 DNS",
    description: "查看域名需要配置的 DNS 记录。",
    category: "DNS",
  },
  "admin.dns.check": {
    label: "执行 DNS 检测",
    description: "触发 MX、SPF、DKIM、DMARC 检测。",
    category: "DNS",
  },
  "admin.mailboxes.view": {
    label: "查看邮箱账号",
    description: "查看邮箱账号列表和归属用户。",
    category: "邮箱账号",
  },
  "admin.mailboxes.create": {
    label: "创建邮箱账号",
    description: "创建邮箱账号并准备归属用户。",
    category: "邮箱账号",
  },
  "admin.mailboxes.update": {
    label: "编辑邮箱账号",
    description: "修改邮箱归属、显示名、配额和状态。",
    category: "邮箱账号",
  },
  "admin.mailboxes.delete": {
    label: "删除邮箱账号",
    description: "删除邮箱账号及关联邮件文件。",
    category: "邮箱账号",
  },
  "admin.aliases.view": {
    label: "查看别名转发",
    description: "查看别名转发规则。",
    category: "别名转发",
  },
  "admin.aliases.create": {
    label: "创建别名转发",
    description: "创建新的别名转发。",
    category: "别名转发",
  },
  "admin.aliases.update": {
    label: "编辑别名转发",
    description: "修改别名转发来源、目标和启用状态。",
    category: "别名转发",
  },
  "admin.aliases.delete": {
    label: "删除别名转发",
    description: "删除别名转发规则。",
    category: "别名转发",
  },
  "admin.messages.view": {
    label: "查看邮件列表",
    description: "查看全局邮件列表和搜索结果。",
    category: "邮件审计",
  },
  "admin.messages.read": {
    label: "查看邮件正文",
    description: "查看任意邮箱及未注册收件人的邮件正文。",
    category: "邮件审计",
  },
  "admin.messages.attachments": {
    label: "下载邮件附件",
    description: "下载全局邮件中的附件。",
    category: "邮件审计",
  },
  "admin.settings.view": {
    label: "查看系统设置",
    description: "查看系统、SMTP、安全和邮件设置。",
    category: "系统设置",
  },
  "admin.settings.update": {
    label: "修改系统设置",
    description: "保存系统、SMTP、安全和邮件设置。",
    category: "系统设置",
  },
  "admin.settings.test_smtp": {
    label: "测试 SMTP",
    description: "发送 SMTP 测试邮件。",
    category: "系统设置",
  },
  "admin.templates.view": {
    label: "查看邮件模板",
    description: "查看系统邮件模板。",
    category: "邮件模板",
  },
  "admin.templates.update": {
    label: "编辑邮件模板",
    description: "修改系统邮件模板内容。",
    category: "邮件模板",
  },
  "admin.templates.reset": {
    label: "恢复邮件模板",
    description: "将系统邮件模板恢复默认。",
    category: "邮件模板",
  },
}

export function localizePermissionInfo(item: PermissionInfo): PermissionInfo {
  const source = permissionMessages[item.key]
  if (!source) return { ...item, label: item.key, description: "", category: uiText("其他权限") }
  return {
    key: item.key,
    label: uiText(source.label),
    description: uiText(source.description),
    category: uiText(source.category),
  }
}
