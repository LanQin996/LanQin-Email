package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"lanqin-email-api/internal/backup"
	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/install"
	"lanqin-email-api/internal/backup/storage"
)

var errBackupRestoreBusy = errors.New("backup_restore_busy")
var errBackupRestoreSource = errors.New("backup_restore_source_unavailable")
var errBackupRestoreConfiguration = errors.New("backup_restore_configuration_unavailable")

// The recovery identity is transferred directly to THIS instance's existing
// bounded worker, never serialized in a job, environment, report or log.
type backupRestoreExecution struct {
	Lease          backupLease
	Identity       []byte
	EncryptionMode string
	Secret         []byte
}

type backupRestoreRequest struct {
	SourceJobID          string `json:"sourceJobId"`
	DestinationID        string `json:"destinationId"`
	ExpectedSHA256       string `json:"expectedSHA256"`
	DestinationsRevision string `json:"destinationsRevision"`
	RequestID            string `json:"requestId"`
	Identity             string `json:"identity"`
	EncryptionMode       string `json:"encryptionMode,omitempty"`
	BackupPassword       string `json:"backupPassword,omitempty"`
	Password             string `json:"password"`
	TwoFactorCode        string `json:"twoFactorCode"`
	Confirm              bool   `json:"confirm"`
	DiscoveryEntryID     string `json:"discoveryEntryId,omitempty"`
}

func (a *App) backupRestoreRoot() (string, error) {
	if _, err := a.backupMasterKey(); err != nil || a.backupRestoreRequests == nil {
		return "", errBackupRestoreConfiguration
	}
	root, err := resolvedBackupDirectory(a.cfg.BackupRestoreDir)
	if err != nil {
		return "", errBackupRestoreConfiguration
	}
	info, err := os.Stat(root)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errBackupRestoreConfiguration
	}
	// Recovery data must neither become a live source nor be recursively captured
	// into a subsequent backup. Reject overlap in BOTH directions.
	for _, name := range []string{a.cfg.DataDir, a.cfg.MaildirRoot, a.cfg.BackupArtifactDir, a.cfg.BackupWorkspace, filepath.Dir(a.cfg.DBPath)} {
		if strings.TrimSpace(name) == "" || name == "." && a.cfg.DBPath == "" {
			continue
		}
		other, err := backupRestoreConfiguredPath(name)
		if err != nil || backupPathWithin(root, other) || backupPathWithin(other, root) {
			return "", errBackupRestoreConfiguration
		}
	}
	return root, nil
}

// A disaster may have removed a local catalog or old Maildir. Still prevent
// overlap with their configured future locations without requiring them to
// exist before a cloud recovery can start. Existing ancestor symlinks resolve.
func backupRestoreConfiguredPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	base := abs
	for {
		info, err := os.Stat(base)
		if err == nil {
			if !info.IsDir() {
				return "", errBackupRestoreConfiguration
			}
			resolved, err := filepath.EvalSymlinks(base)
			if err != nil {
				return "", err
			}
			relative, err := filepath.Rel(base, abs)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, relative), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(base) == base {
			return "", errBackupRestoreConfiguration
		}
		if _, err := os.Lstat(base); err == nil || !errors.Is(err, os.ErrNotExist) {
			// In particular, do not treat a dangling symlink/junction as an
			// absent ordinary component whose future location is known.
			return "", errBackupRestoreConfiguration
		}
		base = filepath.Dir(base)
	}
}

