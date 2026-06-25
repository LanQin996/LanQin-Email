export type PermissionKey =
  | "mail.access"
  | "mail.messages.read"
  | "mail.messages.send"
  | "mail.messages.drafts"
  | "mail.messages.schedule"
  | "mail.messages.organize"
  | "mail.labels.manage"
  | "mail.attachments.download"
  | "mail.contacts.manage"
  | "mail.signatures.manage"
  | "mail.rules.manage"
  | "mail.blocked_senders.manage"
  | "mail.stats.view"
  | "mail.mailboxes.apply"
  | "admin.overview.view"
  | "admin.users.view"
  | "admin.users.create"
  | "admin.users.update"
  | "admin.users.delete"
  | "admin.users.reset_password"
  | "admin.permission_groups.view"
  | "admin.permission_groups.create"
  | "admin.permission_groups.update"
  | "admin.permission_groups.delete"
  | "admin.domains.view"
  | "admin.domains.create"
  | "admin.domains.update"
  | "admin.domains.delete"
  | "admin.dns.view"
  | "admin.dns.check"
  | "admin.mailboxes.view"
  | "admin.mailboxes.create"
  | "admin.mailboxes.update"
  | "admin.mailboxes.delete"
  | "admin.aliases.view"
  | "admin.aliases.create"
  | "admin.aliases.update"
  | "admin.aliases.delete"
  | "admin.messages.view"
  | "admin.messages.read"
  | "admin.messages.attachments"
  | "admin.settings.view"
  | "admin.settings.update"
  | "admin.settings.test_smtp"
  | "admin.templates.view"
  | "admin.templates.update"
  | "admin.templates.reset"
