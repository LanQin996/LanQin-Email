package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type contextKey string

const userContextKey contextKey = "user"

func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(a.corsMiddleware)

	r.Post("/auth-policy", a.handleAuthPolicy)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, http.StatusOK, map[string]any{"ok": true, "time": a.now().UTC()})
	})

	r.Route("/api", func(r chi.Router) {
		r.Get("/public/settings", a.handlePublicSettings)
		r.Post("/auth/register", a.handleRegister)
		r.Post("/auth/login", a.handleLogin)
		r.Post("/auth/logout", a.handleLogout)
		r.With(a.requireAuth).Get("/me", a.handleMe)
		r.With(a.requireAuth).Post("/me/profile", a.handleUpdateProfile)
		r.With(a.requireAuth).Post("/me/password", a.handleChangePassword)
		r.With(a.requireAuth, a.requirePermission(PermissionMailboxApply)).Get("/me/mailbox-apply-options", a.handleMailboxApplyOptions)
		r.With(a.requireAuth, a.requirePermission(PermissionMailboxApply)).Post("/me/mailboxes/apply", a.handleApplyMailbox)
		r.With(a.requireAuth).Post("/me/2fa/setup", a.handleTwoFactorSetup)
		r.With(a.requireAuth).Post("/me/2fa/enable", a.handleTwoFactorEnable)
		r.With(a.requireAuth).Post("/me/2fa/disable", a.handleTwoFactorDisable)
		r.With(a.requireAuth, a.requirePermission(PermissionMailContacts)).Get("/me/contacts", a.handleListContacts)
		r.With(a.requireAuth, a.requirePermission(PermissionMailContacts)).Post("/me/contacts", a.handleCreateContact)
		r.With(a.requireAuth, a.requirePermission(PermissionMailContacts)).Delete("/me/contacts/{id}", a.handleDeleteContact)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Get("/me/signatures", a.handleListSignatures)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Post("/me/signatures", a.handleCreateSignature)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Post("/me/signatures/{id}", a.handleUpdateSignature)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Post("/me/signatures/{id}/default", a.handleSetDefaultSignature)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Delete("/me/signatures/{id}", a.handleDeleteSignature)
		r.With(a.requireAuth, a.requirePermission(PermissionMailSignatures)).Get("/me/signatures/default", a.handleDefaultSignature)
		r.With(a.requireAuth, a.requirePermission(PermissionMailRules)).Get("/me/rules", a.handleListRules)
		r.With(a.requireAuth, a.requirePermission(PermissionMailRules)).Post("/me/rules", a.handleCreateRule)
		r.With(a.requireAuth, a.requirePermission(PermissionMailRules)).Delete("/me/rules/{id}", a.handleDeleteRule)
		r.With(a.requireAuth, a.requirePermission(PermissionMailBlocked)).Get("/me/blocked-senders", a.handleListBlockedSenders)
		r.With(a.requireAuth, a.requirePermission(PermissionMailBlocked)).Post("/me/blocked-senders", a.handleCreateBlockedSender)
		r.With(a.requireAuth, a.requirePermission(PermissionMailBlocked)).Delete("/me/blocked-senders/{id}", a.handleDeleteBlockedSender)
		r.With(a.requireAuth, a.requirePermission(PermissionMailStats)).Get("/me/stats", a.handleMailStats)
		r.With(a.requireAuth, a.requirePermission(PermissionMailOrganize)).Post("/me/cleanup", a.handleMailCleanup)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Get("/me/external-imap-accounts", a.handleListExternalIMAPAccounts)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-accounts", a.handleCreateExternalIMAPAccount)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-accounts/{id}", a.handleUpdateExternalIMAPAccount)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Delete("/me/external-imap-accounts/{id}", a.handleDeleteExternalIMAPAccount)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-accounts/{id}/test", a.handleTestExternalIMAPAccount)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Get("/me/external-imap-accounts/{id}/runs", a.handleExternalIMAPSyncRuns)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-accounts/{id}/sync", a.handleSyncExternalIMAPAccount)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-accounts/{id}/sync-folder", a.handleSyncExternalIMAPFolder)
		r.With(a.requireAuth, a.requirePermission(PermissionMailAccess), a.requireExternalIMAPEnabled).Post("/me/external-imap-oauth/{provider}/start", a.handleStartExternalIMAPOAuth)
		r.With(a.requireExternalIMAPEnabled).Get("/external-imap-oauth/{provider}/callback", a.handleExternalIMAPOAuthCallback)
		r.With(a.requireAuth).Get("/events", a.handleEvents)

		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.With(a.requirePermission(PermissionMailAccess)).Get("/mail/mailboxes", a.handleMyMailboxes)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/folders", a.handleMailFolders)
			r.With(a.requirePermission(PermissionMailOrganize)).Post("/mail/folders", a.handleCreateMailFolder)
			r.With(a.requirePermission(PermissionMailOrganize)).Post("/mail/folders/reorder", a.handleReorderMailFolders)
			r.With(a.requirePermission(PermissionMailOrganize)).Delete("/mail/folders/{id}", a.handleDeleteMailFolder)
			r.With(a.requireAnyPermission(PermissionMailRead, PermissionMailLabels)).Get("/mail/labels", a.handleMailLabels)
			r.With(a.requirePermission(PermissionMailLabels)).Post("/mail/labels", a.handleCreateMailLabel)
			r.With(a.requirePermission(PermissionMailLabels)).Delete("/mail/labels/{id}", a.handleDeleteMailLabel)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/messages", a.handleMailMessages)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/starred", a.handleStarredMessages)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/messages/{id}", a.handleMailMessage)
			r.With(a.requirePermission(PermissionMailRead), a.requireExternalIMAPEnabled).Get("/mail/external-accounts", a.handleMailExternalAccounts)
			r.With(a.requirePermission(PermissionMailRead), a.requireExternalIMAPEnabled).Get("/mail/external-accounts/{id}/folders", a.handleExternalIMAPFolders)
			r.With(a.requirePermission(PermissionMailRead), a.requireExternalIMAPEnabled).Get("/mail/external-accounts/{id}/messages", a.handleExternalIMAPMessages)
			r.With(a.requirePermission(PermissionMailRead), a.requireExternalIMAPEnabled).Get("/mail/external-accounts/{id}/messages/{remoteId}", a.handleExternalIMAPMessage)
			r.With(a.requirePermission(PermissionMailAttachments), a.requireExternalIMAPEnabled).Get("/mail/external-accounts/{id}/attachments/{remoteId}/{partId}", a.handleExternalIMAPAttachment)
			r.With(a.requirePermission(PermissionMailOrganize), a.requireExternalIMAPEnabled).Post("/mail/external-accounts/{id}/messages/{remoteId}/mark-read", a.handleExternalIMAPMarkRead)
			r.With(a.requirePermission(PermissionMailSend)).Post("/mail/send", a.handleMailSend)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/send-queue", a.handleSendQueue)
			r.With(a.requirePermission(PermissionMailRead)).Get("/mail/send-queue/{id}/audit", a.handleSendQueueAudit)
			r.With(a.requirePermission(PermissionMailSend)).Post("/mail/send-queue/{id}/retry", a.handleRetrySendQueue)
			r.With(a.requirePermission(PermissionMailSend)).Delete("/mail/send-queue/{id}", a.handleCancelSendQueue)
			r.With(a.requirePermission(PermissionMailSchedule)).Get("/mail/scheduled-sends", a.handleScheduledSends)
			r.With(a.requirePermission(PermissionMailSchedule), a.requirePermission(PermissionMailSend)).Post("/mail/schedule-send", a.handleScheduleSend)
			r.With(a.requirePermission(PermissionMailSchedule)).Delete("/mail/schedule-send/{id}", a.handleCancelScheduledSend)
			r.With(a.requirePermission(PermissionMailDrafts)).Post("/mail/drafts", a.handleSaveDraft)
			r.With(a.requirePermission(PermissionMailDrafts)).Post("/mail/drafts/{id}", a.handleSaveDraft)
			r.With(a.requirePermission(PermissionMailDrafts)).Delete("/mail/drafts/{id}", a.handleDeleteDraft)
			r.With(a.requirePermission(PermissionMailOrganize)).Post("/mail/messages/{id}/mark-read", a.handleMarkRead)
			r.With(a.requirePermission(PermissionMailOrganize)).Post("/mail/messages/{id}/star", a.handleStar)
			r.With(a.requirePermission(PermissionMailLabels)).Post("/mail/messages/{id}/labels", a.handleAddMessageLabel)
			r.With(a.requirePermission(PermissionMailLabels)).Delete("/mail/messages/{id}/labels/{labelID}", a.handleRemoveMessageLabel)
			r.With(a.requirePermission(PermissionMailOrganize)).Post("/mail/messages/{id}/move", a.handleMove)
			r.With(a.requirePermission(PermissionMailOrganize)).Delete("/mail/messages/{id}", a.handleDeleteMessage)
			r.With(a.requirePermission(PermissionMailAttachments)).Get("/mail/attachments/{id}", a.handleAttachment)
		})

		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.Use(a.requireAdminAccess)
			r.With(a.requirePermission(PermissionAdminOverview)).Get("/admin/overview", a.handleAdminOverview)
			r.With(a.requireAnyPermission(PermissionUsersView, PermissionMailboxesView)).Get("/admin/users", a.handleListUsers)
			r.With(a.requirePermission(PermissionUsersCreate)).Post("/admin/users", a.handleCreateUser)
			r.With(a.requirePermission(PermissionUsersUpdate)).Post("/admin/users/{id}", a.handleUpdateUser)
			r.With(a.requirePermission(PermissionUsersResetPassword)).Post("/admin/users/{id}/password", a.handleResetUserPassword)
			r.With(a.requirePermission(PermissionUsersDelete)).Delete("/admin/users/{id}", a.handleDeleteUser)
			r.With(a.requireAnyPermission(PermissionGroupsView, PermissionUsersView)).Get("/admin/permission-limits/defaults", a.handleDefaultPermissionLimits)
			r.With(a.requireAnyPermission(PermissionGroupsView, PermissionUsersView)).Get("/admin/permissions", a.handlePermissionCatalog)
			r.With(a.requireAnyPermission(PermissionGroupsView, PermissionUsersView)).Get("/admin/permission-groups", a.handleListPermissionGroups)
			r.With(a.requirePermission(PermissionGroupsCreate)).Post("/admin/permission-groups", a.handleCreatePermissionGroup)
			r.With(a.requirePermission(PermissionGroupsUpdate)).Post("/admin/permission-groups/{id}", a.handleUpdatePermissionGroup)
			r.With(a.requirePermission(PermissionGroupsDelete)).Delete("/admin/permission-groups/{id}", a.handleDeletePermissionGroup)
			r.With(a.requireAnyPermission(PermissionDomainsView, PermissionDNSView, PermissionMailboxesView, PermissionAliasesView, PermissionSettingsView, PermissionTemplatesView)).Get("/admin/domains", a.handleListDomains)
			r.With(a.requirePermission(PermissionDomainsCreate)).Post("/admin/domains", a.handleCreateDomain)
			r.With(a.requirePermission(PermissionDomainsUpdate)).Post("/admin/domains/{id}", a.handleUpdateDomain)
			r.With(a.requirePermission(PermissionDomainsDelete)).Delete("/admin/domains/{id}", a.handleDeleteDomain)
			r.With(a.requireAnyPermission(PermissionMailboxesView, PermissionMessagesView)).Get("/admin/mailboxes", a.handleListMailboxes)
			r.With(a.requirePermission(PermissionMailboxesCreate)).Post("/admin/mailboxes", a.handleCreateMailbox)
			r.With(a.requirePermission(PermissionMailboxesUpdate)).Post("/admin/mailboxes/{id}", a.handleUpdateMailbox)
			r.With(a.requirePermission(PermissionMailboxesDelete)).Delete("/admin/mailboxes/{id}", a.handleDeleteMailbox)
			r.With(a.requirePermission(PermissionAliasesView)).Get("/admin/aliases", a.handleListAliases)
			r.With(a.requirePermission(PermissionAliasesCreate)).Post("/admin/aliases", a.handleCreateAlias)
			r.With(a.requirePermission(PermissionAliasesUpdate)).Post("/admin/aliases/{id}", a.handleUpdateAlias)
			r.With(a.requirePermission(PermissionAliasesDelete)).Delete("/admin/aliases/{id}", a.handleDeleteAlias)
			r.With(a.requirePermission(PermissionMessagesView)).Get("/admin/messages", a.handleAdminMessages)
			r.With(a.requirePermission(PermissionMessagesView)).Get("/admin/send-audit", a.handleAdminSendAudit)
			r.With(a.requirePermission(PermissionMessagesRead)).Get("/admin/messages/{id}", a.handleAdminMessage)
			r.With(a.requirePermission(PermissionMessagesAttachment)).Get("/admin/attachments/{id}", a.handleAdminAttachment)
			r.With(a.requirePermission(PermissionSettingsView)).Get("/admin/settings", a.handleGetSystemSettings)
			r.With(a.requirePermission(PermissionSettingsView)).Get("/admin/maildir-sync/health", a.handleMaildirSyncHealth)
			r.With(a.requirePermission(PermissionSettingsUpdate)).Post("/admin/settings", a.handleUpdateSystemSettings)
			r.With(a.requirePermission(PermissionSettingsTestSMTP)).Post("/admin/settings/test-smtp", a.handleTestSMTP)
			r.With(a.requirePermission(PermissionTemplatesView)).Get("/admin/mail-templates", a.handleListMailTemplates)
			r.With(a.requirePermission(PermissionTemplatesUpdate)).Post("/admin/mail-templates/{key}", a.handleUpdateMailTemplate)
			r.With(a.requirePermission(PermissionTemplatesReset)).Post("/admin/mail-templates/{key}/reset", a.handleResetMailTemplate)
			r.With(a.requirePermission(PermissionDNSView)).Get("/admin/domains/{id}/dns-records", a.handleDNSRecords)
			r.With(a.requirePermission(PermissionDNSCheck)).Post("/admin/domains/{id}/check-dns", a.handleDNSCheck)
		})
	})

	return r
}