func (a *App) handleCreateBackupRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req backupRestoreRequest
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	user := currentUser(r)
	if retry, err := a.checkLoginRateLimit(r.Context(), user.Email, r.RemoteAddr); err != nil {
		backupError(w, err)
		return
	} else if retry > 0 {
		respondLoginRateLimited(w, retry)
		return
	}
	if len(req.Password) > maxPasswordBytes || len(req.TwoFactorCode) > 6 || a.verifyLinuxDoReauthentication(r.Context(), user.ID, req.Password, req.TwoFactorCode) != nil {
		retry, err := a.recordLoginFailure(r.Context(), user.Email, r.RemoteAddr)
		if err != nil {
			backupError(w, err)
		} else if retry > 0 {
			respondLoginRateLimited(w, retry)
		} else {
			respondError(w, http.StatusForbidden, "backup_restore_reauthentication_failed")
		}
		return
	}
	req.Password, req.TwoFactorCode = "", ""
	job, execution, err := a.admitBackupRestore(r.Context(), user.ID, req)
	req.Identity, req.BackupPassword = "", ""
	if err != nil {
		backupError(w, err)
		return
	}
	if execution != nil {
		// Admission acquired the shared lease and checked the capacity. There is
		// exactly one active request across instances; this channel has one slot.
		a.backupRestoreRequests <- *execution
	}
	respondJSON(w, http.StatusAccepted, backupJobResponse(job))
}

