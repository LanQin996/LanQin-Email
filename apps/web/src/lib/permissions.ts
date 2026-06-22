import type { PermissionKey, User } from "@/lib/api-types"

export const MAIL_PERMISSIONS: PermissionKey[] = [
  "mail.access",
  "mail.messages.read",
  "mail.messages.send",
  "mail.messages.drafts",
  "mail.messages.schedule",
  "mail.messages.organize",
  "mail.labels.manage",
  "mail.attachments.download",
  "mail.contacts.manage",
  "mail.signatures.manage",
  "mail.rules.manage",
  "mail.blocked_senders.manage",
  "mail.stats.view",
  "mail.mailboxes.apply",
]

export const ADMIN_PERMISSIONS: PermissionKey[] = [
  "admin.overview.view",
  "admin.users.view",
  "admin.users.create",
  "admin.users.update",
  "admin.users.delete",
  "admin.users.reset_password",
  "admin.permission_groups.view",
  "admin.permission_groups.create",
  "admin.permission_groups.update",
  "admin.permission_groups.delete",
  "admin.domains.view",
  "admin.domains.create",
  "admin.domains.update",
  "admin.domains.delete",
  "admin.dns.view",
  "admin.dns.check",
  "admin.mailboxes.view",
  "admin.mailboxes.create",
  "admin.mailboxes.update",
  "admin.mailboxes.delete",
  "admin.aliases.view",
  "admin.aliases.create",
  "admin.aliases.update",
  "admin.aliases.delete",
  "admin.messages.view",
  "admin.messages.read",
  "admin.messages.attachments",
  "admin.settings.view",
  "admin.settings.update",
  "admin.settings.test_smtp",
  "admin.templates.view",
  "admin.templates.update",
  "admin.templates.reset",
]

export function hasPermission(user: User | undefined | null, permission: PermissionKey) {
  if (!user) return false
  if (user.role === "admin") return true
  return (user.permissions || []).includes(permission)
}

export function hasAnyPermission(user: User | undefined | null, permissions: PermissionKey[]) {
  if (!user) return false
  if (user.role === "admin") return true
  return permissions.some((permission) => (user.permissions || []).includes(permission))
}

export function hasAdminAccess(user: User | undefined | null) {
  return hasAnyPermission(user, ADMIN_PERMISSIONS)
}

export function hasMailAccess(user: User | undefined | null) {
  return hasPermission(user, "mail.access")
}
