package httpx_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

// errTestTermsRequired stands in for consent.terms_acceptance_required
// (httpx does not import consent).
var errTestTermsRequired = problem.New(problem.Forbidden, "testonly.terms_required", "Accept the terms first")

// actorResolver resolves the bearer token "<user id>" to that user.
type actorResolver struct{}

func (actorResolver) Resolve(ctx context.Context, cred httpx.Credential) (context.Context, error) {
	for _, u := range []uuid.UUID{pendingUser, acceptedUser, brokenUser, pendingStaff} {
		if cred.TokenHash == sha256.Sum256([]byte(u.String())) {
			a := authz.Actor{Kind: authz.KindUser, UserID: u}
			if u == pendingStaff {
				a.PlatformRoles = []authz.PlatformRole{authz.PlatformAdmin, authz.PlatformDPO, authz.PlatformFinanceAdmin}
				a.MFAVerified = true
			}
			return authz.WithActor(ctx, a), nil
		}
	}
	return nil, problem.ErrUnauthenticated
}

var (
	pendingUser  = uuid.NewV7() // has not accepted the current terms
	acceptedUser = uuid.NewV7()
	brokenUser   = uuid.NewV7() // the checker fails for this one
	pendingStaff = uuid.NewV7() // platform admin, DPO and finance admin who has not accepted the current terms
)

// termsChecker refuses pendingUser and counts its calls.
type termsChecker struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (c *termsChecker) CheckTerms(_ context.Context, userID uuid.UUID) error {
	c.mu.Lock()
	c.calls = append(c.calls, userID)
	c.mu.Unlock()
	switch userID {
	case pendingUser, pendingStaff:
		return errTestTermsRequired
	case brokenUser:
		return errors.New("terms store unreachable")
	}
	return nil
}

