package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
	"lanqin-email-api/internal/backup/storage"
)

const backupDriveScope = "https://www.googleapis.com/auth/drive.file"
const backupDriveOAuthPrefix = "backup.drive.oauth.v1."

var backupDriveStatePattern = regexp.MustCompile(`^([A-Za-z0-9_-]{1,64})\.([a-f0-9]{64})$`)
var errBackupDriveOAuth = errors.New("backup_drive_authorization_failed")

type backupDriveOAuthState struct {
	Version       int       `json:"version"`
	ActorID       string    `json:"actorId"`
	DestinationID string    `json:"destinationId"`
	StateHash     string    `json:"stateHash"`
	Fingerprint   string    `json:"fingerprint"`
	ClientID      string    `json:"clientId"`
	RedirectURI   string    `json:"redirectUri"`
	Verifier      string    `json:"verifier"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Used          bool      `json:"used"`
}

func (a *App) backupDriveOAuthConfig() (*oauth2.Config, error) {
	base, err := url.Parse(a.cfg.PublicBaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" || base.Path != "" && base.Path != "/" || strings.TrimSpace(a.cfg.BackupDriveClientID) == "" || strings.TrimSpace(a.cfg.BackupDriveClientSecret) == "" {
		return nil, errBackupExecution
	}
	return &oauth2.Config{ClientID: a.cfg.BackupDriveClientID, ClientSecret: a.cfg.BackupDriveClientSecret,
		RedirectURL: strings.TrimRight(a.cfg.PublicBaseURL, "/") + "/api/admin/backups/drive/oauth/callback", Scopes: []string{backupDriveScope},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}}, nil
}

func backupDriveStateKey(actor, destination string) string {
	h := sha256.Sum256([]byte(actor + "\x00" + destination))
	return backupDriveOAuthPrefix + hex.EncodeToString(h[:])
}

func backupDriveHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func (a *App) beginBackupDriveOAuth(ctx context.Context, actor, id, revision string) (string, error) {
	config, err := a.backupDriveOAuthConfig()
	if err != nil {
		return "", err
	}
	state, raw, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return "", err
	}
	if revision != backupRevision(raw) {
		return "", errBackupConflict
	}
	var target *backupDestinationRecord
	for i := range state.Items {
		if state.Items[i].Config.ID == id {
			target = &state.Items[i]
			break
		}
	}
	if target == nil {
		return "", errBackupMissing
	}
	if target.Config.Type != "drive" || target.Config.Disabled {
		return "", errBackupInvalid
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errBackupDriveOAuth
	}
	value := id + "." + hex.EncodeToString(random[:])
	verifier := oauth2.GenerateVerifier()
	record := backupDriveOAuthState{Version: 1, ActorID: actor, DestinationID: id, StateHash: backupDriveHash(value), Fingerprint: backupTargetFingerprint(*target), ClientID: config.ClientID, RedirectURI: config.RedirectURL, Verifier: verifier, ExpiresAt: a.now().UTC().Add(10 * time.Minute)}
	key := backupDriveStateKey(actor, id)
	var old backupDriveOAuthState
	previous, err := a.backupRead(ctx, key, &old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err := a.backupCAS(ctx, a.db, key, previous, record); err != nil {
		return "", err
	}
	return config.AuthCodeURL(value, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.SetAuthURLParam("include_granted_scopes", "false"), oauth2.S256ChallengeOption(verifier)), nil
}

func (a *App) consumeBackupDriveOAuth(ctx context.Context, actor, value string) (backupDriveOAuthState, *oauth2.Config, error) {
	var empty backupDriveOAuthState
	parts := backupDriveStatePattern.FindStringSubmatch(value)
	if len(parts) != 3 {
		return empty, nil, errBackupDriveOAuth
	}
	config, err := a.backupDriveOAuthConfig()
	if err != nil {
		return empty, nil, err
	}
	key := backupDriveStateKey(actor, parts[1])
	var record backupDriveOAuthState
	raw, err := a.backupRead(ctx, key, &record)
	if err != nil || record.Version != 1 || record.Used || record.ActorID != actor || record.DestinationID != parts[1] || record.StateHash != backupDriveHash(value) || !record.ExpiresAt.After(a.now()) || record.ClientID != config.ClientID || record.RedirectURI != config.RedirectURL || len(record.Verifier) < 43 {
		return empty, nil, errBackupDriveOAuth
	}
	// Consume before network exchange, including denial/error callbacks. A lost
	// token response requires a new consent flow rather than replaying the code.
	used := record
	used.Used = true
	used.Verifier = ""
	if err := a.backupCAS(ctx, a.db, key, raw, used); err != nil {
		return empty, nil, errBackupDriveOAuth
	}
	vault, _, err := a.loadBackupDestinationState(ctx)
	if err != nil {
		return empty, nil, errBackupDriveOAuth
	}
	for _, target := range vault.Items {
		if target.Config.ID == record.DestinationID && target.Config.Type == "drive" && !target.Config.Disabled && backupTargetFingerprint(target) == record.Fingerprint {
			return record, config, nil
		}
	}
	return empty, nil, errBackupDriveOAuth
}

type backupDriveExchanger func(context.Context, *oauth2.Config, string, string) (*oauth2.Token, error)

func (a *App) processBackupDriveCallback(ctx context.Context, actor, state, code, providerError string, exchange backupDriveExchanger) error {
	record, config, err := a.consumeBackupDriveOAuth(ctx, actor, state)
	if err != nil {
		return errBackupDriveOAuth
	}
	if providerError != "" || code == "" || len(code) > 8192 || strings.IndexAny(code, "\r\n\x00") >= 0 {
		return errBackupDriveOAuth
	}
	token, err := exchange(ctx, config, code, record.Verifier)
	if err != nil || !validBackupDriveToken(token, true) {
		return errBackupDriveOAuth
	}
	if err := a.completeBackupDriveAuthorization(ctx, record.DestinationID, record.Fingerprint, token); err != nil {
		return errBackupDriveOAuth
	}
	return nil
}

func validBackupDriveToken(token *oauth2.Token, requireRefresh bool) bool {
	if token == nil || !validBackupSecret(token.AccessToken) || !strings.EqualFold(token.TokenType, "Bearer") || token.Expiry.IsZero() || !token.Expiry.After(time.Now()) || requireRefresh && !validBackupSecret(token.RefreshToken) {
		return false
	}
	scope, _ := token.Extra("scope").(string)
	for _, part := range strings.Fields(scope) {
		if part == backupDriveScope {
			return true
		}
	}
	return false
}

func exchangeBackupDriveCode(ctx context.Context, config *oauth2.Config, code, verifier string, client *http.Client) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil || !validBackupDriveToken(token, true) {
		return nil, errBackupDriveOAuth
	}
	return token, nil
}

func (a *App) handleBackupDriveAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req struct {
		Revision string `json:"revision"`
	}
	if err := decodeBackupRequest(r, &req); err != nil {
		backupError(w, err)
		return
	}
	value, err := a.beginBackupDriveOAuth(r.Context(), user.ID, chi.URLParam(r, "id"), req.Revision)
	if err != nil {
		backupError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"authorizationUrl": value})
}

func (a *App) handleBackupDriveCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	client := a.backupDriveOAuthClient
	closeClient := func() {}
	if client == nil {
		client, closeClient = storage.GoogleDriveHTTPClient()
	}
	defer closeClient()
	// The parent router's logger must redact this callback's query; no provider
	// strings, codes, state values or token bodies are included in errors here.
	query := r.URL.Query()
	err := a.processBackupDriveCallback(r.Context(), user.ID, query.Get("state"), query.Get("code"), query.Get("error"), func(ctx context.Context, c *oauth2.Config, code, verifier string) (*oauth2.Token, error) {
		return exchangeBackupDriveCode(ctx, c, code, verifier, client)
	})
	result := "success"
	if err != nil {
		result = "failed"
	}
	http.Redirect(w, r, "/admin?section=backups&driveOAuth="+result, http.StatusSeeOther)
}
