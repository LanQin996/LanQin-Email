// Package install prepares authenticated restore staging for a separate,
// maintenance-window installer. Prepare never opens live service directories.
package install

import (
	"bytes"
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
	"unicode/utf8"

	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/online"
)

var errInvalid = errors.New("restore staging is unsafe, incomplete, changed, or unsupported")

const reportName = "data/backup-online.json"
const sourceReportName = "data/restore-source-report.json"
const preparedMarkerName = "data/.lanqin-restore-not-activated"
const preparedMarkerContent = "prepared-online-restore-v1\n"

// Prepared owns a new private tree. Path is not an active deployment location.
// INSTALL-PLAN.json contains only aggregate recovery information, not mail IDs.
type Prepared struct {
	Path  string
	close func() error
}

func (p *Prepared) Close() error {
	if p == nil || p.close == nil {
		return nil
	}
	if err := p.close(); err != nil {
		return err
	}
	p.close = nil
	return nil
}

type reportFile struct {
	Kind         string     `json:"kind"`
	MessageID    string     `json:"messageId"`
	AttachmentID string     `json:"attachmentId"`
	InternalDate *time.Time `json:"internalDate"`
	online.CollectedFile
}

type collectionReport struct {
	Version  int             `json:"version"`
	Scope    string          `json:"scope"`
	Complete bool            `json:"complete"`
	Summary  json.RawMessage `json:"summary"`
	Sources  struct {
		Data string `json:"data"`
		Mail string `json:"mail"`
	} `json:"sources"`
	Files []reportFile `json:"files"`
}

type recoveryPlan struct {
	Version                      int            `json:"version"`
	SendQueue                    map[string]int `json:"sendQueueCanceledByOriginalStatus"`
	Scheduled                    map[string]int `json:"scheduledSendsCancelledByOriginalStatus"`
	LocalDelivery                map[string]int `json:"localDeliveryPausedByOriginalStatus"`
	SessionsRevoked              int            `json:"sessionsRevoked"`
	LoginChallengesRevoked       int            `json:"loginChallengesRevoked"`
	APITokensDisabled            int            `json:"apiTokensDisabled"`
	BackupRuntimeSettingsRemoved int            `json:"backupRuntimeSettingsRemoved"`
	WebhookNotificationsHeld     int            `json:"webhookNotificationsHeld"`
	TelegramNotificationsHeld    int            `json:"telegramNotificationsHeld"`
	Unindexed                    int            `json:"quarantinedUnindexedCandidates"`
	Notice                       string         `json:"notice"`
}

