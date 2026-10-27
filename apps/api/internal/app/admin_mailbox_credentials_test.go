package app

import (
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminCreateMailboxInheritsExistingOwnerCredentials(t *testing.T) {
	a, admin, user, owner, domain := setupQuotaTest(t)
	var originalHash, originalRole string
	if err := a.db.QueryRow(`SELECT password_hash,role FROM users WHERE id=?`, owner.ID).Scan(&originalHash, &originalRole); err != nil {
		t.Fatal(err)
	}
	primary := createTestMailbox(t, admin, domain.ID, "primary", "Primary", "", map[string]any{
		"userId": owner.ID,
	})

	for _, tt := range []struct {
		name     string
		byID     bool
		override bool
	}{
		{name: "id-omitted", byID: true},
		{name: "id-override", byID: true, override: true},
		{name: "email-omitted"},
		{name: "email-override", override: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]any{
				"domainId":    domain.ID,
				"localPart":   tt.name,
				"displayName": "Synthetic mailbox",
			}
			if tt.byID {
				payload["userId"] = owner.ID
				// An explicit user ID must take precedence over ownerEmail.
				payload["ownerEmail"] = "ignored@example.test"
			} else {
				payload["ownerEmail"] = owner.Email
			}
			if tt.override {
				payload["password"] = "DifferentPassword456!"
				payload["role"] = "admin"
			}
			var mailbox Mailbox
			if code := admin.do("POST", "/api/admin/mailboxes", payload, &mailbox); code != http.StatusCreated {
				t.Fatalf("create mailbox code=%d", code)
			}
			if mailbox.UserID != owner.ID {
				t.Fatal("mailbox has the wrong owner")
			}
			var mailboxHash string
			if err := a.db.QueryRow(`SELECT password_hash FROM mailboxes WHERE id=?`, mailbox.ID).Scan(&mailboxHash); err != nil {
				t.Fatal(err)
			}
			if mailboxHash != originalHash {
				t.Fatal("mailbox did not inherit the existing login password hash")
			}
			if _, _, err := a.authenticateSubmission(t.Context(), mailbox.Address, "Password123!"); err != nil {
				t.Fatalf("submission with owner password: %v", err)
			}
			if _, _, err := a.authenticateSubmission(t.Context(), mailbox.Address, "DifferentPassword456!"); err == nil {
				t.Fatal("submitted password override must not take effect")
			}
			var hash, role string
			if err := a.db.QueryRow(`SELECT password_hash,role FROM users WHERE id=?`, owner.ID).Scan(&hash, &role); err != nil {
				t.Fatal(err)
			}
			if hash != originalHash || role != originalRole {
				t.Fatal("existing owner's credentials or role were changed")
			}
		})
	}
	var primaryHash string
	if err := a.db.QueryRow(`SELECT password_hash FROM mailboxes WHERE id=?`, primary.ID).Scan(&primaryHash); err != nil {
		t.Fatal(err)
	}
	if primaryHash != originalHash {
		t.Fatal("existing mailbox password was changed")
	}
	var login map[string]any
	if code := user.do("POST", "/api/auth/login", map[string]string{
		"email": owner.Email, "password": "Password123!",
	}, &login); code != http.StatusOK {
		t.Fatalf("original login no longer works: code=%d", code)
	}

	if _, err := a.db.Exec(`UPDATE users SET disabled=1 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		id     string
		status int
	}{
		{id: owner.ID, status: http.StatusBadRequest},
		{id: "usr_missing", status: http.StatusNotFound},
	} {
		if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
			"domainId": domain.ID, "localPart": "rejected", "userId": tt.id,
		}, nil); code != tt.status {
			t.Fatalf("invalid owner code=%d want=%d", code, tt.status)
		}
	}
}

func TestAdminCreateMailboxNewOwnerValidationAndRollback(t *testing.T) {
	a, admin, _, _, domain := setupQuotaTest(t)
	for _, tt := range []struct {
		name     string
		password string
		role     string
	}{
		{name: "missing-password"},
		{name: "short-password", password: "short"},
		{name: "invalid-role", password: "Password123!", role: "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
				"domainId": domain.ID, "localPart": tt.name, "password": tt.password, "role": tt.role,
			}, nil); code != http.StatusBadRequest {
				t.Fatalf("invalid new user code=%d", code)
			}
		})
	}
	mailbox := createTestMailbox(t, admin, domain.ID, "new-owner", "New Owner", "Password123!", map[string]any{
		"role": "admin",
	})
	var hash, role string
	if err := a.db.QueryRow(`SELECT password_hash,role FROM users WHERE id=?`, mailbox.UserID).Scan(&hash, &role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("Password123!")) != nil {
		t.Fatal("new user's requested password or role was not applied")
	}
	if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
		"domainId": domain.ID, "localPart": mailbox.LocalPart,
		"ownerEmail": "rollback@example.test", "password": "Password123!",
	}, nil); code != http.StatusBadRequest {
		t.Fatalf("duplicate mailbox code=%d", code)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, "rollback@example.test").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed mailbox creation left an orphan owner")
	}
}
