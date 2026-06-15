export type User = { id: string; email: string; displayName: string; role: "admin" | "user"; disabled: boolean; twoFactorEnabled: boolean; createdAt: string }
export type AdminUser = User & { mailboxCount: number; mailboxes?: string[] }
export type AdminOverview = { users: number; activeUsers: number; domains: number; mailboxes: number; activeMailboxes: number; aliases: number; messages: number; unreadMessages: number; storageBytes: number }
export type Domain = { id: string; name: string; status: string; dkimSelector: string; dkimPublicKey?: string; dnsStatus: string; dnsCheckedAt?: string; createdAt: string }
export type Mailbox = { id: string; userId: string; userEmail?: string; domainId: string; localPart: string; address: string; displayName: string; quotaMb: number; status: string; createdAt: string }
export type Alias = { id: string; domainId: string; source: string; destination: string; enabled: boolean; createdAt: string }
export type MailFolder = { id: string; name: string; role: string; unreadCount: number; totalCount: number }
export type Attachment = { id: string; messageId: string; filename: string; contentType: string; sizeBytes: number; createdAt: string }
export type MailLabel = { id: string; mailboxId?: string; name: string; color: string; messageCount?: number }
export type MailMessage = {
  id: string; mailboxId?: string; mailboxAddress?: string; ownerEmail?: string; recipientAddress?: string; folderId: string; folder: string; messageUid: string; messageId: string; subject: string; from: string; to: string[]; cc: string[]; bcc?: string[]; sentAt: string; receivedAt: string; snippet: string; bodyText?: string; bodyHtml?: string; isRead: boolean; isStarred: boolean; hasAttachments: boolean; sizeBytes: number; attachments?: Attachment[]
  labels?: MailLabel[]
}
export type DNSRecord = { type: string; name: string; value: string; ttl: number }
export type DNSCheckResult = { domain: string; status: string; checks: Record<string, { ok: boolean; message: string; found?: string[] }> }
export type ListResponse<T> = { items: T[]; nextCursor?: string }
export type SendPayload = { mailboxId?: string; to: string[]; cc: string[]; bcc: string[]; subject: string; text: string; html: string; attachments: { filename: string; contentType: string; contentBase64: string }[] }
export type Contact = { id: string; name: string; email: string; note: string; createdAt: string }
export type MailRuleCondition = { field: "from" | "to" | "subject" | "body"; operator: "contains" | "not-contains" | "equals" | "not-equals" | "starts-with" | "ends-with"; value: string }
export type MailRuleAction = { type: "archive" | "trash" | "star" | "mark-read" | "label" | "move"; value?: string; labelId?: string }
export type MailRule = { id: string; mailboxId: string; name: string; matchMode: "all" | "any"; conditions: MailRuleCondition[]; actions: MailRuleAction[]; applyToExisting: boolean; stopProcessing: boolean; fromContains: string; subjectContains: string; action: "archive" | "trash" | "star" | "mark-read" | "label" | "move"; enabled: boolean; createdAt: string; appliedExistingCount?: number }
export type BlockedSender = { id: string; mailboxId: string; email: string; reason: string; createdAt: string }
export type MailStats = { totalMessages: number; unreadMessages: number; starredMessages: number; attachmentCount: number; storageBytes: number; byFolder: { folder: string; role: string; count: number; unread: number; bytes: number }[] }
export type MailTemplate = { key: string; name: string; subject: string; bodyText: string; bodyHtml: string; updatedAt: string }
export type SystemSettings = {
  publicHostname: string
  publicBaseUrl: string
  smtpHost: string
  smtpPort: string
  smtpUsername: string
  smtpPasswordSet: boolean
  smtpRequireTls: boolean
  maildirRoot: string
  maildirScanSeconds: number
  sessionTtlHours: number
  allowInsecureHttp: boolean
  openRegistration: boolean
  twoFactorEnabled: boolean
  turnstileEnabled: boolean
  turnstileSiteKey: string
  turnstileSecretSet: boolean
  catchAllEnabled: boolean
  mailAutoRefresh: boolean
  mailRefreshSeconds: number
}
export type SystemSettingsPayload = Omit<SystemSettings, "smtpPasswordSet" | "turnstileSecretSet"> & { smtpPassword: string; turnstileSecretKey: string }
export type PublicSettings = { turnstileEnabled: boolean; turnstileSiteKey: string; mailAutoRefresh: boolean; mailRefreshMs: number }
export type LoginPayload = { email?: string; password?: string; turnstileToken?: string; challengeToken?: string; twoFactorCode?: string }
export type LoginResponse = { user?: User; twoFactorRequired?: boolean; challengeToken?: string }