// Prepare accepts only backup.Restore's READY + snapshot container and a complete
// online collector report. Offline archives lack authoritative source mappings
// and must not be guessed. runtime roots are Linux/container paths, NEVER host
// deployment paths. The caller must provide an existing private workspace;
// Windows inherits its operator-configured ACL (0700 alone does not set a DACL).
func Prepare(ctx context.Context, restoredContainer, workspace, runtimeDataRoot, runtimeMailRoot string, limits backup.Limits) (_ *Prepared, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.MaxBytes < 1 || limits.MaxEntries < 1 || !runtimeRoot(runtimeDataRoot) || !runtimeRoot(runtimeMailRoot) || rootsOverlap(runtimeDataRoot, runtimeMailRoot) {
		return nil, errInvalid
	}
	sourcePath, err := materializeDirectory(restoredContainer)
	if err != nil {
		return nil, errInvalid
	}
	workPath, err := materializeDirectory(workspace)
	if err != nil || within(sourcePath, workPath) || within(workPath, sourcePath) {
		return nil, errInvalid
	}
	workInfo, err := os.Stat(workPath)
	if err != nil || runtime.GOOS != "windows" && workInfo.Mode().Perm()&0o077 != 0 {
		return nil, errInvalid
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil || !materializePrivateInfo(sourceInfo) || !materializePrivateInfo(workInfo) {
		return nil, errInvalid
	}
	container, err := os.OpenRoot(sourcePath)
	if err != nil {
		return nil, errInvalid
	}
	defer container.Close()
	containerHandle, err := container.Open(".")
	if err != nil {
		return nil, errInvalid
	}
	pinnedContainer, containerStatErr := containerHandle.Stat()
	containerCloseErr := containerHandle.Close()
	if containerStatErr != nil || containerCloseErr != nil || !pinnedContainer.IsDir() || !os.SameFile(sourceInfo, pinnedContainer) || !materializePrivateInfo(pinnedContainer) {
		return nil, errInvalid
	}
	ready, err := container.Lstat("READY")
	if err != nil || !ready.Mode().IsRegular() || ready.Size() < 1 || ready.Size() > 4096 {
		return nil, errInvalid
	}
	readyHandle, err := container.Open("READY")
	if err != nil {
		return nil, errInvalid
	}
	pinnedReady, statErr := readyHandle.Stat()
	readyCloseErr := readyHandle.Close()
	if statErr != nil || readyCloseErr != nil || !pinnedReady.Mode().IsRegular() || !os.SameFile(ready, pinnedReady) || pinnedReady.Size() != ready.Size() {
		return nil, errInvalid
	}
	ready = pinnedReady
	si, err := container.Lstat("snapshot")
	if err != nil || !si.IsDir() || si.Mode()&os.ModeSymlink != 0 || !materializePrivateInfo(si) {
		return nil, errInvalid
	}
	source, err := container.OpenRoot("snapshot")
	if err != nil {
		return nil, errInvalid
	}
	defer source.Close()
	snapshotHandle, err := source.Open(".")
	if err != nil {
		return nil, errInvalid
	}
	pinnedSnapshot, snapshotStatErr := snapshotHandle.Stat()
	snapshotCloseErr := snapshotHandle.Close()
	if snapshotStatErr != nil || snapshotCloseErr != nil || !pinnedSnapshot.IsDir() || !os.SameFile(si, pinnedSnapshot) || !materializePrivateInfo(pinnedSnapshot) {
		return nil, errInvalid
	}
	si = pinnedSnapshot
	files, dirs, err := inventory(ctx, source, limits)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, ok := files["data/lanqin.db"+suffix]; ok {
			return nil, errInvalid
		}
	}
	if _, ok := files[reportName]; !ok || files[reportName].Size() > 64<<20 {
		return nil, errInvalid
	}
	if _, ok := files["data/lanqin.db"]; !ok {
		return nil, errInvalid
	}
	directory, err := os.MkdirTemp(workPath, ".lanqin-install-*")
	if err != nil {
		return nil, errInvalid
	}
	ownedDir, err := os.Open(directory)
	if err != nil {
		os.Remove(directory) // Only remove this newly allocated empty directory.
		return nil, errInvalid
	}
	owned, statErr := ownedDir.Stat()
	closeErr := ownedDir.Close()
	if statErr != nil || closeErr != nil {
		os.Remove(directory)
		return nil, errInvalid
	}
	cleanup := func() error {
		current, err := os.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(owned, current) {
			return errInvalid
		}
		return os.RemoveAll(directory)
	}
	defer func() {
		if result != nil {
			if err := cleanup(); err != nil {
				result = errors.New("cannot clean failed private restore preparation")
			}
		}
	}()
	output, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errInvalid
	}
	defer output.Close()
	for _, name := range dirs {
		if err := output.MkdirAll(name, 0o700); err != nil {
			return nil, errInvalid
		}
	}
	mappings := make([]online.FileMapping, 0, len(files))
	for name, info := range files {
		mappings = append(mappings, online.FileMapping{Source: name, Destination: name, ExpectedInfo: info})
	}
	// Never preserve archive modes, owners, links or executable bits. The pinned
	// copier creates independent 0600 regular files and rehashes its open input.
	copied, err := online.CopyFiles(ctx, source, output, mappings, online.FileLimits{MaxBytes: limits.MaxBytes, MaxEntries: limits.MaxEntries})
	if err != nil {
		return nil, err
	}
	content, err := output.ReadFile(reportName)
	if err != nil {
		return nil, errInvalid
	}
	report, _, err := validateReport(content, copied)
	if err != nil {
		return nil, err
	}
	// Revalidate the whole source after copying, including files copied before a
	// later file. No extra payload can be silently ignored or promoted.
	finalFiles, finalDirs, err := inventory(ctx, source, limits)
	if err != nil || len(finalFiles) != len(files) || len(finalDirs) != len(dirs) {
		return nil, errInvalid
	}
	for _, file := range copied {
		before, after := files[file.Destination], finalFiles[file.Destination]
		if after == nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return nil, errInvalid
		}
		if err := verifyFile(ctx, source, file, before); err != nil {
			return nil, err
		}
	}
	currentReady, err := container.Lstat("READY")
	currentSnapshot, snapshotErr := container.Lstat("snapshot")
	if err != nil || snapshotErr != nil || !os.SameFile(ready, currentReady) || !currentReady.Mode().IsRegular() || !os.SameFile(si, currentSnapshot) || !currentSnapshot.IsDir() || currentSnapshot.Mode()&os.ModeSymlink != 0 {
		return nil, errInvalid
	}
	plan, err := remapDatabase(ctx, filepath.Join(directory, "data", "lanqin.db"), report, runtimeDataRoot, runtimeMailRoot, limits)
	if err != nil {
		return nil, err
	}
	for _, item := range report.Files {
		if item.InternalDate != nil {
			if err := output.Chtimes(item.Destination, *item.InternalDate, *item.InternalDate); err != nil {
				return nil, errInvalid
			}
		}
	}
	plan.Version = 1
	plan.Notice = "Prepared restore is NOT activated: use the maintenance-window installer; never mount this tree directly or skip install. Sessions and login challenges revoked; API tokens disabled with metadata retained. Send payloads retained; canceled or restore_paused work requires explicit operator review before requeue. Pending webhook and Telegram notification payloads retained with next_attempt_at held at 9999-12-31T23:59:59Z. Unindexed candidates remain quarantined. External-account synchronization is not paused by this tool. Review worker activation before starting services; this tool never starts services. Deployment secrets and live service activation are separate."
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, errInvalid
	}
	data = append(data, '\n')
	// Account for rewritten SQLite growth AND all new metadata files before
	// publishing any activation metadata. Renaming the report adds no bytes.
	finalFiles, finalDirs, err = inventory(ctx, output, limits)
	if err != nil {
		return nil, err
	}
	if len(finalFiles)+len(finalDirs) > limits.MaxEntries-3 {
		return nil, errInvalid
	}
	metadataBytes := int64(2*len(data)) + int64(len(preparedMarkerContent))
	if metadataBytes > limits.MaxBytes {
		return nil, errInvalid
	}
	remaining := limits.MaxBytes - metadataBytes
	for _, file := range finalFiles {
		if file.Size() > remaining {
			return nil, errInvalid
		}
		remaining -= file.Size()
	}
	// Reserve the new name exclusively before replacing only our own empty
	// reservation. No imported destination or previous report is overwritten.
	if err := writePreparedExclusive(output, sourceReportName, nil); err != nil {
		return nil, err
	}
	if err := output.Rename(reportName, sourceReportName); err != nil {
		return nil, errInvalid
	}
	if err := writePreparedExclusive(output, preparedMarkerName, []byte(preparedMarkerContent)); err != nil {
		return nil, err
	}
	if err := writePreparedExclusive(output, "INSTALL-PLAN.json", data); err != nil {
		return nil, errInvalid
	}
	// Keep a private copy with installed data: the root plan belongs to the
	// temporary preparation tree and is deleted by Prepared.Close after Apply.
	if err := writePreparedExclusive(output, "data/restore-install-plan.json", data); err != nil {
		return nil, errInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Prepared{Path: directory, close: cleanup}, nil
}

