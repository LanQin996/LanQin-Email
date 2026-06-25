package app

import "time"

type User struct {
	ID                 string                   `json:"id"`
	Email              string                   `json:"email"`
	DisplayName        string                   `json:"displayName"`
	Role               string                   `json:"role"`
	Disabled           bool                     `json:"disabled"`
	Protected          bool                     `json:"protected"`
	TwoFactorEnabled   bool                     `json:"twoFactorEnabled"`
	Permissions        []string                 `json:"permissions"`
	Limits             PermissionLimits         `json:"limits"`
	PermissionGroupIDs []string                 `json:"permissionGroupIds"`
	PermissionGroups   []PermissionGroupSummary `json:"permissionGroups"`
	CreatedAt          time.Time                `json:"createdAt"`
}

type AdminUser struct {
	User
	MailboxCount int      `json:"mailboxCount"`
	Mailboxes    []string `json:"mailboxes"`
}

type Domain struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	DKIMSelector  string     `json:"dkimSelector"`
	DKIMPublicKey string     `json:"dkimPublicKey,omitempty"`
	DNSStatus     string     `json:"dnsStatus"`
	DNSCheckedAt  *time.Time `json:"dnsCheckedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type Mailbox struct {
	ID          string    `json:"id"`
	UserID      string    `json:"userId"`
	UserEmail   string    `json:"userEmail,omitempty"`
	DomainID    string    `json:"domainId"`
	LocalPart   string    `json:"localPart"`
	Address     string    `json:"address"`
	DisplayName string    `json:"displayName"`
	QuotaMB     int       `json:"quotaMb"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Alias struct {
	ID          string    `json:"id"`
	DomainID    string    `json:"domainId"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

type MailFolder struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	SortOrder     int    `json:"sortOrder"`
	UnreadCount   int    `json:"unreadCount"`
	TotalCount    int    `json:"totalCount"`
	UIDValidity   int64  `json:"uidValidity"`
	UIDNext       int64  `json:"uidNext"`
	HighestModSeq int64  `json:"highestModseq"`
}

type MailLabel struct {
	ID           string `json:"id"`
	MailboxID    string `json:"mailboxId,omitempty"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	MessageCount int    `json:"messageCount,omitempty"`
}

type MailMessage struct {
	ID             string             `json:"id"`
	MailboxID      string             `json:"mailboxId,omitempty"`
	MailboxAddress string             `json:"mailboxAddress,omitempty"`
	OwnerEmail     string             `json:"ownerEmail,omitempty"`
	RecipientAddr  string             `json:"recipientAddress,omitempty"`
	FolderID       string             `json:"folderId"`
	Folder         string             `json:"folder"`
	MessageUID     string             `json:"messageUid"`
	IMAPUID        int64              `json:"imapUid"`
	IMAPModSeq     int64              `json:"imapModseq"`
	MessageID      string             `json:"messageId"`
	Subject        string             `json:"subject"`
	From           string             `json:"from"`
	FromName       string             `json:"fromName,omitempty"`
	To             []string           `json:"to"`
	CC             []string           `json:"cc"`
	BCC            []string           `json:"bcc,omitempty"`
	SentAt         time.Time          `json:"sentAt"`
	ReceivedAt     time.Time          `json:"receivedAt"`
	Snippet        string             `json:"snippet"`
	BodyText       string             `json:"bodyText,omitempty"`
	BodyHTML       string             `json:"bodyHtml,omitempty"`
	IsRead         bool               `json:"isRead"`
	IsStarred      bool               `json:"isStarred"`
	HasAttachments bool               `json:"hasAttachments"`
	SizeBytes      int64              `json:"sizeBytes"`
	Labels         []MailLabel        `json:"labels,omitempty"`
	Attachments    []Attachment       `json:"attachments,omitempty"`
	Authentication MailAuthentication `json:"authentication"`
}

type MailAuthentication struct {
	AuthenticationResults string `json:"authenticationResults"`
	ReceivedSPF           string `json:"receivedSpf"`
	SPF                   string `json:"spf"`
	DKIM                  string `json:"dkim"`
	DMARC                 string `json:"dmarc"`
}

