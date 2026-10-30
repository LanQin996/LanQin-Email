package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"lanqin-email-api/internal/backup/storage"
)

// backupDriveToken refreshes before every job rather than handing the uploader a
// nearly expired cached access token. Credential generations are bound to an
// authorization ID; a late refresh cannot overwrite a newly authorized target.
func (a *App) backupDriveToken(ctx context.Context, id string, expectedFingerprint ...string) (string, error) {
	config, err := a.backupDriveOAuthConfig()
	if err != nil {
		return "", errBackupDriveOAuth
	}
	client := a.backupDriveOAuthClient
	closeClient := func() {}
	if client == nil {
		client, closeClient = storage.GoogleDriveHTTPClient()
	}
	defer closeClient()
	for range 3 {
		vault, _, err := a.loadBackupDestinationState(ctx)
		if err != nil {
			return "", errBackupDriveOAuth
		}
		index := -1
		for i := range vault.Items {
			if vault.Items[i].Config.ID == id {
				index = i
				break
			}
		}
		if index < 0 {
			return "", errBackupDriveOAuth
		}
		original := vault.Items[index]
		if original.Config.Type != "drive" || original.Config.Disabled || original.Config.Drive == nil || original.Config.Drive.FolderID == "" || original.Credentials.DriveClientID != config.ClientID || original.Credentials.DriveAuthID == "" || !validBackupSecret(original.Credentials.DriveRefreshToken) {
			return "", errBackupDriveOAuth
		}
		fingerprint := backupTargetFingerprint(original)
		if len(expectedFingerprint) > 0 && fingerprint != expectedFingerprint[0] {
			return "", errBackupDriveOAuth
		}
		token, err := refreshBackupDriveAccess(ctx, config, original.Credentials.DriveRefreshToken, client)
		if err != nil {
			// A concurrent successful refresh may have rotated the refresh token.
			// Retry only that demonstrated generation-preserving change.
			current, _, loadErr := a.loadBackupDestinationState(ctx)
			if loadErr == nil {
				changed := false
				for _, item := range current.Items {
					if item.Config.ID == id && backupTargetFingerprint(item) == fingerprint && item.Credentials.DriveRefreshToken != original.Credentials.DriveRefreshToken {
						changed = true
					}
				}
				if changed {
					continue
				}
			}
			return "", errBackupDriveOAuth
		}
		// Retry metadata CAS with this SAME successful token. A global vault
		// conflict can be an unrelated destination edit; making another network
		// refresh with the old token could lose an already rotated grant.
		retryCurrentGrant := false
		for range 5 {
			current, currentRaw, err := a.loadBackupDestinationState(ctx)
			if err != nil {
				return "", errBackupDriveOAuth
			}
			found := -1
			for i := range current.Items {
				if current.Items[i].Config.ID == id {
					found = i
					break
				}
			}
			if found < 0 || current.Items[found].Config.Disabled || backupTargetFingerprint(current.Items[found]) != fingerprint {
				return "", errBackupDriveOAuth
			}
			if current.Items[found].Credentials.DriveRefreshToken != original.Credentials.DriveRefreshToken {
				retryCurrentGrant = true // A demonstrated concurrent refresh won.
				break
			}
			current.Items[found].Credentials.DriveAccessToken = token.AccessToken
			current.Items[found].Credentials.DriveTokenExpiry = token.Expiry
			current.Items[found].Credentials.DriveRefreshToken = token.RefreshToken
			if err := a.saveBackupDestinationState(ctx, currentRaw, current); err != nil {
				if errors.Is(err, errBackupConflict) {
					continue
				}
				return "", errBackupDriveOAuth
			}
			return token.AccessToken, nil
		}
		if !retryCurrentGrant {
			return "", errBackupDriveOAuth
		}
	}
	return "", errBackupDriveOAuth
}

func refreshBackupDriveAccess(ctx context.Context, config *oauth2.Config, refresh string, client *http.Client) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := config.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil || token == nil || !validBackupSecret(token.AccessToken) || !strings.EqualFold(token.TokenType, "Bearer") || token.Expiry.IsZero() || !token.Expiry.After(time.Now()) {
		return nil, errBackupDriveOAuth
	}
	scope, _ := token.Extra("scope").(string)
	if scope != "" {
		granted := false
		for _, part := range strings.Fields(scope) {
			if part == backupDriveScope {
				granted = true
			}
		}
		if !granted {
			return nil, errBackupDriveOAuth
		}
	}
	// Scope omission on refresh retains the initial, already validated grant.
	if token.RefreshToken == "" {
		token.RefreshToken = refresh
	}
	if !validBackupSecret(token.RefreshToken) {
		return nil, errBackupDriveOAuth
	}
	return token, nil
}
