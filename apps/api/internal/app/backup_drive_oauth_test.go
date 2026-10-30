package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"lanqin-email-api/internal/backup/storage"
)

func backupDriveOAuthFixture(t *testing.T) (*App, string, string) {
	t.Helper()
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	a.cfg.PublicBaseURL = "https://mail.example.test"
	a.cfg.BackupDriveClientID = "synthetic-client"
	a.cfg.BackupDriveClientSecret = "synthetic-client-secret"
	record := backupDestinationRecord{Config: storage.Destination{ID: "bkd_drive", Name: "Synthetic Drive", Type: "drive", Drive: &storage.DriveConfig{}}}
	if err := a.saveBackupDestinationState(t.Context(), "", backupDestinationState{Version: 1, Items: []backupDestinationRecord{record}}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := a.loadBackupDestinationState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return a, record.Config.ID, backupRevision(raw)
}

func driveAuthorizationState(t *testing.T, a *App, actor, id, revision string) (string, url.Values) {
	t.Helper()
	link, err := a.beginBackupDriveOAuth(t.Context(), actor, id, revision)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "https" || u.Host != "accounts.google.com" || q.Get("scope") != backupDriveScope || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") != "https://mail.example.test/api/admin/backups/drive/oauth/callback" || strings.Contains(link, a.cfg.BackupDriveClientSecret) {
		t.Fatal("unsafe OAuth authorization request")
	}
	return q.Get("state"), q
}

func TestBackupDriveOAuthStateIsBoundEncryptedSingleUseAndPKCE(t *testing.T) {
	a, id, revision := backupDriveOAuthFixture(t)
	value, q := driveAuthorizationState(t, a, "actor-one", id, revision)
	var stored backupDriveOAuthState
	raw, err := a.backupRead(t.Context(), backupDriveStateKey("actor-one", id), &stored)
	if err != nil || strings.Contains(raw, stored.Verifier) || strings.Contains(raw, value) || strings.Contains(raw, "synthetic-client") {
		t.Fatal("OAuth state not encrypted", err)
	}
	config, err := a.backupDriveOAuthConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.AuthCodeURL("test", oauth2.S256ChallengeOption(stored.Verifier)) == "" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(stored.Verifier) {
		t.Fatal("PKCE verifier not bound")
	}
	if _, _, err := a.consumeBackupDriveOAuth(t.Context(), "actor-other", value); err == nil {
		t.Fatal("state crossed administrator session")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, _, err := a.consumeBackupDriveOAuth(t.Context(), "actor-one", value); err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("single-use state replay accepted", successes.Load())
	}
	if _, _, err := a.consumeBackupDriveOAuth(t.Context(), "actor-one", value); err == nil {
		t.Fatal("consumed state replayed")
	}
	var consumed backupDriveOAuthState
	if _, err := a.backupRead(t.Context(), backupDriveStateKey("actor-one", id), &consumed); err != nil || !consumed.Used || consumed.Verifier != "" {
		t.Fatal("verifier retained after consumption", err)
	}
}

func TestBackupDriveOAuthRefusesExpiredSupersededAndChangedTargets(t *testing.T) {
	for _, scenario := range []string{"expired", "superseded", "changed-target", "changed-client", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			a, id, revision := backupDriveOAuthFixture(t)
			state, _ := driveAuthorizationState(t, a, "actor", id, revision)
			switch scenario {
			case "expired":
				now := a.now()
				a.now = func() time.Time { return now.Add(11 * time.Minute) }
			case "superseded":
				driveAuthorizationState(t, a, "actor", id, revision)
			case "changed-client":
				a.cfg.BackupDriveClientID = "another-client"
			case "changed-target":
				vault, raw, err := a.loadBackupDestinationState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				vault.Items[0].Config.Disabled = true
				if err := a.saveBackupDestinationState(t.Context(), raw, vault); err != nil {
					t.Fatal(err)
				}
			}
			var exchanges int
			exchange := func(context.Context, *oauth2.Config, string, string) (*oauth2.Token, error) {
				exchanges++
				return nil, errBackupDriveOAuth
			}
			providerError := ""
			if scenario == "denied" {
				providerError = "untrusted-provider-error-with-private-data"
			}
			if err := a.processBackupDriveCallback(t.Context(), "actor", state, "synthetic-code", providerError, exchange); err == nil || strings.Contains(err.Error(), "private-data") {
				t.Fatal("invalid authorization accepted or provider body exposed")
			}
			if exchanges != 0 {
				t.Fatal("invalid state reached token exchange")
			}
			if scenario == "denied" {
				if _, _, err := a.consumeBackupDriveOAuth(t.Context(), "actor", state); err == nil {
					t.Fatal("denial left reusable state")
				}
			}
		})
	}
}

func TestBackupDriveOAuthTLSExchangeBindsVerifierAndRejectsPartialScope(t *testing.T) {
	for _, scenario := range []string{"success", "scope-missing", "scope-wrong", "refresh-missing", "provider-error"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "synthetic-code" || r.Form.Get("code_verifier") != "synthetic-verifier" || r.Form.Get("client_id") != "synthetic-client" || r.Form.Get("client_secret") != "synthetic-secret" || r.Form.Get("redirect_uri") != "https://mail.example.test/callback" {
					t.Error("token exchange omitted binding")
				}
				w.Header().Set("Content-Type", "application/json")
				body := map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": backupDriveScope}
				switch scenario {
				case "scope-missing":
					delete(body, "scope")
				case "scope-wrong":
					body["scope"] = "https://www.googleapis.com/auth/drive.metadata.readonly"
				case "refresh-missing":
					delete(body, "refresh_token")
				case "provider-error":
					w.WriteHeader(http.StatusBadRequest)
					body = map[string]any{"error": "invalid_grant", "error_description": "synthetic-secret-must-not-leak"}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			config := &oauth2.Config{ClientID: "synthetic-client", ClientSecret: "synthetic-secret", RedirectURL: "https://mail.example.test/callback", Endpoint: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}
			token, err := exchangeBackupDriveCode(t.Context(), config, "synthetic-code", "synthetic-verifier", server.Client())
			if scenario == "success" {
				if err != nil || token.RefreshToken != "synthetic-refresh" {
					t.Fatal("TLS exchange failed", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "synthetic-secret") || token != nil {
				t.Fatal("bad grant accepted or secret exposed")
			}
		})
	}
}

func TestBackupDriveOAuthConfigRefusesInsecureOrUntrustedCallbacks(t *testing.T) {
	a, _, _ := backupDriveOAuthFixture(t)
	for _, base := range []string{"http://mail.example.test", "https://user:password@mail.example.test", "https://mail.example.test/?secret=x", "https://mail.example.test/#fragment", "https://mail.example.test/other-path"} {
		a.cfg.PublicBaseURL = base
		if _, err := a.backupDriveOAuthConfig(); err == nil || strings.Contains(err.Error(), "password") {
			t.Fatal("unsafe callback configuration accepted")
		}
	}
}
