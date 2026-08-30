import type { User, AdminUser, AdminOverview, Domain, Mailbox, MailboxShare, MailboxSharePayload, MailboxShareUpdatePayload, MailboxShareAuditEvent, UserNotification, ShareUser, Alias, MailFolder, Attachment, MailLabel, MailMessage, MailTranslation, DNSRecord, DNSCheckResult, ListResponse, SendPayload, DraftPayload, ScheduleSendPayload, ScheduledSend, SendQueueItem, SendQueueAuditEvent, SendQueueStatus, AdminDeliveryQueueItem, Contact, MailSignature, MailRule, MailRuleCondition, MailRuleAction, ForwardAddress, TelegramSettings, BlockedSender, MailStats, ExternalImapAccount, ExternalImapAccountPayload, ExternalImapFolder, ExternalImapOAuthProvider, ExternalImapOAuthStartPayload, ExternalImapSyncRun, MailboxApplyOptions, MailTemplate, MaildirSyncHealth, SystemSettings, SystemSettingsPayload, PublicSettings, LoginPayload, LoginResponse, RegisterPayload, LinuxDoIdentity, LinuxDoPendingRegistration, LinuxDoRegistrationPayload, RegistrationInvite, PermissionGroup, PermissionInfo, PermissionKey, PermissionLimits, APIToken } from "./api-types"
export * from "./api-types"

const REQUEST_TIMEOUT_MS = 15_000
const MAIL_DELIVERY_TIMEOUT_MS = 60_000

