package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"lanqin-email-api/internal/backup/storage"
)

const backupDiscoveryPrefix = "backup.discovery.v1."
const backupDiscoveryPageSize = 32
const backupDiscoveryMaxEntries = 128

var backupDiscoveryEntryPattern = regexp.MustCompile(`^bdi_[a-f0-9]{64}$`)
var backupDiscoveryDestinationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type backupDiscoveryRequest struct {
	DestinationID        string `json:"destinationId"`
	DestinationsRevision string `json:"destinationsRevision"`
	RequestID            string `json:"requestId"`
	SourceFolderID       string `json:"sourceFolderId,omitempty"`
}

type backupDiscoveryState struct {
	Count          int    `json:"count"`
	Ready          bool   `json:"ready"`
	SourceFolderID string `json:"sourceFolderId,omitempty"`
	Pages          int    `json:"pages"`
}

type backupDiscoveryView struct {
	Count          int    `json:"count"`
	Ready          bool   `json:"ready"`
	SourceFolderID string `json:"sourceFolderId,omitempty"`
}

type backupDiscoveryItem struct {
	ID                    string    `json:"id"`
	OriginalDestinationID string    `json:"originalDestinationId"`
	ObjectKey             string    `json:"objectKey"`
	SHA256                string    `json:"sha256"`
	Bytes                 int64     `json:"bytes"`
	CompletedAt           time.Time `json:"completedAt"`
}

type backupDiscoveryPage struct {
	Version int                          `json:"version"`
	JobID   string                       `json:"jobId"`
	Page    int                          `json:"page"`
	Entries []storage.RecoveryIndexEntry `json:"entries"`
}

type backupDiscoveryPageGuard struct {
	key, raw string
	value    backupDiscoveryPage
}

func validBackupDiscoveryJob(job backupJob) bool {
	state := job.Discovery
	if len(job.Targets) != 1 || state == nil || job.Artifact.ID != "" || state.Count < 0 || state.Count > backupDiscoveryMaxEntries || state.Pages < 0 || state.Pages > backupDiscoveryMaxEntries/backupDiscoveryPageSize || state.Pages != (state.Count+backupDiscoveryPageSize-1)/backupDiscoveryPageSize || job.Phase != "queued" && job.Phase != "discovering" && job.Phase != "completed" {
		return false
	}
	if job.Targets[0].Config.Type == "drive" {
		return storage.ValidDriveID(state.SourceFolderID) && job.Targets[0].Config.Drive != nil && state.SourceFolderID == job.Targets[0].Config.Drive.FolderID
	}
	return state.SourceFolderID == ""
}

func (a *App) handleCreateBackupDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req backupDiscoveryRequest
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	job, err := a.enqueueBackupDiscovery(r.Context(), currentUser(r).ID, req)
	if err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, backupJobResponse(job))
}

