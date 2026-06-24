package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	netmail "net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtpserver "github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSubmissionMaxRecipients = 200
)

type SubmissionServers struct {
	Plain *smtpserver.Server
	TLS   *smtpserver.Server
}

func (s *SubmissionServers) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.Plain != nil {
		if err := s.Plain.Shutdown(ctx); err != nil && !errors.Is(err, smtpserver.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if s.TLS != nil {
		if err := s.TLS.Shutdown(ctx); err != nil && !errors.Is(err, smtpserver.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *App) NewSubmissionServers(tlsConfig *tls.Config) *SubmissionServers {
	return &SubmissionServers{
		Plain: a.newSubmissionServer(a.cfg.SubmissionAddr, tlsConfig),
		TLS:   a.newSubmissionServer(a.cfg.SubmissionTLSAddr, tlsConfig),
	}
}

func (a *App) newSubmissionServer(addr string, tlsConfig *tls.Config) *smtpserver.Server {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	s := smtpserver.NewServer(submissionBackend{app: a})
	s.Addr = addr
	s.Domain = a.cfg.PublicHostname
	s.TLSConfig = tlsConfig
	s.AllowInsecureAuth = false
	s.MaxRecipients = defaultSubmissionMaxRecipients
	s.MaxMessageBytes = int64(a.cfg.SubmissionMaxMessageMB) * 1024 * 1024
	s.ReadTimeout = smtpSessionTimeout
	s.WriteTimeout = smtpSessionTimeout
	s.ErrorLog = log.New(submissionLogWriter{log: a.log}, "smtp/submission ", 0)
	return s
}

func LoadServerTLSConfig(cfg Config) (*tls.Config, error) {
	certFile, keyFile := strings.TrimSpace(cfg.TLSCertFile), strings.TrimSpace(cfg.TLSKeyFile)
	if certFile == "" || keyFile == "" {
		return nil, errors.New("LANQIN_TLS_CERT_FILE and LANQIN_TLS_KEY_FILE are required when SMTP submission is enabled")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}, nil
}

type submissionLogWriter struct {
	log slogLogger
}

func (w submissionLogWriter) Write(p []byte) (int, error) {
	if w.log != nil {
		w.log.Warn(strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

type slogLogger interface {
	Warn(msg string, args ...any)
}

type submissionBackend struct {
	app *App
}

func (b submissionBackend) NewSession(*smtpserver.Conn) (smtpserver.Session, error) {
	return &submissionSession{app: b.app}, nil
}

type submissionSession struct {
	app        *App
	user       *User
	mailbox    *Mailbox
	mailFrom   string
	recipients []string
}

func (s *submissionSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

func (s *submissionSession) Auth(mech string) (sasl.Server, error) {
	if !strings.EqualFold(mech, sasl.Plain) {
		return nil, smtpserver.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		user, mailbox, err := s.app.authenticateSubmission(context.Background(), username, password)
		if err != nil {
			return smtpserver.ErrAuthFailed
		}
		s.user, s.mailbox = user, mailbox
		return nil
	}), nil
}

func (s *submissionSession) Mail(from string, _ *smtpserver.MailOptions) error {
	if s.user == nil || s.mailbox == nil {
		return smtpserver.ErrAuthRequired
	}
	from = normalizeEmail(from)
	authorized, _, err := s.app.authorizedSender(context.Background(), s.mailbox, from)
	if err != nil || from == "" || from != authorized {
		return smtpError(553, smtpserver.EnhancedCode{5, 7, 1}, "sender must match authenticated mailbox")
	}
	s.mailFrom = from
	s.recipients = nil
	return nil
}

func (s *submissionSession) Rcpt(to string, _ *smtpserver.RcptOptions) error {
	if s.user == nil || s.mailbox == nil {
		return smtpserver.ErrAuthRequired
	}
	to = normalizeEmail(to)
	if to == "" || !strings.Contains(to, "@") {
		return smtpError(501, smtpserver.EnhancedCode{5, 1, 3}, "invalid recipient")
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *submissionSession) Data(r io.Reader) error {
	if s.user == nil || s.mailbox == nil {
		return smtpserver.ErrAuthRequired
	}
	if s.mailFrom == "" || len(s.recipients) == 0 {
		return smtpError(503, smtpserver.EnhancedCode{5, 5, 1}, "missing sender or recipients")
	}
	if err := s.app.submitSMTPMessage(context.Background(), s.user, s.mailbox, s.mailFrom, s.recipients, r); err != nil {
		var smtpErr *smtpserver.SMTPError
		if errors.As(err, &smtpErr) {
			return smtpErr
		}
		return smtpError(451, smtpserver.EnhancedCode{4, 0, 0}, "message submission failed")
	}
	s.Reset()
	return nil
}

func (s *submissionSession) Reset() {
	s.mailFrom = ""
	s.recipients = nil
}

func (s *submissionSession) Logout() error {
	s.Reset()
	return nil
}

func (a *App) authenticateSubmission(ctx context.Context, username, password string) (*User, *Mailbox, error) {
	address := normalizeEmail(username)
	if address == "" {
		return nil, nil, errors.New("missing username")
	}
	var mb Mailbox
	var passwordHash, created string
	row := a.db.QueryRowContext(ctx, `SELECT id,user_id,domain_id,local_part,address,display_name,password_hash,quota_mb,status,created_at
		FROM mailboxes WHERE address=? AND status='active'`, address)
	if err := row.Scan(&mb.ID, &mb.UserID, &mb.DomainID, &mb.LocalPart, &mb.Address, &mb.DisplayName, &passwordHash, &mb.QuotaMB, &mb.Status, &created); err != nil {
		return nil, nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return nil, nil, err
	}
	mb.CreatedAt = parseTime(created)
	user, err := a.userByID(ctx, mb.UserID)
	if err != nil {
		return nil, nil, err
	}
	if user.Disabled {
		return nil, nil, errors.New("user disabled")
	}
	if !userHasPermission(user, PermissionMailSend) {
		return nil, nil, errors.New("send permission required")
	}
	return user, &mb, nil
}

func (a *App) submitSMTPMessage(ctx context.Context, user *User, mb *Mailbox, mailFrom string, recipients []string, r io.Reader) error {
	if err := a.recordSMTPRate(ctx, user, mb); err != nil {
		if errors.Is(err, errSMTPRateLimited) {
			return smtpError(452, smtpserver.EnhancedCode{4, 7, 0}, err.Error())
		}
		return err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	prepared, msg, attachments, err := a.prepareSubmittedMessage(ctx, raw, mb, mailFrom, recipients)
	if err != nil {
		return err
	}
	msg.MailboxID = mb.ID
	sentID, insertedSent, err := a.insertSentMessageOnce(ctx, msg, attachments)
	if err != nil {
		return err
	}
	a.recordSendAudit(ctx, sendAuditAccepted, sendQueueStatusQueued, sendAuditInput{UserID: user.ID, MailboxID: mb.ID, SentMessageID: sentID, Source: sendSourceSubmission, MailFrom: mailFrom, HeaderFrom: msg.From, Recipients: recipients})
	if sentID != "" {
		if _, err := a.enqueueSend(ctx, sendQueueInput{UserID: user.ID, MailboxID: mb.ID, SentMessageID: sentID, MessageID: msg.MessageID, Source: sendSourceSubmission, MailFrom: mailFrom, HeaderFrom: msg.From, Recipients: recipients, MIMEBytes: prepared, Now: a.now().UTC()}); err != nil {
			if insertedSent {
				a.deleteMessage(ctx, sentID)
				if sentFolderID, ferr := a.ensureFolder(ctx, mb.ID, "Sent"); ferr == nil {
					a.deleteSentDedupeKey(ctx, mb.ID, sentFolderID, msg.MessageID)
				}
			}
			return err
		}
	}
	return nil
}

func (a *App) prepareSubmittedMessage(ctx context.Context, raw []byte, mb *Mailbox, mailFrom string, recipients []string) ([]byte, storedMessage, []AttachmentInput, error) {
	header, body, err := readMessageHeader(raw)
	if err != nil {
		return nil, storedMessage{}, nil, smtpError(554, smtpserver.EnhancedCode{5, 6, 0}, "invalid message")
	}
	fromAddress, fromName, ok := singleHeaderAddress(header.Get("From"))
	if !ok || fromAddress == "" {
		return nil, storedMessage{}, nil, smtpError(550, smtpserver.EnhancedCode{5, 7, 1}, "From header must contain exactly one address")
	}
	authAddress, fromName, err := a.authorizedSender(ctx, mb, fromAddress)
	if err != nil || normalizeEmail(mailFrom) != authAddress || normalizeEmail(fromAddress) != authAddress {
		return nil, storedMessage{}, nil, smtpError(553, smtpserver.EnhancedCode{5, 7, 1}, "sender must match authenticated mailbox")
	}
	now := a.now().UTC()
	messageID := strings.TrimSpace(header.Get("Message-Id"))
	if messageID == "" {
		messageID = fmt.Sprintf("<%s@%s>", newID("msg"), domainPart(authAddress))
		header.Set("Message-ID", messageID)
	} else {
		header.Set("Message-ID", messageID)
	}
	sentAt := parseMailDate(header.Get("Date"))
	if sentAt.IsZero() {
		sentAt = now
		header.Set("Date", sentAt.Format(time.RFC1123Z))
	}
	header.Del("Bcc")
	prepared := serializeMessage(header, body)
	msg, attachments, err := a.parseMaildirMessage(prepared, authAddress)
	if err != nil {
		return nil, storedMessage{}, nil, smtpError(554, smtpserver.EnhancedCode{5, 6, 0}, "invalid message")
	}
	if msg.MessageID == "" {
		msg.MessageID = messageID
	}
	if msg.SentAt.IsZero() {
		msg.SentAt = sentAt
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = sentAt
	}
	msg.From = authAddress
	msg.FromName = fromName
	msg.To = dedupeEmails(msg.To)
	msg.CC = dedupeEmails(msg.CC)
	msg.BCC = deduceBCCRecipients(recipients, addressList(header.Get("To")), addressList(header.Get("Cc")))
	msg.IsRead = true
	msg.RawPath = ""
	if msg.Subject == "" {
		msg.Subject = "(no subject)"
	}
	if msg.Snippet == "" {
		msg.Snippet = snippetFrom(msg.BodyText, msg.BodyHTML)
	}
	return prepared, msg, attachments, nil
}

func (a *App) insertSentMessageOnce(ctx context.Context, msg storedMessage, attachments []AttachmentInput) (string, bool, error) {
	sentFolderID, err := a.ensureFolder(ctx, msg.MailboxID, "Sent")
	if err != nil {
		return "", false, err
	}
	msg.FolderID = sentFolderID
	if msg.MessageUID == "" {
		msg.MessageUID = newID("uid")
	}
	if msg.MessageID != "" {
		var existing string
		err := a.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=? AND message_id <> '' LIMIT 1`, msg.MailboxID, sentFolderID, msg.MessageID).Scan(&existing)
		if err == nil {
			if err := a.insertSentDedupeKey(ctx, msg.MailboxID, sentFolderID, msg.MessageID); err != nil && !errors.Is(err, errSentDedupeExists) {
				return "", false, err
			}
			return existing, false, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
		if err := a.insertSentDedupeKey(ctx, msg.MailboxID, sentFolderID, msg.MessageID); err != nil {
			if errors.Is(err, errSentDedupeExists) {
				var existing string
				if err := a.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE mailbox_id=? AND folder_id=? AND message_id=? AND message_id <> '' LIMIT 1`, msg.MailboxID, sentFolderID, msg.MessageID).Scan(&existing); err == nil {
					return existing, false, nil
				}
				return "", false, nil
			}
			return "", false, err
		}
	}
	id, err := a.insertMessage(ctx, msg, attachments)
	if err != nil {
		if msg.MessageID != "" {
			a.deleteSentDedupeKey(ctx, msg.MailboxID, sentFolderID, msg.MessageID)
		}
		return "", false, err
	}
	return id, true, nil
}

var errSentDedupeExists = errors.New("sent message already exists")

func (a *App) insertSentDedupeKey(ctx context.Context, mailboxID, folderID, messageID string) error {
	if strings.TrimSpace(messageID) == "" {
		return nil
	}
	res, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO sent_message_dedupe_keys(mailbox_id,folder_id,message_id,created_at) VALUES(?,?,?,?)`, mailboxID, folderID, messageID, a.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return errSentDedupeExists
	}
	return nil
}

func (a *App) deleteSentDedupeKey(ctx context.Context, mailboxID, folderID, messageID string) {
	if strings.TrimSpace(messageID) == "" {
		return
	}
	_, _ = a.db.ExecContext(ctx, `DELETE FROM sent_message_dedupe_keys WHERE mailbox_id=? AND folder_id=? AND message_id=?`, mailboxID, folderID, messageID)
}

func readMessageHeader(raw []byte) (textproto.MIMEHeader, []byte, error) {
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, nil, err
	}
	return textproto.MIMEHeader(msg.Header), body, nil
}

func serializeMessage(header textproto.MIMEHeader, body []byte) []byte {
	var buf bytes.Buffer
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return textproto.CanonicalMIMEHeaderKey(keys[i]) < textproto.CanonicalMIMEHeaderKey(keys[j])
	})
	for _, key := range keys {
		values := header[key]
		canonical := textproto.CanonicalMIMEHeaderKey(key)
		for _, value := range values {
			fmt.Fprintf(&buf, "%s: %s\r\n", canonical, strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", " "))
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes()
}

func singleHeaderAddress(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false
	}
	items, err := netmail.ParseAddressList(value)
	if err != nil || len(items) != 1 {
		decoded := decodeMIMEHeader(value)
		items, err = netmail.ParseAddressList(decoded)
		if err != nil || len(items) != 1 {
			return "", "", false
		}
	}
	item := items[0]
	return normalizeEmail(item.Address), strings.TrimSpace(decodeMIMEHeader(item.Name)), true
}

func deduceBCCRecipients(envelope, to, cc []string) []string {
	visible := map[string]bool{}
	for _, item := range append(to, cc...) {
		if email := normalizeEmail(item); email != "" {
			visible[email] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range envelope {
		email := normalizeEmail(item)
		if email == "" || visible[email] || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	return out
}

func domainPart(email string) string {
	parts := strings.SplitN(normalizeEmail(email), "@", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "lanqin.local"
	}
	return parts[1]
}

func smtpError(code int, enhanced smtpserver.EnhancedCode, message string) *smtpserver.SMTPError {
	return &smtpserver.SMTPError{Code: code, EnhancedCode: enhanced, Message: message}
}
