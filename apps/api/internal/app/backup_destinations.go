package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"lanqin-email-api/internal/backup/storage"
)

const backupDestinationSetting = "backup.destinations.v1"
const maxBackupDestinationCiphertext = 60 << 10 // Fits even a MySQL TEXT column.

var (
	errBackupKey      = errors.New("backup_key_unavailable")
	errBackupInvalid  = errors.New("backup_destination_invalid")
	errBackupAuth     = errors.New("backup_credentials_required")
	errBackupConflict = errors.New("backup_revision_conflict")
	errBackupLimit    = errors.New("backup_destination_limit")
	errBackupMissing  = errors.New("backup_destination_not_found")
)

type backupCredentials struct {
	AccessKeyID       string    `json:"accessKeyId,omitempty"`
	SecretAccessKey   string    `json:"secretAccessKey,omitempty"`
	SessionToken      string    `json:"sessionToken,omitempty"`
	Username          string    `json:"username,omitempty"`
	Password          string    `json:"password,omitempty"`
	DriveRefreshToken string    `json:"driveRefreshToken,omitempty"`
	DriveAccessToken  string    `json:"driveAccessToken,omitempty"`
	DriveTokenExpiry  time.Time `json:"driveTokenExpiry,omitempty"`
	DriveClientID     string    `json:"driveClientId,omitempty"`
	DriveAuthID       string    `json:"driveAuthId,omitempty"`
}

// The entire record is encrypted, including endpoint, ID and credentials. An
// edited database endpoint cannot redirect stored credentials to another host.
type backupDestinationRecord struct {
	Config      storage.Destination `json:"config"`
	Credentials backupCredentials   `json:"credentials"`
}

type backupDestinationState struct {
	Version int                       `json:"version"`
	Items   []backupDestinationRecord `json:"items"`
}

type backupDestinationView struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Type            string               `json:"type"`
	Disabled        bool                 `json:"disabled"`
	S3              *backupS3Settings    `json:"s3,omitempty"`
	WebDAV          *backupDAVSettings   `json:"webdav,omitempty"`
	Drive           *backupDriveSettings `json:"drive,omitempty"`
	CredentialsSet  bool                 `json:"credentialsSet"`
	SessionTokenSet bool                 `json:"sessionTokenSet"`
	LastSuccessAt   *time.Time           `json:"lastSuccessAt,omitempty"`
}

type backupDriveSettings struct {
	FolderID string `json:"folderId"`
}

type backupS3Settings struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	PathStyle bool   `json:"pathStyle"`
}

type backupDAVSettings struct {
	URL string `json:"url"`
}

type backupDestinationList struct {
	EncryptionConfigured bool                    `json:"encryptionConfigured"`
	Revision             string                  `json:"revision"`
	Items                []backupDestinationView `json:"items"`
}

func (a *App) backupCipher() (cipher.AEAD, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(a.cfg.BackupSecretKey))
	if err != nil || len(key) != 32 {
		return nil, errBackupKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errBackupKey
	}
	return cipher.NewGCM(block)
}

func backupRevision(raw string) string {
	if raw == "" {
		return "0"
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (a *App) loadBackupDestinationState(ctx context.Context) (backupDestinationState, string, error) {
	state := backupDestinationState{Version: 1, Items: []backupDestinationRecord{}}
	var raw string
	err := a.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, backupDestinationSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, "", nil
	}
	if err != nil {
		return state, "", err
	}
	aead, err := a.backupCipher()
	if err != nil || len(raw) > maxBackupDestinationCiphertext || !strings.HasPrefix(raw, "v1:") {
		return state, "", errBackupKey
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "v1:"))
	if err != nil || len(data) < aead.NonceSize()+aead.Overhead() {
		return state, "", errBackupKey
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(backupDestinationSetting))
	if err != nil || json.Unmarshal(plain, &state) != nil || state.Version != 1 || len(state.Items) > 64 {
		return backupDestinationState{}, "", errBackupKey
	}
	return state, raw, nil
}

