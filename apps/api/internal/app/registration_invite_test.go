package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInvitationRegistrationLifecycle(t *testing.T) {
	a := newTestApp(t)
	a.cfg.InviteRegistrationEnabled = true
	a.cfg.ReservedMailboxPrefixes = "admin,root"
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}

	var created RegistrationInvite
	if code := admin.do("POST", "/api/admin/registration-invites", map[string]any{"code": "team-2026", "maxUses": 2}, &created); code != http.StatusCreated {
		t.Fatalf("create invite code=%d item=%+v", code, created)
	}
	if created.Code != "TEAM-2026" || created.MaxUses != 2 || created.RemainingUses != 2 {
		t.Fatalf("unexpected created invite: %+v", created)
	}
	var list struct {
		Items []RegistrationInvite `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/registration-invites", nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Code != created.Code {
		t.Fatalf("list invite code=%d items=%+v", code, list.Items)
	}

	register := func(client *testClient, email, invite string) (int, map[string]any) {
		var body map[string]any
		code := client.do("POST", "/api/auth/register", map[string]string{
			"email": email, "displayName": email, "password": "Password123!", "inviteCode": invite,
		}, &body)
		return code, body
	}
	if code, body := register(&testClient{t: t, server: ts}, "invalid@example.test", "WRONG-CODE"); code != http.StatusForbidden {
		t.Fatalf("invalid invite code=%d body=%v", code, body)
	}
	if code, body := register(&testClient{t: t, server: ts}, "admin@lanqin.local", "WRONG-CODE"); code != http.StatusForbidden {
		t.Fatalf("invalid invite must not reveal existing email code=%d body=%v", code, body)
	}
	var reservedBody map[string]any
	if code := (&testClient{t: t, server: ts}).do("POST", "/api/auth/register", map[string]any{
		"email": "reserved@example.test", "displayName": "Reserved", "password": "Password123!", "inviteCode": "TEAM-2026",
		"domainId": mustDefaultDomainID(t, a), "localPart": "root",
	}, &reservedBody); code != http.StatusForbidden {
		t.Fatalf("reserved prefix code=%d body=%v", code, reservedBody)
	}
	var reservedUserCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, "reserved@example.test").Scan(&reservedUserCount); err != nil || reservedUserCount != 0 {
		t.Fatalf("reserved prefix created user count=%d err=%v", reservedUserCount, err)
	}
	if code, body := register(&testClient{t: t, server: ts}, "first@example.test", "team-2026"); code != http.StatusCreated {
		t.Fatalf("first invite registration code=%d body=%v", code, body)
	}
	if code, body := register(&testClient{t: t, server: ts}, "first@example.test", "TEAM-2026"); code != http.StatusConflict {
		t.Fatalf("duplicate registration code=%d body=%v", code, body)
	}
	if code := admin.do("GET", "/api/admin/registration-invites", nil, &list); code != http.StatusOK || list.Items[0].UsedCount != 1 {
		t.Fatalf("duplicate email consumed invite: code=%d item=%+v", code, list.Items[0])
	}
	if code, body := register(&testClient{t: t, server: ts}, "second@example.test", "TEAM-2026"); code != http.StatusCreated {
		t.Fatalf("second invite registration code=%d body=%v", code, body)
	}
	if code, body := register(&testClient{t: t, server: ts}, "third@example.test", "TEAM-2026"); code != http.StatusForbidden {
		t.Fatalf("exhausted invite code=%d body=%v", code, body)
	}
	if code := admin.do("GET", "/api/admin/registration-invites", nil, &list); code != http.StatusOK || list.Items[0].UsedCount != 2 || list.Items[0].RemainingUses != 0 {
		t.Fatalf("exhausted invite counts code=%d item=%+v", code, list.Items[0])
	}

	a.cfg.OpenRegistration = true
	if code, body := register(&testClient{t: t, server: ts}, "open@example.test", ""); code != http.StatusCreated {
		t.Fatalf("open registration should not require invite code=%d body=%v", code, body)
	}
	if code := admin.do("GET", "/api/admin/registration-invites", nil, &list); code != http.StatusOK || list.Items[0].UsedCount != 2 {
		t.Fatalf("open registration consumed invite: code=%d item=%+v", code, list.Items[0])
	}
	if code := admin.do("DELETE", "/api/admin/registration-invites/"+created.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("delete invite code=%d", code)
	}
}

func TestInvitationRegistrationPublicSettings(t *testing.T) {
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	client := &testClient{t: t, server: ts}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	var systemSettings SystemSettings
	if code := client.do("GET", "/api/admin/settings", nil, &systemSettings); code != http.StatusOK {
		t.Fatalf("get system settings code=%d", code)
	}
	payload := systemSettingsPayload(systemSettings)
	payload["inviteRegistrationEnabled"] = true
	if code := client.do("POST", "/api/admin/settings", payload, &systemSettings); code != http.StatusOK || !systemSettings.InviteRegistrationEnabled {
		t.Fatalf("enable invitation registration code=%d settings=%+v", code, systemSettings)
	}
	var settings PublicSettings
	if code := client.do("GET", "/api/public/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("public settings code=%d", code)
	}
	if !settings.InviteRegistrationEnabled || settings.OpenRegistration || len(settings.MailboxDomains) == 0 {
		t.Fatalf("unexpected public settings: %+v", settings)
	}
}
