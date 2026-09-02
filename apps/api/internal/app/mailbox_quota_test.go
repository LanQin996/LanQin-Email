package app

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// setupQuotaTest returns an admin client, a logged-in regular user client, the
// created user and an active domain that self-service applications may use.
func setupQuotaTest(t *testing.T) (*App, *testClient, *testClient, AdminUser, Domain) {
	t.Helper()
	a := newTestApp(t)
	ts := httptest.NewServer(a.Router())
	t.Cleanup(ts.Close)
	admin := &testClient{t: t, server: ts}

	var login map[string]any
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, &login); code != http.StatusOK {
		t.Fatalf("admin login code=%d", code)
	}
	domain := createTestDomain(t, admin, "quota.test")

	var settings SystemSettings
	if code := admin.do("GET", "/api/admin/settings", nil, &settings); code != http.StatusOK {
		t.Fatalf("get settings code=%d", code)
	}
	update := systemSettingsPayload(settings)
	update["userMailboxApplyEnabled"] = true
	update["userMailboxDomainIds"] = []string{domain.ID}
	if code := admin.do("POST", "/api/admin/settings", update, &settings); code != http.StatusOK {
		t.Fatalf("enable apply code=%d", code)
	}

	var created AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{"email": "quota@example.net", "displayName": "Quota", "role": "user", "password": "Password123!", "disabled": false}, &created); code != http.StatusCreated {
		t.Fatalf("create user code=%d", code)
	}
	user := &testClient{t: t, server: ts}
	if code := user.do("POST", "/api/auth/login", map[string]string{"email": "quota@example.net", "password": "Password123!"}, &login); code != http.StatusOK {
		t.Fatalf("user login code=%d", code)
	}
	return a, admin, user, created, domain
}

// setRegularGroupMailboxLimit updates the limit inherited by every regular user.
func setRegularGroupMailboxLimit(t *testing.T, admin *testClient, limit int) {
	t.Helper()
	var groups struct {
		Items []PermissionGroup `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/permission-groups", nil, &groups); code != http.StatusOK {
		t.Fatalf("list groups code=%d", code)
	}
	var target *PermissionGroup
	for i := range groups.Items {
		if groups.Items[i].ID == PermissionGroupRegular {
			target = &groups.Items[i]
			break
		}
	}
	if target == nil {
		t.Fatal("regular permission group not found")
	}
	limits := target.Limits
	limits.MaxMailboxes = limit
	var updated PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups/"+target.ID, map[string]any{
		"name":        target.Name,
		"description": target.Description,
		"permissions": target.Permissions,
		"limits":      limits,
	}, &updated); code != http.StatusOK {
		t.Fatalf("update group code=%d", code)
	}
	if updated.Limits.MaxMailboxes != limit {
		t.Fatalf("limit not persisted: got %d want %d", updated.Limits.MaxMailboxes, limit)
	}
}

func applyMailbox(t *testing.T, user *testClient, domainID, localPart string) int {
	t.Helper()
	var body map[string]any
	return user.do("POST", "/api/me/mailboxes/apply", map[string]string{"domainId": domainID, "localPart": localPart}, &body)
}

func TestMailboxQuotaEnforcedOnApply(t *testing.T) {
	_, admin, user, _, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 2)

	if code := applyMailbox(t, user, domain.ID, "one"); code != http.StatusCreated {
		t.Fatalf("first apply code=%d want 201", code)
	}
	if code := applyMailbox(t, user, domain.ID, "two"); code != http.StatusCreated {
		t.Fatalf("second apply code=%d want 201", code)
	}
	if code := applyMailbox(t, user, domain.ID, "three"); code != http.StatusConflict {
		t.Fatalf("third apply code=%d want 409", code)
	}
}

func TestMailboxQuotaZeroMeansUnlimited(t *testing.T) {
	_, admin, user, _, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 0)

	for _, local := range []string{"a", "b", "c", "d", "e"} {
		if code := applyMailbox(t, user, domain.ID, local); code != http.StatusCreated {
			t.Fatalf("apply %q code=%d want 201 (0 must mean unlimited)", local, code)
		}
	}
}

// TestMailboxQuotaCountsDeletedMailboxes pins the decision that the quota is a
// lifetime counter: deleting a mailbox must not hand the allowance back, which
// is what stops a user from cycling create/delete to exceed the limit.
func TestMailboxQuotaCountsDeletedMailboxes(t *testing.T) {
	a, admin, user, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 2)

	if code := applyMailbox(t, user, domain.ID, "one"); code != http.StatusCreated {
		t.Fatalf("first apply code=%d", code)
	}
	if code := applyMailbox(t, user, domain.ID, "two"); code != http.StatusCreated {
		t.Fatalf("second apply code=%d", code)
	}

	var mailboxID string
	if err := a.db.QueryRow(`SELECT id FROM mailboxes WHERE user_id=? AND local_part=?`, created.ID, "one").Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	var ok map[string]any
	if code := admin.do("DELETE", "/api/admin/mailboxes/"+mailboxID, nil, &ok); code != http.StatusOK {
		t.Fatalf("delete mailbox code=%d", code)
	}

	var live int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM mailboxes WHERE user_id=?`, created.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live mailbox count=%d want 1", live)
	}
	var total int
	if err := a.db.QueryRow(`SELECT mailboxes_created_total FROM users WHERE id=?`, created.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("mailboxes_created_total=%d want 2 (counter must not decrease)", total)
	}
	if code := applyMailbox(t, user, domain.ID, "three"); code != http.StatusConflict {
		t.Fatalf("apply after delete code=%d want 409", code)
	}
}

