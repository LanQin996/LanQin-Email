package app

import (
	"net/http"
	"testing"
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
		override bool
	}{
		{name: "credentials-omitted"},
		{name: "credentials-override", override: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]any{
				"domainId":    domain.ID,
				"localPart":   tt.name,
				"displayName": "Synthetic mailbox",
				"userId":      owner.ID,
			}
			if tt.override {
				payload["password"] = "DifferentPassword456!"
				payload["role"] = "admin"
				payload["ownerEmail"] = "ignored@example.test"
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

func TestAdminCreateMailboxRequiresExistingOwner(t *testing.T) {
	a, admin, _, owner, domain := setupQuotaTest(t)
	var originalUsers, originalMailboxes int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&originalUsers); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM mailboxes`).Scan(&originalMailboxes); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name       string
		userID     string
		ownerEmail string
	}{
		{name: "missing-user"},
		{name: "blank-user", userID: "  "},
		{name: "new-owner-email", ownerEmail: "new-owner@example.test"},
		{name: "existing-owner-email", ownerEmail: owner.Email},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
				"domainId": domain.ID, "localPart": tt.name, "userId": tt.userID,
				"ownerEmail": tt.ownerEmail, "password": "Password123!", "role": "admin",
			}, nil); code != http.StatusBadRequest {
				t.Fatalf("missing owner ID code=%d", code)
			}
		})
	}
	var users, mailboxes int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM mailboxes`).Scan(&mailboxes); err != nil {
		t.Fatal(err)
	}
	if users != originalUsers || mailboxes != originalMailboxes {
		t.Fatal("rejected mailbox creation changed users or mailboxes")
	}
	mailbox := createTestMailbox(t, admin, domain.ID, "attached", "Attached", "", map[string]any{"userId": owner.ID})
	if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
		"domainId": domain.ID, "localPart": mailbox.LocalPart,
		"userId": owner.ID,
	}, nil); code != http.StatusBadRequest {
		t.Fatalf("duplicate mailbox code=%d", code)
	}
	var count int
	if err := a.db.QueryRow(`SELECT mailboxes_created_total FROM users WHERE id=?`, owner.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("failed mailbox creation changed the owner's mailbox counter")
	}
}
