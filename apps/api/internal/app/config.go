package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Addr                    string
	DBPath                  string
	DataDir                 string
	CookieName              string
	SessionTTLHours         int
	AdminEmail              string
	AdminPassword           string
	PublicHostname          string
	PublicBaseURL           string
	SMTPHost                string
	SMTPPort                string
	SMTPUsername            string
	SMTPPassword            string
	SMTPRequireTLS          bool
	SubmissionAddr          string
	SubmissionTLSAddr       string
	SubmissionMaxMessageMB  int
	TLSCertFile             string
	TLSKeyFile              string
	MaildirRoot             string
	MaildirScanSeconds      int
	AllowInsecureHTTP       bool
	OpenRegistration        bool
	TwoFactorEnabled        bool
	TurnstileEnabled        bool
	TurnstileSiteKey        string
	TurnstileSecretKey      string
	CatchAllEnabled         bool
	MailAutoRefresh         bool
	MailRefreshSeconds      int
	UserMailboxApplyEnabled bool
	UserMailboxDomainIDs    string
	ReservedMailboxPrefixes string
}

func LoadConfig() Config {
	dataDir := getenv("LANQIN_DATA_DIR", "./data")
	return Config{
		Addr:                    getenv("LANQIN_ADDR", ":8080"),
		DBPath:                  getenv("LANQIN_DB_PATH", filepath.Join(dataDir, "lanqin.db")),
		DataDir:                 dataDir,
		CookieName:              getenv("LANQIN_COOKIE_NAME", "lanqin_session"),
		SessionTTLHours:         getenvInt("LANQIN_SESSION_TTL_HOURS", 24*7),
		AdminEmail:              strings.ToLower(getenv("LANQIN_ADMIN_EMAIL", "admin@lanqin.local")),
		AdminPassword:           getenv("LANQIN_ADMIN_PASSWORD", ""),
		PublicHostname:          getenv("LANQIN_PUBLIC_HOSTNAME", "mail.lanqin.local"),
		PublicBaseURL:           getenv("LANQIN_PUBLIC_BASE_URL", "http://localhost:5173"),
		SMTPHost:                getenv("LANQIN_SMTP_HOST", ""),
		SMTPPort:                getenv("LANQIN_SMTP_PORT", "25"),
		SMTPUsername:            getenv("LANQIN_SMTP_USERNAME", ""),
		SMTPPassword:            getenv("LANQIN_SMTP_PASSWORD", ""),
		SMTPRequireTLS:          getenvBool("LANQIN_SMTP_REQUIRE_TLS", false),
		SubmissionAddr:          getenv("LANQIN_SUBMISSION_ADDR", ""),
		SubmissionTLSAddr:       getenv("LANQIN_SUBMISSION_TLS_ADDR", ""),
		SubmissionMaxMessageMB:  getenvInt("LANQIN_SUBMISSION_MAX_MESSAGE_MB", 35),
		TLSCertFile:             getenv("LANQIN_TLS_CERT_FILE", ""),
		TLSKeyFile:              getenv("LANQIN_TLS_KEY_FILE", ""),
		MaildirRoot:             getenv("LANQIN_MAILDIR_ROOT", ""),
		MaildirScanSeconds:      getenvInt("LANQIN_MAILDIR_SCAN_SECONDS", 30),
		AllowInsecureHTTP:       getenvBool("LANQIN_ALLOW_INSECURE_HTTP", true),
		OpenRegistration:        getenvBool("LANQIN_OPEN_REGISTRATION", false),
		TwoFactorEnabled:        getenvBool("LANQIN_TWO_FACTOR_ENABLED", false),
		TurnstileEnabled:        getenvBool("LANQIN_TURNSTILE_ENABLED", false),
		TurnstileSiteKey:        getenv("LANQIN_TURNSTILE_SITE_KEY", ""),
		TurnstileSecretKey:      getenv("LANQIN_TURNSTILE_SECRET_KEY", ""),
		CatchAllEnabled:         getenvBool("LANQIN_CATCH_ALL_ENABLED", false),
		MailAutoRefresh:         getenvBool("LANQIN_MAIL_AUTO_REFRESH", true),
		MailRefreshSeconds:      getenvInt("LANQIN_MAIL_REFRESH_SECONDS", 30),
		UserMailboxApplyEnabled: getenvBool("LANQIN_USER_MAILBOX_APPLY_ENABLED", false),
		UserMailboxDomainIDs:    getenv("LANQIN_USER_MAILBOX_DOMAIN_IDS", ""),
		ReservedMailboxPrefixes: getenv("LANQIN_RESERVED_MAILBOX_PREFIXES", "admin,postmaster,abuse,hostmaster,webmaster,root,security,noreply,no-reply,mailer-daemon"),
	}
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func getenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
