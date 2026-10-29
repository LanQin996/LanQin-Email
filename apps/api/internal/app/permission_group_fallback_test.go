package app

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestExplicitPermissionGroupsReplaceRegularFallback(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var body map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &body); code != http.StatusOK {
		t.Fatalf("admin login: %d", code)
	}
	limits := minimalLimits()
	var emptyGroup PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups", map[string]any{
		"name": "No functions", "permissions": []string{}, "limits": limits,
	}, &emptyGroup); code != http.StatusCreated {
		t.Fatalf("create empty group: %d", code)
	}
	reader := createAssignableGroup(t, admin, "Mail reader", []string{PermissionMailAccess, PermissionMailRead})
	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "fallback@example.test", "displayName": "Fallback", "role": "user", "password": "Password123!",
	}, &created); code != http.StatusCreated {
		t.Fatalf("create user: %d", code)
	}
	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": "fallback@example.test", "password": "Password123!"}, &body); code != http.StatusOK {
		t.Fatalf("user login: %d", code)
	}
	assign := func(ids []string) {
		t.Helper()
		if code := admin.do("POST", "/api/admin/users/"+created.ID, map[string]any{
			"displayName": "Fallback", "role": "user", "permissionGroupIds": ids,
		}, &body); code != http.StatusOK {
			t.Fatalf("assign groups: %d", code)
		}
	}
	me := func() User {
		t.Helper()
		var response struct {
			User User `json:"user"`
		}
		if code := user.do("GET", "/api/me", nil, &response); code != http.StatusOK {
			t.Fatalf("read current session: %d", code)
		}
		return response.User
	}
	initial := me()
	if !userHasPermission(&initial, PermissionMailAccess) || !reflect.DeepEqual(initial.PermissionGroupIDs, []string{PermissionGroupRegular}) {
		t.Fatalf("unassigned user did not receive regular fallback: %+v", initial)
	}
	var token struct {
		Token string `json:"token"`
	}
	if code := user.do("POST", "/api/me/api-tokens", map[string]any{
		"name": "Synthetic revocation check", "scopes": []string{"messages:read"},
	}, &token); code != http.StatusCreated {
		t.Fatalf("create token: %d", code)
	}
	tokenStatus := func() int {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/open/v1/send", nil)
		req.Header.Set("Authorization", "Bearer "+token.Token)
		recorder := httptest.NewRecorder()
		a.Router().ServeHTTP(recorder, req)
		return recorder.Code
	}
	if code := tokenStatus(); code != http.StatusOK {
		t.Fatalf("initial token access = %d", code)
	}
	assign([]string{emptyGroup.ID})
	restricted := me() // Reuse the existing cookie: revocation must take effect immediately.
	if len(restricted.Permissions) != 0 || !reflect.DeepEqual(restricted.PermissionGroupIDs, []string{emptyGroup.ID}) || restricted.Limits != limits {
		t.Fatalf("empty group inherited default authorization: %+v", restricted)
	}
	for _, path := range []string{
		"/api/mail/mailboxes", "/api/mail/messages", "/api/mail/folders?mailboxId=shared-test",
		"/api/mail/external-accounts", "/api/mail/external-accounts/test/messages",
		"/api/mail/attachments/test", "/api/mail/external-accounts/test/attachments/test/1",
		"/api/mail/messages/test/thread", "/api/admin/users",
	} {
		if code := user.do("GET", path, nil, &body); code != http.StatusForbidden {
			t.Errorf("zero-permission GET %s = %d, want 403", path, code)
		}
	}
	if code := user.do("POST", "/api/mail/send", map[string]any{}, &body); code != http.StatusForbidden {
		t.Fatalf("zero-permission send = %d, want 403", code)
	}
	if code := tokenStatus(); code != http.StatusForbidden {
		t.Fatalf("existing token retained revoked permissions: %d", code)
	}
	// Delegation checks and invite registration must use the same rule, inside
	// the caller's transaction rather than trying to acquire another connection.
	tx, err := a.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	permissions, err := a.effectivePermissionsForUserGroups(t.Context(), tx, []string{emptyGroup.ID})
	if err != nil || len(permissions) != 0 {
		t.Fatalf("transaction permissions = %v, error = %v", permissions, err)
	}
	inviteLimits, err := a.permissionLimitsForGroupsTx(t.Context(), tx, []string{emptyGroup.ID})
	if err != nil || inviteLimits != limits {
		t.Fatalf("invite limits = %+v, error = %v", inviteLimits, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assign([]string{emptyGroup.ID, reader.ID})
	combined := me()
	if !userHasPermission(&combined, PermissionMailRead) || userHasPermission(&combined, PermissionMailSend) || containsString(combined.PermissionGroupIDs, PermissionGroupRegular) {
		t.Fatalf("explicit group union incorrectly includes defaults: %+v", combined)
	}
	assign([]string{})
	restored := me()
	if !userHasPermission(&restored, PermissionMailSend) || !reflect.DeepEqual(restored.PermissionGroupIDs, []string{PermissionGroupRegular}) || restored.Limits != initial.Limits {
		t.Fatalf("clearing custom groups did not restore defaults: %+v", restored)
	}
	if code := user.do("GET", "/api/mail/mailboxes", nil, &body); code != http.StatusOK {
		t.Fatalf("restored mailbox access = %d", code)
	}
	if code := tokenStatus(); code != http.StatusOK {
		t.Fatalf("restored token access = %d", code)
	}
}

func assertExplicitPermissionGroupDatabaseContract(t *testing.T, a *App) {
	t.Helper()
	ctx := t.Context()
	userID, groupID := newID("usr"), newID("pg")
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, userID+"@example.test", "Contract fallback", "user", "unused", 0, now, now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := a.db.Exec(`DELETE FROM users WHERE id=?`, userID); err != nil {
			t.Error(err)
		}
		if _, err := a.db.Exec(`DELETE FROM permission_groups WHERE id=?`, groupID); err != nil {
			t.Error(err)
		}
	})
	limits := minimalLimits()
	if _, err := a.db.ExecContext(ctx, `INSERT INTO permission_groups(id,name,description,permissions_json,limits_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		groupID, groupID, "Contract fallback", "[]", encodePermissionLimits(limits), now, now); err != nil {
		t.Fatal(err)
	}
	initial, err := a.authorizationForUser(ctx, userID, "user")
	if err != nil || !reflect.DeepEqual(initial.GroupIDs, []string{PermissionGroupRegular}) {
		t.Fatalf("default authorization = %+v, error = %v", initial, err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO user_permission_groups(user_id,group_id,created_at) VALUES(?,?,?)`, userID, groupID, now); err != nil {
		t.Fatal(err)
	}
	auth, err := a.authorizationForUser(ctx, userID, "user")
	if err != nil || len(auth.Permissions) != 0 || auth.Limits != limits || !reflect.DeepEqual(auth.GroupIDs, []string{groupID}) {
		t.Fatalf("explicit authorization = %+v, error = %v", auth, err)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	actual, err := a.permissionLimitsForGroupsTx(ctx, tx, []string{groupID})
	if err != nil || actual != limits {
		t.Fatalf("transaction limits = %+v, error = %v", actual, err)
	}
}

func TestExplicitPermissionGroupSQLiteContract(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	assertExplicitPermissionGroupDatabaseContract(t, a)
}
