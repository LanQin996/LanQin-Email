package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var errBackupNotDraft = errors.New("backup message is not a local draft")
var errBackupDraftInvalid = errors.New("backup draft metadata or staged attachment is invalid")
var errBackupDraftLimit = errors.New("backup draft exceeds reconstruction limits")

// rebuildBackupDraft reconstructs only a local draft from an already frozen,
// read-only database. The old live Maildir MIME can lag the committed draft body,
// so it is deliberately never read here. Attachments must already have been
// captured under snapshotData; neither sourceDataDir nor raw_path is opened.
// The caller owns the private staging root and must not mutate it concurrently.
// maxMIMEBytes caps the estimated MIME plus input memory before BuildMIME, then
// the actual encoded result. This does not modify either database or directory.
func rebuildBackupDraft(ctx context.Context, frozen *sql.DB, messageID, sourceDataDir string, snapshotData *os.Root, maxMIMEBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frozen == nil || snapshotData == nil || strings.TrimSpace(sourceDataDir) == "" || maxMIMEBytes < 8192 || maxMIMEBytes > 256<<20 {
		return nil, errBackupDraftLimit
	}
	if !backupDraftID(messageID) {
		return nil, errBackupDraftInvalid
	}
	// Bound Go allocations before loading potentially large draft strings. The
	// independent snapshot connection is the only DB connection used here.
	var headerBytes, bodyBytes int64
	err := frozen.QueryRowContext(ctx, `SELECT
		length(CAST(m.subject AS BLOB))+length(CAST(m.from_addr AS BLOB))+length(CAST(m.from_name AS BLOB))+
		length(CAST(m.to_addrs AS BLOB))+length(CAST(m.cc_addrs AS BLOB))+length(CAST(m.bcc_addrs AS BLOB))+length(CAST(m.message_id AS BLOB)),
		length(CAST(m.body_text AS BLOB))+length(CAST(m.body_html AS BLOB))
		FROM messages m JOIN folders f ON f.id=m.folder_id AND f.mailbox_id=m.mailbox_id
		JOIN mailboxes mb ON mb.id=m.mailbox_id
		WHERE m.id=? AND lower(f.name)='drafts' AND f.role='drafts'
		AND NOT EXISTS(SELECT 1 FROM external_imap_messages em WHERE em.local_message_id=m.id)
		AND NOT EXISTS(SELECT 1 FROM external_imap_folder_states efs WHERE efs.local_folder_id=f.id)`, messageID).Scan(&headerBytes, &bodyBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errBackupNotDraft
	}
	if err != nil {
		return nil, errBackupDraftInvalid
	}
	if headerBytes < 0 || bodyBytes < 0 || headerBytes > maxMIMEBytes/8 || bodyBytes > maxMIMEBytes/2 {
		return nil, errBackupDraftLimit
	}
	estimated := int64(8192) + 8*headerBytes + 2*bodyBytes
	if estimated > maxMIMEBytes {
		return nil, errBackupDraftLimit
	}
	snapshotApp := App{db: frozen}
	msg, err := snapshotApp.storedMessageByID(ctx, messageID)
	if err != nil {
		return nil, errBackupDraftInvalid
	}
	// The normal getter deliberately tolerates malformed address JSON. A backup
	// must fail instead of silently losing draft recipients during reconstruction.
	var addresses [3]string
	if err := frozen.QueryRowContext(ctx, `SELECT to_addrs,cc_addrs,bcc_addrs FROM messages WHERE id=?`, messageID).Scan(&addresses[0], &addresses[1], &addresses[2]); err != nil {
		return nil, errBackupDraftInvalid
	}
	for i, target := range []*[]string{&msg.To, &msg.CC, &msg.BCC} {
		if err := json.Unmarshal([]byte(addresses[i]), target); err != nil {
			return nil, errBackupDraftInvalid
		}
	}
	date := msg.SentAt
	if date.IsZero() {
		date = msg.ReceivedAt
	}
	if date.IsZero() {
		return nil, errBackupDraftInvalid // Never manufacture a current-time date.
	}
	var count, minimum, total, nameBytes, typeBytes, pathBytes int64
	if err := frozen.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(size_bytes),0),COALESCE(sum(size_bytes),0),
		COALESCE(max(length(CAST(filename AS BLOB))),0),COALESCE(max(length(CAST(content_type AS BLOB))),0),
		COALESCE(max(length(CAST(storage_path AS BLOB))),0) FROM attachments WHERE message_id=?`, messageID).Scan(&count, &minimum, &total, &nameBytes, &typeBytes, &pathBytes); err != nil {
		return nil, errBackupDraftInvalid
	}
	if count > 1024 || minimum < 0 || total < 0 || total > maxMIMEBytes/2 || nameBytes > 4096 || typeBytes > 4096 || pathBytes > 4096 {
		return nil, errBackupDraftLimit
	}
	rows, err := frozen.QueryContext(ctx, `SELECT filename,content_type,storage_path,size_bytes FROM attachments WHERE message_id=? ORDER BY filename,id`, messageID)
	if err != nil {
		return nil, errBackupDraftInvalid
	}
	defer rows.Close()
	attachments := []AttachmentInput{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var filename, contentType, storagePath string
		var size int64
		if err := rows.Scan(&filename, &contentType, &storagePath, &size); err != nil {
			return nil, errBackupDraftInvalid
		}
		if size < 0 || strings.IndexFunc(filename+contentType, unicode.IsControl) >= 0 {
			return nil, errBackupDraftInvalid
		}
		if contentType != "" {
			if _, _, err := mime.ParseMediaType(contentType); err != nil {
				return nil, errBackupDraftInvalid
			}
		}
		estimated += 2048 + 8*int64(len(filename)+len(contentType)) + 2*size
		if estimated > maxMIMEBytes {
			return nil, errBackupDraftLimit
		}
		relative, err := backupDraftAttachmentPath(sourceDataDir, storagePath, messageID)
		if err != nil {
			return nil, err
		}
		content, err := readBackupDraftAttachment(ctx, snapshotData, relative, size)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, AttachmentInput{Filename: filename, ContentType: contentType, decodedContent: content, contentDecoded: true})
	}
	if rows.Err() != nil || int64(len(attachments)) != count {
		return nil, errBackupDraftInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := BuildMIME(MIMEMessage{From: msg.From, FromName: msg.FromName, To: msg.To, CC: msg.CC, BCC: msg.BCC,
		Subject: msg.Subject, Text: msg.BodyText, HTML: msg.BodyHTML, MessageID: msg.MessageID, Date: date, Attachments: attachments})
	if err != nil {
		return nil, errBackupDraftInvalid
	}
	if int64(len(result)) > maxMIMEBytes {
		return nil, errBackupDraftLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func backupDraftID(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				return false
			}
		}
	}
	return true
}

func backupDraftAttachmentPath(dataDir, storagePath, messageID string) (string, error) {
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return "", errBackupDraftInvalid
	}
	full, err := filepath.Abs(storagePath)
	if err != nil || strings.IndexFunc(storagePath, unicode.IsControl) >= 0 {
		return "", errBackupDraftInvalid
	}
	relative, err := filepath.Rel(base, full)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errBackupDraftInvalid
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 || parts[0] != "attachments" || parts[1] != messageID || parts[2] == "" || strings.ContainsAny(parts[2], "\\:") {
		return "", errBackupDraftInvalid
	}
	return relative, nil
}

func readBackupDraftAttachment(ctx context.Context, root *os.Root, name string, size int64) ([]byte, error) {
	if !backupDraftPathIsRegular(root, name) {
		return nil, errBackupDraftInvalid
	}
	before, err := root.Lstat(name)
	if err != nil || before.Size() != size {
		return nil, errBackupDraftInvalid
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errBackupDraftInvalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != size || !opened.Mode().IsRegular() {
		return nil, errBackupDraftInvalid
	}
	data, err := io.ReadAll(io.LimitReader(backupDraftContextReader{ctx: ctx, source: file}, size+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errBackupDraftInvalid
	}
	after, err := file.Stat()
	pathInfo, pathErr := root.Lstat(name)
	if err != nil || pathErr != nil || int64(len(data)) != size || !os.SameFile(before, after) || !os.SameFile(before, pathInfo) || after.Size() != size || !after.ModTime().Equal(before.ModTime()) || !backupDraftPathIsRegular(root, name) {
		return nil, errBackupDraftInvalid
	}
	return data, nil
}

func backupDraftPathIsRegular(root *os.Root, name string) bool {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

type backupDraftContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r backupDraftContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