// Compare-and-swap works across processes, not just an in-memory mutex. A stale
// browser or another API instance cannot silently overwrite a changed target.
func (a *App) saveBackupDestinationState(ctx context.Context, previous string, state backupDestinationState) error {
	if len(state.Items) > 64 {
		return errBackupLimit
	}
	aead, err := a.backupCipher()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	raw := "v1:" + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(backupDestinationSetting)))
	if len(raw) > maxBackupDestinationCiphertext {
		return errBackupLimit
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	key := keyColumnSQL(a.cfg.DBDriver)
	var result sql.Result
	if previous == "" {
		query := insertIgnoreSQL(a.cfg.DBDriver, `INSERT INTO system_settings(`+key+`,value,updated_at) VALUES(?,?,?)`, `(`+key+`)`)
		result, err = a.db.ExecContext(ctx, query, backupDestinationSetting, raw, now)
	} else {
		result, err = a.db.ExecContext(ctx, `UPDATE system_settings SET value=?,updated_at=? WHERE `+key+`=? AND value=?`, raw, now, backupDestinationSetting, previous)
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

func backupDestinationResponse(state backupDestinationState, raw string, configured bool) backupDestinationList {
	response := backupDestinationList{EncryptionConfigured: configured, Revision: backupRevision(raw), Items: []backupDestinationView{}}
	for _, record := range state.Items {
		d := record.Config
		view := backupDestinationView{ID: d.ID, Name: d.Name, Type: d.Type, Disabled: d.Disabled, CredentialsSet: true, SessionTokenSet: record.Credentials.SessionToken != ""}
		if d.S3 != nil {
			view.S3 = &backupS3Settings{Endpoint: d.S3.Endpoint, Region: d.S3.Region, Bucket: d.S3.Bucket, Prefix: d.S3.Prefix, PathStyle: d.S3.PathStyle}
		}
		if d.WebDAV != nil {
			view.WebDAV = &backupDAVSettings{URL: d.WebDAV.URL}
		}
		if d.Drive != nil {
			view.Drive = &backupDriveSettings{FolderID: d.Drive.FolderID}
			view.CredentialsSet = backupDriveBound(record)
		}
		response.Items = append(response.Items, view)
	}
	return response
}

type backupDestinationRequest struct {
	Revision string          `json:"revision"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Disabled bool            `json:"disabled"`
	S3       *backupS3Input  `json:"s3,omitempty"`
	WebDAV   *backupDAVInput `json:"webdav,omitempty"`
	Drive    *struct{}       `json:"drive,omitempty"`
}

type backupS3Input struct {
	backupS3Settings
	AccessKeyID       string `json:"accessKeyId"`
	SecretAccessKey   string `json:"secretAccessKey"`
	SessionToken      string `json:"sessionToken"`
	ClearSessionToken bool   `json:"clearSessionToken"`
}

type backupDAVInput struct {
	backupDAVSettings
	Username string `json:"username"`
	Password string `json:"password"`
}

func validBackupSecret(s string) bool {
	return s != "" && len(s) <= 8192 && strings.IndexFunc(s, unicode.IsControl) < 0
}

func buildBackupDestination(id string, req backupDestinationRequest, old *backupDestinationRecord) (backupDestinationRecord, error) {
	result := backupDestinationRecord{Config: storage.Destination{ID: id, Name: strings.TrimSpace(req.Name), Type: req.Type, Disabled: req.Disabled}}
	if old != nil {
		if req.Type != old.Config.Type {
			return result, errBackupInvalid
		}
		result.Credentials = old.Credentials
	}
	switch req.Type {
	case "s3":
		if req.S3 == nil || req.WebDAV != nil || req.Drive != nil {
			return result, errBackupInvalid
		}
		s := req.S3
		result.Config.S3 = &storage.S3Config{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix, PathStyle: s.PathStyle, AccessKeyIDEnv: "BACKUP_KEY", SecretAccessKeyEnv: "BACKUP_SECRET"}
		if old != nil && old.Config.S3.Endpoint != s.Endpoint {
			result.Credentials = backupCredentials{} // Never carry secrets to a new endpoint.
		}
		if (s.AccessKeyID == "") != (s.SecretAccessKey == "") || (s.ClearSessionToken && s.SessionToken != "") {
			return result, errBackupAuth
		}
		if s.AccessKeyID != "" {
			result.Credentials.AccessKeyID, result.Credentials.SecretAccessKey = s.AccessKeyID, s.SecretAccessKey
			// Rotating the key pair must not retain a token for the old key pair.
			result.Credentials.SessionToken = s.SessionToken
		} else if s.SessionToken != "" {
			result.Credentials.SessionToken = s.SessionToken
		}
		if s.ClearSessionToken {
			result.Credentials.SessionToken = ""
		}
		c := result.Credentials
		if !validBackupSecret(c.AccessKeyID) || !validBackupSecret(c.SecretAccessKey) || (c.SessionToken != "" && !validBackupSecret(c.SessionToken)) {
			return result, errBackupAuth
		}
	case "webdav":
		if req.WebDAV == nil || req.S3 != nil || req.Drive != nil {
			return result, errBackupInvalid
		}
		d := req.WebDAV
		result.Config.WebDAV = &storage.WebDAVConfig{URL: d.URL, UsernameEnv: "BACKUP_USER", PasswordEnv: "BACKUP_PASSWORD"}
		if old != nil && old.Config.WebDAV.URL != d.URL {
			result.Credentials = backupCredentials{}
		}
		if (d.Username == "") != (d.Password == "") {
			return result, errBackupAuth
		}
		if d.Username != "" {
			result.Credentials.Username, result.Credentials.Password = d.Username, d.Password
		}
		c := result.Credentials
		if !validBackupSecret(c.Username) || strings.Contains(c.Username, ":") || !validBackupSecret(c.Password) {
			return result, errBackupAuth
		}
	case "drive":
		if req.Drive == nil || req.S3 != nil || req.WebDAV != nil {
			return result, errBackupInvalid
		}
		result.Config.Drive = &storage.DriveConfig{AccessTokenEnv: "BACKUP_DRIVE_TOKEN"}
		if old != nil && old.Config.Drive != nil {
			copy := *old.Config.Drive
			result.Config.Drive = &copy
		}
		// Unbound Drive destinations are valid configuration, not valid upload
		// targets. Only the OAuth callback may bind a folder and refresh token.
		validated := result.Config
		if validated.Drive.FolderID == "" {
			copy := *validated.Drive
			copy.FolderID = "pending"
			validated.Drive = &copy
		}
		if err := (storage.Config{Version: 1, Destinations: []storage.Destination{validated}}).Validate(); err != nil {
			return result, errBackupInvalid
		}
		return result, nil
	default:
		return result, errBackupInvalid
	}
	if err := (storage.Config{Version: 1, Destinations: []storage.Destination{result.Config}}).Validate(); err != nil {
		return result, errBackupInvalid
	}
	return result, nil
}
