package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/parties"
)

// BE-075 over HTTP, with identity's real re-authentication: every R
// operation — identity's own and another module's over identity's Verify —
// writes exactly one audit record for a refused proof, of any action: the
// denied reauth_failed record of the operation (03 §6.2). A wrong and a
// missing proof are 403, the attempt after the fifth failure 429 (the
// account is locked); an overloaded hasher is 503 with no record at all.
// identity's Verify audits nothing, so the other modules' record is the
// only one (no double record); identity's own operations used to write
// theirs without reason or actor and none for a missing proof or a 429.
func TestWire_IdentityReauthDenied_OneRecordPerRefusal(t *testing.T) {
	t.Parallel()
	const (
		good    = `, "reauth": {"password": "` + matrixReauthPassword + `"}`
		wrong   = `, "reauth": {"password": "wrong-password-123"}`
		goodCur = `, "current_password": "` + matrixReauthPassword + `"`
		badCur  = `, "current_password": "wrong-password-123"`
	)
	target := uuid.NewV7() // nothing reads it before the proof
	cases := []struct {
		name         string
		role         authz.PlatformRole
		action       authz.Action
		method, path string
		body         string // %s: the proof member(s)
		good, wrong  string
		resType      string
		resID        func(staff uuid.UUID) *uuid.UUID
		meta         map[string]any
		setup        func(t *testing.T, e *reauthBudgetEnv)
	}{
		{name: "identity changeMyPassword", role: authz.PlatformSupport, action: authz.IdentityPasswordChange,
			method: http.MethodPost, path: "/api/v1/me/password", body: `{"new_password": "Yeni-Ve-Uzun-Parola-4821"%s}`,
			good: goodCur, wrong: badCur, resType: "user", resID: func(staff uuid.UUID) *uuid.UUID { return &staff }},
		{name: "identity adminGrantPlatformRole", role: authz.PlatformAdmin, action: authz.IdentityPlatformRoleGrant,
			method: http.MethodPost, path: "/api/v1/admin/platform-roles",
			body: `{"user_id": "` + target.String() + `", "role": "moderator", "reason": "needs the role for the pilot"%s}`,
			good: good, wrong: wrong, resType: "platform_role_assignment", meta: map[string]any{"step": "request"}},
		{name: "orgs adminSuspendOrg over identity's Verify", role: authz.PlatformAdmin, action: authz.OrgsOrgSuspend,
			method: http.MethodPost, path: "/api/v1/admin/orgs/" + target.String() + "/suspend",
			body: `{"reason": "Kullanım koşulları ihlali"%s}`, good: good, wrong: wrong,
			resType: authz.TypeOrg, resID: func(uuid.UUID) *uuid.UUID { return &target }, meta: map[string]any{"transition": "suspend"}},
		{name: "payments adminCreateCollectionAccount over identity's Verify", role: authz.PlatformFinanceAdmin,
			action: authz.PaymentsCollectionAccountCreate, method: http.MethodPost, path: "/api/v1/admin/collection-accounts",
			body: `{"label": "Wire hesap", "legal_party_id": "` + wirePlatformParty.String() + `", "iban": "TR330006100519786457841326",
				"holder_name": "Koro Teknoloji A.Ş.", "bank_name": "Örnek Bank"%s}`,
			good: good, wrong: wrong, resType: authz.TypeCollectionAccount,
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
			body := func(proof string) string { return strings.Replace(tc.body, "%s", proof, 1) }
			failures := func() int {
				cur, err := e.mem.User(t.Context(), staff)
				if err != nil {
					t.Fatal(err)
				}
				return cur.FailedLogins
			}
			var resID *uuid.UUID
			if tc.resID != nil {
				resID = tc.resID(staff)
			}
			e.markLogs()

			// Overloaded: no verdict, no record of any action, nothing counted.
			n := len(identityReauthTried(e))
			late.late.Store(true)
			e.overloaded(t, tok, tc.method, tc.path, body(tc.good))
			late.late.Store(false)
			e.quiet(t)
			if rows := identityReauthTried(e)[n:]; len(rows) != 0 {
				t.Fatalf("audit records tried for the overloaded request: %+v", rows)
			}
			if f := failures(); f != 0 {
				t.Fatalf("the overload counted as a wrong password: failed_logins=%d", f)
			}

			// Every refusal is a verdict: exactly one record, the operation's denied one.
			for i, step := range []struct {
				proof  string
				status int
				code   string
				failed int
			}{
				{tc.wrong, http.StatusForbidden, "identity.reauth_failed", 1},
				{"", http.StatusForbidden, "identity.reauth_required", 1},
				{tc.wrong, http.StatusForbidden, "identity.reauth_failed", 2},
				{tc.wrong, http.StatusForbidden, "identity.reauth_failed", 3},
				{tc.wrong, http.StatusForbidden, "identity.reauth_failed", 4},
				{tc.wrong, http.StatusForbidden, "identity.reauth_failed", 5}, // locks the account
				{tc.good, http.StatusTooManyRequests, "rate_limited", 5},
			} {
				n := len(identityReauthTried(e))
				status, code, raw := e.do(t.Context(), tok, tc.method, tc.path, body(step.proof))
				if status != step.status || code != step.code {
					t.Fatalf("refusal %d = %d %s, want %d %s", i+1, status, raw, step.status, step.code)
				}
				rows := identityReauthTried(e)[n:]
				if len(rows) != 1 {
					t.Fatalf("refusal %d (%s): %d audit records of any action, want exactly the operation's denied one: %+v",
						i+1, step.code, len(rows), rows)
				}
				r := rows[0]
				var meta map[string]any
				if err := json.Unmarshal(r.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				switch {
				case r.Action != string(tc.action) || r.Outcome != audit.Denied || r.Reason != audit.ReasonReauthFailed:
					t.Fatalf("refusal %d: %s %s %q, want %s denied reauth_failed", i+1, r.Action, r.Outcome, r.Reason, tc.action)
				case r.ActorUserID == nil || *r.ActorUserID != staff || r.ActorKind != "user":
					t.Fatalf("refusal %d: actor %v (%s), want %s", i+1, r.ActorUserID, r.ActorKind, staff)
				case r.ResourceType != tc.resType || !sameUUID(r.ResourceID, resID) || r.OrgID != nil:
					t.Fatalf("refusal %d: resource %s %v org %v, want %s %v and no org", i+1, r.ResourceType, r.ResourceID, r.OrgID, tc.resType, resID)
				case r.RequestID == "":
					t.Fatalf("refusal %d: the denied record is not tied to its request", i+1)
				case len(meta) != len(tc.meta) || (len(meta) > 0 && !reflect.DeepEqual(meta, tc.meta)):
					t.Fatalf("refusal %d: metadata %v, want %v", i+1, meta, tc.meta)
				}
				if f := failures(); f != step.failed {
					t.Fatalf("refusal %d: failed_logins=%d, want %d", i+1, f, step.failed)
				}
			}
			if out := e.logs.String()[e.mark:]; strings.Contains(out, `"level":"ERROR"`) {
				t.Fatalf("ERROR logged: %s", out)
			}
		})
	}
}

// identityReauthTried returns every audit record tried so far, of any
// action.
func identityReauthTried(e *reauthBudgetEnv) []audit.Record {
	e.audits.mu.Lock()
	defer e.audits.mu.Unlock()
	return slices.Clone(e.audits.tried)
}
