package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/parties"
	"koro/internal/platform/token"
)

// BE-074 over HTTP, with identity's real re-authentication behind the R
// operations of the modules that used to return a refused proof without a
// record (03 §6.2). A wrong password is a verdict: 403
// identity.reauth_failed, one denied reauth_failed row of the operation
// tied to the request, one failed attempt counted. When identity cannot
// check the proof within the request budget (every hasher slot taken until
// the deadline) the operation answers 503 unavailable after the whole
// budget, logs one WARN line and no ERROR, writes no row and counts
// nothing. Every module binds identity's service as its verifier directly
// (wire_<module>.go: d.Reauth = svc), so the same holds for ledger and
// parties, whose fixtures the service tests cover
// (TestReauthDenied_EveryROperation in each module).
func TestWire_ReauthDenied_WrongProof403Denied_Overload503Nothing(t *testing.T) {
	t.Parallel()
	const wrong = `"reauth": {"password": "wrong-password-123"}`
	const good = `"reauth": {"password": "` + matrixReauthPassword + `"}`
	target := uuid.NewV7() // suspend and set_sensitivity check the proof before reading it
	cases := []struct {
		name    string
		role    authz.PlatformRole
		action  authz.Action
		method  string
		path    string
		body    string // %s: the reauth member
		resType string
		resID   *uuid.UUID
		meta    map[string]any
		setup   func(t *testing.T, e *reauthBudgetEnv)
	}{
		{name: "orgs adminSuspendOrg", role: authz.PlatformAdmin, action: authz.OrgsOrgSuspend, method: http.MethodPost,
			path: "/api/v1/admin/orgs/" + target.String() + "/suspend", body: `{"reason": "Kullanım koşulları ihlali", %s}`,
			resType: authz.TypeOrg, resID: &target, meta: map[string]any{"transition": "suspend"}},
		{name: "directory adminSetClubSensitivity", role: authz.PlatformAdmin, action: authz.DirectoryClubSetSensitivity,
			method: http.MethodPost, path: "/api/v1/admin/clubs/" + target.String() + "/sensitivity",
			body:    `{"is_sensitive": true, "sensitive_reason": "political_opinion", %s}`,
			resType: authz.TypeClub, resID: &target},
		{name: "catalog adminCreateCommercePolicy", role: authz.PlatformFinanceAdmin, action: authz.CatalogPolicyManage,
			method: http.MethodPost, path: "/api/v1/admin/commerce-policies",
			body:    `{"scope": "default", "model_code": "A", "commission_rate_bp": 1200, %s}`,
			resType: "commerce_policy", meta: map[string]any{"change": "created"}},
		{name: "billing adminPutIntegration", role: authz.PlatformFinanceAdmin, action: authz.BillingIntegrationManage,
			method: http.MethodPut, path: "/api/v1/admin/integrations/parasut",
			body:    `{"credentials": {"access_token": "wire-access-token-not-real"}, %s}`,
			resType: "integration", meta: map[string]any{"provider": "parasut"}},
		{name: "payments adminCreateCollectionAccount", role: authz.PlatformFinanceAdmin, action: authz.PaymentsCollectionAccountCreate,
			method: http.MethodPost, path: "/api/v1/admin/collection-accounts",
			body: `{"label": "Wire hesap", "legal_party_id": "` + wirePlatformParty.String() + `", "iban": "TR330006100519786457841326",
				"holder_name": "Koro Teknoloji A.Ş.", "bank_name": "Örnek Bank", %s}`,
			resType: authz.TypeCollectionAccount,
			setup: func(t *testing.T, e *reauthBudgetEnv) {
				if _, err := e.c.parties.store.InsertParty(t.Context(), parties.LegalParty{ID: wirePlatformParty, Kind: parties.KindPlatform,
					LegalType: parties.LegalType("company"), Title: "Koro Teknoloji A.Ş.", VATStatus: parties.VATUnknown,
					Status: parties.StatusDraft}); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			late := &verifyLateHasher{PasswordHasher: testHasher()}
			e := newReauthBudgetEnv(t, func(m *identity.MemStore) identity.Store { return m }, late)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			tok, staff := e.passwordStaff(t, tc.role)
			if status, _, raw := e.do(t.Context(), tok, http.MethodGet, "/api/v1/me", ""); status != http.StatusOK {
				t.Fatalf("GET /me = %d %s", status, raw) // warm the stack up outside the bubble
			}
			e.markLogs()

			// Overloaded: no verdict, no row, nothing counted.
			late.late.Store(true)
			e.overloaded(t, tok, tc.method, tc.path, strings.Replace(tc.body, "%s", good, 1))
			late.late.Store(false)
			e.quiet(t)
			if rows := e.audits.of(tc.action); len(rows) != 0 {
				t.Fatalf("audit rows tried for the overloaded request: %+v", rows)
			}
			if cur, _ := e.mem.User(t.Context(), staff); cur.FailedLogins != 0 {
				t.Fatalf("the overload counted as a wrong password: failed_logins=%d", cur.FailedLogins)
			}

			// A wrong password is a verdict: 403, one denied row, counted.
			status, code, raw := e.do(t.Context(), tok, tc.method, tc.path, strings.Replace(tc.body, "%s", wrong, 1))
			if status != http.StatusForbidden || code != "identity.reauth_failed" {
				t.Fatalf("wrong password = %d %s, want 403 identity.reauth_failed", status, raw)
			}
			rows := e.audits.of(tc.action)
			if len(rows) != 1 {
				t.Fatalf("%d audit rows for the wrong password, want one denied record: %+v", len(rows), rows)
			}
			r := rows[0]
			var meta map[string]any
			if err := json.Unmarshal(r.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			switch {
			case r.Outcome != audit.Denied || r.Reason != audit.ReasonReauthFailed:
				t.Fatalf("outcome %q reason %q, want denied reauth_failed", r.Outcome, r.Reason)
			case r.ActorUserID == nil || *r.ActorUserID != staff || r.ActorKind != "user":
				t.Fatalf("actor %v (%s), want %s", r.ActorUserID, r.ActorKind, staff)
			case r.ResourceType != tc.resType || !sameUUID(r.ResourceID, tc.resID) || r.OrgID != nil:
				t.Fatalf("resource %s %v org %v, want %s %v and no org", r.ResourceType, r.ResourceID, r.OrgID, tc.resType, tc.resID)
			case r.RequestID == "":
				t.Fatal("the denied row is not tied to its request (no request ID)")
			case len(meta) != len(tc.meta) || (len(meta) > 0 && !reflect.DeepEqual(meta, tc.meta)):
				t.Fatalf("metadata %v, want %v", meta, tc.meta)
			}
			if cur, _ := e.mem.User(t.Context(), staff); cur.FailedLogins != 1 {
				t.Fatalf("wrong password not counted: failed_logins=%d", cur.FailedLogins)
			}
			if out := e.logs.String()[e.mark:]; strings.Contains(out, `"level":"ERROR"`) {
				t.Fatalf("ERROR logged: %s", out)
			}
		})
	}
}

// wirePlatformParty is Koro's own legal party in the payments case.
var wirePlatformParty = uuid.MustParse("0198f1a2-7b44-7c1e-9a51-3f2d6c0b0e74")

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// passwordStaff creates an active account of a platform role without TOTP
// (re-authentication by password, like the matrix personas) and an
// MFA-verified mobile session (03 §6.4: the role-level MFA is the
// session's); it returns the session token and the user ID.
func (e *reauthBudgetEnv) passwordStaff(t *testing.T, role authz.PlatformRole) (string, uuid.UUID) {
	t.Helper()
	now := e.c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	email := id.String() + "@staff.koro.test"
	u := identity.User{
		ID: id, Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		PasswordHash: testPasswordHash(), FirstName: "Deniz", LastName: "Kaya", Persona: identity.PersonaCompany, Locale: identity.LocaleTR,
	}
	if err := e.mem.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if err := e.mem.InsertRoleAssignment(t.Context(), identity.RoleAssignment{ID: uuid.NewV7(), UserID: id, Role: role,
		Reason: "bootstrap", Status: identity.RoleActive, RequestedAt: now, GrantedAt: &now}); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	s := identity.Session{ID: uuid.NewV7(), UserID: id, TokenHash: hash, ClientKind: identity.ClientMobile, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(30 * time.Minute), AbsoluteExpiresAt: now.Add(12 * time.Hour), MFAVerifiedAt: &now}
	if err := e.mem.InsertSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return raw, id
}