func writePreparedExclusive(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errInvalid
	}
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errInvalid
	}
	return nil
}

func runtimeRoot(s string) bool {
	return s != "/" && strings.HasPrefix(s, "/") && path.Clean(s) == s && utf8.ValidString(s) && !strings.ContainsAny(s, "\\:\x00") && strings.IndexFunc(s, unicode.IsControl) < 0 && len(s) <= 4096
}

func rootsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func materializeDirectory(name string) (string, error) {
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errInvalid
	}
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

func within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && filepath.IsLocal(rel)
}

func allowed(name string) bool {
	if !fs.ValidPath(name) || name == "." || !utf8.ValidString(name) || len(name) > 4096 || strings.Count(name, "/") > 64 || strings.ContainsAny(name, "\\:\x00") || strings.IndexFunc(name, unicode.IsControl) >= 0 || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	first := strings.SplitN(name, "/", 2)[0]
	return first == "data" || first == "mail" || first == "dkim"
}

func inventory(ctx context.Context, root *os.Root, limits backup.Limits) (map[string]os.FileInfo, []string, error) {
	files := map[string]os.FileInfo{}
	dirs := []string{}
	var total int64
	entries := 0
	for _, name := range []string{"data", "mail", "dkim"} {
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errInvalid
		}
	}
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return errInvalid
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > limits.MaxEntries || !allowed(name) {
			return errInvalid
		}
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errInvalid
		}
		if info.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limits.MaxBytes-total {
			return errInvalid
		}
		// Stat the open handle now (not a lazy Windows pathname identity).
		f, err := root.Open(name)
		if err != nil {
			return errInvalid
		}
		pinned, statErr := f.Stat()
		closeErr := f.Close()
		if statErr != nil || closeErr != nil || !pinned.Mode().IsRegular() || !os.SameFile(info, pinned) || info.Size() != pinned.Size() || !info.ModTime().Equal(pinned.ModTime()) {
			return errInvalid
		}
		total += pinned.Size()
		files[name] = pinned
		return nil
	})
	return files, dirs, err
}