func (c *termsChecker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// registerTermsOps registers a gated self write, its read, a public write
// and two exempt operations (rows of the exemption table).
func registerTermsOps(api huma.API) {
	h := func(context.Context, *emptyInput) (*okOutput, error) { return ok(), nil }
	for _, o := range []struct {
		id, method, path string
		surface          op.Surface
	}{
		{"createTestThing", http.MethodPost, "/things", op.SurfaceSelf},
		{"updateTestThing", http.MethodPatch, "/orgs/{org_id}/things", op.SurfaceOrg},
		{"getTestThing", http.MethodGet, "/things", op.SurfaceSelf},
		{"createTestPublicThing", http.MethodPost, "/public-things", op.SurfacePublic},
		{"acceptTerms", http.MethodPost, "/me/terms-acceptances", op.SurfaceSelf},
		{"deleteCurrentSession", http.MethodDelete, "/auth/sessions/current", op.SurfaceSelf},
	} {
		m := op.Meta{Action: "testonly.thing.write", Surface: o.surface}
		if o.method == http.MethodPost {
			m.Idempotency = op.IdempotencyOptional
		}
		op.Register(api, "testonly", huma.Operation{OperationID: o.id, Method: o.method, Path: o.path}, m, h)
	}
}

func newTermsApp(t *testing.T, terms httpx.TermsChecker) *testApp {
	t.Helper()
	app := newTestApp(t, func(d *httpx.Deps) { d.Actors = actorResolver{} })
	if terms != nil {
		app.API.UseMiddleware(httpx.TermsGate(app.API, terms))
	}
	registerTermsOps(app.V1)
	return app
}

func bearer(u uuid.UUID) map[string]string {
	return map[string]string{"Authorization": "Bearer " + u.String()}
}

// TestTermsGate_RefusesWritesUntilAccepted (BE-124): a signed-in user who
// has not accepted the current terms gets the checker's 403 from every
// protected write; reads, public operations and the exempt rows run, and
// the checker is asked only for the gated operations. A user who accepted
// writes; a checker failure is a 500, never a pass.
func TestTermsGate_RefusesWritesUntilAccepted(t *testing.T) {
	t.Parallel()
	terms := &termsChecker{}
	app := newTermsApp(t, terms)
	org := "/api/v1/orgs/" + uuid.NewV7().String() + "/things"

	for _, c := range []struct{ method, path string }{{http.MethodPost, "/api/v1/things"}, {http.MethodPatch, org}} {
		m := problemBody(t, app.do(t, c.method, c.path, nil, bearer(pendingUser)), http.StatusForbidden)
		if m["code"] != "testonly.terms_required" {
			t.Fatalf("%s %s: code = %v", c.method, c.path, m["code"])
		}
	}
	if n := terms.count(); n != 2 {
		t.Fatalf("checker asked %d times for two gated writes", n)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/things"},
		{http.MethodPost, "/api/v1/public-things"},
		{http.MethodPost, "/api/v1/me/terms-acceptances"},
		{http.MethodDelete, "/api/v1/auth/sessions/current"},
	} {
		if rec := app.do(t, c.method, c.path, nil, bearer(pendingUser)); rec.Code != http.StatusOK {
			t.Fatalf("%s %s by a user without the current terms = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if n := terms.count(); n != 2 {
		t.Fatalf("checker asked for an ungated operation (%d calls)", n)
	}
	if rec := app.do(t, http.MethodPost, "/api/v1/things", nil, bearer(acceptedUser)); rec.Code != http.StatusOK {
		t.Fatalf("write by a user who accepted = %d %s", rec.Code, rec.Body)
	}
	m := problemBody(t, app.do(t, http.MethodPost, "/api/v1/things", nil, bearer(brokenUser)), http.StatusInternalServerError)
	if m["code"] != "internal_error" || !strings.Contains(app.logs.String(), "unreachable") {
		t.Fatalf("checker failure = %v; logs %s", m, app.logs)
	}
	// Anonymous: the auth middleware answers first; the checker is not asked.
	before := terms.count()
	problemBody(t, app.do(t, http.MethodPost, "/api/v1/things", nil, nil), http.StatusUnauthorized)
	if terms.count() != before {
		t.Fatal("checker asked for an anonymous request")
	}

	// Without the middleware nothing is gated.
	plain := newTermsApp(t, nil)
	if rec := plain.do(t, http.MethodPost, "/api/v1/things", nil, bearer(pendingUser)); rec.Code != http.StatusOK {
		t.Fatalf("write without the gate = %d", rec.Code)
	}
}

// TestTermsGate_StaffIsNotExempt (BE-152, Q-20): staff are users too — a
// platform admin, DPO or finance admin who has not accepted the current
// terms is refused every protected write (their own and the org or admin
// surfaces alike) and keeps only the exempt rows; no role opens the gate.
func TestTermsGate_StaffIsNotExempt(t *testing.T) {
	t.Parallel()
	terms := &termsChecker{}
	app := newTermsApp(t, terms)
	org := "/api/v1/orgs/" + uuid.NewV7().String() + "/things"
	for _, c := range []struct{ method, path string }{{http.MethodPost, "/api/v1/things"}, {http.MethodPatch, org}} {
		m := problemBody(t, app.do(t, c.method, c.path, nil, bearer(pendingStaff)), http.StatusForbidden)
		if m["code"] != "testonly.terms_required" {
			t.Fatalf("%s %s by staff: code = %v", c.method, c.path, m["code"])
		}
	}
	if rec := app.do(t, http.MethodPost, "/api/v1/me/terms-acceptances", nil, bearer(pendingStaff)); rec.Code != http.StatusOK {
		t.Fatalf("acceptTerms by staff = %d %s", rec.Code, rec.Body)
	}
	if n := terms.count(); n != 2 {
		t.Fatalf("checker asked %d times, want the two gated writes", n)
	}
}

// TestTermsGate_Q20Exemptions (BE-152, Q-20): the notification preferences
// (refusing a message is always possible, Law 6563 m.8) and the company
// block list (the user's own protection) are on the exemption table with
// their operations; answering a contact request and cancelling a
// registration are not (using the service rests on the contract).
func TestTermsGate_Q20Exemptions(t *testing.T) {
	t.Parallel()
	exempt := httpx.TermsExemptOperations()
	for id, want := range map[string]string{
		"putMyNotificationPreferences": "PUT /api/v1/me/notification-preferences",
		"blockCompany":                 "PUT /api/v1/me/company-blocks/{org_id}",
		"unblockCompany":               "DELETE /api/v1/me/company-blocks/{org_id}",
	} {
		if exempt[id] != want {
			t.Errorf("exemption %s = %q, want %q", id, exempt[id], want)
		}
	}
	for _, id := range []string{"respondMyContactRequest", "cancelMyRegistration", "registerMyPushToken", "muteOrg", "markMyNotificationRead"} {
		if row, ok := exempt[id]; ok {
			t.Errorf("%s is exempt (%s); it rests on the contract", id, row)
		}
	}
}

// TestTermsGate_MarksProtectedWrites (BE-124): the spec carries
// x-koro-terms-gate on every protected write operation, exempt on the
// listed ones; reads and public operations carry none.
func TestTermsGate_MarksProtectedWrites(t *testing.T) {
	t.Parallel()
	app := newTermsApp(t, nil)
	got := map[string]any{}
	for _, item := range app.API.OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Post, item.Patch, item.Delete} {
			if o != nil {
				got[o.OperationID] = o.Extensions[httpx.ExtTermsGate]
			}
		}
	}
	want := map[string]any{
		"createTestThing": httpx.TermsGateEnforced, "updateTestThing": httpx.TermsGateEnforced,
		"getTestThing": nil, "createTestPublicThing": nil,
		"acceptTerms": httpx.TermsGateExempt, "deleteCurrentSession": httpx.TermsGateExempt,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %s = %v, want %v", id, httpx.ExtTermsGate, got[id], w)
		}
	}
	missing := httpx.TermsExemptionsUnregistered(app.API.OpenAPI())
	if slices.Contains(missing, "acceptTerms") || slices.Contains(missing, "deleteCurrentSession") ||
		len(missing) != len(httpx.TermsExemptOperations())-2 {
		t.Fatalf("unregistered exemptions = %v", missing)
	}
}

// TestTermsGate_ExemptionMatchesItsOperation (BE-124): an exemption row
// whose operation is registered with another method or path, or on a
// surface the gate never covers, stops the registration.
func TestTermsGate_ExemptionMatchesItsOperation(t *testing.T) {
	t.Parallel()
	h := func(context.Context, *emptyInput) (*okOutput, error) { return ok(), nil }
	for _, c := range []struct {
		name, method, path string
		surface            op.Surface
	}{
		{"moved", http.MethodPost, "/me/terms", op.SurfaceSelf},
		{"read", http.MethodGet, "/me/terms-acceptances", op.SurfaceSelf},
		{"public", http.MethodPost, "/me/terms-acceptances", op.SurfacePublic},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t, nil)
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "acceptTerms") {
					t.Fatalf("recover() = %v, want a panic naming acceptTerms", r)
				}
			}()
			m := op.Meta{Action: "testonly.thing.write", Surface: c.surface}
			if c.method == http.MethodPost {
				m.Idempotency = op.IdempotencyOptional
			}
			op.Register(app.V1, "testonly", huma.Operation{OperationID: "acceptTerms", Method: c.method, Path: c.path}, m, h)
		})
	}
}