export type PermissionInfo = { key: PermissionKey; label: string; description: string; category: string }
export type PermissionLimits = { maxAttachmentMb: number; smtpDailyLimit: number; smtpMinuteLimit: number; imapMinuteLimit: number; pop3MinuteLimit: number }
export type PermissionGroupSummary = { id: string; name: string }
export type PermissionGroup = { id: string; name: string; description: string; permissions: PermissionKey[]; limits: PermissionLimits; system: boolean; userCount: number; createdAt: string; updatedAt: string }
export type User = { id: string; email: string; displayName: string; role: "admin" | "user"; disabled: boolean; protected: boolean; twoFactorEnabled: boolean; permissions: PermissionKey[]; limits: PermissionLimits; permissionGroupIds: string[]; permissionGroups: PermissionGroupSummary[]; createdAt: string }
export type AdminUser = User & { mailboxCount: number; mailboxes?: string[] }
export type AdminOverview = { users: number; activeUsers: number; domains: number; mailboxes: number; activeMailboxes: number; aliases: number; messages: number; unreadMessages: number; storageBytes: number }
export type Domain = { id: string; name: string; status: string; dkimSelector: string; dkimPublicKey?: string; dnsStatus: string; dnsCheckedAt?: string; createdAt: string }
export type Mailbox = { id: string; userId: string; userEmail?: string; domainId: string; localPart: string; address: string; displayName: string; quotaMb: number; status: string; createdAt: string }
export type Alias = { id: string; domainId: string; source: string; destination: string; enabled: boolean; createdAt: string }
export type MailFolder = { id: string; name: string; role: string; sortOrder: number; unreadCount: number; totalCount: number; uidValidity: number; uidNext: number; highestModseq: number }
export type Attachment = { id: string; messageId: string; filename: string; contentType: string; sizeBytes: number; createdAt: string }
export type MailLabel = { id: string; mailboxId?: string; name: string; color: string; messageCount?: number }
export type MailMessage = {
  id: string; mailboxId?: string; mailboxAddress?: string; ownerEmail?: string; recipientAddress?: string; folderId: string; folder: string; messageUid: string; imapUid: number; imapModseq: number; messageId: string; subject: string; from: string; fromName?: string; to: string[]; cc: string[]; bcc?: string[]; sentAt: string; receivedAt: string; snippet: string; bodyText?: string; bodyHtml?: string; isRead: boolean; isStarred: boolean; hasAttachments: boolean; sizeBytes: number; attachments?: Attachment[]
  labels?: MailLabel[]
  sendQueueId?: string
  sendQueueStatus?: SendQueueStatus
  externalAccountId?: string
}
export type DNSRecord = { type: string; name: string; value: string; ttl: number }
export type DNSCheckResult = { domain: string; status: string; checks: Record<string, { ok: boolean; message: string; found?: string[] }> }
export type ListResponse<T> = { items: T[]; nextCursor?: string }
export type SendPayload = { mailboxId?: string; to: string[]; cc: string[]; bcc: string[]; subject: string; text: string; html: string; attachments: { filename: string; contentType: string; contentBase64: string }[] }
export type DraftPayload = Omit<SendPayload, "attachments"> & { attachments?: SendPayload["attachments"] }
export type ScheduleSendPayload = SendPayload & { draftId?: string; sendAt: string }
export type ScheduledSend = { id: string; mailboxId: string; draftId?: string; subject: string; to: string[]; snippet: string; sendAt: string; status: "pending" | "sending" | "sent" | "failed" | "cancelled"; error?: string; createdAt: string; updatedAt: string; sentAt?: string }
export type SendQueueStatus = "queued" | "sending" | "delivered" | "failed" | "canceled"
export type SendQueueItem = {
  id: string
  mailboxId: string
  sentMessageId?: string
  messageId?: string
  mailFrom?: string
  headerFrom?: string
  subject: string
  recipients: string[]
  source: string
  status: SendQueueStatus
  attemptCount: number
  maxAttempts: number
  nextAttemptAt?: string
  lastError?: string
  error?: string
  failureReason?: string
  createdAt: string
  updatedAt: string
  deliveredAt?: string
}
export type SendQueueAuditEvent = {
  id: string
  queueId?: string
  mailboxId?: string
  mailboxAddress?: string
  sentMessageId?: string
  messageId?: string
  source?: string
  status?: SendQueueStatus
  event?: string
  eventType?: string
  mailFrom?: string
  headerFrom?: string
  recipients?: string[]
  message?: string
  error?: string
  attemptCount?: number
  createdAt: string
}
export type Contact = { id: string; name: string; email: string; note: string; createdAt: string }
export type MailSignature = { id: string; mailboxId: string; name: string; content: string; isDefault: boolean; createdAt: string; updatedAt: string }
export type MailRuleConditionField = "from" | "to" | "cc" | "subject" | "body" | "attachment" | "size" | "date"
export type MailRuleConditionOperator = "contains" | "not-contains" | "equals" | "not-equals" | "starts-with" | "ends-with" | "gt" | "gte" | "lt" | "lte" | "before" | "after" | "on"
export type MailRuleCondition = { field?: MailRuleConditionField; operator?: MailRuleConditionOperator; value?: string; matchMode?: "all" | "any"; conditions?: MailRuleCondition[] }
export type MailRuleAction = { type: "archive" | "trash" | "star" | "mark-read" | "label" | "move"; value?: string; labelId?: string }
export type MailRule = { id: string; mailboxId: string; name: string; matchMode: "all" | "any"; conditions: MailRuleCondition[]; actions: MailRuleAction[]; applyToExisting: boolean; stopProcessing: boolean; fromContains: string; subjectContains: string; action: "archive" | "trash" | "star" | "mark-read" | "label" | "move"; enabled: boolean; createdAt: string; appliedExistingCount?: number }
export type BlockedSender = { id: string; mailboxId: string; email: string; reason: string; createdAt: string }
export type MailStats = { totalMessages: number; unreadMessages: number; starredMessages: number; attachmentCount: number; attachmentBytes: number; storageBytes: number; quotaBytes: number; quotaUsedPct: number; byFolder: { folder: string; role: string; count: number; unread: number; bytes: number }[] }
export type ExternalImapStorageMode = "local" | "remote"
export type ExternalImapTlsMode = "tls" | "starttls" | "plain"
export type ExternalImapAuthMode = "password" | "oauth2"
export type ExternalImapAccount = { id: string; mailboxId: string; name: string; host: string; port: number; tlsMode: ExternalImapTlsMode; username: string; authMode: ExternalImapAuthMode; oauthProvider?: ExternalImapOAuthProvider; oauthEmail?: string; oauthConfigured?: boolean; storageMode: ExternalImapStorageMode; syncReadState: boolean; enabled: boolean; lastSyncAt?: string; lastStatus: string; lastError?: string; createdAt: string; updatedAt: string }
export type ExternalImapAccountPayload = { mailboxId: string; name: string; host: string; port: number; tlsMode: ExternalImapTlsMode; username: string; password?: string; storageMode: ExternalImapStorageMode; syncReadState: boolean; enabled: boolean }
export type ExternalImapOAuthProvider = "gmail" | "outlook"
export type ExternalImapOAuthStartPayload = { mailboxId: string; name?: string; email?: string; storageMode: ExternalImapStorageMode; syncReadState: boolean; enabled: boolean }
export type ExternalImapFolder = { name: string; role: string; unreadCount: number; totalCount: number }
export type ExternalImapSyncRun = { id: string; accountId: string; folder?: string; status: string; imported: number; skipped: number; failed: number; error?: string; startedAt: string; finishedAt?: string }
export type MailTemplate = { key: string; name: string; subject: string; bodyText: string; bodyHtml: string; updatedAt: string }
export type MailboxApplyOptions = { enabled: boolean; domains: Domain[]; reservedPrefixes?: string[] }
export type MaildirSyncCounts = { filesScanned: number; imported: number; backfilled: number; cleaned: number; fileErrors: number }
export type MaildirSyncRun = { startedAt: string; finishedAt?: string; durationMs: number; status: "running" | "success" | "partial" | "error"; error?: string; counts: MaildirSyncCounts }
export type MaildirSyncHealth = {
  configured: boolean
  enabled: boolean
  root: string
  scanSeconds: number
  workerStarted: boolean
  running: boolean
  lastRun?: MaildirSyncRun
  lastError?: string
  nextRunAt?: string
  recentErrors: string[]
  summary: MaildirSyncCounts
}
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
  userMailboxApplyEnabled: boolean
  userMailboxDomainIds: string[]
  reservedMailboxPrefixes: string
}
export type SystemSettingsPayload = Omit<SystemSettings, "smtpPasswordSet" | "turnstileSecretSet"> & { smtpPassword: string; turnstileSecretKey: string }
export type PublicDomain = { id: string; name: string }
export type PublicSettings = { openRegistration: boolean; turnstileEnabled: boolean; turnstileSiteKey: string; publicHostname: string; mailAutoRefresh: boolean; mailRefreshMs: number; mailboxDomains?: PublicDomain[] }
export type LoginPayload = { email?: string; password?: string; turnstileToken?: string; challengeToken?: string; twoFactorCode?: string }
export type LoginResponse = { user?: User; twoFactorRequired?: boolean; challengeToken?: string }
export type RegisterPayload = { email: string; displayName: string; password: string; turnstileToken?: string; domainId?: string; localPart?: string }