func validateReport(data []byte, copied []online.CollectedFile) (collectionReport, map[string]reportFile, error) {
	var report collectionReport
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&report) != nil || decoder.Decode(new(any)) != io.EOF || report.Version != 1 || report.Scope != "sqlite-reference-closure" || !report.Complete || len(bytes.TrimSpace(report.Summary)) == 0 || bytes.TrimSpace(report.Summary)[0] != '{' || report.Sources.Data == "" || report.Sources.Mail == "" || !validSourceRoot(report.Sources.Data) || !validSourceRoot(report.Sources.Mail) || report.Files == nil || len(report.Files)+1 != len(copied) {
		return report, nil, errInvalid
	}
	expected := map[string]reportFile{}
	for _, item := range report.Files {
		if !allowed(item.Destination) || item.Destination == reportName || item.Bytes < 0 {
			return report, nil, errInvalid
		}
		if _, exists := expected[item.Destination]; exists {
			return report, nil, errInvalid
		}
		hash, err := hex.DecodeString(item.SHA256)
		if err != nil || len(hash) != sha256.Size {
			return report, nil, errInvalid
		}
		switch item.Kind {
		case "database":
			if item.Destination != "data/lanqin.db" {
				return report, nil, errInvalid
			}
		case "attachment":
			if !strings.HasPrefix(item.Destination, "data/attachments/") || item.AttachmentID == "" || item.MessageID == "" {
				return report, nil, errInvalid
			}
		case "message", "reconstructed-draft":
			if !strings.HasPrefix(item.Destination, "mail/") || item.MessageID == "" {
				return report, nil, errInvalid
			}
		case "unindexed-candidate":
			if !strings.HasPrefix(item.Destination, "data/backup-unindexed/") || item.MessageID != "" || item.AttachmentID != "" {
				return report, nil, errInvalid
			}
		case "dkim":
			if !strings.HasPrefix(item.Destination, "dkim/") {
				return report, nil, errInvalid
			}
		default:
			return report, nil, errInvalid
		}
		if item.InternalDate != nil && (item.InternalDate.IsZero() || item.Kind != "message" && item.Kind != "unindexed-candidate") {
			return report, nil, errInvalid
		}
		expected[item.Destination] = item
	}
	if expected["data/lanqin.db"].Kind != "database" || len(expected)+1 != len(copied) {
		return report, nil, errInvalid
	}
	for _, item := range copied {
		if item.Destination == reportName {
			continue
		}
		want, ok := expected[item.Destination]
		if !ok || item.Bytes != want.Bytes || !strings.EqualFold(item.SHA256, want.SHA256) {
			return report, nil, errInvalid
		}
	}
	return report, expected, nil
}

func validSourceRoot(name string) bool {
	// Collector roots may be Windows paths even though runtime roots are Linux.
	return utf8.ValidString(name) && len(name) <= 4096 && strings.IndexFunc(name, unicode.IsControl) < 0 && name != "/" && name != "\\" && strings.TrimRight(name, "/\\") == name
}

