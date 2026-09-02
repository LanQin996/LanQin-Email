package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// createAssignableGroup makes a non-system permission group that invites and the
// Linux.do setting are allowed to bind.
func createAssignableGroup(t *testing.T, admin *testClient, name string, permissions []string) PermissionGroup {
	t.Helper()
	var group PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups", map[string]any{
		"name": name, "description": "", "permissions": permissions,
	}, &group); code != http.StatusCreated {
		t.Fatalf("create group %q code=%d", name, code)
	}
	return group
}

func enableInviteRegistration(t *testing.T, admin *testClient) {
	t.Helper()
	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := systemSettingsPayload(settings)
	update["openRegistration"] = false
	update["inviteRegistrationEnabled"] = true
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK {
		t.Fatalf("enable invite registration code=%d", code)
	}
}

func TestInviteBindsPermissionGroupOnRegistration(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	createTestDomain(t, admin, "invite.test")
	enableInviteRegistration(t, admin)

	group := createAssignableGroup(t, admin, "邀请码组一", []string{PermissionMailStats})

	var invite RegistrationInvite
	if code := admin.do("POST", "/api/admin/registration-invites", map[string]any{
		"maxUses": 1, "permissionGroupIds": []string{group.ID},
	}, &invite); code != http.StatusCreated {
		t.Fatalf("create invite code=%d invite=%+v", code, invite)
	}
	if len(invite.PermissionGroupIDs) != 1 || invite.PermissionGroupIDs[0] != group.ID {
		t.Fatalf("invite did not persist the bound group: %+v", invite.PermissionGroupIDs)
	}

	user := &testClient{t: t, server: ts}
	var registered map[string]any
	if code := user.do("POST", "/api/auth/register", map[string]any{
		"email": "invited@invite.test", "displayName": "Invited",
		"password": "Password123!", "inviteCode": invite.Code,
	}, &registered); code != http.StatusCreated {
		t.Fatalf("register with invite code=%d body=%v", code, registered)
	}

	var me struct {
		User User `json:"user"`
	}
	if code := user.do("GET", "/api/me", nil, &me); code != http.StatusOK {
		t.Fatalf("me code=%d", code)
	}
	found := false
	for _, id := range me.User.PermissionGroupIDs {
		if id == group.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered user is not in the invite's group: %+v", me.User.PermissionGroupIDs)
	}
	if !userHasPermission(&me.User, PermissionMailStats) {
		t.Error("registered user did not inherit the group's permission")
	}
}

// TestInviteRejectsSuperAdminGroup pins the rule that an invite can never hand out
// super administrator rights, which would otherwise turn a leaked code into a
// full takeover.
func TestInviteRejectsSuperAdminGroup(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	for _, groupID := range []string{PermissionGroupSuperAdmin, PermissionGroupRegular, "pg_does_not_exist"} {
		var body map[string]any
		if code := admin.do("POST", "/api/admin/registration-invites", map[string]any{
			"maxUses": 1, "permissionGroupIds": []string{groupID},
		}, &body); code != http.StatusBadRequest {
			t.Errorf("binding %q returned code=%d, want 400", groupID, code)
		}
	}
}

// TestInviteEndpointsRequireSuperAdmin covers the deliberate permission tightening:
// binding groups makes invites an authorization tool, so admin.settings.update is
// no longer sufficient.
func TestInviteEndpointsRequireSuperAdmin(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	settingsGroup := createAssignableGroup(t, admin, "仅系统设置", []string{PermissionSettingsView, PermissionSettingsUpdate})

	var operator AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "ops@example.net", "displayName": "Ops", "role": "user",
		"password": "Password123!", "disabled": false,
		"permissionGroupIds": []string{settingsGroup.ID},
	}, &operator); code != http.StatusCreated {
		t.Fatalf("create operator code=%d", code)
	}
	ops := &testClient{t: t, server: ts}
	if code := ops.do("POST", "/api/auth/login", map[string]string{"email": "ops@example.net", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("operator login code=%d", code)
	}

	var body map[string]any
	if code := ops.do("GET", "/api/admin/registration-invites", nil, &body); code != http.StatusForbidden {
		t.Errorf("list invites as non-super-admin code=%d, want 403", code)
	}
	if code := ops.do("POST", "/api/admin/registration-invites", map[string]any{"maxUses": 1}, &body); code != http.StatusForbidden {
		t.Errorf("create invite as non-super-admin code=%d, want 403", code)
	}
	if code := ops.do("DELETE", "/api/admin/registration-invites/inv_missing", nil, &body); code != http.StatusForbidden {
		t.Errorf("delete invite as non-super-admin code=%d, want 403", code)
	}
}

