package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"lanqin-email-api/internal/backup/catalog"
	"lanqin-email-api/internal/backup/storage"
)

const backupRegistryKey = "backup.queue.v1"
const backupJobPrefix = "backup.job.v1."
const backupSQLTime = "2006-01-02T15:04:05.000000000Z"
const backupMaxBytes int64 = 1 << 30
const backupLeaseTTL = 30 * time.Second

var backupJobIDPattern = regexp.MustCompile(`^bkj_[a-f0-9]{32}$`)
var backupRequestPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)
var errBackupQueue = errors.New("backup_queue_full")
var errBackupExecution = errors.New("backup_execution_unavailable")

type backupTarget struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Fingerprint string              `json:"fingerprint,omitempty"`
	Config      storage.Destination `json:"config"`
}

type backupJob struct {
	Version            int              `json:"version"`
	ID                 string           `json:"id"`
	Artifact           catalog.Entry    `json:"artifact"`
	Status             string           `json:"status"`
	CreatedAt          time.Time        `json:"createdAt"`
	StartedAt          *time.Time       `json:"startedAt,omitempty"`
	CompletedAt        *time.Time       `json:"completedAt,omitempty"`
	Targets            []backupTarget   `json:"targets"`
	Results            []storage.Result `json:"results"`
	CancelRequested    bool             `json:"cancelRequested"`
	ErrorCode          string           `json:"errorCode,omitempty"`
	RequestFingerprint string           `json:"requestFingerprint,omitempty"`
	WorkerToken        string           `json:"workerToken,omitempty"`
}

type backupLease struct {
	ID, Token string
	Until     time.Time
}
type backupSuccess struct {
	At                 time.Time
	Fingerprint, JobID string
}
type backupRegistry struct {
	Version int                      `json:"version"`
	Pending []string                 `json:"pending"`
	Active  *backupLease             `json:"active,omitempty"`
	Success map[string]backupSuccess `json:"success"`
}

// Explicit DTOs ensure private fingerprints and fencing tokens never escape.
type backupJobView struct {
	ID               string              `json:"id"`
	Artifact         catalog.Entry       `json:"artifact"`
	Status           string              `json:"status"`
	CreatedAt        time.Time           `json:"createdAt"`
	StartedAt        *time.Time          `json:"startedAt,omitempty"`
	CompletedAt      *time.Time          `json:"completedAt,omitempty"`
	DestinationNames []string            `json:"destinationNames"`
	Locations        []backupJobLocation `json:"locations"`
	Results          []storage.Result    `json:"results"`
	CancelRequested  bool                `json:"cancelRequested"`
	ErrorCode        string              `json:"errorCode,omitempty"`
}

type backupJobLocation struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Region   string `json:"region,omitempty"`
}

func backupJobResponse(job backupJob) backupJobView {
	names := []string{}
	locations := []backupJobLocation{}
	for _, t := range job.Targets {
		names = append(names, t.Name)
		location := backupJobLocation{ID: t.ID, Name: t.Name, Type: t.Config.Type}
		if t.Config.S3 != nil {
			location.Endpoint = t.Config.S3.Endpoint
			location.Bucket = t.Config.S3.Bucket
			location.Prefix = t.Config.S3.Prefix
			location.Region = t.Config.S3.Region
		}
		if t.Config.WebDAV != nil {
			location.Endpoint = t.Config.WebDAV.URL
		}
		locations = append(locations, location)
	}
	return backupJobView{ID: job.ID, Artifact: job.Artifact, Status: job.Status, CreatedAt: job.CreatedAt, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt, DestinationNames: names, Locations: locations, Results: job.Results, CancelRequested: job.CancelRequested, ErrorCode: job.ErrorCode}
}

func (a *App) backupMasterKey() ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(a.cfg.BackupSecretKey))
	if err != nil || len(key) != 32 {
		return nil, errBackupKey
	}
	return key, nil
}

func (a *App) backupSeal(key string, value any) (string, error) {
	crypt, err := a.backupCipher()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, crypt.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	raw := "v1:" + base64.StdEncoding.EncodeToString(crypt.Seal(nonce, nonce, plain, []byte(key)))
	if len(raw) > maxBackupDestinationCiphertext {
		return "", errBackupLimit
	}
	return raw, nil
}

