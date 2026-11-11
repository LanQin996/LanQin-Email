package app

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestBackupEncryptionSettingsEncryptedCRUDAndRotation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	a.cfg.BackupRecipient = ""
	client := backupLoggedInAdmin(t, a)
	var view backupEncryptionView
	if code := client.do("GET", "/api/admin/backups/encryption", nil, &view); code != http.StatusOK || view.Configured || view.PasswordSet || view.Mode != "password" || view.Revision != "0" {
		t.Fatal("initial backup encryption state", code, view)
	}
	const password = "  synthetic backup password  "
	request := map[string]string{"revision": view.Revision, "mode": "password", "password": password, "confirmPassword": password}
	var out map[string]any
	if code := client.do("POST", "/api/admin/backups/encryption", request, &out); code != http.StatusOK {
		t.Fatal("cannot save backup password", code)
	}
	if code := client.do("GET", "/api/admin/backups/encryption", nil, &view); code != http.StatusOK || !view.Configured || !view.PasswordSet || view.Mode != "password" || !passwordDescriptor(view.Recipient) {
		t.Fatal("saved backup encryption state", code, view)
	}
	if strings.Contains(jsonEncode(view), password) || strings.Contains(jsonEncode(out), password) {
		t.Fatal("backup password exposed in API response")
	}
	var record backupEncryptionRecord
	raw, err := a.backupRead(t.Context(), backupEncryptionSetting, &record)
	if err != nil || record.Password != password || strings.Contains(raw, password) || !backupEncryptionGeneration.MatchString(record.Generation) {
		t.Fatal("backup password was not encrypted at rest", err)
	}
	if err := a.saveBackupEncryption(t.Context(), "0", "password", "different synthetic password", "different synthetic password", password); !errors.Is(err, errBackupEncryptionConflict) {
		t.Fatal("stale password configuration overwrote current one", err)
	}
	if err := a.saveBackupEncryption(t.Context(), view.Revision, "password", password, password, strings.TrimSpace(password)); !errors.Is(err, errBackupEncryptionCurrentPassword) {
		t.Fatal("password was trimmed or old password bypassed", err)
	}
	if err := a.saveBackupEncryption(t.Context(), view.Revision, "password", password, password, password); err != nil {
		t.Fatal("same-password update rejected", err)
	}
	same, err := a.backupEncryptionConfiguration(t.Context())
	if err != nil || same.Recipient != view.Recipient {
		t.Fatal("unchanged password invalidated schedule binding", err)
	}
	const nextPassword = "new synthetic backup password"
	if err := a.saveBackupEncryption(t.Context(), same.Revision, "password", nextPassword, nextPassword, password); err != nil {
		t.Fatal(err)
	}
	rotated, err := a.backupEncryptionConfiguration(t.Context())
	if err != nil || rotated.Recipient == same.Recipient || rotated.Password != nextPassword {
		t.Fatal("password rotation did not change capture binding", err)
	}
	if err := a.saveBackupEncryption(t.Context(), rotated.Revision, "key", "", "", nextPassword); !errors.Is(err, errBackupEncryptionInvalid) {
		t.Fatal("key mode accepted without configured key", err)
	}
	identity, _ := age.GenerateX25519Identity()
	a.cfg.BackupRecipient = identity.Recipient().String()
	if err := a.saveBackupEncryption(t.Context(), rotated.Revision, "key", "", "", nextPassword); !errors.Is(err, errBackupEncryptionInvalid) {
		t.Fatal("environment key enabled forbidden key mode", err)
	}
	keyMode, err := a.backupEncryptionConfiguration(t.Context())
	if err != nil || keyMode.Mode != "password" || keyMode.Password != nextPassword || keyMode.Recipient != rotated.Recipient {
		t.Fatal("rejected key mode changed password configuration", err)
	}
}

func TestBackupEncryptionSettingsValidationAndAuthorization(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = ""
	admin := backupLoggedInAdmin(t, a)
	var out map[string]any
	for _, method := range []string{"GET", "POST"} {
		if code := admin.do(method, "/api/admin/backups/encryption", map[string]string{"revision": "0", "mode": "password", "password": "synthetic password", "confirmPassword": "synthetic password"}, &out); code != http.StatusServiceUnavailable {
			t.Fatal("missing server key enabled password storage", method, code)
		}
	}
	a.cfg.BackupSecretKey = backupTestKey
	for _, password := range []string{"short", strings.Repeat("x", 1025), strings.Repeat(" ", 20), "synthetic\npassword", "bad\x00password", string([]byte{0xff, 0xfe})} {
		if err := a.saveBackupEncryption(t.Context(), "0", "password", password, password, ""); !errors.Is(err, errBackupEncryptionInvalid) {
			t.Fatal("invalid new backup password accepted", err)
		}
	}
	if err := a.saveBackupEncryption(t.Context(), "0", "password", "synthetic password", "mismatched synthetic password", ""); !errors.Is(err, errBackupEncryptionInvalid) {
		t.Fatal("password confirmation bypassed", err)
	}
	if code := admin.do("POST", "/api/admin/backups/encryption", map[string]string{"revision": "0", "mode": "password", "password": "synthetic password", "confirmPassword": "synthetic password", "unexpected": "private-do-not-echo"}, &out); code != http.StatusBadRequest || strings.Contains(jsonEncode(out), "private-do-not-echo") {
		t.Fatal("unknown field accepted or echoed", code)
	}
	group := createAssignableGroup(t, admin, "synthetic encryption operator", []string{PermissionSettingsView, PermissionSettingsUpdate})
	var user AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{"email": "encryption-ops@example.test", "displayName": "Synthetic operator", "role": "user", "password": "SyntheticEncrypt123!", "permissionGroupIds": []string{group.ID}}, &user); code != http.StatusCreated {
		t.Fatal(code)
	}
	operator := &testClient{t: t, server: admin.server}
	if code := operator.do("POST", "/api/auth/login", map[string]string{"email": user.Email, "password": "SyntheticEncrypt123!"}, &out); code != http.StatusOK {
		t.Fatal(code)
	}
	token := createTestAPIToken(t, admin, "synthetic encryption token")
	for _, denied := range []*testClient{operator, {t: t, server: admin.server}, {t: t, server: admin.server, bearer: token}} {
		for _, method := range []string{"GET", "POST"} {
			code := denied.do(method, "/api/admin/backups/encryption", map[string]string{"revision": "0", "mode": "password", "password": "synthetic password", "confirmPassword": "synthetic password"}, &out)
			if code != http.StatusUnauthorized && code != http.StatusForbidden {
				t.Fatal("non-super-admin session reached backup password settings", code)
			}
		}
	}
}

func assertBackupEncryptionDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	oldKey := a.cfg.BackupSecretKey
	a.cfg.BackupSecretKey = backupTestKey
	t.Cleanup(func() {
		if _, err := a.db.Exec(`DELETE FROM system_settings WHERE `+keyColumnSQL(a.cfg.DBDriver)+`=?`, backupEncryptionSetting); err != nil {
			t.Error(err)
		}
		a.cfg.BackupSecretKey = oldKey
	})
	const password = "synthetic database contract password"
	if err := a.saveBackupEncryption(t.Context(), "0", "password", password, password, ""); err != nil {
		t.Fatal("password setting contract save", err)
	}
	current, err := a.backupEncryptionConfiguration(t.Context())
	if err != nil || current.Password != password || !passwordDescriptor(current.Recipient) {
		t.Fatal("password setting contract read", err)
	}
	if err := a.saveBackupEncryption(t.Context(), "0", "password", password, password, password); !errors.Is(err, errBackupEncryptionConflict) {
		t.Fatal("password setting contract CAS did not reject stale revision", err)
	}
	if err := a.saveBackupEncryption(t.Context(), current.Revision, "password", password+" rotated", password+" rotated", password); err != nil {
		t.Fatal("password setting contract rotation", err)
	}
}

func TestBackupEncryptionSQLiteContract(t *testing.T) {
	assertBackupEncryptionDatabaseContract(t, newTestApp(t))
}

func TestBackupEncryptionCharacterLimitsAndLegacyConfiguration(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	client := backupLoggedInAdmin(t, a)
	for _, password := range []string{"abcdefgh", "備份密碼測試八字", strings.Repeat("🔐", 8), strings.Repeat("x", 128), strings.Repeat("🔐", 128), "  abcdef  "} {
		current := backupTestEncryption(t, a)
		var out map[string]any
		request := map[string]string{"revision": current.Revision, "mode": "password", "password": password, "confirmPassword": password, "currentPassword": current.Password}
		if code := client.do("POST", "/api/admin/backups/encryption", request, &out); code != http.StatusOK {
			t.Fatal("valid character-count password rejected", code)
		}
		if saved := backupTestEncryption(t, a); saved.Password != password {
			t.Fatal("password was trimmed or normalized")
		}
	}
	for _, password := range []string{"abcdefg", strings.Repeat("密", 7), strings.Repeat("🔐", 7), strings.Repeat("x", 129), strings.Repeat("密", 129), strings.Repeat("🔐", 129), strings.Repeat("\u0085", 8), "abcdefgh\x00", "abcdef\u007fg"} {
		current := backupTestEncryption(t, a)
		if err := a.saveBackupEncryption(t.Context(), current.Revision, "password", password, password, current.Password); !errors.Is(err, errBackupEncryptionInvalid) {
			t.Fatal("invalid character-count password accepted", err)
		}
	}
	// Prior versions allowed four Chinese characters or more than 128 ASCII
	// characters. Those saved settings must remain readable and rotatable.
	for _, historical := range []string{"備份密碼", strings.Repeat("x", 1024)} {
		var old backupEncryptionRecord
		raw, err := a.backupRead(t.Context(), backupEncryptionSetting, &old)
		if err != nil {
			t.Fatal(err)
		}
		old.Password = historical
		if err := a.backupCAS(t.Context(), a.db, backupEncryptionSetting, raw, old); err != nil {
			t.Fatal(err)
		}
		current := backupTestEncryption(t, a)
		if current.Password != historical || !validStoredBackupPassword(historical) || validBackupPassword(historical) {
			t.Fatal("historical setting was invalidated by creation policy")
		}
		if err := a.saveBackupEncryption(t.Context(), current.Revision, "password", "abcdefgh", "abcdefgh", historical); err != nil {
			t.Fatal("historical password could not be rotated", err)
		}
	}
}