type Attachment struct {
	ID          string    `json:"id"`
	MessageID   string    `json:"messageId"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	SizeBytes   int64     `json:"sizeBytes"`
	CreatedAt   time.Time `json:"createdAt"`
}

type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

type DNSCheckResult struct {
	Domain string                    `json:"domain"`
	Status string                    `json:"status"`
	Checks map[string]DNSCheckStatus `json:"checks"`
}

type DNSCheckStatus struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Found   []string `json:"found,omitempty"`
}

type Contact struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
}

type MailSignature struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	MailboxID string    `json:"mailboxId"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	IsDefault bool      `json:"isDefault"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type MailRule struct {
	ID                   string              `json:"id"`
	UserID               string              `json:"userId,omitempty"`
	MailboxID            string              `json:"mailboxId"`
	Name                 string              `json:"name"`
	MatchMode            string              `json:"matchMode"`
	Conditions           []MailRuleCondition `json:"conditions"`
	Actions              []MailRuleAction    `json:"actions"`
	ApplyToExisting      bool                `json:"applyToExisting"`
	StopProcessing       bool                `json:"stopProcessing"`
	FromContains         string              `json:"fromContains"`
	SubjectContains      string              `json:"subjectContains"`
	Action               string              `json:"action"`
	Enabled              bool                `json:"enabled"`
	CreatedAt            time.Time           `json:"createdAt"`
	AppliedExistingCount int64               `json:"appliedExistingCount,omitempty"`
}

type MailRuleCondition struct {
	Field      string              `json:"field,omitempty"`
	Operator   string              `json:"operator,omitempty"`
	Value      string              `json:"value,omitempty"`
	MatchMode  string              `json:"matchMode,omitempty"`
	Conditions []MailRuleCondition `json:"conditions,omitempty"`
}

type MailRuleAction struct {
	Type    string `json:"type"`
	Value   string `json:"value,omitempty"`
	LabelID string `json:"labelId,omitempty"`
}

type BlockedSender struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	MailboxID string    `json:"mailboxId"`
	Email     string    `json:"email"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"createdAt"`
}

type MailStats struct {
	TotalMessages   int64                  `json:"totalMessages"`
	UnreadMessages  int64                  `json:"unreadMessages"`
	StarredMessages int64                  `json:"starredMessages"`
	AttachmentCount int64                  `json:"attachmentCount"`
	AttachmentBytes int64                  `json:"attachmentBytes"`
	StorageBytes    int64                  `json:"storageBytes"`
	QuotaBytes      int64                  `json:"quotaBytes"`
	QuotaUsedPct    float64                `json:"quotaUsedPct"`
	ByFolder        []MailStatsFolderCount `json:"byFolder"`
}

type MailStatsFolderCount struct {
	Folder string `json:"folder"`
	Role   string `json:"role"`
	Count  int64  `json:"count"`
	Unread int64  `json:"unread"`
	Bytes  int64  `json:"bytes"`
}

type SendQueueEntry struct {
	ID            string     `json:"id"`
	MailboxID     string     `json:"mailboxId"`
	SentMessageID string     `json:"sentMessageId"`
	MessageID     string     `json:"messageId"`
	Subject       string     `json:"subject"`
	Source        string     `json:"source"`
	MailFrom      string     `json:"mailFrom"`
	HeaderFrom    string     `json:"headerFrom"`
	Recipients    []string   `json:"recipients"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attemptCount"`
	MaxAttempts   int        `json:"maxAttempts"`
	NextAttemptAt time.Time  `json:"nextAttemptAt"`
	LastError     string     `json:"lastError"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
}

type SendAuditEvent struct {
	ID             string    `json:"id"`
	QueueID        string    `json:"queueId"`
	MailboxID      string    `json:"mailboxId"`
	MailboxAddress string    `json:"mailboxAddress,omitempty"`
	SentMessageID  string    `json:"sentMessageId"`
	MessageID      string    `json:"messageId,omitempty"`
	Source         string    `json:"source"`
	Event          string    `json:"event"`
	Status         string    `json:"status"`
	MailFrom       string    `json:"mailFrom"`
	HeaderFrom     string    `json:"headerFrom"`
	Recipients     []string  `json:"recipients"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}