func (a *App) admitBackupRestore(ctx context.Context, actor string, req backupRestoreRequest) (backupJob, *backupRestoreExecution, error) {
	var empty backupJob
	if !req.Confirm || !backupRequestPattern.MatchString(req.RequestID) || !backupJobIDPattern.MatchString(req.SourceJobID) || req.DestinationID == "" || req.DestinationsRevision == "" || len(req.Identity) > 256 || len(req.BackupPassword) > 1024 {
		return empty, nil, errBackupInvalid
	}
	want, err := hex.DecodeString(req.ExpectedSHA256)
	if err != nil || len(want) != sha256.Size {
		return empty, nil, errBackupInvalid
	}
	mode := req.EncryptionMode
	if mode == "" {
		mode = "key"
	}
	publicRecipient := ""
	secret := ""
	secretBinding := ""
	if mode == "password" {
		if req.Identity != "" || !utf8.ValidString(req.BackupPassword) || req.BackupPassword == "" {
			return empty, nil, errBackupInvalid
		}
		if _, err := age.NewScryptIdentity(req.BackupPassword); err != nil {
			return empty, nil, errBackupInvalid
		}
		master, err := a.backupMasterKey()
		if err != nil {
			return empty, nil, err
		}
		mac := hmac.New(sha256.New, master)
		mac.Write([]byte("backup/restore/password-binding/v1\x00"))
		mac.Write([]byte(req.BackupPassword))
		clear(master)
		secretBinding = "password:" + hex.EncodeToString(mac.Sum(nil))
		secret = req.BackupPassword
	} else if mode == "key" {
		if req.BackupPassword != "" {
			return empty, nil, errBackupInvalid
		}
		identity, err := age.ParseX25519Identity(strings.TrimSpace(req.Identity))
		if err != nil {
			return empty, nil, errBackupInvalid
		}
		publicRecipient = identity.Recipient().String()
		secretBinding = publicRecipient
		secret = identity.String()
	} else {
		return empty, nil, errBackupInvalid
	}
	root, err := a.backupRestoreRoot()
	if err != nil {
		return empty, nil, err
	}
	digest := sha256.Sum256([]byte("restore\x00" + actor + "\x00" + req.RequestID))
	id := "bkj_" + hex.EncodeToString(digest[:16])
	fingerprintInput := req.SourceJobID + "\x00" + req.DestinationID + "\x00" + strings.ToLower(req.ExpectedSHA256) + "\x00" + secretBinding
	if req.DiscoveryEntryID != "" {
		fingerprintInput += "\x00discover\x00" + req.DiscoveryEntryID
	}
	fingerprint := sha256.Sum256([]byte(fingerprintInput))
	requestFingerprint := hex.EncodeToString(fingerprint[:])
	if existing, _, err := a.backupReadJob(ctx, id); err == nil {
		if existing.Kind != "restore" || existing.RequestFingerprint != requestFingerprint {
			return empty, nil, errBackupConflict
		}
		return existing, nil, nil // Never replay a private-key operation after restart.
	} else if !errors.Is(err, errBackupMissing) {
		return empty, nil, err
	}
	registry, registryRaw, err := a.backupRegistry(ctx)
	if err != nil {
		return empty, nil, err
	}
	if registry.Active != nil || len(registry.Pending) > 0 || len(a.backupRestoreRequests) > 0 {
		return empty, nil, errBackupRestoreBusy
	}
	source, sourceRaw, err := a.backupReadJob(ctx, req.SourceJobID)
	if err != nil {
		return empty, nil, err
	}
	if source.Kind == "restore" || source.Status != "succeeded" && source.Status != "partial" || source.Pruned[req.DestinationID].State != "" {
		return empty, nil, errBackupRestoreSource
	}
	var target *backupTarget
	for _, t := range source.Targets {
		if t.ID == req.DestinationID {
			copy := t
			target = &copy
		}
	}
	object := ""
	artifact := source.Artifact
	sourceDestinationID, sourceFolderID := "", ""
	var discoveryGuard *backupDiscoveryPageGuard
	if source.Kind == "discover" {
		entry, page, err := a.readBackupDiscoveryEntry(ctx, source, req.DiscoveryEntryID)
		if err != nil {
			return empty, nil, err
		}
		if entry.Bytes > backupMaxBytes {
			return empty, nil, errBackupLimit
		}
		if entry.Bytes < 1 || !strings.EqualFold(entry.SHA256, req.ExpectedSHA256) {
			return empty, nil, errBackupRestoreSource
		}
		artifactDigest := sha256.Sum256([]byte("discovered-artifact\x00" + source.ID + "\x00" + req.DiscoveryEntryID))
		artifact = catalog.Entry{Version: 1, ID: "bac_" + hex.EncodeToString(artifactDigest[:16]), SHA256: entry.SHA256, Bytes: entry.Bytes, PublishedAt: entry.CompletedAt}
		object, sourceDestinationID = entry.ObjectKey, entry.DestinationID
		sourceFolderID = source.Discovery.SourceFolderID
		discoveryGuard = &page
	} else {
		if req.DiscoveryEntryID != "" || !strings.EqualFold(source.Artifact.SHA256, req.ExpectedSHA256) || source.Artifact.Bytes < 1 || source.Artifact.Bytes > backupMaxBytes || mode == "key" && source.Recipient != "" && source.Recipient != publicRecipient || mode == "password" && source.Recipient != "" && !passwordDescriptor(source.Recipient) {
			return empty, nil, errBackupRestoreSource
		}
		for _, result := range source.Results {
			if result.DestinationID == req.DestinationID && result.State == "succeeded" && result.CompletedAt != nil {
				object = result.ObjectKey
			}
		}
	}
	if target == nil || object == "" {
		return empty, nil, errBackupRestoreSource
	}
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return empty, nil, err
	}
	if req.DestinationsRevision != backupRevision(vaultRaw) {
		return empty, nil, errBackupConflict
	}
	valid := false
	for _, record := range vault.Items {
		if record.Config.ID == target.ID && !record.Config.Disabled && backupTargetFingerprint(record) == target.Fingerprint {
			valid = true
		}
	}
	if !valid {
		return empty, nil, errBackupRestoreSource
	}
	// Bound private retained data as well as active execution; no automatic
	// deletion of a previous successful recovery or operator-owned contents.
	directory, err := os.Open(root)
	if err != nil {
		return empty, nil, errBackupExecution
	}
	entries, readErr := directory.ReadDir(9)
	directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(entries) >= 8 {
		return empty, nil, errBackupLimit
	}
	now := a.now().UTC()
	lease := backupLease{ID: id, Token: newID("bkl"), Until: now.Add(backupLeaseTTL)}
	jobRecipient := publicRecipient
	if mode == "password" {
		jobRecipient = "password"
	}
	job := backupJob{Version: 1, Kind: "restore", ID: id, Phase: "downloading", Status: "running", Artifact: artifact, Recipient: jobRecipient, Summary: source.Summary, CreatedAt: now, StartedAt: &now, Targets: []backupTarget{*target}, Results: []storage.Result{}, WorkerToken: lease.Token, RequestFingerprint: requestFingerprint,
		Restore: &backupRestoreState{SourceJobID: source.ID, DestinationID: target.ID, ObjectKey: object, Root: root, DiscoveryEntryID: req.DiscoveryEntryID, SourceDestinationID: sourceDestinationID, SourceFolderID: sourceFolderID}}
	registry.Active = &lease
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, nil, err
	}
	defer tx.Rollback()
	// A concurrent retention intent or destination edit must invalidate admission.
	for _, guard := range []struct {
		key, raw string
		value    any
	}{{backupRegistryKey, registryRaw, registry}, {backupJobPrefix + source.ID, sourceRaw, source}, {backupDestinationSetting, vaultRaw, vault}, {backupJobPrefix + id, "", job}} {
		if err := a.backupCAS(ctx, tx, guard.key, guard.raw, guard.value); err != nil {
			return empty, nil, err
		}
	}
	if discoveryGuard != nil {
		if err := a.backupCAS(ctx, tx, discoveryGuard.key, discoveryGuard.raw, discoveryGuard.value); err != nil {
			return empty, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return empty, nil, err
	}
	execution := &backupRestoreExecution{Lease: lease, EncryptionMode: mode}
	if mode == "password" {
		execution.Secret = []byte(secret)
	} else {
		execution.Identity = []byte(secret)
	}
	return job, execution, nil
}

