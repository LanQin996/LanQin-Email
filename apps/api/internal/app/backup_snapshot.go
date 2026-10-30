package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"lanqin-email-api/internal/backup/online"
)

var errBackupSnapshotInvalid = errors.New("online backup contains unsupported or inconsistent references")

type OnlineBackupOptions struct {
	MaxBytes     int64
	MaxEntries   int
	MaxMIMEBytes int64
	Attempts     int
}

func DefaultOnlineBackupOptions() OnlineBackupOptions {
	return OnlineBackupOptions{MaxBytes: 10 << 30, MaxEntries: 100000, MaxMIMEBytes: 64 << 20, Attempts: 3}
}

type OnlineBackupSummary struct {
	SnapshotStarted            time.Time `json:"snapshotStarted"`
	SnapshotFinished           time.Time `json:"snapshotFinished"`
	Attempts                   int       `json:"attempts"`
	Messages                   int       `json:"messages"`
	Drafts                     int       `json:"drafts"`
	Attachments                int       `json:"attachments"`
	Unindexed                  int       `json:"unindexed"`
	DKIMKeys                   int       `json:"dkimKeys"`
	ExternalCachedMessagesOnly int       `json:"externalCachedMessagesOnly"`
	RuntimeBackupJobsOmitted   int       `json:"runtimeBackupJobsOmitted"`
	RuntimeBackupQueueCleared  bool      `json:"runtimeBackupQueueCleared"`
	// Bytes excludes the collection report itself, whose final bytes are also
	// charged against MaxBytes. It is not a ciphertext-size prediction.
	Bytes int64 `json:"bytesBeforeReport"`
}

type OnlineBackupFile struct {
	Kind         string     `json:"kind"`
	MessageID    string     `json:"messageId,omitempty"`
	AttachmentID string     `json:"attachmentId,omitempty"`
	InternalDate *time.Time `json:"internalDate,omitempty"`
	online.CollectedFile
}

// This report is written INSIDE the encrypted payload. Paths and message IDs
// must never be copied into public job DTOs, logs or CLI standard output.
type OnlineBackupReport struct {
	Version  int                 `json:"version"`
	Scope    string              `json:"scope"`
	Complete bool                `json:"complete"`
	Summary  OnlineBackupSummary `json:"summary"`
	Sources  struct {
		Data string `json:"data"`
		Mail string `json:"mail"`
	} `json:"sources"`
	Files []OnlineBackupFile `json:"files"`
}

type PreparedOnlineBackup struct {
	Path   string
	Report OnlineBackupReport
	close  func() error
}

func (p *PreparedOnlineBackup) Close() error {
	if p == nil || p.close == nil {
		return nil
	}
	err := p.close()
	if err == nil {
		p.close = nil
	}
	return err
}

// PrepareOnlineBackup never stops writers, creates App workers, or borrows the
// application's connection pool. It prepares a PRIVATE reference-closed SQLite
// data copy. Unindexed Maildir candidates are quarantined, NOT activated mail.
// It does not include Postfix queues, remote-only IMAP mail or deployment secrets.
// The caller must Close the result after encrypting it with backup.WriteOnline.
func PrepareOnlineBackup(ctx context.Context, cfg Config, workspace string, opts OnlineBackupOptions) (*PreparedOnlineBackup, error) {
	return prepareOnlineBackup(ctx, cfg, workspace, opts, nil)
}

type backupSnapshotHooks struct {
	afterSnapshot  func(int, string) error
	afterInventory func(int) error
}