// TestMailboxQuotaAdminBypassStillCounts covers the deliberate asymmetry: admin
// creation ignores the limit but still advances the counter.
func TestMailboxQuotaAdminBypassStillCounts(t *testing.T) {
	a, admin, user, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 1)

	if code := applyMailbox(t, user, domain.ID, "one"); code != http.StatusCreated {
		t.Fatalf("first apply code=%d", code)
	}
	if code := applyMailbox(t, user, domain.ID, "two"); code != http.StatusConflict {
		t.Fatalf("self-service beyond limit code=%d want 409", code)
	}

	var mailbox Mailbox
	if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
		"domainId": domain.ID, "localPart": "byadmin", "userId": created.ID,
		"displayName": "By Admin", "password": "Password123!", "quotaMb": 1024,
	}, &mailbox); code != http.StatusCreated {
		t.Fatalf("admin create code=%d mailbox=%+v", code, mailbox)
	}

	var total int
	if err := a.db.QueryRow(`SELECT mailboxes_created_total FROM users WHERE id=?`, created.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("mailboxes_created_total=%d want 2 (admin path must still count)", total)
	}
}

// TestMinimalLimitsHasNoZeroField guards a silent-failure mode rather than a
// behaviour: mergeLimitValue treats 0 as "unlimited" and lets it absorb every
// other value, so a field left at its zero value in minimalLimits would make
// that limit permanently unlimited without raising an error anywhere.
func TestMinimalLimitsHasNoZeroField(t *testing.T) {
	limits := minimalLimits()
	value := reflect.ValueOf(limits)
	typ := value.Type()
	for i := 0; i < value.NumField(); i++ {
		if value.Field(i).Kind() != reflect.Int {
			continue
		}
		if value.Field(i).Int() == 0 {
			t.Errorf("minimalLimits().%s is 0, which mergeLimitValue treats as unlimited; it must be a positive floor", typ.Field(i).Name)
		}
	}
}

// TestPermissionLimitsFieldsAllPlumbed catches the other half of the same class
// of bug: a newly added limit field that nobody wired into merge, normalize or
// the grant check. Each helper is probed with values that only produce the
// expected result when the field is actually handled.
func TestPermissionLimitsFieldsAllPlumbed(t *testing.T) {
	typ := reflect.TypeOf(PermissionLimits{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Int {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			// merge must pick the larger of two finite values.
			low := PermissionLimits{}
			high := PermissionLimits{}
			reflect.ValueOf(&low).Elem().Field(i).SetInt(1)
			reflect.ValueOf(&high).Elem().Field(i).SetInt(9)
			merged := mergePermissionLimits(low, high)
			if got := reflect.ValueOf(merged).Field(i).Int(); got != 9 {
				t.Errorf("mergePermissionLimits does not handle %s: got %d want 9", field.Name, got)
			}

			// normalize must reject negatives.
			negative := PermissionLimits{}
			reflect.ValueOf(&negative).Elem().Field(i).SetInt(-1)
			if _, err := normalizePermissionLimits(negative); err == nil {
				t.Errorf("normalizePermissionLimits accepts negative %s", field.Name)
			}

			// A non-admin actor must not grant more than they hold.
			actor := &User{Role: "user", Limits: low}
			if actorCanGrantLimits(actor, high) {
				t.Errorf("actorCanGrantLimits does not check %s", field.Name)
			}
		})
	}
}
