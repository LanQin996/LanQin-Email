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
		r.With(a.requireAuth).Get("/me/mailbox-apply-options", a.handleMailboxApplyOptions)
		r.With(a.requireAuth).Post("/me/mailboxes/apply", a.handleApplyMailbox)
		r.With(a.requireAuth).Post("/me/2fa/setup", a.handleTwoFactorSetup)
		r.With(a.requireAuth).Post("/me/2fa/enable", a.handleTwoFactorEnable)
		r.With(a.requireAuth).Post("/me/2fa/disable", a.handleTwoFactorDisable)
		r.With(a.requireAuth).Get("/me/contacts", a.handleListContacts)
		r.With(a.requireAuth).Post("/me/contacts", a.handleCreateContact)
		r.With(a.requireAuth).Delete("/me/contacts/{id}", a.handleDeleteContact)
		r.With(a.requireAuth).Get("/me/signatures", a.handleListSignatures)
		r.With(a.requireAuth).Post("/me/signatures", a.handleCreateSignature)
		r.With(a.requireAuth).Post("/me/signatures/{id}", a.handleUpdateSignature)
		r.With(a.requireAuth).Post("/me/signatures/{id}/default", a.handleSetDefaultSignature)
		r.With(a.requireAuth).Delete("/me/signatures/{id}", a.handleDeleteSignature)
		r.With(a.requireAuth).Get("/me/signatures/default", a.handleDefaultSignature)
		r.With(a.requireAuth).Get("/me/rules", a.handleListRules)
		r.With(a.requireAuth).Post("/me/rules", a.handleCreateRule)
		r.With(a.requireAuth).Delete("/me/rules/{id}", a.handleDeleteRule)
		r.With(a.requireAuth).Get("/me/blocked-senders", a.handleListBlockedSenders)
		r.With(a.requireAuth).Post("/me/blocked-senders", a.handleCreateBlockedSender)
		r.With(a.requireAuth).Delete("/me/blocked-senders/{id}", a.handleDeleteBlockedSender)
		r.With(a.requireAuth).Get("/me/stats", a.handleMailStats)
		r.With(a.requireAuth).Post("/me/cleanup", a.handleMailCleanup)
		r.With(a.requireAuth).Get("/events", a.handleEvents)

		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.Get("/mail/mailboxes", a.handleMyMailboxes)
			r.Get("/mail/folders", a.handleMailFolders)
			r.Get("/mail/labels", a.handleMailLabels)
			r.Post("/mail/labels", a.handleCreateMailLabel)
			r.Get("/mail/messages", a.handleMailMessages)
			r.Get("/mail/starred", a.handleStarredMessages)
			r.Get("/mail/messages/{id}", a.handleMailMessage)
			r.Post("/mail/send", a.handleMailSend)
			r.Post("/mail/messages/{id}/mark-read", a.handleMarkRead)
			r.Post("/mail/messages/{id}/star", a.handleStar)
			r.Post("/mail/messages/{id}/labels", a.handleAddMessageLabel)
			r.Delete("/mail/messages/{id}/labels/{labelID}", a.handleRemoveMessageLabel)
			r.Post("/mail/messages/{id}/move", a.handleMove)
			r.Delete("/mail/messages/{id}", a.handleDeleteMessage)
			r.Get("/mail/attachments/{id}", a.handleAttachment)
		})

		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.Use(a.requireAdmin)
			r.Get("/admin/overview", a.handleAdminOverview)
			r.Get("/admin/users", a.handleListUsers)
			r.Post("/admin/users", a.handleCreateUser)
			r.Post("/admin/users/{id}", a.handleUpdateUser)
			r.Post("/admin/users/{id}/password", a.handleResetUserPassword)
			r.Delete("/admin/users/{id}", a.handleDeleteUser)
			r.Get("/admin/domains", a.handleListDomains)
			r.Post("/admin/domains", a.handleCreateDomain)
			r.Post("/admin/domains/{id}", a.handleUpdateDomain)
			r.Delete("/admin/domains/{id}", a.handleDeleteDomain)
			r.Get("/admin/mailboxes", a.handleListMailboxes)
			r.Post("/admin/mailboxes", a.handleCreateMailbox)
			r.Post("/admin/mailboxes/{id}", a.handleUpdateMailbox)
			r.Delete("/admin/mailboxes/{id}", a.handleDeleteMailbox)
			r.Get("/admin/aliases", a.handleListAliases)
			r.Post("/admin/aliases", a.handleCreateAlias)
			r.Post("/admin/aliases/{id}", a.handleUpdateAlias)
			r.Delete("/admin/aliases/{id}", a.handleDeleteAlias)
			r.Get("/admin/messages", a.handleAdminMessages)
			r.Get("/admin/messages/{id}", a.handleAdminMessage)
			r.Get("/admin/attachments/{id}", a.handleAdminAttachment)
			r.Get("/admin/settings", a.handleGetSystemSettings)
			r.Post("/admin/settings", a.handleUpdateSystemSettings)
			r.Post("/admin/settings/test-smtp", a.handleTestSMTP)
			r.Get("/admin/mail-templates", a.handleListMailTemplates)
			r.Post("/admin/mail-templates/{key}", a.handleUpdateMailTemplate)
			r.Post("/admin/mail-templates/{key}/reset", a.handleResetMailTemplate)
			r.Get("/admin/domains/{id}/dns-records", a.handleDNSRecords)
			r.Post("/admin/domains/{id}/check-dns", a.handleDNSCheck)
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

func (a *App) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		if user == nil || user.Role != "admin" {
			respondError(w, http.StatusForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
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
	return &u, nil
}