func prepareOnlineBackup(ctx context.Context, cfg Config, workspace string, opts OnlineBackupOptions, hooks *backupSnapshotHooks) (*PreparedOnlineBackup, error) {
	cfg = normalizeDatabaseConfig(cfg)
	if cfg.DBDriver != databaseDriverSQLite || opts.MaxBytes < 4096 || opts.MaxBytes > 1<<40 || opts.MaxEntries < 4 || opts.MaxEntries > 100000 || opts.MaxMIMEBytes < 8192 || opts.MaxMIMEBytes > 256<<20 || opts.Attempts < 1 || opts.Attempts > 3 {
		return nil, errors.New("online backup requires SQLite and valid bounded collection limits")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var err error
	dataDirectory, err := resolvedBackupDirectory(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	mailDirectory, err := resolvedBackupDirectory(cfg.MaildirRoot)
	if err != nil {
		return nil, err
	}
	workspace, err = resolvedBackupDirectory(workspace)
	if err != nil || backupPathWithin(dataDirectory, workspace) || backupPathWithin(mailDirectory, workspace) {
		return nil, errors.New("online backup workspace must exist outside source directories")
	}
	info, err := os.Stat(workspace)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("online backup workspace must be private to its owner")
	}
	// Database references retain the configured lexical root, even when it is
	// an explicitly configured symlink. Only root descriptors resolve the alias;
	// untrusted child paths are never EvalSymlinks'd into a different namespace.
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, online.ErrUnsafePath
	}
	cfg.MaildirRoot, err = filepath.Abs(cfg.MaildirRoot)
	if err != nil {
		return nil, online.ErrUnsafePath
	}
	dataRoot, err := os.OpenRoot(dataDirectory)
	if err != nil {
		return nil, online.ErrFileIO
	}
	defer dataRoot.Close()
	mailRoot, err := os.OpenRoot(mailDirectory)
	if err != nil {
		return nil, online.ErrFileIO
	}
	defer mailRoot.Close()
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory, err := os.MkdirTemp(workspace, ".lanqin-online-*")
		if err != nil {
			return nil, online.ErrFileIO
		}
		// Keep the allocated, verified absolute path in the closure, not a
		// caller-mutable exported Path field. Never delete a source directory.
		directory, err = filepath.Abs(directory)
		if err != nil || !backupPathWithin(workspace, directory) || directory == workspace {
			return nil, online.ErrUnsafePath
		}
		ownedDirectory, err := os.Open(directory)
		if err != nil {
			return nil, online.ErrFileIO
		}
		// File.Stat fixes Windows FileID now; Lstat may resolve it lazily after
		// an unrelated directory has been moved onto the same pathname.
		owned, statErr := ownedDirectory.Stat()
		closeErr := ownedDirectory.Close()
		if statErr != nil || closeErr != nil || !owned.IsDir() {
			return nil, online.ErrUnsafePath
		}
		cleanup := func() error {
			current, err := os.Lstat(directory)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(owned, current) {
				return online.ErrUnsafePath
			}
			return os.RemoveAll(directory)
		}
		report, collectErr := collectOnlineBackup(ctx, cfg, directory, dataRoot, mailRoot, opts, attempt, hooks)
		if collectErr == nil {
			return &PreparedOnlineBackup{Path: directory, Report: report, close: cleanup}, nil
		}
		if err := cleanup(); err != nil {
			return nil, errors.New("cannot clean private failed backup workspace")
		}
		if !errors.Is(collectErr, online.ErrSourceMissing) && !errors.Is(collectErr, online.ErrSourceChanged) {
			return nil, collectErr
		}
		if attempt == opts.Attempts {
			return nil, collectErr
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, online.ErrSourceChanged
}

func resolvedBackupDirectory(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", online.ErrUnsafePath
	}
	name, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", online.ErrFileIO
	}
	name, err = filepath.Abs(name)
	if err != nil {
		return "", online.ErrUnsafePath
	}
	info, err := os.Stat(name)
	if err != nil || !info.IsDir() {
		return "", online.ErrUnsafePath
	}
	return name, nil
}

func backupPathWithin(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && filepath.IsLocal(rel)
}

