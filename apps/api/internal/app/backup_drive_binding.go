package app

import (
	"context"
	"errors"
	"time"

	"golang.org/x/oauth2"
	"lanqin-email-api/internal/backup/storage"
)

func backupDriveBound(record backupDestinationRecord) bool {
	return record.Config.Type == "drive" && record.Config.Drive != nil && storage.ValidDriveID(record.Config.Drive.FolderID) && validBackupSecret(record.Credentials.DriveRefreshToken) && record.Credentials.DriveAuthID != "" && record.Credentials.DriveClientID != ""
}

func (a *App) completeBackupDriveAuthorization(ctx context.Context, id, expected string, token *oauth2.Token) error {
	if !validBackupDriveToken(token, true) {
		return errBackupDriveOAuth
	}
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return err
	}
	name := ""
	for _, r := range vault.Items {
		if r.Config.ID == id && r.Config.Type == "drive" && !r.Config.Disabled && backupTargetFingerprint(r) == expected {
			name = r.Config.Name
			break
		}
	}
	if name == "" {
		return errBackupConflict
	}
	client := a.backupDriveOAuthClient
	closeClient := func() {}
	if client == nil {
		client, closeClient = storage.GoogleDriveHTTPClient()
	}
	defer closeClient()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	folder, err := storage.CreateDriveBackupFolder(ctx, client, token.AccessToken, name, id)
	if err != nil {
		return errBackupDriveOAuth
	}
	// Network work finished before taking a DB write slot. A concurrently
	// changed/deleted destination must not receive this authorization.
	for range 5 {
		current, raw, err := a.loadBackupDestinationState(ctx)
		if err != nil {
			return err
		}
		found := false
		for i := range current.Items {
			r := &current.Items[i]
			if r.Config.ID != id {
				continue
			}
			if r.Config.Type != "drive" || r.Config.Disabled || backupTargetFingerprint(*r) != expected {
				return errBackupConflict
			}
			r.Config.Drive = &storage.DriveConfig{FolderID: folder, AccessTokenEnv: "BACKUP_DRIVE_TOKEN"}
			r.Credentials = backupCredentials{DriveRefreshToken: token.RefreshToken, DriveAccessToken: token.AccessToken, DriveTokenExpiry: token.Expiry, DriveClientID: a.cfg.BackupDriveClientID, DriveAuthID: newID("bda")}
			found = true
			break
		}
		if !found {
			return errBackupMissing
		}
		err = a.saveBackupDestinationState(ctx, raw, current)
		if !errors.Is(err, errBackupConflict) {
			return err
		}
	}
	return errBackupConflict
}