func (a *App) runBackupRestore(parent context.Context, execution backupRestoreExecution) {
	defer clear(execution.Identity)
	defer clear(execution.Secret)
	lease := execution.Lease
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	reason := make(chan string, 1)
	done := make(chan struct{})
	go a.monitorBackupUpload(ctx, lease, cancel, reason, done)
	code := a.executeBackupRestore(ctx, execution)
	cancel()
	<-done
	select {
	case why := <-reason:
		code = why
	default:
	}
	if parent.Err() != nil {
		code = "server_shutdown"
	}
	finishCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	results := []storage.Result{}
	job, _, err := a.backupReadJob(finishCtx, lease.ID)
	if err == nil && code == "" && job.Restore != nil && job.Restore.Ready {
		now := a.now().UTC()
		results = append(results, storage.Result{DestinationID: job.Restore.DestinationID, State: "succeeded", CompletedAt: &now})
	}
	if err := a.finishBackupUpload(finishCtx, lease, results, code); err != nil && !errors.Is(err, errBackupConflict) {
		a.log.Warn("backup restore result persistence failed")
	}
}

func (a *App) executeBackupRestore(ctx context.Context, execution backupRestoreExecution) (code string) {
	job, _, err := a.backupReadJob(ctx, execution.Lease.ID)
	if err != nil || job.Kind != "restore" || job.Restore == nil || job.WorkerToken != execution.Lease.Token {
		return "restore_interrupted"
	}
	root, err := a.backupRestoreRoot()
	if err != nil || root != job.Restore.Root {
		return "restore_configuration_changed"
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return "restore_workspace_unavailable"
	}
	defer parent.Close()
	path, err := os.MkdirTemp(root, job.ID+"-")
	if err != nil {
		return "restore_workspace_unavailable"
	}
	name := filepath.Base(path)
	created, err := os.Stat(path)
	if err != nil {
		return "restore_workspace_unavailable"
	}
	defer func() {
		if code == "" {
			return // Verified private data is retained for maintenance installation.
		}
		current, err := parent.Lstat(name)
		if err != nil || !os.SameFile(created, current) || parent.RemoveAll(name) != nil {
			code = "restore_cleanup_failed"
		}
	}()
	if err := a.mutateBackupRetention(ctx, execution.Lease, job.ID, func(job *backupJob) error { job.Restore.Directory = name; return nil }); err != nil {
		return "restore_interrupted"
	}
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return "credentials_unavailable"
	}
	var record *backupDestinationRecord
	for _, item := range vault.Items {
		if item.Config.ID == job.Targets[0].ID && !item.Config.Disabled && backupTargetFingerprint(item) == job.Targets[0].Fingerprint {
			copy := item
			record = &copy
		}
	}
	if record == nil {
		return "destination_changed"
	}
	c := record.Credentials
	credentials := storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Username: c.Username, Password: c.Password}
	if record.Config.Type == "drive" {
		credentials.DriveAccessToken, err = a.backupDriveToken(ctx, record.Config.ID, job.Targets[0].Fingerprint)
		if err != nil {
			return "reauthorization_required"
		}
	}
	download := a.backupDownloadFile
	if download == nil {
		download = storage.Download
	}
	output := filepath.Join(path, "ciphertext.age")
	opts := storage.DefaultOptions()
	opts.MaxBytes = job.Artifact.Bytes
	config := record.Config
	if job.Restore.SourceDestinationID != "" {
		config.ID = job.Restore.SourceDestinationID
	}
	if job.Restore.SourceFolderID != "" {
		if config.Type != "drive" || config.Drive == nil {
			return "restore_configuration_changed"
		}
		copy := *config.Drive
		copy.FolderID = job.Restore.SourceFolderID
		config.Drive = &copy
	}
	receipt, err := download(ctx, config, job.Restore.ObjectKey, job.Artifact.SHA256, output, opts, credentials)
	if err != nil || receipt.Bytes != job.Artifact.Bytes || !strings.EqualFold(receipt.SHA256, job.Artifact.SHA256) {
		return "restore_download_failed"
	}
	if err := a.mutateBackupRetention(ctx, execution.Lease, job.ID, func(job *backupJob) error { job.Phase = "decrypting"; return nil }); err != nil {
		return "restore_interrupted"
	}
	file, err := os.Open(output)
	if err != nil {
		return "restore_download_failed"
	}
	var manifest backup.Manifest
	var restoreErr error
	if execution.EncryptionMode == "password" {
		manifest, restoreErr = backup.RestorePassword(ctx, file, filepath.Join(path, "recovered"), string(execution.Secret), job.Artifact.SHA256, backup.Limits{MaxBytes: backupMaxBytes, MaxEntries: 100000})
	} else {
		manifest, restoreErr = backup.Restore(ctx, file, filepath.Join(path, "recovered"), string(execution.Identity), job.Artifact.SHA256, backup.Limits{MaxBytes: backupMaxBytes, MaxEntries: 100000})
	}
	closeErr := file.Close()
	if restoreErr != nil || closeErr != nil {
		return "restore_verification_failed"
	}
	reportSHA256 := ""
	if manifest.Scope == "online-data" {
		reportSHA256, err = backup.StagedOnlineReportSHA256(ctx, filepath.Join(path, "recovered"))
		if err != nil {
			return "restore_verification_failed"
		}
		validation := filepath.Join(path, "validation")
		if err := os.Mkdir(validation, 0o700); err != nil {
			return "restore_workspace_unavailable"
		}
		// Reuse installer validation for every referenced payload and digest,
		// schema, path, DKIM and runtime-state compatibility. This disposable copy
		// uses inert validation roots; recovered remains untouched for install.
		prepared, err := install.Prepare(ctx, filepath.Join(path, "recovered"), validation, "/validation-data", "/validation-mail", backup.Limits{MaxBytes: backupMaxBytes, MaxEntries: 100000}, reportSHA256)
		if err != nil {
			return "restore_verification_failed"
		}
		if err := prepared.Close(); err != nil {
			return "restore_cleanup_failed"
		}
		if err := os.Remove(validation); err != nil {
			return "restore_cleanup_failed"
		}
	} else if manifest.Scope == "online-native-data" {
		reportSHA256, err = backup.StagedOnlineReportSHA256(ctx, filepath.Join(path, "recovered"), manifest.Driver)
		if err != nil {
			return "restore_verification_failed"
		}
		if _, err := install.VerifyNativeStaged(ctx, filepath.Join(path, "recovered"), manifest.Driver, reportSHA256, backup.Limits{MaxBytes: backupMaxBytes, MaxEntries: 100000}); err != nil {
			return "restore_verification_failed"
		}
	}
	if err := os.Remove(output); err != nil {
		return "restore_cleanup_failed"
	}
	// One last fencing/cancellation check before publishing verified readiness.
	if err := a.mutateBackupRetention(ctx, execution.Lease, job.ID, func(job *backupJob) error {
		job.Restore.Manifest, job.Restore.Ready, job.Restore.ReportSHA256 = &manifest, true, reportSHA256
		return nil
	}); err != nil {
		return "restore_interrupted"
	}
	return ""
}