async function request<T>(path: string, init: RequestInit & { timeoutMs?: number } = {}): Promise<T> {
  const { timeoutMs, ...requestInit } = init
  const controller = new AbortController()
  const timeout = window.setTimeout(() => controller.abort(), timeoutMs || REQUEST_TIMEOUT_MS)
  const externalSignal = requestInit.signal
  if (externalSignal) {
    if (externalSignal.aborted) controller.abort()
    else externalSignal.addEventListener("abort", () => controller.abort(), { once: true })
  }
  try {
    const res = await fetch(path, { credentials: "include", headers: { "Content-Type": "application/json", ...(requestInit.headers || {}) }, ...requestInit, signal: controller.signal })
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`
      try { const body = await res.json(); message = body.error || message } catch {}
      throw new Error(message)
    }
    return res.json() as Promise<T>
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") {
      throw new Error("请求超时，请检查后端服务是否正常")
    }
    throw error instanceof Error ? error : new Error("网络请求失败")
  } finally {
    window.clearTimeout(timeout)
  }
}

export const api = {
  publicSettings: () => request<PublicSettings>("/api/public/settings"),
  register: (payload: RegisterPayload) => request<{ user: User }>("/api/auth/register", { method: "POST", body: JSON.stringify(payload) }),
  login: (payload: LoginPayload) => request<LoginResponse>("/api/auth/login", { method: "POST", body: JSON.stringify(payload) }),
  linuxDoPendingRegistration: () => request<LinuxDoPendingRegistration>("/api/auth/linuxdo/pending-registration"),
  linuxDoRegister: (payload: LinuxDoRegistrationPayload) => request<{ user: User }>("/api/auth/linuxdo/register", { method: "POST", body: JSON.stringify(payload) }),
  linuxDoTwoFactor: (code: string) => request<{ user: User }>("/api/auth/linuxdo/2fa", { method: "POST", body: JSON.stringify({ code }) }),
  logout: () => request<{ ok: boolean }>("/api/auth/logout", { method: "POST" }),
  me: () => request<{ user: User }>("/api/me"),
  linuxDoIdentity: () => request<LinuxDoIdentity>("/api/me/auth/linuxdo"),
  startLinuxDoLink: (payload: { currentPassword: string; twoFactorCode?: string }) => request<{ url: string }>("/api/me/auth/linuxdo/link", { method: "POST", body: JSON.stringify(payload) }),
  unlinkLinuxDo: (payload: { currentPassword: string; twoFactorCode?: string }) => request<{ ok: boolean; unlinked: boolean }>("/api/me/auth/linuxdo", { method: "DELETE", body: JSON.stringify(payload) }),
  updateProfile: (payload: { displayName: string }) => request<{ user: User }>("/api/me/profile", { method: "POST", body: JSON.stringify(payload) }),
  changePassword: (payload: { currentPassword: string; newPassword: string }) => request<{ ok: boolean }>("/api/me/password", { method: "POST", body: JSON.stringify(payload) }),
  apiTokens: () => request<ListResponse<APIToken>>("/api/me/api-tokens"),
  createApiToken: (payload: { name: string; expiresAt?: string; scopes: string[] }) => request<{ token: string; item: APIToken }>("/api/me/api-tokens", { method: "POST", body: JSON.stringify(payload) }),
  updateApiToken: (id: string, payload: { name?: string; expiresAt?: string; disabled?: boolean; scopes?: string[] }) => request<APIToken>(`/api/me/api-tokens/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteApiToken: (id: string) => request<{ ok: boolean }>(`/api/me/api-tokens/${id}`, { method: "DELETE" }),
  setupTwoFactor: () => request<{ secret: string; otpauthUrl: string }>("/api/me/2fa/setup", { method: "POST" }),
  enableTwoFactor: (code: string) => request<{ user: User }>("/api/me/2fa/enable", { method: "POST", body: JSON.stringify({ code }) }),
  disableTwoFactor: (code: string) => request<{ user: User }>("/api/me/2fa/disable", { method: "POST", body: JSON.stringify({ code }) }),
  contacts: () => request<ListResponse<Contact>>("/api/me/contacts"),
  createContact: (payload: { name: string; email: string; note: string }) => request<Contact>("/api/me/contacts", { method: "POST", body: JSON.stringify(payload) }),
  deleteContact: (id: string) => request<{ ok: boolean }>(`/api/me/contacts/${id}`, { method: "DELETE" }),
  signatures: () => request<ListResponse<MailSignature>>("/api/me/signatures"),
  createSignature: (payload: { mailboxId: string; name: string; content: string; isDefault: boolean }) => request<MailSignature>("/api/me/signatures", { method: "POST", body: JSON.stringify(payload) }),
  updateSignature: (id: string, payload: { mailboxId: string; name: string; content: string; isDefault: boolean }) => request<MailSignature>(`/api/me/signatures/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  setDefaultSignature: (id: string) => request<MailSignature>(`/api/me/signatures/${id}/default`, { method: "POST" }),
  deleteSignature: (id: string) => request<{ ok: boolean }>(`/api/me/signatures/${id}`, { method: "DELETE" }),
  defaultSignature: (mailboxId?: string) => request<{ signature: MailSignature | null }>(`/api/me/signatures/default${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  rules: () => request<ListResponse<MailRule>>("/api/me/rules"),
  createRule: (payload: { mailboxId: string; name: string; matchMode: "all" | "any"; conditions: MailRuleCondition[]; actions: MailRuleAction[]; applyToExisting: boolean; stopProcessing: boolean; enabled: boolean }) => request<MailRule>("/api/me/rules", { method: "POST", body: JSON.stringify(payload) }),
  deleteRule: (id: string) => request<{ ok: boolean }>(`/api/me/rules/${id}`, { method: "DELETE" }),
  telegramSettings: () => request<TelegramSettings>("/api/me/telegram"),
  saveTelegramSettings: (payload: { botToken?: string; chatId: string; enabled: boolean }) => request<TelegramSettings>("/api/me/telegram", { method: "POST", body: JSON.stringify(payload) }),
  deleteTelegramSettings: () => request<{ ok: boolean }>("/api/me/telegram", { method: "DELETE" }),
  testTelegramSettings: () => request<{ ok: boolean }>("/api/me/telegram/test", { method: "POST" }),
  forwardAddresses: () => request<ListResponse<ForwardAddress>>("/api/me/forward-addresses"),
  requestForwardAddressVerification: (email: string) => request<ForwardAddress>("/api/me/forward-addresses/request", { method: "POST", body: JSON.stringify({ email }) }),
  verifyForwardAddress: (id: string, code: string) => request<ForwardAddress>(`/api/me/forward-addresses/${id}/verify`, { method: "POST", body: JSON.stringify({ code }) }),
  deleteForwardAddress: (id: string) => request<{ ok: boolean }>(`/api/me/forward-addresses/${id}`, { method: "DELETE" }),
  blockedSenders: () => request<ListResponse<BlockedSender>>("/api/me/blocked-senders"),
  createBlockedSender: (payload: { mailboxId: string; email: string; reason: string }) => request<BlockedSender>("/api/me/blocked-senders", { method: "POST", body: JSON.stringify(payload) }),
  deleteBlockedSender: (id: string) => request<{ ok: boolean }>(`/api/me/blocked-senders/${id}`, { method: "DELETE" }),
  mailStats: (mailboxId?: string) => request<MailStats>(`/api/me/stats${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  cleanupMail: (payload: { mailboxId: string; target: "empty-trash" | "empty-spam" | "archive-read-inbox" }) => request<{ ok: boolean; affected: number }>("/api/me/cleanup", { method: "POST", body: JSON.stringify(payload) }),
  mailboxApplyOptions: () => request<MailboxApplyOptions>("/api/me/mailbox-apply-options"),
  applyMailbox: (payload: { domainId: string; localPart: string; displayName: string }) => request<Mailbox>("/api/me/mailboxes/apply", { method: "POST", body: JSON.stringify(payload) }),
  shareUsers: (q: string) => request<ListResponse<ShareUser>>(`/api/me/share-users?q=${encodeURIComponent(q)}`),
  mailboxShares: (mailboxId?: string) => request<ListResponse<MailboxShare>>(`/api/me/mailbox-shares${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  receivedMailboxShares: () => request<ListResponse<MailboxShare>>("/api/me/mailbox-shares/received"),
  createMailboxShare: (payload: MailboxSharePayload) => request<MailboxShare>("/api/me/mailbox-shares", { method: "POST", body: JSON.stringify(payload) }),
  updateMailboxShare: (id: string, payload: MailboxShareUpdatePayload) => request<MailboxShare>(`/api/me/mailbox-shares/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteMailboxShare: (id: string) => request<{ ok: boolean }>(`/api/me/mailbox-shares/${id}`, { method: "DELETE" }),
  leaveMailboxShare: (id: string) => request<{ ok: boolean }>(`/api/me/mailbox-shares/${id}/leave`, { method: "DELETE" }),
  mailboxShareAudit: (id: string) => request<ListResponse<MailboxShareAuditEvent>>(`/api/me/mailbox-shares/${id}/audit`),
  notifications: () => request<ListResponse<UserNotification>>("/api/me/notifications"),
  readNotification: (id: string) => request<{ ok: boolean }>(`/api/me/notifications/${id}/read`, { method: "POST" }),
  externalImapAccounts: (mailboxId?: string) => request<ListResponse<ExternalImapAccount>>(`/api/me/external-imap-accounts${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  createExternalImapAccount: (payload: ExternalImapAccountPayload) => request<ExternalImapAccount>("/api/me/external-imap-accounts", { method: "POST", body: JSON.stringify(payload) }),
  updateExternalImapAccount: (id: string, payload: ExternalImapAccountPayload) => request<ExternalImapAccount>(`/api/me/external-imap-accounts/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteExternalImapAccount: (id: string) => request<{ ok: boolean }>(`/api/me/external-imap-accounts/${id}`, { method: "DELETE" }),
  startExternalImapOAuth: (provider: ExternalImapOAuthProvider, payload: ExternalImapOAuthStartPayload) => request<{ url: string }>(`/api/me/external-imap-oauth/${provider}/start`, { method: "POST", body: JSON.stringify(payload) }),
  testExternalImapAccount: (id: string) => request<{ ok: boolean; folders: number }>(`/api/me/external-imap-accounts/${id}/test`, { method: "POST", timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  externalImapSyncRuns: (id: string) => request<ListResponse<ExternalImapSyncRun>>(`/api/me/external-imap-accounts/${id}/runs`),
  syncExternalImapAccount: (id: string) => request<ExternalImapSyncRun>(`/api/me/external-imap-accounts/${id}/sync`, { method: "POST", timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  syncExternalImapFolder: (id: string, folder: string) => request<ExternalImapSyncRun>(`/api/me/external-imap-accounts/${id}/sync-folder`, { method: "POST", body: JSON.stringify({ folder }), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  adminOverview: () => request<AdminOverview>("/api/admin/overview"),
  users: () => request<ListResponse<AdminUser>>("/api/admin/users"),
  permissionGroups: () => request<ListResponse<PermissionGroup> & { catalog: PermissionInfo[] }>("/api/admin/permission-groups"),
  createPermissionGroup: (payload: { name: string; description: string; permissions: PermissionKey[]; limits: PermissionLimits }) => request<PermissionGroup>("/api/admin/permission-groups", { method: "POST", body: JSON.stringify(payload) }),
  updatePermissionGroup: (id: string, payload: { name: string; description: string; permissions: PermissionKey[]; limits: PermissionLimits }) => request<PermissionGroup>(`/api/admin/permission-groups/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  defaultPermissionLimits: () => request<PermissionLimits>("/api/admin/permission-limits/defaults"),
  deletePermissionGroup: (id: string) => request<{ ok: boolean }>(`/api/admin/permission-groups/${id}`, { method: "DELETE" }),
  createUser: (payload: { email: string; displayName: string; role: "admin" | "user"; password: string; disabled: boolean; permissionGroupIds?: string[] }) => request<AdminUser>("/api/admin/users", { method: "POST", body: JSON.stringify(payload) }),
  updateUser: (id: string, payload: { displayName: string; role: "admin" | "user"; disabled: boolean; permissionGroupIds?: string[] }) => request<AdminUser>(`/api/admin/users/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  resetUserPassword: (id: string, password: string) => request<{ ok: boolean }>(`/api/admin/users/${id}/password`, { method: "POST", body: JSON.stringify({ password }) }),
  deleteUser: (id: string) => request<{ ok: boolean }>(`/api/admin/users/${id}`, { method: "DELETE" }),
  domains: () => request<ListResponse<Domain>>("/api/admin/domains"),
  createDomain: (name: string) => request<Domain>("/api/admin/domains", { method: "POST", body: JSON.stringify({ name }) }),
  updateDomain: (id: string, payload: { status: string }) => request<Domain>(`/api/admin/domains/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteDomain: (id: string) => request<{ ok: boolean }>(`/api/admin/domains/${id}`, { method: "DELETE" }),
  mailboxes: () => request<ListResponse<Mailbox>>("/api/admin/mailboxes"),
  createMailbox: (payload: { domainId: string; localPart: string; displayName: string; password: string; quotaMb: number; role: "admin" | "user"; ownerEmail?: string; userId?: string }) => request<Mailbox>("/api/admin/mailboxes", { method: "POST", body: JSON.stringify(payload) }),
  updateMailbox: (id: string, payload: { userId: string; displayName: string; quotaMb: number; status: string }) => request<Mailbox>(`/api/admin/mailboxes/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteMailbox: (id: string) => request<{ ok: boolean }>(`/api/admin/mailboxes/${id}`, { method: "DELETE" }),
  aliases: () => request<ListResponse<Alias>>("/api/admin/aliases"),
  createAlias: (payload: { domainId: string; source: string; destination: string; enabled: boolean }) => request<Alias>("/api/admin/aliases", { method: "POST", body: JSON.stringify(payload) }),
  updateAlias: (id: string, payload: { source: string; destination: string; enabled: boolean }) => request<Alias>(`/api/admin/aliases/${id}`, { method: "POST", body: JSON.stringify(payload) }),
  deleteAlias: (id: string) => request<{ ok: boolean }>(`/api/admin/aliases/${id}`, { method: "DELETE" }),
  adminMessages: (params: { mailboxId?: string; folder?: string; q?: string; cursor?: string } = {}) => {
    const query = new URLSearchParams()
    if (params.mailboxId) query.set("mailboxId", params.mailboxId)
    if (params.folder) query.set("folder", params.folder)
    if (params.q) query.set("q", params.q)
    if (params.cursor) query.set("cursor", params.cursor)
    const suffix = query.toString()
    return request<ListResponse<MailMessage>>(`/api/admin/messages${suffix ? `?${suffix}` : ""}`)
  },
  adminMessage: (id: string) => request<MailMessage>(`/api/admin/messages/${id}`),
  adminSendAudit: (params: { mailboxId?: string; messageId?: string; event?: string; from?: string; to?: string; cursor?: string } = {}) => {
    const query = new URLSearchParams()
    if (params.mailboxId) query.set("mailboxId", params.mailboxId)
    if (params.messageId) query.set("messageId", params.messageId)
    if (params.event) query.set("event", params.event)
    if (params.from) query.set("from", params.from)
    if (params.to) query.set("to", params.to)
    if (params.cursor) query.set("cursor", params.cursor)
    const suffix = query.toString()
    return request<ListResponse<SendQueueAuditEvent>>(`/api/admin/send-audit${suffix ? `?${suffix}` : ""}`)
  },
  adminDeliveryQueue: (params: { queueType?: string; status?: string; page?: number; limit?: number } = {}) => {
    const query = new URLSearchParams()
    if (params.queueType && params.queueType !== "all") query.set("queueType", params.queueType)
    if (params.status && params.status !== "all") query.set("status", params.status)
    if (params.page) query.set("page", String(params.page))
    if (params.limit) query.set("limit", String(params.limit))
    const suffix = query.toString()
    return request<ListResponse<AdminDeliveryQueueItem> & { page: number; limit: number }>(`/api/admin/delivery-queue${suffix ? `?${suffix}` : ""}`)
  },
  retryAdminDeliveryQueue: (queueType: AdminDeliveryQueueItem["queueType"], id: string) => request<{ ok: boolean }>(`/api/admin/delivery-queue/${queueType}/${encodeURIComponent(id)}/retry`, { method: "POST" }),
  cancelAdminDeliveryQueue: (queueType: AdminDeliveryQueueItem["queueType"], id: string) => request<{ ok: boolean }>(`/api/admin/delivery-queue/${queueType}/${encodeURIComponent(id)}`, { method: "DELETE" }),
  systemSettings: () => request<SystemSettings>("/api/admin/settings"),
  registrationInvites: () => request<ListResponse<RegistrationInvite>>("/api/admin/registration-invites"),
  createRegistrationInvite: (payload: { code?: string; maxUses: number }) => request<RegistrationInvite>("/api/admin/registration-invites", { method: "POST", body: JSON.stringify(payload) }),
  deleteRegistrationInvite: (id: string) => request<{ ok: boolean }>(`/api/admin/registration-invites/${id}`, { method: "DELETE" }),
  maildirSyncHealth: () => request<MaildirSyncHealth>("/api/admin/maildir-sync/health"),
  updateSystemSettings: (payload: SystemSettingsPayload) => request<SystemSettings>("/api/admin/settings", { method: "POST", body: JSON.stringify(payload) }),
  testSmtp: (to: string) => request<{ ok: boolean }>("/api/admin/settings/test-smtp", { method: "POST", body: JSON.stringify({ to }), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  mailTemplates: () => request<ListResponse<MailTemplate>>("/api/admin/mail-templates"),
  updateMailTemplate: (key: string, payload: { subject: string; bodyText: string; bodyHtml: string }) => request<MailTemplate>(`/api/admin/mail-templates/${encodeURIComponent(key)}`, { method: "POST", body: JSON.stringify(payload) }),
  resetMailTemplate: (key: string) => request<MailTemplate>(`/api/admin/mail-templates/${encodeURIComponent(key)}/reset`, { method: "POST" }),
  dnsRecords: (domainId: string) => request<{ items: DNSRecord[] }>(`/api/admin/domains/${domainId}/dns-records`),
  checkDns: (domainId: string) => request<DNSCheckResult>(`/api/admin/domains/${domainId}/check-dns`, { method: "POST" }),
  myMailboxes: () => request<ListResponse<Mailbox>>("/api/mail/mailboxes"),
  myOwnedMailboxes: () => request<ListResponse<Mailbox>>("/api/mail/mailboxes?owned=1"),
  externalMailAccounts: () => request<ListResponse<ExternalImapAccount>>("/api/mail/external-accounts"),
  externalFolders: (id: string) => request<ListResponse<ExternalImapFolder>>(`/api/mail/external-accounts/${id}/folders`),
  externalMessages: (id: string, folder: string, cursor = "", q = "", limit = 30) => {
    const params = new URLSearchParams({ folder, cursor, q })
    params.set("limit", String(limit))
    return request<ListResponse<MailMessage>>(`/api/mail/external-accounts/${id}/messages?${params.toString()}`)
  },
  externalMessage: (id: string, remoteId: string) => request<MailMessage>(`/api/mail/external-accounts/${id}/messages/${encodeURIComponent(remoteId)}`),
  markExternalRead: (id: string, remoteId: string, read: boolean) => request<{ ok: boolean }>(`/api/mail/external-accounts/${id}/messages/${encodeURIComponent(remoteId)}/mark-read`, { method: "POST", body: JSON.stringify({ read }) }),
  folders: (mailboxId?: string) => request<ListResponse<MailFolder>>(`/api/mail/folders${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  createFolder: (payload: { mailboxId?: string; name: string }) => {
    const query = payload.mailboxId ? `?mailboxId=${encodeURIComponent(payload.mailboxId)}` : ""
    return request<MailFolder>(`/api/mail/folders${query}`, { method: "POST", body: JSON.stringify({ name: payload.name }) })
  },
  reorderFolders: (payload: { mailboxId?: string; folderIds: string[]; folders?: { id: string; sortOrder: number }[] }) => {
    const query = payload.mailboxId ? `?mailboxId=${encodeURIComponent(payload.mailboxId)}` : ""
    return request<{ ok: boolean }>(`/api/mail/folders/reorder${query}`, { method: "POST", body: JSON.stringify(payload.folders ? { folders: payload.folders } : { folderIds: payload.folderIds }) })
  },
  deleteFolder: (id: string, mailboxId?: string) => request<{ ok: boolean; moved: number }>(`/api/mail/folders/${id}${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`, { method: "DELETE" }),
  labels: (mailboxId?: string) => request<ListResponse<MailLabel>>(`/api/mail/labels${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  createLabel: (payload: { mailboxId?: string; name: string; color?: string }) => {
    const query = payload.mailboxId ? `?mailboxId=${encodeURIComponent(payload.mailboxId)}` : ""
    return request<MailLabel>(`/api/mail/labels${query}`, { method: "POST", body: JSON.stringify({ name: payload.name, color: payload.color || "" }) })
  },
  deleteLabel: (id: string, mailboxId?: string) => request<{ labels: MailLabel[] }>(`/api/mail/labels/${id}${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`, { method: "DELETE" }),
  messages: (folder: string, q = "", cursor = "", mailboxId?: string, limit = 30) => {
    const params = new URLSearchParams({ folder, q, cursor })
    params.set("limit", String(limit))
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/messages?${params.toString()}`)
  },
  threads: (mailboxId?: string) => request<ListResponse<MailMessage>>(`/api/mail/threads${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  messageThread: (id: string) => request<ListResponse<MailMessage>>(`/api/mail/messages/${id}/thread`),
  labelMessages: (labelId: string, q = "", cursor = "", mailboxId?: string, limit = 30) => {
    const params = new URLSearchParams({ labelId, q, cursor })
    params.set("limit", String(limit))
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/messages?${params.toString()}`)
  },
  starredMessages: (q = "", cursor = "", mailboxId?: string, limit = 30) => {
    const params = new URLSearchParams({ q, cursor })
    params.set("limit", String(limit))
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/starred?${params.toString()}`)
  },
  message: (id: string, options: { markRead?: boolean } = {}) => request<MailMessage>(`/api/mail/messages/${id}${options.markRead === false ? "?markRead=0" : ""}`),
  translateMessage: (id: string, targetLanguage: string) => request<MailTranslation>(`/api/mail/messages/${id}/translate`, { method: "POST", body: JSON.stringify({ targetLanguage }), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  translateExternalMessage: (id: string, remoteId: string, targetLanguage: string) => request<MailTranslation>(`/api/mail/external-accounts/${id}/messages/${encodeURIComponent(remoteId)}/translate`, { method: "POST", body: JSON.stringify({ targetLanguage }), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  send: (payload: SendPayload) => request<MailMessage>("/api/mail/send", { method: "POST", body: JSON.stringify(payload), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  scheduledSends: (mailboxId?: string) => request<ListResponse<ScheduledSend>>(`/api/mail/scheduled-sends${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  scheduleSend: (payload: ScheduleSendPayload) => request<ScheduledSend>("/api/mail/schedule-send", { method: "POST", body: JSON.stringify(payload), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  cancelScheduledSend: (id: string) => request<{ ok: boolean }>(`/api/mail/schedule-send/${id}`, { method: "DELETE" }),
  sendQueue: (params: { mailboxId?: string; status?: SendQueueStatus | "all"; cursor?: string; messageId?: string; recipient?: string; from?: string; to?: string } = {}) => {
    const query = new URLSearchParams()
    if (params.mailboxId) query.set("mailboxId", params.mailboxId)
    if (params.status && params.status !== "all") query.set("status", params.status)
    if (params.cursor) query.set("cursor", params.cursor)
    if (params.messageId) query.set("messageId", params.messageId)
    if (params.recipient) query.set("recipient", params.recipient)
    if (params.from) query.set("from", params.from)
    if (params.to) query.set("to", params.to)
    const suffix = query.toString()
    return request<ListResponse<SendQueueItem>>(`/api/mail/send-queue${suffix ? `?${suffix}` : ""}`)
  },
  sendQueueAudit: (id: string) => request<ListResponse<SendQueueAuditEvent>>(`/api/mail/send-queue/${id}/audit`),
  retrySendQueue: (id: string) => request<SendQueueItem>(`/api/mail/send-queue/${id}/retry`, { method: "POST", timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  cancelSendQueue: (id: string) => request<SendQueueItem>(`/api/mail/send-queue/${id}`, { method: "DELETE" }),
  saveDraft: (payload: DraftPayload, id?: string) => request<MailMessage>(id ? `/api/mail/drafts/${id}` : "/api/mail/drafts", { method: "POST", body: JSON.stringify(payload), timeoutMs: MAIL_DELIVERY_TIMEOUT_MS }),
  deleteDraft: (id: string) => request<{ ok: boolean }>(`/api/mail/drafts/${id}`, { method: "DELETE" }),
  markRead: (id: string, read: boolean) => request<{ ok: boolean }>(`/api/mail/messages/${id}/mark-read`, { method: "POST", body: JSON.stringify({ read }) }),
  star: (id: string, starred: boolean) => request<{ ok: boolean }>(`/api/mail/messages/${id}/star`, { method: "POST", body: JSON.stringify({ starred }) }),
  addLabel: (id: string, payload: { name: string; color?: string }) => request<{ labels: MailLabel[] }>(`/api/mail/messages/${id}/labels`, { method: "POST", body: JSON.stringify(payload) }),
  removeLabel: (id: string, labelID: string) => request<{ labels: MailLabel[] }>(`/api/mail/messages/${id}/labels/${labelID}`, { method: "DELETE" }),
  move: (id: string, folder: string) => request<{ ok: boolean }>(`/api/mail/messages/${id}/move`, { method: "POST", body: JSON.stringify({ folder }) }),
  delete: (id: string) => request<{ ok: boolean }>(`/api/mail/messages/${id}`, { method: "DELETE" }),
}



