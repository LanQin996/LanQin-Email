package app

import (
	"errors"
	"fmt"
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
	// A per-day limit of 0 keeps the owned-mailbox limit the only constraint under test.
	setRegularGroupMailboxLimits(t, admin, limit, 0)
}

// setRegularGroupMailboxLimits sets both the owned-mailbox and per-day allowance.
func setRegularGroupMailboxLimits(t *testing.T, admin *testClient, limit, perDay int) {
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
	limits.MaxMailboxesPerDay = perDay
	var updated PermissionGroup
	if code := admin.do("POST", "/api/admin/permission-groups/"+target.ID, map[string]any{
		"name":        target.Name,
		"description": target.Description,
		"permissions": target.Permissions,
		"limits":      limits,
	}, &updated); code != http.StatusOK {
		t.Fatalf("update group code=%d", code)
	}
	if updated.Limits.MaxMailboxes != limit || updated.Limits.MaxMailboxesPerDay != perDay {
		t.Fatalf("limits not persisted: got %d/%d want %d/%d", updated.Limits.MaxMailboxes, updated.Limits.MaxMailboxesPerDay, limit, perDay)
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

// Deletion releases the total allowance without erasing creation history.
func TestMailboxQuotaReleasesDeletedMailboxes(t *testing.T) {
	for _, path := range []string{"/api/admin/mailboxes/", "/api/open/mailboxes/", "/api/open/v1/mailboxes/"} {
		t.Run(path, func(t *testing.T) {
			a, admin, user, created, domain := setupQuotaTest(t)
			setRegularGroupMailboxLimit(t, admin, 2)
			var first Mailbox
			if code := admin.do("POST", "/api/admin/mailboxes", map[string]any{
				"domainId": domain.ID, "localPart": "one", "userId": created.ID,
				"displayName": "Synthetic", "password": "Password123!",
			}, &first); code != http.StatusCreated {
				t.Fatalf("admin create code=%d", code)
			}
			if code := applyMailbox(t, user, domain.ID, "two"); code != http.StatusCreated {
				t.Fatalf("second apply code=%d", code)
			}

			assertMailboxQuotaUsage(t, admin, user, created.ID, 2, 2)
			if code := applyMailbox(t, user, domain.ID, "blocked"); code != http.StatusConflict {
				t.Fatalf("apply at limit code=%d want 409", code)
			}
			deleter := admin
			if path != "/api/admin/mailboxes/" {
				deleter = &testClient{t: t, server: admin.server, bearer: createTestAPIToken(t, admin, "quota-delete")}
			}
			var ok map[string]any
			if code := deleter.do("DELETE", path+first.ID, nil, &ok); code != http.StatusOK {
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
			assertMailboxQuotaUsage(t, admin, user, created.ID, 1, 2)
			if code := applyMailbox(t, user, domain.ID, "three"); code != http.StatusCreated {
				t.Fatalf("apply after delete code=%d want 201", code)
			}
			assertMailboxQuotaUsage(t, admin, user, created.ID, 2, 3)
			if code := applyMailbox(t, user, domain.ID, "four"); code != http.StatusConflict {
				t.Fatalf("apply after refilling slot code=%d want 409", code)
			}
		})
	}
}

func assertMailboxQuotaUsage(t *testing.T, admin, user *testClient, userID string, count, total int) {
	t.Helper()
	var response struct {
		User User `json:"user"`
	}
	if code := user.do("GET", "/api/me", nil, &response); code != http.StatusOK {
		t.Fatalf("get current user code=%d", code)
	}
	me := response.User
	if me.MailboxCount != count || me.MailboxesCreatedTotal != total {
		t.Fatalf("current user usage=%d total=%d want %d/%d", me.MailboxCount, me.MailboxesCreatedTotal, count, total)
	}
	var users struct {
		Items []AdminUser `json:"items"`
	}
	if code := admin.do("GET", "/api/admin/users", nil, &users); code != http.StatusOK {
		t.Fatalf("list users code=%d", code)
	}
	for _, item := range users.Items {
		if item.ID == userID {
			if item.MailboxCount != count || len(item.Mailboxes) != count || item.MailboxesCreatedTotal != total {
				t.Fatalf("admin user count=%d addresses=%d total=%d want %d/%d", item.MailboxCount, len(item.Mailboxes), item.MailboxesCreatedTotal, count, total)
			}
			return
		}
	}
	t.Fatal("quota user missing from admin list")
}

func TestMailboxQuotaCountsDisabledAndReassignedMailboxes(t *testing.T) {
	_, admin, user, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 1)
	first := createTestMailbox(t, admin, domain.ID, "disabled", "Synthetic", "Password123!", map[string]any{"userId": created.ID})
	var updated Mailbox
	if code := admin.do("POST", "/api/admin/mailboxes/"+first.ID, map[string]any{
		"userId": created.ID, "displayName": first.DisplayName, "quotaMb": first.QuotaMB, "status": "disabled",
	}, &updated); code != http.StatusOK {
		t.Fatalf("disable mailbox code=%d", code)
	}
	assertMailboxQuotaUsage(t, admin, user, created.ID, 1, 1)
	if code := applyMailbox(t, user, domain.ID, "blocked"); code != http.StatusConflict {
		t.Fatalf("disabled mailbox must consume allowance: code=%d", code)
	}
	var recipient AdminUser
	if code := admin.do("POST", "/api/admin/users", map[string]any{
		"email": "recipient@example.test", "role": "user", "password": "Password123!",
	}, &recipient); code != http.StatusCreated {
		t.Fatalf("create recipient code=%d", code)
	}
	if code := admin.do("POST", "/api/admin/mailboxes/"+first.ID, map[string]any{
		"userId": recipient.ID, "displayName": first.DisplayName, "quotaMb": first.QuotaMB, "status": "active",
	}, &updated); code != http.StatusOK {
		t.Fatalf("reassign mailbox code=%d", code)
	}
	assertMailboxQuotaUsage(t, admin, user, created.ID, 0, 1)
	recipientClient := &testClient{t: t, server: admin.server}
	if code := recipientClient.do("POST", "/api/auth/login", map[string]string{
		"email": recipient.Email, "password": "Password123!",
	}, nil); code != http.StatusOK {
		t.Fatalf("recipient login code=%d", code)
	}
	assertMailboxQuotaUsage(t, admin, recipientClient, recipient.ID, 1, 0)
	if code := applyMailbox(t, recipientClient, domain.ID, "blocked-recipient"); code != http.StatusConflict {
		t.Fatalf("transferred mailbox must consume recipient allowance: code=%d", code)
	}
	if code := applyMailbox(t, user, domain.ID, "replacement"); code != http.StatusCreated {
		t.Fatalf("reassignment must release previous owner's allowance: code=%d", code)
	}
}

func TestMailboxQuotaSerializesConcurrentCreations(t *testing.T) {
	a, admin, _, created, domain := setupQuotaTest(t)
	setRegularGroupMailboxLimit(t, admin, 1)
	user, err := a.userByID(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 4
	results := make(chan error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		go func(n int) {
			<-start
			_, err := a.createMailboxWithPasswordHash(t.Context(), created.ID, domain.ID,
				fmt.Sprintf("race%d", n), "Synthetic", "unused", 1024, "active", user)
			results <- err
		}(i)
	}
	close(start)
	succeeded := 0
	for range attempts {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, errMailboxCountLimitReached) {
			t.Errorf("concurrent creation failed unexpectedly: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent creations succeeded, want 1", succeeded)
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