func (a *App) enqueueBackupDiscovery(ctx context.Context, actor string, req backupDiscoveryRequest) (backupJob, error) {
	var empty backupJob
	if actor == "" || len(actor) > 128 || !backupRequestPattern.MatchString(req.RequestID) || !backupDiscoveryDestinationPattern.MatchString(req.DestinationID) || req.DestinationsRevision == "" || req.SourceFolderID != "" && !storage.ValidDriveID(req.SourceFolderID) {
		return empty, errBackupInvalid
	}
	key, err := a.backupMasterKey()
	if err != nil {
		return empty, err
	}
	clear(key)
	if _, err := a.backupNetworkPolicy(); err != nil {
		return empty, err
	}
	digest := sha256.Sum256([]byte("discover\x00" + actor + "\x00" + req.RequestID))
	id := "bkj_" + hex.EncodeToString(digest[:16])
	fingerprint := sha256.Sum256([]byte("discover\x00" + req.DestinationID + "\x00" + req.SourceFolderID))
	requestFingerprint := hex.EncodeToString(fingerprint[:])
	if previous, _, err := a.backupReadJob(ctx, id); err == nil {
		if previous.Kind != "discover" || previous.RequestFingerprint != requestFingerprint {
			return empty, errBackupConflict
		}
		return previous, nil
	} else if !errors.Is(err, errBackupMissing) {
		return empty, err
	}
	registry, registryRaw, err := a.backupRegistry(ctx)
	if err != nil {
		return empty, err
	}
	if len(registry.Pending) >= 16 {
		return empty, errBackupQueue
	}
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return empty, err
	}
	if backupRevision(vaultRaw) != req.DestinationsRevision {
		return empty, errBackupConflict
	}
	var selected *backupDestinationRecord
	for _, record := range vault.Items {
		if record.Config.ID == req.DestinationID && !record.Config.Disabled {
			copy := record
			selected = &copy
			break
		}
	}
	if selected == nil {
		return empty, errBackupMissing
	}
	if selected.Config.Type != "drive" && req.SourceFolderID != "" {
		return empty, errBackupInvalid
	}
	config := selected.Config
	folder := ""
	if config.Type == "drive" {
		if !backupDriveBound(*selected) {
			return empty, errBackupAuth
		}
		copy := *config.Drive
		if req.SourceFolderID != "" {
			copy.FolderID = req.SourceFolderID
		}
		config.Drive = &copy
		folder = copy.FolderID
	}
	job := backupJob{Kind: "discover", Phase: "queued", Version: 1, ID: id, Status: "queued", CreatedAt: a.now().UTC(), Targets: []backupTarget{{ID: config.ID, Name: config.Name, Fingerprint: backupTargetFingerprint(*selected), Config: config}}, Results: []storage.Result{}, RequestFingerprint: requestFingerprint, Discovery: &backupDiscoveryState{SourceFolderID: folder}}
	sealed, err := a.backupSeal(backupJobPrefix+job.ID, job)
	if err != nil || len(sealed) > 32<<10 {
		return empty, errBackupLimit
	}
	registry.Pending = append(registry.Pending, job.ID)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	for _, guard := range []struct {
		key, raw string
		value    any
	}{{backupRegistryKey, registryRaw, registry}, {backupDestinationSetting, vaultRaw, vault}, {backupJobPrefix + id, "", job}} {
		if err := a.backupCAS(ctx, tx, guard.key, guard.raw, guard.value); err != nil {
			return empty, err
		}
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	if a.backupWake != nil {
		select {
		case a.backupWake <- struct{}{}:
		default:
		}
	}
	return job, nil
}

func discoveryEntryID(entry storage.RecoveryIndexEntry) string {
	entry.CompletedAt = entry.CompletedAt.UTC()
	data, _ := json.Marshal(entry)
	digest := sha256.Sum256(data)
	return "bdi_" + hex.EncodeToString(digest[:])
}

func discoveryEntryView(entry storage.RecoveryIndexEntry) backupDiscoveryItem {
	return backupDiscoveryItem{ID: discoveryEntryID(entry), OriginalDestinationID: entry.DestinationID, ObjectKey: entry.ObjectKey, SHA256: entry.SHA256, Bytes: entry.Bytes, CompletedAt: entry.CompletedAt}
}