func sourceRelative(root, name string) (string, error) {
	separator := "/"
	if strings.Contains(root, "\\") {
		separator = "\\"
	}
	if !strings.HasPrefix(name, root+separator) {
		return "", errInvalid
	}
	rel := strings.TrimPrefix(name, root+separator)
	if separator == "\\" {
		rel = strings.ReplaceAll(rel, "\\", "/")
	}
	if !allowed("data/" + rel) {
		return "", errInvalid
	}
	return rel, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func verifyFile(ctx context.Context, root *os.Root, expected online.CollectedFile, observed os.FileInfo) error {
	checkParents := func() error {
		parts := strings.Split(expected.Destination, "/")
		for i := 1; i < len(parts); i++ {
			info, err := root.Lstat(strings.Join(parts[:i], "/"))
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errInvalid
			}
		}
		return nil
	}
	stable := func(info os.FileInfo) bool {
		return info != nil && info.Mode().IsRegular() && os.SameFile(observed, info) && observed.Size() == info.Size() && observed.ModTime().Equal(info.ModTime())
	}
	if err := checkParents(); err != nil {
		return err
	}
	before, err := root.Lstat(expected.Destination)
	if err != nil || !stable(before) {
		return errInvalid
	}
	f, err := root.Open(expected.Destination)
	if err != nil {
		return errInvalid
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !stable(opened) {
		return errInvalid
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, io.LimitReader(f, expected.Bytes+1)})
	if err != nil {
		return err
	}
	if n != expected.Bytes || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), expected.SHA256) {
		return errInvalid
	}
	final, err := f.Stat()
	if err != nil || !stable(final) {
		return errInvalid
	}
	pathFinal, err := root.Lstat(expected.Destination)
	if err != nil || !stable(pathFinal) {
		return errInvalid
	}
	if err := checkParents(); err != nil {
		return err
	}
	return nil
}

