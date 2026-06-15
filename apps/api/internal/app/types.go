package app

import "time"

type User struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	DisplayName      string    `json:"displayName"`
	Role             string    `json:"role"`
	Disabled         bool      `json:"disabled"`
	TwoFactorEnabled bool      `json:"twoFactorEnabled"`
	CreatedAt        time.Time `json:"createdAt"`
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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	UnreadCount int    `json:"unreadCount"`
	TotalCount  int    `json:"totalCount"`
}

type MailLabel struct {
	ID           string `json:"id"`
	MailboxID    string `json:"mailboxId,omitempty"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	MessageCount int    `json:"messageCount,omitempty"`
}

type MailMessage struct {
	ID             string       `json:"id"`
	MailboxID      string       `json:"mailboxId,omitempty"`
	MailboxAddress string       `json:"mailboxAddress,omitempty"`
	OwnerEmail     string       `json:"ownerEmail,omitempty"`
	RecipientAddr  string       `json:"recipientAddress,omitempty"`
	FolderID       string       `json:"folderId"`
	Folder         string       `json:"folder"`
	MessageUID     string       `json:"messageUid"`
	MessageID      string       `json:"messageId"`
	Subject        string       `json:"subject"`
	From           string       `json:"from"`
	To             []string     `json:"to"`
	CC             []string     `json:"cc"`
	BCC            []string     `json:"bcc,omitempty"`
	SentAt         time.Time    `json:"sentAt"`
	ReceivedAt     time.Time    `json:"receivedAt"`
	Snippet        string       `json:"snippet"`
	BodyText       string       `json:"bodyText,omitempty"`
	BodyHTML       string       `json:"bodyHtml,omitempty"`
	IsRead         bool         `json:"isRead"`
	IsStarred      bool         `json:"isStarred"`
	HasAttachments bool         `json:"hasAttachments"`
	SizeBytes      int64        `json:"sizeBytes"`
	Labels         []MailLabel  `json:"labels,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
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

type MailRule struct {
	ID              string    `json:"id"`
	UserID          string    `json:"userId,omitempty"`
	MailboxID       string    `json:"mailboxId"`
	Name            string    `json:"name"`
	FromContains    string    `json:"fromContains"`
	SubjectContains string    `json:"subjectContains"`
	Action          string    `json:"action"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"createdAt"`
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
	StorageBytes    int64                  `json:"storageBytes"`
	ByFolder        []MailStatsFolderCount `json:"byFolder"`
}

type MailStatsFolderCount struct {
	Folder string `json:"folder"`
	Role   string `json:"role"`
	Count  int64  `json:"count"`
	Unread int64  `json:"unread"`
	Bytes  int64  `json:"bytes"`
}