func (a *App) backupOpen(key, raw string, dst any) error {
	crypt, err := a.backupCipher()
	if err != nil || len(raw) > maxBackupDestinationCiphertext || !strings.HasPrefix(raw, "v1:") {
		return errBackupKey
	}
	data, err := base64.StdEncoding.DecodeString(raw[3:])
	if err != nil || len(data) < crypt.NonceSize()+crypt.Overhead() {
		return errBackupKey
	}
	plain, err := crypt.Open(nil, data[:crypt.NonceSize()], data[crypt.NonceSize():], []byte(key))
	if err != nil || json.Unmarshal(plain, dst) != nil {
		return errBackupKey
	}
	return nil
}

func (a *App) backupRead(ctx context.Context, key string, dst any) (string, error) {
	var raw string
	err := a.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, key).Scan(&raw)
	if err != nil {
		return "", err
	}
	return raw, a.backupOpen(key, raw, dst)
}

func (a *App) backupRegistry(ctx context.Context) (backupRegistry, string, error) {
	value := backupRegistry{Version: 1, Pending: []string{}, Success: map[string]backupSuccess{}}
	raw, err := a.backupRead(ctx, backupRegistryKey, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return value, "", nil
	}
	if err != nil {
		return value, "", err
	}
	if value.Version != 1 || len(value.Pending) > 16 || len(value.Success) > 64 {
		return value, "", errBackupInvalid
	}
	for _, id := range value.Pending {
		if !backupJobIDPattern.MatchString(id) {
			return value, "", errBackupInvalid
		}
	}
	if value.Active != nil && (!backupJobIDPattern.MatchString(value.Active.ID) || value.Active.Token == "" || value.Active.Until.IsZero()) {
		return value, "", errBackupInvalid
	}
	if value.Success == nil {
		value.Success = map[string]backupSuccess{}
	}
	return value, raw, nil
}

func (a *App) backupReadJob(ctx context.Context, id string) (backupJob, string, error) {
	var job backupJob
	if !backupJobIDPattern.MatchString(id) {
		return job, "", errBackupMissing
	}
	raw, err := a.backupRead(ctx, backupJobPrefix+id, &job)
	if errors.Is(err, sql.ErrNoRows) {
		return job, "", errBackupMissing
	}
	if err != nil {
		return job, "", err
	}
	if job.Version != 1 || job.ID != id || len(job.Targets) > 16 || !catalog.ValidID(job.Artifact.ID) {
		return job, "", errBackupInvalid
	}
	return job, raw, nil
}

// All reads finish before a Tx is reserved; no network or filesystem hashing is
// performed in a transaction or while SQLite's single connection is held.
func (a *App) backupCAS(ctx context.Context, db dbExecutor, key, previous string, value any) error {
	raw, err := a.backupSeal(key, value)
	if err != nil {
		return err
	}
	column := keyColumnSQL(a.cfg.DBDriver)
	var result sql.Result
	if previous == "" {
		query := insertIgnoreSQL(a.cfg.DBDriver, `INSERT INTO system_settings(`+column+`,value,updated_at) VALUES(?,?,?)`, `(`+column+`)`)
		result, err = db.ExecContext(ctx, query, key, raw, a.now().UTC().Format(backupSQLTime))
	} else {
		result, err = db.ExecContext(ctx, `UPDATE system_settings SET value=?,updated_at=? WHERE `+column+`=? AND value=?`, raw, a.now().UTC().Format(backupSQLTime), key, previous)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errBackupConflict
	}
	return nil
}

func backupTargetFingerprint(record backupDestinationRecord) string {
	record.Config.Name = ""
	record.Config.Disabled = false
	hash := sha256.Sum256([]byte(jsonEncode(record)))
	return hex.EncodeToString(hash[:])
}