func TestInviteFailsWhenBoundGroupDeleted(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	createTestDomain(t, admin, "stale.test")
	enableInviteRegistration(t, admin)
	group := createAssignableGroup(t, admin, "将被删除的组", []string{PermissionMailStats})

	var invite RegistrationInvite
	if code := admin.do("POST", "/api/admin/registration-invites", map[string]any{
		"maxUses": 1, "permissionGroupIds": []string{group.ID},
	}, &invite); code != http.StatusCreated {
		t.Fatalf("create invite code=%d", code)
	}
	var ok map[string]any
	if code := admin.do("DELETE", "/api/admin/permission-groups/"+group.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete group code=%d body=%v", code, ok)
	}

	user := &testClient{t: t, server: ts}
	var body map[string]any
	if code := user.do("POST", "/api/auth/register", map[string]any{
		"email": "stale@stale.test", "displayName": "Stale",
		"password": "Password123!", "inviteCode": invite.Code,
	}, &body); code != http.StatusConflict {
		t.Fatalf("register with stale group code=%d want 409 body=%v", code, body)
	}

	// The whole registration must roll back, including the use counter.
	var used int
	if err := a.db.QueryRow(`SELECT used_count FROM registration_invites WHERE id=?`, invite.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Errorf("used_count=%d, want 0 (failed registration must not spend the code)", used)
	}
	var users int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, "stale@stale.test").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("user count=%d, want 0 (failed registration must not create the user)", users)
	}
}

func TestLinuxDoRegistrationGroupSettingRoundTrips(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	group := createAssignableGroup(t, admin, "Linux.do 用户组", []string{PermissionMailStats})

	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := systemSettingsPayload(settings)
	update["linuxDoRegistrationGroupIds"] = []string{group.ID}
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK {
		t.Fatalf("save settings code=%d", code)
	}
	if len(settings.LinuxDoRegistrationGroupIDs) != 1 || settings.LinuxDoRegistrationGroupIDs[0] != group.ID {
		t.Fatalf("setting did not round-trip: %+v", settings.LinuxDoRegistrationGroupIDs)
	}

	if got := a.linuxDoRegistrationGroupIDs(t.Context(), nil); len(got) != 1 || got[0] != group.ID {
		t.Fatalf("resolver returned %+v, want [%s]", got, group.ID)
	}

	// A deleted group must be skipped rather than breaking SSO sign-up entirely.
	var ok map[string]any
	if code := admin.do("DELETE", "/api/admin/permission-groups/"+group.ID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete group code=%d", code)
	}
	if got := a.linuxDoRegistrationGroupIDs(t.Context(), nil); len(got) != 0 {
		t.Fatalf("resolver returned %+v for a deleted group, want empty", got)
	}
}

func TestMailboxDailyRateLimitDoesNotAccumulate(t *testing.T) {
	a, admin, user, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimits(t, admin, 10, 1)

	if code := applyMailbox(t, user, domain.ID, "day-one"); code != http.StatusCreated {
		t.Fatalf("first apply code=%d", code)
	}
	if code := applyMailbox(t, user, domain.ID, "day-one-again"); code != http.StatusTooManyRequests {
		t.Fatalf("second apply on the same day code=%d, want 429", code)
	}

	// Move the recorded event outside the rolling window: the allowance returns,
	// but only one, never two.
	past := a.now().UTC().Add(-25 * time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE mailbox_creation_events SET created_at=? WHERE user_id=?`, past, created.ID); err != nil {
		t.Fatal(err)
	}
	if code := applyMailbox(t, user, domain.ID, "day-two"); code != http.StatusCreated {
		t.Fatalf("apply after the window code=%d, want 201", code)
	}
	if code := applyMailbox(t, user, domain.ID, "day-two-again"); code != http.StatusTooManyRequests {
		t.Fatalf("allowance accumulated across days: code=%d, want 429", code)
	}
}

// TestMailboxDailyRateSurvivesDeletion is the reason mailbox_creation_events has no
// foreign key on mailbox_id: if deleting the mailbox erased the event, create then
// delete then create would reset the daily allowance.
func TestMailboxDailyRateSurvivesDeletion(t *testing.T) {
	a, admin, user, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimits(t, admin, 10, 1)

	if code := applyMailbox(t, user, domain.ID, "gone"); code != http.StatusCreated {
		t.Fatalf("apply code=%d", code)
	}
	var mailboxID string
	if err := a.db.QueryRow(`SELECT id FROM mailboxes WHERE user_id=? AND local_part=?`, created.ID, "gone").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	var ok map[string]any
	if code := admin.do("DELETE", "/api/admin/mailboxes/"+mailboxID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete mailbox code=%d", code)
	}

	var events int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM mailbox_creation_events WHERE user_id=?`, created.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("creation events=%d, want 1 (deleting a mailbox must not erase the log)", events)
	}
	if code := applyMailbox(t, user, domain.ID, "retry"); code != http.StatusTooManyRequests {
		t.Fatalf("create-delete-create bypassed the daily limit: code=%d, want 429", code)
	}
}

func TestMailboxDailyRateZeroMeansUnlimited(t *testing.T) {
	_, admin, user, _, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimits(t, admin, 0, 0)

	for _, local := range []string{"a", "b", "c"} {
		if code := applyMailbox(t, user, domain.ID, local); code != http.StatusCreated {
			t.Fatalf("apply %q code=%d, want 201 (0 must mean unlimited)", local, code)
		}
	}
}