func backupRelativePath(root, name string) (string, error) {
	if name == "" || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", online.ErrUnsafePath
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", online.ErrUnsafePath
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", online.ErrUnsafePath
	}
	rel = filepath.ToSlash(rel)
	if !fs.ValidPath(rel) || strings.Contains(rel, "\\") || len(rel) > 4096 {
		return "", online.ErrUnsafePath
	}
	return rel, nil
}

func openFrozenBackupDB(name string) (*sql.DB, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, errBackupSnapshotInvalid
	}
	name = filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=ro&immutable=1&_pragma=trusted_schema(OFF)"}
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

type backupSnapshotBudget struct {
	bytes   int64
	entries int
	report  *OnlineBackupReport
}

func (b *backupSnapshotBudget) parents(root *os.Root, name string) error {
	if !fs.ValidPath(name) || strings.Contains(name, "\\") || len(name) > 4096 {
		return online.ErrUnsafePath
	}
	parts := strings.Split(path.Dir(name), "/")
	for i := range parts {
		if parts[i] == "." {
			continue
		}
		dir := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if b.entries < 1 {
				return online.ErrFileLimit
			}
			if err := root.Mkdir(dir, 0o700); err != nil {
				return online.ErrFileIO
			}
			b.entries--
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return online.ErrUnsafePath
		}
	}
	return nil
}

func (b *backupSnapshotBudget) account(kind, messageID, attachmentID string, file online.CollectedFile) error {
	if file.Bytes < 0 || file.Bytes > b.bytes || b.entries < 1 {
		return online.ErrFileLimit
	}
	b.bytes -= file.Bytes
	b.entries--
	b.report.Files = append(b.report.Files, OnlineBackupFile{Kind: kind, MessageID: messageID, AttachmentID: attachmentID, CollectedFile: file})
	return nil
}

func (b *backupSnapshotBudget) write(ctx context.Context, root *os.Root, kind, messageID, name, source string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > b.bytes || b.entries < 1 {
		return online.ErrFileLimit
	}
	if err := b.parents(root, name); err != nil {
		return err
	}
	if b.entries < 1 {
		return online.ErrFileLimit
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return online.ErrFileIO
	}
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return online.ErrFileIO
	}
	hash := sha256.Sum256(data)
	return b.account(kind, messageID, "", online.CollectedFile{Source: source, Destination: name, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(data))})
}

func (b *backupSnapshotBudget) copy(ctx context.Context, source, output *os.Root, mappings []online.FileMapping) ([]online.CollectedFile, error) {
	if len(mappings) == 0 {
		return nil, nil
	}
	for _, m := range mappings {
		if err := b.parents(output, m.Destination); err != nil {
			return nil, err
		}
	}
	return online.CopyFiles(ctx, source, output, mappings, online.FileLimits{MaxBytes: b.bytes, MaxEntries: b.entries})
}

