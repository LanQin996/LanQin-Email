package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
)

type maildirMailbox struct {
	ID              string
	Address         string
	LocalPart       string
	Domain          string
	Unregistered    bool
	RecipientDomain string
}

type maildirFolder struct {
	ID   string
	Name string
	Role string
}

type parsedMail struct {
	Text        string
	HTML        string
	Attachments []AttachmentInput
}

func (a *App) maildirWorker(ctx context.Context) {
	interval := time.Duration(a.cfg.MaildirScanSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	a.log.Info("maildir sync worker started", "root", a.cfg.MaildirRoot, "interval", interval.String())
	if n, err := a.syncMaildirOnce(ctx); err != nil {
		a.log.Warn("initial maildir sync failed", "error", err)
	} else if n > 0 {
		a.log.Info("initial maildir sync imported messages", "count", n)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Info("maildir sync worker stopped")
			return
		case <-ticker.C:
			n, err := a.syncMaildirOnce(ctx)
			if err != nil {
				a.log.Warn("maildir sync failed", "error", err)
				continue
			}
			if n > 0 {
				a.log.Info("maildir sync imported messages", "count", n)
			}
		}
	}
}

func (a *App) syncMaildirOnce(ctx context.Context) (int, error) {
	root := strings.TrimSpace(a.cfg.MaildirRoot)
	if root == "" {
		return 0, nil
	}
	mailboxes, err := a.maildirMailboxes(ctx)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, mb := range mailboxes {
		if mb.Unregistered {
			count, err := a.syncUnregisteredMaildir(ctx, mb)
			if err != nil {
				return imported, err
			}
			imported += count
			continue
		}
		folders, err := a.maildirFolders(ctx, mb.ID)
		if err != nil {
			return imported, err
		}
		base := filepath.Join(root, mb.Domain, mb.LocalPart, "Maildir")
		for _, folder := range folders {
			folderBase := maildirFolderPath(base, folder.Name)
			for _, sub := range []string{"new", "cur"} {
				select {
				case <-ctx.Done():
					return imported, ctx.Err()
				default:
				}
				dir := filepath.Join(folderBase, sub)
				entries, err := os.ReadDir(dir)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return imported, err
				}
				for _, entry := range entries {
					if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
						continue
					}
					path := filepath.Join(dir, entry.Name())
					ok, err := a.syncMaildirFile(ctx, mb, folder, path)
					if err != nil {
						a.log.Warn("maildir file import failed", "path", path, "error", err)
						continue
					}
					if ok {
						imported++
					}
				}
			}
		}
	}
	return imported, nil
}

