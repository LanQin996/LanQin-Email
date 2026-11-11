package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/oauth2"
)

const backupDriveClientSetting = "backup.drive.client.v1"

var (
	errBackupDriveClientInvalid        = errors.New("backup_drive_client_invalid")
	errBackupDriveClientConflict       = errors.New("backup_drive_client_conflict")
	errBackupDriveClientUnavailable    = errors.New("backup_drive_client_unavailable")
	backupDriveClientIDPattern         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,512}$`)
	backupDriveClientGenerationPattern = regexp.MustCompile(`^bdc_[A-Za-z0-9_-]{16,80}$`)
)

type backupDriveClientSecret struct {
	Version      int    `json:"version"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Generation   string `json:"generation"`
}

type backupDriveClientView struct {
	Source          string `json:"source"`
	Configured      bool   `json:"configured"`
	Editable        bool   `json:"editable"`
	ClientID        string `json:"clientId"`
	ClientSecretSet bool   `json:"clientSecretSet"`
	Revision        string `json:"revision"`
	RedirectURI     string `json:"redirectUri"`
}

type effectiveDriveClient struct {
	ClientID        string
	ClientSecret    string
	Generation      string
	Source          string
	Editable        bool
	ClientSecretSet bool
	Revision        string
}

func driveClientGeneration(clientID, secret string) string {
	d := sha256.Sum256([]byte("drive-oauth-client\x00" + clientID + "\x00" + secret))
	return hex.EncodeToString(d[:])
}

// backupDriveClient loads the one effective client atomically. If either
// environment field is present, the environment owns this setting: a partial
// environment is locked and unavailable, never a silent database fallback.
func (a *App) backupDriveClient(ctx context.Context) (effectiveDriveClient, error) {
	id := strings.TrimSpace(a.cfg.BackupDriveClientID)
	secret := strings.TrimSpace(a.cfg.BackupDriveClientSecret)
	if id != "" || secret != "" {
		if id == "" || secret == "" || !backupDriveClientIDPattern.MatchString(id) || len(secret) > 4096 || strings.IndexFunc(secret, unicode.IsControl) >= 0 {
			return effectiveDriveClient{Source: "environment", Editable: false}, errBackupDriveClientUnavailable
		}
		return effectiveDriveClient{ClientID: id, ClientSecret: secret, Generation: driveClientGeneration(id, secret), Source: "environment", Editable: false, ClientSecretSet: true, Revision: "environment"}, nil
	}
	if _, err := a.backupCipher(); err != nil {
		return effectiveDriveClient{Source: "none", Editable: true}, errBackupDriveClientUnavailable
	}
	var config backupDriveClientSecret
	raw, err := a.backupRead(ctx, backupDriveClientSetting, &config)
	if errors.Is(err, sql.ErrNoRows) {
		return effectiveDriveClient{Source: "none", Editable: true, Revision: "0"}, nil
	}
	if err != nil || config.Version != 1 || !backupDriveClientIDPattern.MatchString(config.ClientID) || strings.TrimSpace(config.ClientSecret) == "" || config.ClientSecret != strings.TrimSpace(config.ClientSecret) || len(config.ClientSecret) > 4096 || strings.IndexFunc(config.ClientSecret, unicode.IsControl) >= 0 || !backupDriveClientGenerationPattern.MatchString(config.Generation) {
		return effectiveDriveClient{Source: "database", Editable: true}, errBackupDriveClientUnavailable
	}
	return effectiveDriveClient{ClientID: config.ClientID, ClientSecret: config.ClientSecret, Generation: config.Generation, Source: "database", Editable: true, ClientSecretSet: true, Revision: backupRevision(raw)}, nil
}

func backupDriveRedirectURI(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Path != "" && parsed.Path != "/" {
		return "", errBackupDriveClientUnavailable
	}
	return strings.TrimRight(base, "/") + "/api/admin/backups/drive/oauth/callback", nil
}

func (a *App) backupDriveClientView(ctx context.Context) (backupDriveClientView, error) {
	current, err := a.backupDriveClient(ctx)
	redirect, redirectErr := backupDriveRedirectURI(a.cfg.PublicBaseURL)
	if redirectErr != nil {
		return backupDriveClientView{}, redirectErr
	}
	if err != nil {
		return backupDriveClientView{}, err
	}
	configured := err == nil && current.ClientID != "" && current.ClientSecretSet
	view := backupDriveClientView{Source: current.Source, Configured: configured, Editable: current.Editable,
		ClientID: current.ClientID, ClientSecretSet: current.ClientSecretSet, Revision: current.Revision, RedirectURI: redirect}
	return view, nil
}

