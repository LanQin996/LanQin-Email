package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func (a *App) writeStoredMessageToMaildir(ctx context.Context, messageID string, msg storedMessage, attachments []AttachmentInput) error {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" || strings.TrimSpace(msg.MailboxID) == "" || strings.TrimSpace(msg.FolderID) == "" {
		return nil
	}
	raw, err := BuildMIME(MIMEMessage{
		From:        msg.From,
		FromName:    msg.FromName,
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		Subject:     msg.Subject,
		Text:        msg.BodyText,
		HTML:        msg.BodyHTML,
		MessageID:   msg.MessageID,
		Date:        messageDate(msg),
		Attachments: attachments,
	})
	if err != nil {
		return err
	}
	return a.writeRawMessageToMaildir(ctx, messageID, raw, false)
}

func (a *App) rewriteMessageMaildir(ctx context.Context, messageID string) error {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" {
		return nil
	}
	msg, err := a.storedMessageByID(ctx, messageID)
	if err != nil {
		return err
	}
	attachments, err := a.attachmentInputsForMessage(ctx, messageID)
	if err != nil {
		return err
	}
	raw, err := BuildMIME(MIMEMessage{
		From:        msg.From,
		FromName:    msg.FromName,
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		Subject:     msg.Subject,
		Text:        msg.BodyText,
		HTML:        msg.BodyHTML,
		MessageID:   msg.MessageID,
		Date:        messageDate(msg),
		Attachments: attachments,
	})
	if err != nil {
		return err
	}
	return a.writeRawMessageToMaildir(ctx, messageID, raw, true)
}

func (a *App) writeRawMessageToMaildir(ctx context.Context, messageID string, raw []byte, replace bool) error {
	state, err := a.maildirMessageState(ctx, messageID)
	if err != nil {
		return err
	}
	return a.writeRawMessageToMaildirFolder(ctx, messageID, state.FolderID, raw, replace, false)
}