func collectOnlineBackup(ctx context.Context, cfg Config, directory string, dataRoot, mailRoot *os.Root, opts OnlineBackupOptions, attempt int, hooks *backupSnapshotHooks) (OnlineBackupReport, error) {
	report := OnlineBackupReport{Version: 1, Scope: "sqlite-reference-closure", Files: []OnlineBackupFile{}}
	report.Sources.Data, report.Sources.Mail = cfg.DataDir, cfg.MaildirRoot
	report.Summary.Attempts = attempt
	budget := backupSnapshotBudget{bytes: opts.MaxBytes, entries: opts.MaxEntries, report: &report}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return report, online.ErrFileIO
	}
	defer root.Close()
	for _, name := range []string{"data/lanqin.db", "mail/.placeholder", "dkim/.placeholder"} {
		if err := budget.parents(root, name); err != nil {
			return report, err
		}
	}
	database := filepath.Join(directory, "data", "lanqin.db")
	report.Summary.SnapshotStarted = time.Now().UTC()
	if err := online.SnapshotSQLite(ctx, cfg.DBPath, database, budget.bytes); err != nil {
		return report, err
	}
	report.Summary.SnapshotFinished = time.Now().UTC()
	omitted, err := sanitizeBackupExportRuntime(ctx, database)
	if err != nil {
		return report, err
	}
	report.Summary.RuntimeBackupJobsOmitted = omitted
	report.Summary.RuntimeBackupQueueCleared = true
	file, err := hashBackupStagedFile(ctx, root, "data/lanqin.db")
	if err != nil {
		return report, err
	}
	if err := budget.account("database", "", "", file); err != nil {
		return report, err
	}
	if hooks != nil && hooks.afterSnapshot != nil {
		if err := hooks.afterSnapshot(attempt, database); err != nil {
			return report, err
		}
	}
	frozen, err := openFrozenBackupDB(database)
	if err != nil {
		return report, errBackupSnapshotInvalid
	}
	defer frozen.Close()
	if err := collectBackupAttachments(ctx, frozen, cfg, dataRoot, root, &budget); err != nil {
		return report, err
	}
	limits := online.DefaultMaildirLimits()
	limits.MaxFiles = opts.MaxEntries
	inventory, err := online.ScanMaildir(ctx, mailRoot, limits)
	if err != nil {
		return report, err
	}
	index, err := online.NewMaildirIndex(inventory)
	if err != nil {
		return report, err
	}
	if hooks != nil && hooks.afterInventory != nil {
		if err := hooks.afterInventory(attempt); err != nil {
			return report, err
		}
	}
	if err := collectBackupMessages(ctx, frozen, cfg, root, mailRoot, inventory, index, &budget, opts); err != nil {
		return report, err
	}
	dkim, err := root.OpenRoot("dkim")
	if err != nil {
		return report, online.ErrFileIO
	}
	count, err := online.CollectDKIMLimited(ctx, database, dkim, budget.entries, budget.bytes)
	dkim.Close()
	if err != nil {
		return report, err
	}
	keys, err := root.FS().Open("dkim")
	if err != nil {
		return report, online.ErrFileIO
	}
	dir, ok := keys.(fs.ReadDirFile)
	if !ok {
		keys.Close()
		return report, online.ErrFileIO
	}
	names, err := dir.ReadDir(count + 1)
	keys.Close()
	if err != nil && err != io.EOF || len(names) != count {
		return report, online.ErrFileIO
	}
	for _, entry := range names {
		file, err := hashBackupStagedFile(ctx, root, "dkim/"+entry.Name())
		if err != nil {
			return report, err
		}
		if err := budget.account("dkim", "", "", file); err != nil {
			return report, err
		}
	}
	report.Summary.DKIMKeys = count
	report.Summary.Bytes = opts.MaxBytes - budget.bytes
	report.Complete = true
	metadata, err := json.Marshal(report)
	if err != nil || len(metadata) > 64<<20 {
		return report, online.ErrFileLimit
	}
	// The report does not recursively include a hash of itself.
	if err := budget.write(ctx, root, "report", "", "data/backup-online.json", "", metadata); err != nil {
		return report, err
	}
	report.Files = report.Files[:len(report.Files)-1]
	return report, ctx.Err()
}

func hashBackupStagedFile(ctx context.Context, root *os.Root, name string) (online.CollectedFile, error) {
	f, err := root.Open(name)
	if err != nil {
		return online.CollectedFile{}, online.ErrFileIO
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, backupSnapshotReader{ctx, f})
	if err != nil {
		if ctx.Err() != nil {
			return online.CollectedFile{}, ctx.Err()
		}
		return online.CollectedFile{}, online.ErrFileIO
	}
	return online.CollectedFile{Destination: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: n}, nil
}

type backupSnapshotReader struct {
	ctx context.Context
	r   io.Reader
}