const REQUEST_TIMEOUT_MS = 15_000

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const controller = new AbortController()
  const timeout = window.setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS)
  const externalSignal = init.signal
  if (externalSignal) {
    if (externalSignal.aborted) controller.abort()
    else externalSignal.addEventListener("abort", () => controller.abort(), { once: true })
  }
  try {
    const res = await fetch(path, { credentials: "include", headers: { "Content-Type": "application/json", ...(init.headers || {}) }, ...init, signal: controller.signal })
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
  login: (payload: LoginPayload) => request<LoginResponse>("/api/auth/login", { method: "POST", body: JSON.stringify(payload) }),
  logout: () => request<{ ok: boolean }>("/api/auth/logout", { method: "POST" }),
  me: () => request<{ user: User }>("/api/me"),
  updateProfile: (payload: { displayName: string }) => request<{ user: User }>("/api/me/profile", { method: "POST", body: JSON.stringify(payload) }),
  changePassword: (payload: { currentPassword: string; newPassword: string }) => request<{ ok: boolean }>("/api/me/password", { method: "POST", body: JSON.stringify(payload) }),
  setupTwoFactor: () => request<{ secret: string; otpauthUrl: string }>("/api/me/2fa/setup", { method: "POST" }),
  enableTwoFactor: (code: string) => request<{ user: User }>("/api/me/2fa/enable", { method: "POST", body: JSON.stringify({ code }) }),
  disableTwoFactor: (code: string) => request<{ user: User }>("/api/me/2fa/disable", { method: "POST", body: JSON.stringify({ code }) }),
  contacts: () => request<ListResponse<Contact>>("/api/me/contacts"),
  createContact: (payload: { name: string; email: string; note: string }) => request<Contact>("/api/me/contacts", { method: "POST", body: JSON.stringify(payload) }),
  deleteContact: (id: string) => request<{ ok: boolean }>(`/api/me/contacts/${id}`, { method: "DELETE" }),
  rules: () => request<ListResponse<MailRule>>("/api/me/rules"),
  createRule: (payload: { mailboxId: string; name: string; matchMode: "all" | "any"; conditions: MailRuleCondition[]; actions: MailRuleAction[]; applyToExisting: boolean; stopProcessing: boolean; enabled: boolean }) => request<MailRule>("/api/me/rules", { method: "POST", body: JSON.stringify(payload) }),
  deleteRule: (id: string) => request<{ ok: boolean }>(`/api/me/rules/${id}`, { method: "DELETE" }),
  blockedSenders: () => request<ListResponse<BlockedSender>>("/api/me/blocked-senders"),
  createBlockedSender: (payload: { mailboxId: string; email: string; reason: string }) => request<BlockedSender>("/api/me/blocked-senders", { method: "POST", body: JSON.stringify(payload) }),
  deleteBlockedSender: (id: string) => request<{ ok: boolean }>(`/api/me/blocked-senders/${id}`, { method: "DELETE" }),
  mailStats: (mailboxId?: string) => request<MailStats>(`/api/me/stats${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  cleanupMail: (payload: { mailboxId: string; target: "empty-trash" | "empty-spam" | "archive-read-inbox" }) => request<{ ok: boolean; affected: number }>("/api/me/cleanup", { method: "POST", body: JSON.stringify(payload) }),
  adminOverview: () => request<AdminOverview>("/api/admin/overview"),
  users: () => request<ListResponse<AdminUser>>("/api/admin/users"),
  createUser: (payload: { email: string; displayName: string; role: "admin" | "user"; password: string; disabled: boolean }) => request<AdminUser>("/api/admin/users", { method: "POST", body: JSON.stringify(payload) }),
  updateUser: (id: string, payload: { displayName: string; role: "admin" | "user"; disabled: boolean }) => request<AdminUser>(`/api/admin/users/${id}`, { method: "POST", body: JSON.stringify(payload) }),
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
  systemSettings: () => request<SystemSettings>("/api/admin/settings"),
  updateSystemSettings: (payload: SystemSettingsPayload) => request<SystemSettings>("/api/admin/settings", { method: "POST", body: JSON.stringify(payload) }),
  testSmtp: (to: string) => request<{ ok: boolean }>("/api/admin/settings/test-smtp", { method: "POST", body: JSON.stringify({ to }) }),
  mailTemplates: () => request<ListResponse<MailTemplate>>("/api/admin/mail-templates"),
  updateMailTemplate: (key: string, payload: { subject: string; bodyText: string; bodyHtml: string }) => request<MailTemplate>(`/api/admin/mail-templates/${encodeURIComponent(key)}`, { method: "POST", body: JSON.stringify(payload) }),
  resetMailTemplate: (key: string) => request<MailTemplate>(`/api/admin/mail-templates/${encodeURIComponent(key)}/reset`, { method: "POST" }),
  dnsRecords: (domainId: string) => request<{ items: DNSRecord[] }>(`/api/admin/domains/${domainId}/dns-records`),
  checkDns: (domainId: string) => request<DNSCheckResult>(`/api/admin/domains/${domainId}/check-dns`, { method: "POST" }),
  myMailboxes: () => request<ListResponse<Mailbox>>("/api/mail/mailboxes"),
  folders: (mailboxId?: string) => request<ListResponse<MailFolder>>(`/api/mail/folders${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  labels: (mailboxId?: string) => request<ListResponse<MailLabel>>(`/api/mail/labels${mailboxId ? `?mailboxId=${encodeURIComponent(mailboxId)}` : ""}`),
  createLabel: (payload: { mailboxId?: string; name: string; color?: string }) => {
    const query = payload.mailboxId ? `?mailboxId=${encodeURIComponent(payload.mailboxId)}` : ""
    return request<MailLabel>(`/api/mail/labels${query}`, { method: "POST", body: JSON.stringify({ name: payload.name, color: payload.color || "" }) })
  },
  messages: (folder: string, q = "", cursor = "", mailboxId?: string) => {
    const params = new URLSearchParams({ folder, q, cursor })
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/messages?${params.toString()}`)
  },
  labelMessages: (labelId: string, q = "", cursor = "", mailboxId?: string) => {
    const params = new URLSearchParams({ labelId, q, cursor })
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/messages?${params.toString()}`)
  },
  starredMessages: (q = "", cursor = "", mailboxId?: string) => {
    const params = new URLSearchParams({ q, cursor })
    if (mailboxId) params.set("mailboxId", mailboxId)
    return request<ListResponse<MailMessage>>(`/api/mail/starred?${params.toString()}`)
  },
  message: (id: string, options: { markRead?: boolean } = {}) => request<MailMessage>(`/api/mail/messages/${id}${options.markRead === false ? "?markRead=0" : ""}`),
  send: (payload: SendPayload) => request<MailMessage>("/api/mail/send", { method: "POST", body: JSON.stringify(payload) }),
  markRead: (id: string, read: boolean) => request<{ ok: boolean }>(`/api/mail/messages/${id}/mark-read`, { method: "POST", body: JSON.stringify({ read }) }),
  star: (id: string, starred: boolean) => request<{ ok: boolean }>(`/api/mail/messages/${id}/star`, { method: "POST", body: JSON.stringify({ starred }) }),
  addLabel: (id: string, payload: { name: string; color?: string }) => request<{ labels: MailLabel[] }>(`/api/mail/messages/${id}/labels`, { method: "POST", body: JSON.stringify(payload) }),
  removeLabel: (id: string, labelID: string) => request<{ labels: MailLabel[] }>(`/api/mail/messages/${id}/labels/${labelID}`, { method: "DELETE" }),
  move: (id: string, folder: string) => request<{ ok: boolean }>(`/api/mail/messages/${id}/move`, { method: "POST", body: JSON.stringify({ folder }) }),
  delete: (id: string) => request<{ ok: boolean }>(`/api/mail/messages/${id}`, { method: "DELETE" }),
}