func (a *App) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") || origin == a.cfg.PublicBaseURL) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticateRequest(r)
		if err != nil {
			respondError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	})
}

func currentUser(r *http.Request) *User {
	user, _ := r.Context().Value(userContextKey).(*User)
	return user
}

func (a *App) authenticateRequest(r *http.Request) (*User, error) {
	cookie, err := r.Cookie(a.cfg.CookieName)
	if err != nil || cookie.Value == "" {
		return nil, errors.New("no session")
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT u.id,u.email,u.display_name,u.role,u.disabled,u.two_factor_enabled,u.created_at
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at > ?`, hashToken(cookie.Value), a.now().UTC().Format(time.RFC3339Nano))
	var u User
	var disabled, twoFactorEnabled int
	var created string
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled, &twoFactorEnabled, &created); err != nil {
		return nil, err
	}
	u.Disabled = intBool(disabled)
	u.TwoFactorEnabled = intBool(twoFactorEnabled)
	u.CreatedAt = parseTime(created)
	if u.Disabled {
		return nil, errors.New("disabled")
	}
	if err := a.attachUserAuthorization(r.Context(), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (a *App) userByEmail(ctx context.Context, email string) (*User, string, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,email,display_name,role,password_hash,disabled,two_factor_enabled,created_at FROM users WHERE email=?`, email)
	var u User
	var passwordHash string
	var disabled, twoFactorEnabled int
	var created string
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &passwordHash, &disabled, &twoFactorEnabled, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", errNotFound
		}
		return nil, "", err
	}
	u.Disabled = intBool(disabled)
	u.TwoFactorEnabled = intBool(twoFactorEnabled)
	u.CreatedAt = parseTime(created)
	if err := a.attachUserAuthorization(ctx, &u); err != nil {
		return nil, "", err
	}
	return &u, passwordHash, nil
}

func (a *App) userByID(ctx context.Context, id string) (*User, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id,email,display_name,role,disabled,two_factor_enabled,created_at FROM users WHERE id=?`, id)
	var u User
	var disabled, twoFactorEnabled int
	var created string
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled, &twoFactorEnabled, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	u.Disabled = intBool(disabled)
	u.TwoFactorEnabled = intBool(twoFactorEnabled)
	u.CreatedAt = parseTime(created)
	if err := a.attachUserAuthorization(ctx, &u); err != nil {
		return nil, err
	}
	return &u, nil
}