func (a *App) executeBackupDiscovery(ctx context.Context, lease backupLease, job backupJob) ([]storage.Result, string) {
	if job.Kind != "discover" || !validBackupDiscoveryJob(job) {
		return nil, "discovery_interrupted"
	}
	key, err := a.backupMasterKey()
	if err != nil {
		return nil, "key_unavailable"
	}
	defer clear(key)
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return nil, "credentials_unavailable"
	}
	var selected *backupDestinationRecord
	for _, record := range vault.Items {
		if record.Config.ID == job.Targets[0].ID && !record.Config.Disabled && backupTargetFingerprint(record) == job.Targets[0].Fingerprint {
			copy := record
			selected = &copy
			break
		}
	}
	if selected == nil {
		return nil, "destination_changed"
	}
	config := selected.Config
	c := selected.Credentials
	credentials := storage.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Username: c.Username, Password: c.Password}
	if config.Type == "drive" {
		credentials.DriveAccessToken, err = a.backupDriveToken(ctx, selected.Config.ID, job.Targets[0].Fingerprint)
		if err != nil {
			return nil, "reauthorization_required"
		}
		copy := *config.Drive
		copy.FolderID = job.Discovery.SourceFolderID
		config.Drive = &copy
	}
	if err := a.guardBackupRecoveryIndex(ctx, lease, job.Targets[0]); err != nil {
		return nil, "discovery_interrupted"
	}
	discover := a.backupDiscoverIndexes
	if discover == nil {
		discover = storage.DiscoverRecoveryIndexes
	}
	entries, err := discover(ctx, config, key, credentials)
	if err != nil || len(entries) > backupDiscoveryMaxEntries {
		return nil, "discovery_failed"
	}
	seen := map[string]bool{}
	for i, entry := range entries {
		// The transport returns authenticated entries. Validate their complete
		// shape/location once more before persisting them under task encryption.
		original := config
		original.ID = entry.DestinationID
		document, err := storage.SealRecoveryIndex(original, entry.ObjectKey, entry.SHA256, entry.Bytes, entry.CompletedAt, key)
		if err != nil {
			return nil, "discovery_failed"
		}
		verified, err := storage.OpenRecoveryIndex(config, document, key)
		if err != nil || verified.Location != entry.Location || verified.Type != entry.Type || verified.Version != entry.Version || verified.SHA256 != entry.SHA256 {
			return nil, "discovery_failed"
		}
		identity := entry.DestinationID + "\x00" + entry.ObjectKey
		if seen[identity] {
			return nil, "discovery_failed"
		}
		seen[identity] = true
		entries[i] = verified
	}
	if err := a.publishBackupDiscovery(ctx, lease, entries); err != nil {
		return nil, "discovery_interrupted"
	}
	now := a.now().UTC()
	return []storage.Result{{DestinationID: job.Targets[0].ID, State: "succeeded", CompletedAt: &now}}, ""
}

func (a *App) publishBackupDiscovery(ctx context.Context, lease backupLease, entries []storage.RecoveryIndexEntry) error {
	if len(entries) > backupDiscoveryMaxEntries {
		return errBackupLimit
	}
	for range 5 {
		if err := ctx.Err(); err != nil {
			return err
		}
		registry, registryRaw, err := a.backupRegistry(ctx)
		if err != nil {
			return err
		}
		if registry.Active == nil || registry.Active.ID != lease.ID || registry.Active.Token != lease.Token || !registry.Active.Until.After(a.now()) {
			return errBackupConflict
		}
		job, jobRaw, err := a.backupReadJob(ctx, lease.ID)
		if err != nil || job.Kind != "discover" || job.Status != "running" || job.WorkerToken != lease.Token || job.CancelRequested || job.Discovery.Ready {
			return errBackupConflict
		}
		vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
		if err != nil {
			return err
		}
		bound := false
		for _, record := range vault.Items {
			if record.Config.ID == job.Targets[0].ID && !record.Config.Disabled && backupTargetFingerprint(record) == job.Targets[0].Fingerprint {
				bound = true
			}
		}
		if !bound {
			return errBackupConflict
		}
		pages := []backupDiscoveryPage{}
		for start := 0; start < len(entries); start += backupDiscoveryPageSize {
			page := backupDiscoveryPage{Version: 1, JobID: job.ID, Page: len(pages), Entries: append([]storage.RecoveryIndexEntry{}, entries[start:min(start+backupDiscoveryPageSize, len(entries))]...)}
			if _, err := a.backupSeal(backupDiscoveryPageKey(job.ID, page.Page), page); err != nil {
				return err
			}
			pages = append(pages, page)
		}
		job.Discovery.Count, job.Discovery.Pages, job.Discovery.Ready = len(entries), len(pages), true
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry)
		if err == nil {
			err = a.backupCAS(ctx, tx, backupDestinationSetting, vaultRaw, vault)
		}
		for _, page := range pages {
			if err == nil {
				err = a.backupCAS(ctx, tx, backupDiscoveryPageKey(job.ID, page.Page), "", page)
			}
		}
		if err == nil {
			err = a.backupCAS(ctx, tx, backupJobPrefix+job.ID, jobRaw, job)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if !errors.Is(err, errBackupConflict) {
			return err
		}
	}
	return errBackupConflict
}