func remapDatabase(ctx context.Context, database string, report collectionReport, dataRoot, mailRoot string, limits backup.Limits) (plan recoveryPlan, result error) {
	name := filepath.ToSlash(database)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	// Never activate imported ON UPDATE cascades while rewriting paths. Validate
	// all foreign keys before and after instead, rejecting any resulting orphan.
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=rw&_pragma=trusted_schema(OFF)&_pragma=foreign_keys(OFF)&_pragma=busy_timeout(100)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return plan, errInvalid
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil && result == nil {
			result = errInvalid
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(database + suffix); !errors.Is(err, fs.ErrNotExist) && result == nil {
				result = errInvalid
			}
		}
		if info, err := os.Stat(database); (err != nil || info.Size() > limits.MaxBytes) && result == nil {
			result = errInvalid
		}
	}()
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
		return plan, errInvalid
	}
	if err := checkDatabase(ctx, db); err != nil {
		return plan, err
	}
	for _, table := range []string{"messages", "attachments", "send_queue", "scheduled_sends"} {
		var kind, schema string
		if err := db.QueryRowContext(ctx, "SELECT type,sql FROM sqlite_schema WHERE name=?", table).Scan(&kind, &schema); err != nil || kind != "table" || !strings.HasPrefix(strings.ToLower(schema), "create table ") {
			return plan, errInvalid
		}
	}
	optionalTables := map[string]bool{}
	for _, table := range []string{"sessions", "login_challenges", "api_tokens", "local_delivery_jobs", "status_webhook_outbox", "telegram_notification_outbox", "system_settings"} {
		var name, kind, schema string
		err := db.QueryRowContext(ctx, "SELECT name,type,sql FROM sqlite_schema WHERE name=? COLLATE NOCASE", table).Scan(&name, &kind, &schema)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil || name != table || kind != "table" || !strings.HasPrefix(strings.ToLower(schema), "create table ") {
			return plan, errInvalid
		}
		optionalTables[table] = true
	}
	if err := checkTriggers(ctx, db); err != nil {
		return plan, err
	}
	byMessage, byAttachment := map[string]reportFile{}, map[string]reportFile{}
	for _, item := range report.Files {
		switch item.Kind {
		case "message", "reconstructed-draft":
			if _, exists := byMessage[item.MessageID]; exists {
				return plan, errInvalid
			}
			byMessage[item.MessageID] = item
		case "attachment":
			if _, exists := byAttachment[item.AttachmentID]; exists {
				return plan, errInvalid
			}
			byAttachment[item.AttachmentID] = item
		case "unindexed-candidate":
			plan.Unindexed++
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return plan, errInvalid
	}
	defer tx.Rollback()
	type mapping struct{ id, target string }
	attachments := []mapping{}
	rows, err := tx.QueryContext(ctx, "SELECT id,message_id,storage_path,size_bytes FROM attachments")
	if err != nil {
		return plan, errInvalid
	}
	for rows.Next() {
		var id, message, old string
		var size int64
		if len(attachments) >= limits.MaxEntries || rows.Scan(&id, &message, &old, &size) != nil {
			rows.Close()
			return plan, errInvalid
		}
		rel, err := sourceRelative(report.Sources.Data, old)
		item, ok := byAttachment[id]
		if err != nil || !ok || item.MessageID != message || item.Destination != "data/"+rel || item.Bytes != size {
			rows.Close()
			return plan, errInvalid
		}
		attachments = append(attachments, mapping{id, dataRoot + "/" + rel})
		delete(byAttachment, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(byAttachment) != 0 {
		return plan, errInvalid
	}
	messages := []mapping{}
	rows, err = tx.QueryContext(ctx, `SELECT m.id,m.raw_path,CASE WHEN EXISTS(SELECT 1 FROM external_imap_messages e WHERE e.local_message_id=m.id) OR EXISTS(SELECT 1 FROM external_imap_folder_states f WHERE f.local_folder_id=m.folder_id) THEN 1 ELSE 0 END FROM messages m`)
	if err != nil {
		return plan, errInvalid
	}
	count := 0
	for rows.Next() {
		var id, old string
		var external int
		count++
		if count > limits.MaxEntries || rows.Scan(&id, &old, &external) != nil {
			rows.Close()
			return plan, errInvalid
		}
		item, ok := byMessage[id]
		if old == "" && !ok && external != 0 {
			continue
		}
		if !ok {
			rows.Close()
			return plan, errInvalid
		}
		if old == "" {
			if item.Kind != "reconstructed-draft" || external != 0 {
				rows.Close()
				return plan, errInvalid
			}
		} else {
			rel, err := sourceRelative(report.Sources.Mail, old)
			if err != nil || item.Destination != "mail/"+rel {
				rows.Close()
				return plan, errInvalid
			}
		}
		messages = append(messages, mapping{id, mailRoot + "/" + strings.TrimPrefix(item.Destination, "mail/")})
		delete(byMessage, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(byMessage) != 0 {
		return plan, errInvalid
	}
	for _, item := range attachments {
		if _, err := tx.ExecContext(ctx, "UPDATE attachments SET storage_path=? WHERE id=?", item.target, item.id); err != nil {
			return plan, errInvalid
		}
	}
	for _, item := range messages {
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET raw_path=? WHERE id=?", item.target, item.id); err != nil {
			return plan, errInvalid
		}
	}
	plan.SendQueue, err = pauseWork(ctx, tx, "send_queue", []string{"queued", "sending", "failed", "pending"}, []string{"delivered", "canceled"}, "canceled", limits.MaxEntries)
	if err != nil {
		return plan, err
	}
	plan.Scheduled, err = pauseWork(ctx, tx, "scheduled_sends", []string{"pending", "sending"}, []string{"sent", "failed", "cancelled"}, "cancelled", limits.MaxEntries)
	if err != nil {
		return plan, err
	}
	if optionalTables["local_delivery_jobs"] {
		plan.LocalDelivery, err = pauseWork(ctx, tx, "local_delivery_jobs", []string{"queued", "sending"}, []string{"delivered", "failed", "restore_paused"}, "restore_paused", limits.MaxEntries)
		if err != nil {
			return plan, err
		}
	}
	for _, change := range []struct {
		table, predicate, statement string
		count                       *int
	}{
		{"sessions", "", "DELETE FROM sessions", &plan.SessionsRevoked},
		{"login_challenges", "", "DELETE FROM login_challenges", &plan.LoginChallengesRevoked},
		{"api_tokens", " WHERE disabled IS NOT 1", "UPDATE api_tokens SET disabled=1 WHERE disabled IS NOT 1", &plan.APITokensDisabled},
		{"system_settings", ` WHERE "key" IN ('backup.queue.v1','backup.schedules.v1') OR substr("key",1,14)='backup.job.v1.' OR substr("key",1,22)='backup.drive.oauth.v1.'`, `DELETE FROM system_settings WHERE "key" IN ('backup.queue.v1','backup.schedules.v1') OR substr("key",1,14)='backup.job.v1.' OR substr("key",1,22)='backup.drive.oauth.v1.'`, &plan.BackupRuntimeSettingsRemoved},
		{"status_webhook_outbox", " WHERE delivered_at IS NULL", "UPDATE status_webhook_outbox SET next_attempt_at='9999-12-31T23:59:59Z' WHERE delivered_at IS NULL", &plan.WebhookNotificationsHeld},
		{"telegram_notification_outbox", " WHERE delivered_at IS NULL", "UPDATE telegram_notification_outbox SET next_attempt_at='9999-12-31T23:59:59Z' WHERE delivered_at IS NULL", &plan.TelegramNotificationsHeld},
	} {
		if !optionalTables[change.table] {
			continue
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+change.table+change.predicate).Scan(change.count); err != nil || *change.count < 0 || *change.count > limits.MaxEntries {
			return plan, errInvalid
		}
		if _, err := tx.ExecContext(ctx, change.statement); err != nil {
			return plan, errInvalid
		}
	}
	// No payload, audit history, timestamps, attempt counts or leases are removed.
	// Existing canceled/cancelled states are excluded by the workers' due queries.
	if err := tx.Commit(); err != nil {
		return plan, errInvalid
	}
	if err := checkDatabase(ctx, db); err != nil {
		return plan, err
	}
	return plan, nil
}

func pauseWork(ctx context.Context, tx *sql.Tx, table string, active, terminal []string, canceled string, maxEntries int) (map[string]int, error) {
	counts := map[string]int{}
	states := map[string]bool{}
	for _, state := range active {
		states[state] = true
	}
	for _, state := range terminal {
		states[state] = false
	}
	rows, err := tx.QueryContext(ctx, "SELECT status,COUNT(*) FROM "+table+" GROUP BY status")
	if err != nil {
		return nil, errInvalid
	}
	total := 0
	for rows.Next() {
		var state string
		var count int
		if rows.Scan(&state, &count) != nil || count < 0 {
			rows.Close()
			return nil, errInvalid
		}
		total += count
		isActive, ok := states[state]
		if !ok || total > maxEntries {
			rows.Close()
			return nil, errInvalid
		}
		if isActive {
			counts[state] = count
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errInvalid
	}
	for state := range counts {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET status=? WHERE status=?", canceled, state); err != nil {
			return nil, errInvalid
		}
	}
	return counts, nil
}

func checkDatabase(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return errInvalid
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return errInvalid
	}
	hasViolation := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil || hasViolation {
		return errInvalid
	}
	return nil
}

func normalizedSQL(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(strings.ToLower(s)), " "), "create trigger if not exists ", "create trigger ")
}