func (a *App) writeRawMessageToMaildirFolder(ctx context.Context, messageID, folderID string, raw []byte, replace bool, updateFolder bool) error {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" {
		return nil
	}
	state, err := a.maildirMessageState(ctx, messageID)
	if err != nil {
		return err
	}
	if folderID != "" {
		oldFolderID := state.FolderID
		state.FolderID = folderID
		if updateFolder && oldFolderID != "" && oldFolderID != state.FolderID {
			if _, err := a.bumpFolderModSeq(ctx, oldFolderID); err != nil {
				return err
			}
		}
	}
	if state.MailboxID == "" || state.FolderID == "" {
		return nil
	}
	if !replace && state.RawPath != "" {
		if ok, err := a.pathIsUnderMaildirRoot(state.RawPath); err != nil {
			return err
		} else if ok {
			if _, err := os.Stat(state.RawPath); err == nil {
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	mb, err := a.maildirMailboxByID(ctx, state.MailboxID)
	if err != nil {
		return err
	}
	folderName, err := a.folderNameByID(ctx, state.FolderID)
	if err != nil {
		return err
	}
	base := filepath.Join(strings.TrimSpace(a.cfg.MaildirRoot), mb.Domain, mb.LocalPart, "Maildir")
	folderBase := maildirFolderPath(base, folderName)
	subdir := "cur"
	if strings.EqualFold(folderName, "Inbox") && !state.IsRead {
		subdir = "new"
	}
	if err := ensureMaildirFolderDirs(folderBase); err != nil {
		return err
	}
	filename := maildirFilename(messageID, state.MessageID)
	tmpPath := filepath.Join(folderBase, "tmp", filename)
	finalPath := filepath.Join(folderBase, subdir, filename)
	finalPath = maildirPathWithFlags(finalPath, state.IsRead, state.IsStarred)
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if replace || state.RawPath != "" {
		a.removeMaildirPath(ctx, state.RawPath)
	}
	if updateFolder {
		if state.IMAPUID > 0 && folderID == "" {
			modSeq, metaErr := a.bumpFolderModSeq(ctx, state.FolderID)
			if metaErr != nil {
				return metaErr
			}
			_, err = a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?,raw_path=?,imap_modseq=CASE WHEN ? > 0 THEN ? ELSE imap_modseq END,updated_at=? WHERE id=?`, state.FolderID, finalPath, modSeq, modSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
		} else {
			meta, metaErr := a.nextIMAPMetadata(ctx, a.db, state.FolderID)
			if metaErr != nil {
				return metaErr
			}
			_, err = a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?,raw_path=?,imap_uid=?,imap_modseq=?,updated_at=? WHERE id=?`, state.FolderID, finalPath, meta.UID, meta.ModSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
		}
	} else {
		modSeq, metaErr := a.bumpFolderModSeq(ctx, state.FolderID)
		if metaErr != nil {
			return metaErr
		}
		_, err = a.db.ExecContext(ctx, `UPDATE messages SET raw_path=?,imap_modseq=CASE WHEN ? > 0 THEN ? ELSE imap_modseq END,updated_at=? WHERE id=?`, finalPath, modSeq, modSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
	}
	return err
}

func (a *App) moveMessageMaildir(ctx context.Context, messageID, targetFolderID string) error {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" {
		state, stateErr := a.maildirMessageState(ctx, messageID)
		if stateErr != nil {
			return stateErr
		}
		if state.FolderID != "" && state.FolderID != targetFolderID {
			if _, err := a.bumpFolderModSeq(ctx, state.FolderID); err != nil {
				return err
			}
		}
		meta, metaErr := a.nextIMAPMetadata(ctx, a.db, targetFolderID)
		if metaErr != nil {
			return metaErr
		}
		_, err := a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?,imap_uid=?,imap_modseq=?,updated_at=? WHERE id=?`, targetFolderID, meta.UID, meta.ModSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
		return err
	}
	state, err := a.maildirMessageState(ctx, messageID)
	if err != nil {
		return err
	}
	if state.MailboxID == "" {
		return nil
	}
	state.FolderID = targetFolderID
	if state.RawPath == "" {
		return a.writeMessageToNewMaildirFolder(ctx, messageID, targetFolderID)
	}
	ok, err := a.pathIsUnderMaildirRoot(state.RawPath)
	if err != nil {
		return err
	}
	if !ok {
		return a.writeMessageToNewMaildirFolder(ctx, messageID, targetFolderID)
	}
	if _, err := os.Stat(state.RawPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.writeMessageToNewMaildirFolder(ctx, messageID, targetFolderID)
		}
		return err
	}
	mb, err := a.maildirMailboxByID(ctx, state.MailboxID)
	if err != nil {
		return err
	}
	folderName, err := a.folderNameByID(ctx, targetFolderID)
	if err != nil {
		return err
	}
	base := filepath.Join(strings.TrimSpace(a.cfg.MaildirRoot), mb.Domain, mb.LocalPart, "Maildir")
	folderBase := maildirFolderPath(base, folderName)
	if err := ensureMaildirFolderDirs(folderBase); err != nil {
		return err
	}
	subdir := "cur"
	if strings.EqualFold(folderName, "Inbox") && !state.IsRead {
		subdir = "new"
	}
	targetPath := filepath.Join(folderBase, subdir, filepath.Base(state.RawPath))
	if filepath.Clean(targetPath) != filepath.Clean(state.RawPath) {
		if err := os.Rename(state.RawPath, targetPath); err != nil {
			return err
		}
	}
	if state.FolderID != "" && state.FolderID != targetFolderID {
		if _, err := a.bumpFolderModSeq(ctx, state.FolderID); err != nil {
			return err
		}
	}
	meta, err := a.nextIMAPMetadata(ctx, a.db, targetFolderID)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?,raw_path=?,imap_uid=?,imap_modseq=?,updated_at=? WHERE id=?`, targetFolderID, targetPath, meta.UID, meta.ModSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
	return err
}

func (a *App) writeMessageToNewMaildirFolder(ctx context.Context, messageID, folderID string) error {
	msg, err := a.storedMessageByID(ctx, messageID)
	if err != nil {
		return err
	}
	msg.FolderID = folderID
	attachments, err := a.attachmentInputsForMessage(ctx, messageID)
	if err != nil {
		return err
	}
	raw, err := BuildMIME(MIMEMessage{
		From:        msg.From,
		FromName:    msg.FromName,
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		Subject:     msg.Subject,
		Text:        msg.BodyText,
		HTML:        msg.BodyHTML,
		MessageID:   msg.MessageID,
		Date:        messageDate(msg),
		Attachments: attachments,
	})
	if err != nil {
		return err
	}
	return a.writeRawMessageToMaildirFolder(ctx, messageID, folderID, raw, true, true)
}

func (a *App) deleteMessageMaildirFile(ctx context.Context, messageID string) {
	var rawPath string
	if err := a.db.QueryRowContext(ctx, `SELECT raw_path FROM messages WHERE id=?`, messageID).Scan(&rawPath); err != nil {
		return
	}
	a.removeMaildirPath(ctx, rawPath)
}

func (a *App) updateMessageMaildirFlags(ctx context.Context, messageID string, read, starred *bool) error {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" {
		return nil
	}
	state, err := a.maildirMessageState(ctx, messageID)
	if err != nil {
		return err
	}
	if state.RawPath == "" {
		return nil
	}
	ok, err := a.pathIsUnderMaildirRoot(state.RawPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if _, err := os.Stat(state.RawPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	currentRead := state.IsRead
	currentStarred := state.IsStarred
	if read != nil {
		currentRead = *read
	}
	if starred != nil {
		currentStarred = *starred
	}
	targetPath := maildirPathWithFlags(state.RawPath, currentRead, currentStarred)
	if filepath.Clean(targetPath) == filepath.Clean(state.RawPath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(state.RawPath, targetPath); err != nil {
		return err
	}
	modSeq, err := a.bumpFolderModSeq(ctx, state.FolderID)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE messages SET raw_path=?,imap_modseq=CASE WHEN ? > 0 THEN ? ELSE imap_modseq END,updated_at=? WHERE id=?`, targetPath, modSeq, modSeq, a.now().UTC().Format(time.RFC3339Nano), messageID)
	return err
}

func (a *App) removeMaildirPath(ctx context.Context, rawPath string) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return
	}
	ok, err := a.pathIsUnderMaildirRoot(rawPath)
	if err != nil || !ok {
		if err != nil {
			a.log.Warn("failed to validate maildir path", "path", rawPath, "error", err)
		}
		return
	}
	if err := os.Remove(rawPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("failed to remove maildir message", "path", rawPath, "error", err)
	}
}

func (a *App) backfillSQLiteMessagesToMaildir(ctx context.Context) (int, error) {
	if strings.TrimSpace(a.cfg.MaildirRoot) == "" {
		return 0, nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM messages WHERE COALESCE(mailbox_id,'')<>'' AND COALESCE(folder_id,'')<>'' AND raw_path='' ORDER BY created_at LIMIT 100`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		select {
		case <-ctx.Done():
			rows.Close()
			return 0, ctx.Err()
		default:
		}
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if err := a.rewriteMessageMaildir(ctx, id); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type maildirMessageState struct {
	MailboxID  string
	FolderID   string
	MessageID  string
	RawPath    string
	IsRead     bool
	IsStarred  bool
	IMAPUID    int64
	IMAPModSeq int64
}

func (a *App) maildirMessageState(ctx context.Context, id string) (maildirMessageState, error) {
	var state maildirMessageState
	var mailboxID, folderID sql.NullString
	var read, starred int
	err := a.db.QueryRowContext(ctx, `SELECT mailbox_id,folder_id,message_id,raw_path,is_read,is_starred,imap_uid,imap_modseq FROM messages WHERE id=?`, id).Scan(&mailboxID, &folderID, &state.MessageID, &state.RawPath, &read, &starred, &state.IMAPUID, &state.IMAPModSeq)
	if err != nil {
		return state, err
	}
	state.MailboxID = mailboxID.String
	state.FolderID = folderID.String
	state.IsRead = intBool(read)
	state.IsStarred = intBool(starred)
	return state, nil
}

func (a *App) storedMessageByID(ctx context.Context, id string) (storedMessage, error) {
	row := a.db.QueryRowContext(ctx, `SELECT COALESCE(mailbox_id,''),COALESCE(folder_id,''),recipient_addr,message_uid,message_id,subject,from_addr,from_name,to_addrs,cc_addrs,bcc_addrs,sent_at,received_at,snippet,body_text,body_html,is_read,is_starred,raw_path FROM messages WHERE id=?`, id)
	var msg storedMessage
	var toJSON, ccJSON, bccJSON, sent, received string
	var read, starred int
	err := row.Scan(&msg.MailboxID, &msg.FolderID, &msg.RecipientAddr, &msg.MessageUID, &msg.MessageID, &msg.Subject, &msg.From, &msg.FromName, &toJSON, &ccJSON, &bccJSON, &sent, &received, &msg.Snippet, &msg.BodyText, &msg.BodyHTML, &read, &starred, &msg.RawPath)
	if err != nil {
		return msg, err
	}
	msg.To = jsonDecodeSlice(toJSON)
	msg.CC = jsonDecodeSlice(ccJSON)
	msg.BCC = jsonDecodeSlice(bccJSON)
	msg.SentAt = parseTime(sent)
	msg.ReceivedAt = parseTime(received)
	msg.IsRead = intBool(read)
	msg.IsStarred = intBool(starred)
	return msg, nil
}

func (a *App) attachmentInputsForMessage(ctx context.Context, messageID string) ([]AttachmentInput, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT filename,content_type,storage_path FROM attachments WHERE message_id=? ORDER BY filename`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttachmentInput
	for rows.Next() {
		var filename, contentType, storagePath string
		if err := rows.Scan(&filename, &contentType, &storagePath); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(storagePath)
		if err != nil {
			return nil, err
		}
		out = append(out, AttachmentInput{Filename: filename, ContentType: contentType, ContentBase64: base64.StdEncoding.EncodeToString(data)})
	}
	return out, rows.Err()
}

func (a *App) maildirMailboxByID(ctx context.Context, mailboxID string) (maildirMailbox, error) {
	var mb maildirMailbox
	err := a.db.QueryRowContext(ctx, `SELECT m.id,m.address,m.local_part,d.name FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE m.id=?`, mailboxID).Scan(&mb.ID, &mb.Address, &mb.LocalPart, &mb.Domain)
	return mb, err
}

func (a *App) folderNameByID(ctx context.Context, folderID string) (string, error) {
	var name string
	err := a.db.QueryRowContext(ctx, `SELECT name FROM folders WHERE id=?`, folderID).Scan(&name)
	return name, err
}

func (a *App) pathIsUnderMaildirRoot(path string) (bool, error) {
	root := strings.TrimSpace(a.cfg.MaildirRoot)
	if root == "" || strings.TrimSpace(path) == "" {
		return false, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false, err
	}
	return rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..", nil
}

func ensureMaildirFolderDirs(folderBase string) error {
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(folderBase, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func maildirFilename(messageID, headerMessageID string) string {
	base := strings.TrimSpace(headerMessageID)
	if base == "" {
		base = messageID
	}
	return fmt.Sprintf("%d.%s.%s", time.Now().UnixNano(), safeMaildirName(messageID), safeMaildirName(base))
}

func safeMaildirName(value string) string {
	value = strings.Trim(value, "<>")
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		out = "message"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func messageDate(msg storedMessage) time.Time {
	if !msg.SentAt.IsZero() {
		return msg.SentAt
	}
	if !msg.ReceivedAt.IsZero() {
		return msg.ReceivedAt
	}
	return time.Now().UTC()
}

func maildirPathWithFlags(path string, read, starred bool) string {
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	if read || starred {
		dir = filepath.Join(filepath.Dir(dir), "cur")
	} else if filepath.Base(dir) == "cur" {
		dir = filepath.Join(filepath.Dir(dir), "new")
	}
	base := name
	sep := maildirFlagSeparator()
	existingFlags := ""
	if idx := strings.LastIndex(base, sep); idx >= 0 {
		existingFlags = base[idx+len(sep):]
		base = base[:idx]
	}
	flags := preserveMaildirFlags(existingFlags, "SF")
	if read {
		flags = appendMaildirFlag(flags, 'S')
	}
	if starred {
		flags = appendMaildirFlag(flags, 'F')
	}
	if flags != "" {
		base += sep + flags
	}
	return filepath.Join(dir, base)
}

func preserveMaildirFlags(flags, managed string) string {
	var b strings.Builder
	for _, flag := range flags {
		if strings.ContainsRune(managed, flag) || strings.ContainsRune(b.String(), flag) {
			continue
		}
		b.WriteRune(flag)
	}
	return b.String()
}

func appendMaildirFlag(flags string, flag rune) string {
	if strings.ContainsRune(flags, flag) {
		return flags
	}
	return flags + string(flag)
}

func maildirFlagSeparator() string {
	if runtime.GOOS == "windows" {
		return "!2,"
	}
	return ":2,"
}