func backupDiscoveryPageKey(jobID string, page int) string {
	return backupDiscoveryPrefix + jobID + "." + strconv.Itoa(page)
}

func (a *App) readBackupDiscoveryPage(ctx context.Context, job backupJob, page int) (backupDiscoveryPageGuard, error) {
	var empty backupDiscoveryPageGuard
	if job.Kind != "discover" || job.Status != "succeeded" || job.CancelRequested || job.Discovery == nil || !job.Discovery.Ready || page < 0 || page >= job.Discovery.Pages {
		return empty, errBackupRestoreSource
	}
	name := backupDiscoveryPageKey(job.ID, page)
	var result backupDiscoveryPage
	raw, err := a.backupRead(ctx, name, &result)
	if err != nil {
		return empty, errBackupRestoreSource
	}
	wantCount := min(backupDiscoveryPageSize, job.Discovery.Count-page*backupDiscoveryPageSize)
	if result.Version != 1 || result.JobID != job.ID || result.Page != page || len(result.Entries) != wantCount {
		return empty, errBackupRestoreSource
	}
	return backupDiscoveryPageGuard{key: name, raw: raw, value: result}, nil
}

func (a *App) readBackupDiscoveryEntry(ctx context.Context, job backupJob, id string) (storage.RecoveryIndexEntry, backupDiscoveryPageGuard, error) {
	var empty storage.RecoveryIndexEntry
	if !backupDiscoveryEntryPattern.MatchString(id) || job.Kind != "discover" || job.Discovery == nil || !job.Discovery.Ready || job.Status != "succeeded" || job.CancelRequested {
		return empty, backupDiscoveryPageGuard{}, errBackupRestoreSource
	}
	var matched storage.RecoveryIndexEntry
	var guard backupDiscoveryPageGuard
	found := false
	seen := map[string]bool{}
	for page := 0; page < job.Discovery.Pages; page++ {
		current, err := a.readBackupDiscoveryPage(ctx, job, page)
		if err != nil {
			return empty, backupDiscoveryPageGuard{}, err
		}
		for _, entry := range current.value.Entries {
			entryID := discoveryEntryID(entry)
			if seen[entryID] {
				return empty, backupDiscoveryPageGuard{}, errBackupRestoreSource
			}
			seen[entryID] = true
			if entryID == id {
				matched, guard, found = entry, current, true
			}
		}
	}
	if !found {
		return empty, backupDiscoveryPageGuard{}, errBackupRestoreSource
	}
	return matched, guard, nil
}

func (a *App) handleBackupDiscoveryResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	job, _, err := a.backupReadJob(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		backupError(w, err)
		return
	}
	if job.Kind != "discover" || job.Status != "succeeded" || job.Discovery == nil || !job.Discovery.Ready || job.CancelRequested {
		backupError(w, errBackupConflict)
		return
	}
	page := 0
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		if len(cursor) > 256 {
			backupError(w, errBackupInvalid)
			return
		}
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(cursor)
		parts := strings.Split(string(decoded), ":")
		if decodeErr != nil || len(parts) != 2 || parts[0] != job.ID {
			backupError(w, errBackupInvalid)
			return
		}
		page, err = strconv.Atoi(parts[1])
		if err != nil || page < 1 || page >= job.Discovery.Pages {
			backupError(w, errBackupInvalid)
			return
		}
	}
	items := []backupDiscoveryItem{}
	next := ""
	if job.Discovery.Count > 0 {
		result, err := a.readBackupDiscoveryPage(r.Context(), job, page)
		if err != nil {
			backupError(w, err)
			return
		}
		for _, entry := range result.value.Entries {
			items = append(items, discoveryEntryView(entry))
		}
		if page+1 < job.Discovery.Pages {
			next = base64.RawURLEncoding.EncodeToString([]byte(job.ID + ":" + strconv.Itoa(page+1)))
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}