func (a *App) saveBackupDriveClient(ctx context.Context, revision, id, secret string) error {
	if strings.TrimSpace(a.cfg.BackupDriveClientID) != "" || strings.TrimSpace(a.cfg.BackupDriveClientSecret) != "" {
		return errBackupDriveClientConflict
	}
	if revision == "" || len(revision) > 128 || id != strings.TrimSpace(id) || !backupDriveClientIDPattern.MatchString(id) || len(secret) > 4096 || strings.IndexFunc(secret, unicode.IsControl) >= 0 {
		return errBackupDriveClientInvalid
	}
	current, err := a.backupDriveClient(ctx)
	if err != nil {
		return errBackupDriveClientUnavailable
	}
	if !current.Editable || revision != current.Revision {
		return errBackupDriveClientConflict
	}
	if secret == "" {
		if current.Source != "database" || current.ClientID != id || !current.ClientSecretSet {
			return errBackupDriveClientInvalid
		}
		secret = current.ClientSecret
	}
	if strings.TrimSpace(secret) == "" || secret != strings.TrimSpace(secret) || len(secret) > 4096 || strings.IndexFunc(secret, unicode.IsControl) >= 0 {
		return errBackupDriveClientInvalid
	}
	value := backupDriveClientSecret{Version: 1, ClientID: id, ClientSecret: secret, Generation: newID("bdc")}
	if id == current.ClientID && secret == current.ClientSecret {
		value.Generation = current.Generation
	}
	previous := ""
	if current.Source == "database" {
		var old backupDriveClientSecret
		previous, err = a.backupRead(ctx, backupDriveClientSetting, &old)
		if err != nil {
			return errBackupDriveClientUnavailable
		}
		if backupRevision(previous) != revision {
			return errBackupDriveClientConflict
		}
	}
	if err := a.backupCAS(ctx, a.db, backupDriveClientSetting, previous, value); err != nil {
		if errors.Is(err, errBackupConflict) {
			return errBackupDriveClientConflict
		}
		return errBackupDriveClientUnavailable
	}
	return nil
}

func (a *App) handleBackupDriveClient(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		view, err := a.backupDriveClientView(r.Context())
		if err != nil {
			// Partial environment overrides intentionally remain visible as a
			// locked, not-configured state so the administrator can fix env files.
			current, _ := a.backupDriveClient(r.Context())
			if current.Source == "environment" {
				redirect, _ := backupDriveRedirectURI(a.cfg.PublicBaseURL)
				respondJSON(w, http.StatusOK, backupDriveClientView{Source: "environment", Configured: false, Editable: false, Revision: "environment", RedirectURI: redirect})
				return
			}
			respondError(w, http.StatusServiceUnavailable, errBackupDriveClientUnavailable.Error())
			return
		}
		respondJSON(w, http.StatusOK, view)
		return
	}
	var request struct {
		Revision     string `json:"revision"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := decodeBackupRequest(r, &request); err != nil {
		backupError(w, err)
		return
	}
	err := a.saveBackupDriveClient(r.Context(), request.Revision, request.ClientID, request.ClientSecret)
	request.ClientSecret = ""
	if err != nil {
		switch {
		case errors.Is(err, errBackupDriveClientInvalid):
			respondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errBackupDriveClientConflict):
			respondError(w, http.StatusConflict, err.Error())
		default:
			respondError(w, http.StatusServiceUnavailable, errBackupDriveClientUnavailable.Error())
		}
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) backupDriveOAuthConfigFor(ctx context.Context) (*oauth2.Config, string, error) {
	client, err := a.backupDriveClient(ctx)
	if err != nil || client.ClientID == "" || client.ClientSecret == "" {
		return nil, "", errBackupExecution
	}
	redirect, err := backupDriveRedirectURI(a.cfg.PublicBaseURL)
	if err != nil {
		return nil, "", errBackupExecution
	}
	return &oauth2.Config{ClientID: client.ClientID, ClientSecret: client.ClientSecret, RedirectURL: redirect, Scopes: []string{backupDriveScope},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}}, client.Generation, nil
}

func (a *App) driveClientGenerationCurrent(ctx context.Context, generation string) bool {
	current, err := a.backupDriveClient(ctx)
	return err == nil && current.Generation == generation
}

func (a *App) backupDriveGrantMatches(ctx context.Context, credentials backupCredentials, clientID, generation string) bool {
	if credentials.DriveClientID != clientID {
		return false
	}
	if credentials.DriveClientGeneration == generation {
		return true
	}
	// Existing environment-issued grants predate this binding. Retain those
	// only while the same deployment-owned environment client remains effective.
	current, err := a.backupDriveClient(ctx)
	return err == nil && current.Source == "environment" && credentials.DriveClientGeneration == "" && current.Generation == generation
}