func (r backupSnapshotReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func collectBackupAttachments(ctx context.Context, frozen *sql.DB, cfg Config, data, output *os.Root, budget *backupSnapshotBudget) error {
	rows, err := frozen.QueryContext(ctx, `SELECT a.id,a.message_id,a.storage_path,a.size_bytes,m.id FROM attachments a LEFT JOIN messages m ON m.id=a.message_id ORDER BY a.id`)
	if err != nil {
		return errBackupSnapshotInvalid
	}
	type attachment struct {
		id, message, path string
		size              int64
	}
	items := []attachment{}
	for rows.Next() {
		var item attachment
		var parent sql.NullString
		if len(items) >= budget.entries || rows.Scan(&item.id, &item.message, &item.path, &item.size, &parent) != nil || !parent.Valid || item.size < 0 {
			rows.Close()
			return errBackupSnapshotInvalid
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return errBackupSnapshotInvalid
	}
	mappings := make([]online.FileMapping, 0, len(items))
	for i := range items {
		item := &items[i]
		rel, err := backupRelativePath(cfg.DataDir, item.path)
		parts := strings.Split(rel, "/")
		if err != nil || len(parts) != 3 || parts[0] != "attachments" || parts[1] != item.message || !strings.HasPrefix(parts[2], item.id+"_") {
			return online.ErrUnsafePath
		}
		mappings = append(mappings, online.FileMapping{Source: rel, Destination: "data/" + rel, ExpectedBytes: &item.size})
	}
	collected, err := budget.copy(ctx, data, output, mappings)
	if err != nil {
		return err
	}
	for i, file := range collected {
		if err := budget.account("attachment", items[i].message, items[i].id, file); err != nil {
			return err
		}
	}
	budget.report.Summary.Attachments = len(items)
	return nil
}

func collectBackupMessages(ctx context.Context, frozen *sql.DB, cfg Config, output, mailRoot *os.Root, inventory []online.MaildirEntry, index *online.MaildirIndex, budget *backupSnapshotBudget, opts OnlineBackupOptions) error {
	rows, err := frozen.QueryContext(ctx, `SELECT m.id,m.raw_path,COALESCE(m.mailbox_id,''),m.recipient_addr,COALESCE(f.role,''),COALESCE(d.name,''),COALESCE(b.local_part,''),COALESCE(f.mailbox_id,''),
		CASE WHEN EXISTS(SELECT 1 FROM external_imap_messages em WHERE em.local_message_id=m.id)
		OR EXISTS(SELECT 1 FROM external_imap_folder_states efs WHERE efs.local_folder_id=f.id) THEN 1 ELSE 0 END
		FROM messages m LEFT JOIN mailboxes b ON b.id=m.mailbox_id LEFT JOIN domains d ON d.id=b.domain_id LEFT JOIN folders f ON f.id=m.folder_id ORDER BY m.id`)
	if err != nil {
		return errBackupSnapshotInvalid
	}
	type message struct {
		id, raw, mailbox, recipient, role, domain, local, folderMailbox string
		external                                                        int
	}
	items := []message{}
	for rows.Next() {
		var item message
		if len(items) >= opts.MaxEntries || rows.Scan(&item.id, &item.raw, &item.mailbox, &item.recipient, &item.role, &item.domain, &item.local, &item.folderMailbox, &item.external) != nil {
			rows.Close()
			return errBackupSnapshotInvalid
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return errBackupSnapshotInvalid
	}
	used := map[string]bool{}
	draftRoot, err := output.OpenRoot("data")
	if err != nil {
		return online.ErrFileIO
	}
	defer draftRoot.Close()
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.external != 0 && item.raw == "" {
			// Preserve the DB/attachment cache, without pretending an upstream
			// original RFC822 was downloaded or activating mirrored local mail.
			budget.report.Summary.ExternalCachedMessagesOnly++
			continue
		}
		owner := ""
		if item.mailbox != "" {
			if !backupSnapshotComponent(item.domain) || !backupSnapshotComponent(item.local) || item.mailbox != item.folderMailbox {
				return errBackupSnapshotInvalid
			}
			owner = item.domain + "/" + item.local + "/Maildir"
		} else {
			at := strings.LastIndex(item.recipient, "@")
			if at < 0 || !backupSnapshotComponent(item.recipient[at+1:]) {
				return errBackupSnapshotInvalid
			}
			owner = item.recipient[at+1:] + "/__unregistered__/Maildir"
		}
		rel := ""
		if item.raw != "" {
			rel, err = backupRelativePath(cfg.MaildirRoot, item.raw)
			if err != nil || !strings.HasPrefix(rel, owner+"/") {
				return online.ErrUnsafePath
			}
		}
		if item.role == "drafts" && item.mailbox != "" && item.external == 0 {
			if rel == "" {
				hash := sha256.Sum256([]byte(item.id))
				rel = owner + "/.Drafts/cur/backup-" + hex.EncodeToString(hash[:16]) + ".eml"
			}
			if entry, resolveErr := index.ResolveReference(rel); resolveErr == nil {
				if used[entry.Path] {
					return errBackupSnapshotInvalid
				}
				used[entry.Path] = true
			} else if !errors.Is(resolveErr, online.ErrSourceMissing) {
				return resolveErr
			}
			raw, err := rebuildBackupDraft(ctx, frozen, item.id, cfg.DataDir, draftRoot, min(opts.MaxMIMEBytes, budget.bytes))
			if err != nil {
				return err
			}
			if err := budget.write(ctx, output, "reconstructed-draft", item.id, "mail/"+rel, item.raw, raw); err != nil {
				return err
			}
			budget.report.Summary.Drafts++
		} else {
			if rel == "" {
				return online.ErrSourceMissing // Backfill/reconcile must complete; do not invent original RFC822.
			}
			entry, err := index.ResolveReference(rel)
			if err != nil {
				return err
			}
			if entry.Mailbox != owner || used[entry.Path] {
				return errBackupSnapshotInvalid
			}
			// Ordinary committed MIME business content is immutable; flag/folder
			// operations rename it. Bind the scanned inode, not wall-clock order:
			// Maildir mtime is IMAP INTERNALDATE and may legitimately be future.
			files, err := budget.copy(ctx, mailRoot, output, []online.FileMapping{{Source: entry.Path, Destination: "mail/" + rel, ExpectedBytes: &entry.Bytes, ExpectedInfo: entry.Info}})
			if err != nil {
				return err
			}
			if err := budget.account("message", item.id, "", files[0]); err != nil {
				return err
			}
			internalDate := entry.ModTime
			budget.report.Files[len(budget.report.Files)-1].InternalDate = &internalDate
			used[entry.Path] = true
		}
		budget.report.Summary.Messages++
	}
	// Do not silently drop fresh, unindexed deliveries, and do not automatically
	// resurrect files whose row was deleted near the DB cut. Preserve candidates
	// in an isolated, mapped payload for explicit recovery reconciliation.
	for _, entry := range inventory {
		if used[entry.Path] {
			continue
		}
		hash := sha256.Sum256([]byte(entry.Path))
		name := "data/backup-unindexed/" + hex.EncodeToString(hash[:]) + ".eml"
		files, err := budget.copy(ctx, mailRoot, output, []online.FileMapping{{Source: entry.Path, Destination: name, ExpectedBytes: &entry.Bytes, ExpectedInfo: entry.Info}})
		if err != nil {
			return err
		}
		if err := budget.account("unindexed-candidate", "", "", files[0]); err != nil {
			return err
		}
		internalDate := entry.ModTime
		budget.report.Files[len(budget.report.Files)-1].InternalDate = &internalDate
		budget.report.Summary.Unindexed++
	}
	return nil
}

func backupSnapshotComponent(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 253 && !strings.ContainsAny(s, "/\\:\x00") && strings.IndexFunc(s, unicode.IsControl) < 0
}
