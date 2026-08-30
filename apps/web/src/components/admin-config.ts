import type { PermissionInfo, PermissionLimits } from "@/lib/api"
import type { PermissionKey } from "@/lib/api-types"

export type AdminSection = "overview" | "users" | "permissionGroups" | "domains" | "mailboxes" | "aliases" | "messages" | "sendAudit" | "deliveryQueue" | "settings"

export const adminSectionLabels: Record<AdminSection, string> = {
  overview: "概览",
  users: "用户",
  permissionGroups: "权限组",
  domains: "域名",
  mailboxes: "邮箱账号",
  aliases: "别名转发",
  messages: "全部邮件",
  sendAudit: "发送审计",
  deliveryQueue: "投递队列",
  settings: "系统设置",
}

export const adminSectionKeys = Object.keys(adminSectionLabels) as AdminSection[]

export const adminSectionPermissions: Record<AdminSection, PermissionKey[]> = {
  overview: ["admin.overview.view"],
  users: ["admin.users.view"],
  permissionGroups: ["admin.permission_groups.view"],
  domains: ["admin.domains.view", "admin.dns.view"],
  mailboxes: ["admin.mailboxes.view"],
  aliases: ["admin.aliases.view"],
  messages: ["admin.messages.view"],
  sendAudit: ["admin.messages.view"],
  deliveryQueue: ["admin.settings.view"],
  settings: ["admin.settings.view", "admin.templates.view"],
}

export const defaultPermissionLimits: PermissionLimits = {
  maxAttachmentMb: 25,
  smtpDailyLimit: 200,
  smtpMinuteLimit: 20,
  imapMinuteLimit: 200,
  pop3MinuteLimit: 150,
}

export function groupPermissionCatalog(catalog: PermissionInfo[]) {
  const order: string[] = []
  const grouped = new Map<string, PermissionInfo[]>()
  for (const item of catalog) {
    if (!grouped.has(item.category)) {
      grouped.set(item.category, [])
      order.push(item.category)
    }
    grouped.get(item.category)!.push(item)
  }
  return order.map((category) => ({ category, items: grouped.get(category)! }))
}

export function permissionLimitText(value: number, unit: string) {
  return value > 0 ? `${value} ${unit}` : "不限"
}