// Keep these definitions synchronized with internal/app/message_search.go and
// app.go. In particular, FTS updates only named content columns, NOT raw_path.
// Names alone are not authority: every imported trigger body must match too.
var knownTriggers = map[string]string{
	"messages_fts_ai": `CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
	VALUES(new.rowid,new.subject,new.from_addr,new.from_name,new.to_addrs,new.recipient_addr,new.snippet,new.body_text); END`,
	"messages_fts_ad": `CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts,rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
	VALUES('delete',old.rowid,old.subject,old.from_addr,old.from_name,old.to_addrs,old.recipient_addr,old.snippet,old.body_text); END`,
	"messages_fts_au": `CREATE TRIGGER messages_fts_au AFTER UPDATE OF subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text ON messages BEGIN
	INSERT INTO messages_fts(messages_fts,rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
	VALUES('delete',old.rowid,old.subject,old.from_addr,old.from_name,old.to_addrs,old.recipient_addr,old.snippet,old.body_text);
	INSERT INTO messages_fts(rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
	VALUES(new.rowid,new.subject,new.from_addr,new.from_name,new.to_addrs,new.recipient_addr,new.snippet,new.body_text); END`,
	"trg_send_queue_delete_delivery_events": `CREATE TRIGGER trg_send_queue_delete_delivery_events AFTER DELETE ON send_queue BEGIN DELETE FROM delivery_events WHERE queue_id=OLD.id; END`,
}

func checkTriggers(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name,sql FROM sqlite_schema WHERE type='trigger' AND tbl_name COLLATE NOCASE IN ('messages','attachments','send_queue','scheduled_sends','sessions','login_challenges','api_tokens','local_delivery_jobs','status_webhook_outbox','telegram_notification_outbox','system_settings')`)
	if err != nil {
		return errInvalid
	}
	defer rows.Close()
	for rows.Next() {
		var name, stmt string
		if rows.Scan(&name, &stmt) != nil {
			return errInvalid
		}
		want, ok := knownTriggers[name]
		if !ok || normalizedSQL(want) != normalizedSQL(stmt) {
			return errInvalid
		}
	}
	if rows.Err() != nil {
		return errInvalid
	}
	return nil
}