func (a *App) maildirMailboxes(ctx context.Context) ([]maildirMailbox, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT m.id,m.address,m.local_part,d.name FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE m.status='active' AND d.status='active' ORDER BY m.address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []maildirMailbox
	for rows.Next() {
		var mb maildirMailbox
		if err := rows.Scan(&mb.ID, &mb.Address, &mb.LocalPart, &mb.Domain); err != nil {
			return nil, err
		}
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if a.cfg.CatchAllEnabled {
		domainRows, err := a.db.QueryContext(ctx, `SELECT name FROM domains WHERE status='active' ORDER BY name`)
		if err != nil {
			return nil, err
		}
		defer domainRows.Close()
		for domainRows.Next() {
			var domain string
			if err := domainRows.Scan(&domain); err != nil {
				return nil, err
			}
			out = append(out, maildirMailbox{
				Address:         "__unregistered__@" + domain,
				LocalPart:       "__unregistered__",
				Domain:          domain,
				Unregistered:    true,
				RecipientDomain: domain,
			})
		}
		if err := domainRows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (a *App) syncUnregisteredMaildir(ctx context.Context, mb maildirMailbox) (int, error) {
	base := filepath.Join(strings.TrimSpace(a.cfg.MaildirRoot), mb.Domain, mb.LocalPart, "Maildir")
	imported := 0
	for _, sub := range []string{"new", "cur"} {
		select {
		case <-ctx.Done():
			return imported, ctx.Err()
		default:
		}
		dir := filepath.Join(base, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return imported, err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			ok, err := a.syncUnregisteredMaildirFile(ctx, mb, path)
			if err != nil {
				a.log.Warn("unregistered maildir file import failed", "path", path, "error", err)
				continue
			}
			if ok {
				imported++
			}
		}
	}
	return imported, nil
}

func (a *App) syncUnregisteredMaildirFile(ctx context.Context, mb maildirMailbox, path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	msg, attachments, err := a.parseMaildirMessage(raw, mb.Address)
	if err != nil {
		return false, err
	}
	recipient := unregisteredRecipientFromMessage(msg, mb.RecipientDomain)
	if recipient == "" {
		recipient = mb.Address
	}
	msg.MailboxID = ""
	msg.FolderID = ""
	msg.RecipientAddr = recipient
	msg.RawPath = path
	if msg.MessageUID == "" {
		msg.MessageUID = newID("uid")
	}
	if msg.MessageID == "" {
		msg.MessageID = fmt.Sprintf("<%s@lanqin.local>", newID("msg"))
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = a.now().UTC()
	}
	if msg.SentAt.IsZero() {
		msg.SentAt = msg.ReceivedAt
	}
	if msg.Snippet == "" {
		msg.Snippet = snippetFrom(msg.BodyText, msg.BodyHTML)
	}
	if exists, err := a.unregisteredMaildirMessageExists(ctx, path, msg.MessageID, msg.RecipientAddr); err != nil {
		return false, err
	} else if exists {
		return false, nil
	}
	_, err = a.insertMessage(ctx, msg, attachments)
	return err == nil, err
}

func (a *App) maildirFolders(ctx context.Context, mailboxID string) ([]maildirFolder, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id,name,role FROM folders WHERE mailbox_id=?`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []maildirFolder
	for rows.Next() {
		var f maildirFolder
		if err := rows.Scan(&f.ID, &f.Name, &f.Role); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func maildirFolderPath(base, folder string) string {
	if strings.EqualFold(folder, "Inbox") {
		return base
	}
	folder = strings.TrimSpace(folder)
	folder = strings.TrimPrefix(folder, ".")
	return filepath.Join(base, "."+folder)
}

func (a *App) syncMaildirFile(ctx context.Context, mb maildirMailbox, folder maildirFolder, path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	msg, attachments, err := a.parseMaildirMessage(raw, mb.Address)
	if err != nil {
		return false, err
	}
	msg.MailboxID = mb.ID
	msg.FolderID = folder.ID
	msg.IsRead = !strings.EqualFold(folder.Name, "Inbox")
	msg.RawPath = path
	if msg.MessageUID == "" {
		msg.MessageUID = newID("uid")
	}
	if msg.MessageID == "" {
		msg.MessageID = fmt.Sprintf("<%s@lanqin.local>", newID("msg"))
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = a.now().UTC()
	}
	if msg.SentAt.IsZero() {
		msg.SentAt = msg.ReceivedAt
	}
	if msg.Snippet == "" {
		msg.Snippet = snippetFrom(msg.BodyText, msg.BodyHTML)
	}
	if exists, err := a.maildirMessageExists(ctx, mb.ID, folder.ID, path, msg.MessageID); err != nil {
		return false, err
	} else if exists {
		return false, nil
	}
	id, err := a.insertMessage(ctx, msg, attachments)
	if err == nil && strings.EqualFold(folder.Name, "Inbox") {
		a.applyInboundControls(ctx, id, mb.ID, msg.From, msg.Subject)
	}
	return err == nil, err
}

func (a *App) maildirMessageExists(ctx context.Context, mailboxID, folderID, rawPath, messageID string) (bool, error) {
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages WHERE mailbox_id=? AND (raw_path=? OR (folder_id=? AND message_id=? AND message_id <> ''))`, mailboxID, rawPath, folderID, messageID).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return count > 0, nil
}

func (a *App) unregisteredMaildirMessageExists(ctx context.Context, rawPath, messageID, recipient string) (bool, error) {
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages WHERE mailbox_id IS NULL AND (raw_path=? OR (recipient_addr=? AND message_id=? AND message_id <> ''))`, rawPath, recipient, messageID).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return count > 0, nil
}

func unregisteredRecipientFromMessage(msg storedMessage, domain string) string {
	domain = normalizeDomain(domain)
	for _, address := range append(append([]string{}, msg.To...), msg.CC...) {
		address = normalizeEmail(address)
		if strings.HasSuffix(address, "@"+domain) {
			return address
		}
	}
	return ""
}

func (a *App) parseMaildirMessage(raw []byte, fallbackTo string) (storedMessage, []AttachmentInput, error) {
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return storedMessage{}, nil, err
	}
	subject := decodeMIMEHeader(m.Header.Get("Subject"))
	if strings.TrimSpace(subject) == "" {
		subject = "(no subject)"
	}
	from, fromName := firstAddressParts(m.Header.Get("From"))
	to := addressList(m.Header.Get("To"))
	cc := addressList(m.Header.Get("Cc"))
	if len(to) == 0 {
		to = []string{fallbackTo}
	}
	sentAt := parseMailDate(m.Header.Get("Date"))
	parsed := &parsedMail{}
	if err := parseMailPart(textproto.MIMEHeader(m.Header), m.Body, parsed); err != nil {
		return storedMessage{}, nil, err
	}
	bodyHTML := a.policy.Sanitize(parsed.HTML)
	bodyText := parsed.Text
	if strings.TrimSpace(bodyText) == "" {
		bodyText = stripTags(bodyHTML)
	}
	if strings.TrimSpace(bodyHTML) == "" && strings.TrimSpace(bodyText) != "" {
		bodyHTML = "<p>" + htmlEscape(bodyText) + "</p>"
	}
	receivedAt := a.now().UTC()
	if !sentAt.IsZero() {
		receivedAt = sentAt
	}
	return storedMessage{
		MessageUID: newID("uid"),
		MessageID:  strings.TrimSpace(m.Header.Get("Message-Id")),
		Subject:    subject,
		From:       from,
		FromName:   fromName,
		To:         to,
		CC:         cc,
		SentAt:     sentAt,
		ReceivedAt: receivedAt,
		Snippet:    snippetFrom(bodyText, bodyHTML),
		BodyText:   bodyText,
		BodyHTML:   bodyHTML,
		IsRead:     false,
	}, parsed.Attachments, nil
}

func parseMailPart(header textproto.MIMEHeader, body io.Reader, parsed *parsedMail) error {
	contentType := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if err := parseMailPart(part.Header, part, parsed); err != nil {
				return err
			}
		}
		return nil
	}
	decoded, err := io.ReadAll(transferReader(header.Get("Content-Transfer-Encoding"), body))
	if err != nil {
		return err
	}
	filename := partFilename(header)
	if filename != "" || (!strings.HasPrefix(strings.ToLower(mediaType), "text/") && len(decoded) > 0) {
		if filename == "" {
			filename = "attachment.bin"
		}
		parsed.Attachments = append(parsed.Attachments, AttachmentInput{Filename: filename, ContentType: mediaType, ContentBase64: base64.StdEncoding.EncodeToString(decoded)})
		return nil
	}
	switch strings.ToLower(mediaType) {
	case "text/html":
		if parsed.HTML == "" {
			parsed.HTML = string(decoded)
		}
	case "text/plain":
		if parsed.Text == "" {
			parsed.Text = string(decoded)
		}
	default:
		// Ignore unsupported inline parts for now.
	}
	return nil
}

func transferReader(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r
	}
}

func partFilename(header textproto.MIMEHeader) string {
	if _, params, err := mime.ParseMediaType(header.Get("Content-Disposition")); err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return filepath.Base(decodeMIMEHeader(name))
		}
	}
	if _, params, err := mime.ParseMediaType(header.Get("Content-Type")); err == nil {
		if name := strings.TrimSpace(params["name"]); name != "" {
			return filepath.Base(decodeMIMEHeader(name))
		}
	}
	return ""
}

func firstAddressParts(value string) (string, string) {
	items, err := netmail.ParseAddressList(value)
	if err != nil || len(items) == 0 {
		// ParseAddressList failed — attempt RFC 2047 decode on the raw header,
		// then retry parsing. This handles non-standard From headers where
		// encoded words (e.g. =?UTF-8?B?…?=) cause the initial parse to fail.
		decoded := decodeMIMEHeader(value)
		items, err = netmail.ParseAddressList(decoded)
		if err != nil || len(items) == 0 {
			// Still unparseable: return the decoded value and try to extract
			// a display name from the decoded string.
			email, name := splitNameAndEmail(decoded)
			return normalizeEmail(email), strings.TrimSpace(name)
		}
	}
	item := items[0]
	// Decode item.Name individually so that non-UTF-8 charsets (e.g. GBK,
	// Shift_JIS) are handled by our CharsetReader, while the address list
	// structure is parsed from the raw header (avoiding commas/semicolons
	// inside decoded display names breaking the parser).
	return normalizeEmail(item.Address), strings.TrimSpace(decodeMIMEHeader(item.Name))
}

// decodeMIMEHeader decodes all RFC 2047 encoded words (=?charset?encoding?data?=)
// in the given header value. Falls back to the original value on any error.
// Supports non-UTF-8 charsets (e.g. GBK, GB2312, Shift_JIS) via x/text.
// Per RFC 2047 §6.2, linear whitespace between adjacent encoded words is
// stripped before decoding.
func decodeMIMEHeader(value string) string {
	if !strings.Contains(value, "=?") {
		return value
	}
	// RFC 2047 §6.2: ignore whitespace between adjacent encoded words.
	collapsed := adjacentEncodedWordSpaceRe.ReplaceAllString(value, "$1$2")
	decoder := &mime.WordDecoder{
		CharsetReader: charsetReader,
	}
	decoded, err := decoder.DecodeHeader(collapsed)
	if err != nil {
		return value
	}
	return decoded
}

// adjacentEncodedWordSpaceRe matches whitespace between two adjacent RFC 2047
// encoded words. Per RFC 2047 §6.2, this whitespace must be ignored when
// displaying the header.
var adjacentEncodedWordSpaceRe = regexp.MustCompile(`(\?=)\s+(=\?)`)

// charsetReader converts a non-UTF-8 charset stream into UTF-8 using x/text encodings.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	charset = strings.ToLower(strings.TrimSpace(charset))
	if charset == "utf-8" || charset == "us-ascii" {
		return input, nil
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil {
		return nil, fmt.Errorf("unsupported charset %q: %w", charset, err)
	}
	if enc == nil || enc == encoding.Nop || enc == encoding.Replacement {
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	return enc.NewDecoder().Reader(input), nil
}

// splitNameAndEmail attempts to extract a display name and email address from
// a string like "Display Name <user@example.com>" or plain "user@example.com".
func splitNameAndEmail(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	// Try "Name <email>" pattern
	if idx := strings.LastIndex(value, "<"); idx >= 0 {
		email := strings.TrimSpace(strings.Trim(value[idx+1:], "> "))
		name := strings.TrimSpace(strings.Trim(value[:idx], `" `))
		if strings.Contains(email, "@") {
			return email, name
		}
	}
	// Plain email or unknown format
	if strings.Contains(value, "@") {
		return value, ""
	}
	return value, ""
}

func addressList(value string) []string {
	items, err := netmail.ParseAddressList(value)
	if err != nil || len(items) == 0 {
		// ParseAddressList failed — attempt RFC 2047 decode, then retry.
		decoded := decodeMIMEHeader(value)
		items, err = netmail.ParseAddressList(decoded)
		if err != nil || len(items) == 0 {
			return nil
		}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, normalizeEmail(item.Address))
	}
	return out
}

func parseMailDate(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	if t, err := netmail.ParseDate(value); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
