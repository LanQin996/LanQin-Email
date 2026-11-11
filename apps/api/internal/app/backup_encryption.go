package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const backupEncryptionSetting = "backup.encryption.v1"

var (
	errBackupEncryptionInvalid         = errors.New("backup_encryption_invalid")
	errBackupEncryptionConflict        = errors.New("backup_encryption_conflict")
	errBackupEncryptionUnavailable     = errors.New("backup_encryption_unavailable")
	errBackupEncryptionCurrentPassword = errors.New("backup_encryption_current_password_invalid")
	backupEncryptionGeneration         = regexp.MustCompile(`^bep_[A-Za-z0-9_-]{16,80}$`)
)

type backupEncryptionRecord struct {
	Version    int    `json:"version"`
	Mode       string `json:"mode"`
	Password   string `json:"password,omitempty"`
	Generation string `json:"generation"`
}

type backupEncryptionConfiguration struct {
	Mode      string
	Password  string
	Recipient string
	Revision  string
}

type backupEncryptionView struct {
	Mode                     string `json:"mode"`
	Configured               bool   `json:"configured"`
	PasswordSet              bool   `json:"passwordSet"`
	Recipient                string `json:"recipient"`
	Revision                 string `json:"revision"`
	EnvironmentKeyConfigured bool   `json:"environmentKeyConfigured"`
}

func validBackupPassword(value string) bool {
	characters := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && characters >= 8 && characters <= 128 && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Keep passwords accepted by the previous byte-based policy usable. Tightening
// the creation limit must not disable existing schedules or prevent rotation.
func validStoredBackupPassword(value string) bool {
	return validBackupPassword(value) || utf8.ValidString(value) && len(value) >= 12 && len(value) <= 1024 && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}

func passwordDescriptor(value string) bool {
	return strings.HasPrefix(value, "password:") && backupEncryptionGeneration.MatchString(strings.TrimPrefix(value, "password:"))
}

func (a *App) backupEncryptionConfiguration(ctx context.Context) (backupEncryptionConfiguration, error) {
	configuration := backupEncryptionConfiguration{Mode: "password", Revision: "0"}
	var record backupEncryptionRecord
	raw, err := a.backupRead(ctx, backupEncryptionSetting, &record)
	if errors.Is(err, sql.ErrNoRows) {
		return configuration, nil
	}
	if err != nil {
		return configuration, errBackupEncryptionUnavailable
	}
	if record.Version != 1 || !backupEncryptionGeneration.MatchString(record.Generation) || record.Mode != "key" && record.Mode != "password" || record.Mode == "password" && !validStoredBackupPassword(record.Password) || record.Mode == "key" && record.Password != "" {
		return configuration, errBackupEncryptionUnavailable
	}
	configuration.Revision = backupRevision(raw)
	if record.Mode == "password" {
		configuration.Mode, configuration.Password, configuration.Recipient = "password", record.Password, "password:"+record.Generation
	}
	return configuration, nil
}

func (a *App) saveBackupEncryption(ctx context.Context, revision, mode, password, confirmation, currentPassword string) error {
	if _, err := a.backupCipher(); err != nil {
		return errBackupEncryptionUnavailable
	}
	if revision == "" || len(revision) > 128 || len(currentPassword) > 1024 || mode != "password" {
		return errBackupEncryptionInvalid
	}
	current, err := a.backupEncryptionConfiguration(ctx)
	if err != nil {
		return errBackupEncryptionUnavailable
	}
	if current.Revision != revision {
		return errBackupEncryptionConflict
	}
	if current.Password != "" {
		actual := sha256.Sum256([]byte(currentPassword))
		expected := sha256.Sum256([]byte(current.Password))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			return errBackupEncryptionCurrentPassword
		}
	} else if currentPassword != "" {
		return errBackupEncryptionInvalid
	}
	if !validBackupPassword(password) || password != confirmation {
		return errBackupEncryptionInvalid
	}
	var old backupEncryptionRecord
	previous, readErr := a.backupRead(ctx, backupEncryptionSetting, &old)
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return errBackupEncryptionUnavailable
	}
	if backupRevision(previous) != revision {
		return errBackupEncryptionConflict
	}
	value := backupEncryptionRecord{Version: 1, Mode: mode, Generation: newID("bep")}
	if mode == "password" {
		value.Password = password
	}
	if current.Mode == mode && current.Password == password && previous != "" {
		value.Generation = old.Generation
	}
	if err := a.backupCAS(ctx, a.db, backupEncryptionSetting, previous, value); err != nil {
		if errors.Is(err, errBackupConflict) {
			return errBackupEncryptionConflict
		}
		return errBackupEncryptionUnavailable
	}
	return nil
}

func (a *App) handleBackupEncryption(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		if _, err := a.backupCipher(); err != nil {
			backupEncryptionError(w, errBackupEncryptionUnavailable)
			return
		}
		current, err := a.backupEncryptionConfiguration(r.Context())
		if err != nil {
			backupEncryptionError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, backupEncryptionView{Mode: current.Mode, Configured: current.Password != "", PasswordSet: current.Password != "", Recipient: current.Recipient, Revision: current.Revision})
		return
	}
	var request struct {
		Revision        string `json:"revision"`
		Mode            string `json:"mode"`
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirmPassword"`
		CurrentPassword string `json:"currentPassword"`
	}
	if decodeBackupRequest(r, &request) != nil {
		backupEncryptionError(w, errBackupEncryptionInvalid)
		return
	}
	err := a.saveBackupEncryption(r.Context(), request.Revision, request.Mode, request.Password, request.ConfirmPassword, request.CurrentPassword)
	request.Password, request.ConfirmPassword, request.CurrentPassword = "", "", ""
	if err != nil {
		backupEncryptionError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func backupEncryptionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBackupEncryptionInvalid):
		respondError(w, http.StatusBadRequest, errBackupEncryptionInvalid.Error())
	case errors.Is(err, errBackupEncryptionConflict):
		respondError(w, http.StatusConflict, errBackupEncryptionConflict.Error())
	case errors.Is(err, errBackupEncryptionCurrentPassword):
		respondError(w, http.StatusForbidden, errBackupEncryptionCurrentPassword.Error())
	default:
		respondError(w, http.StatusServiceUnavailable, errBackupEncryptionUnavailable.Error())
	}
}