func (a *App) enqueueBackupUpload(ctx context.Context, actorID, requestID, artifactID string, ids []string, expectedRevision ...string) (backupJob, error) {
	var empty backupJob
	if a.cfg.BackupArtifactDir == "" {
		return empty, errBackupExecution
	}
	if !backupRequestPattern.MatchString(requestID) || !catalog.ValidID(artifactID) || len(ids) < 1 || len(ids) > 16 {
		return empty, errBackupInvalid
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || i > 0 && ids[i-1] == id {
			return empty, errBackupInvalid
		}
	}
	identity := sha256.Sum256([]byte(actorID + "\x00" + requestID))
	id := "bkj_" + hex.EncodeToString(identity[:16])
	fingerprint := sha256.Sum256([]byte(artifactID + "\x00" + strings.Join(ids, "\x00")))
	requestFingerprint := hex.EncodeToString(fingerprint[:])
	if existing, _, err := a.backupReadJob(ctx, id); err == nil {
		if existing.RequestFingerprint != requestFingerprint {
			return empty, errBackupConflict
		}
		return existing, nil
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
	key, err := a.backupMasterKey()
	if err != nil {
		return empty, err
	}
	file, entry, err := catalog.Open(a.cfg.BackupArtifactDir, artifactID, key, backupMaxBytes)
	if err != nil {
		return empty, errBackupExecution
	}
	file.Close()
	vault, vaultRaw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return empty, err
	}
	if len(expectedRevision) > 0 && expectedRevision[0] != backupRevision(vaultRaw) {
		return empty, errBackupConflict
	}
	byID := map[string]backupDestinationRecord{}
	for _, record := range vault.Items {
		byID[record.Config.ID] = record
	}
	job := backupJob{Version: 1, ID: id, Artifact: entry, Status: "queued", CreatedAt: a.now().UTC(), Targets: []backupTarget{}, Results: []storage.Result{}, RequestFingerprint: requestFingerprint}
	for _, id := range ids {
		record, ok := byID[id]
		if !ok || record.Config.Disabled {
			return empty, errBackupInvalid
		}
		job.Targets = append(job.Targets, backupTarget{ID: id, Name: record.Config.Name, Fingerprint: backupTargetFingerprint(record), Config: record.Config})
	}
	registry.Pending = append(registry.Pending, job.ID)
	// Reserve room for terminal per-target reports so a large metadata snapshot
	// cannot make cancellation or completion impossible under the row limit.
	sealed, err := a.backupSeal(backupJobPrefix+job.ID, job)
	if err != nil {
		return empty, err
	}
	if len(sealed) > 32<<10 {
		return empty, errBackupLimit
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err := a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry); err != nil {
		return empty, err
	}
	if err := a.backupCAS(ctx, tx, backupJobPrefix+job.ID, "", job); err != nil {
		return empty, err
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

func (a *App) claimBackupUpload(ctx context.Context) (*backupLease, error) {
	registry, raw, err := a.backupRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if registry.Active != nil {
		if registry.Active.Until.After(a.now()) {
			return nil, nil
		}
		job, jobRaw, err := a.backupReadJob(ctx, registry.Active.ID)
		if err != nil {
			return nil, err
		}
		now := a.now().UTC()
		job.Status, job.ErrorCode, job.CompletedAt, job.WorkerToken = "interrupted", "lease_expired", &now, ""
		registry.Active = nil
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if err := a.backupCAS(ctx, tx, backupRegistryKey, raw, registry); err != nil {
			return nil, err
		}
		if err := a.backupCAS(ctx, tx, backupJobPrefix+job.ID, jobRaw, job); err != nil {
			return nil, err
		}
		return nil, tx.Commit() // Never automatically replay an uncertain upload.
	}
	if len(registry.Pending) == 0 {
		return nil, nil
	}
	job, jobRaw, err := a.backupReadJob(ctx, registry.Pending[0])
	if err != nil {
		return nil, err
	}
	if job.Status != "queued" {
		return nil, errBackupInvalid
	}
	now := a.now().UTC()
	lease := &backupLease{ID: job.ID, Token: newID("bkl"), Until: now.Add(backupLeaseTTL)}
	job.Status, job.StartedAt, job.WorkerToken = "running", &now, lease.Token
	registry.Pending, registry.Active = registry.Pending[1:], lease
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := a.backupCAS(ctx, tx, backupRegistryKey, raw, registry); err != nil {
		return nil, err
	}
	if err := a.backupCAS(ctx, tx, backupJobPrefix+job.ID, jobRaw, job); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (a *App) cancelBackupUpload(ctx context.Context, id string) error {
	for range 5 {
		job, raw, err := a.backupReadJob(ctx, id)
		if err != nil {
			return err
		}
		if job.Status != "queued" && job.Status != "running" {
			// Completion may race the cancel click. Cancellation is idempotent;
			// never rewrite a terminal result or misreport a config conflict.
			return nil
		}
		if job.CancelRequested {
			return nil
		}
		job.CancelRequested = true
		registry, registryRaw, err := a.backupRegistry(ctx)
		if err != nil {
			return err
		}
		if job.Status == "queued" {
			now := a.now().UTC()
			job.Status, job.CompletedAt = "cancelled", &now
			pending := []string{}
			for _, v := range registry.Pending {
				if v != id {
					pending = append(pending, v)
				}
			}
			registry.Pending = pending
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = a.backupCAS(ctx, tx, backupRegistryKey, registryRaw, registry)
		if err == nil {
			err = a.backupCAS(ctx, tx, backupJobPrefix+id, raw, job)
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
