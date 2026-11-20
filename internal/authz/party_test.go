package authz_test

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"uuid"

	"go.yaml.in/yaml/v3"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// ownResourceSurface are the resource-surface actions that run one org's own
// content (a club's profile, an event, its registrations and exports, an
// offering): BE-214 binds them like the org's surface. Transactions between
// two orgs (orders, refund requests, invoices) and the staff decisions of the
// admin surface are not among them.
var ownResourceSurface = []authz.Action{
	authz.DirectoryClubUpdate, authz.EventsEventUpdate, authz.EventsEventCancel, authz.EventsRegistrationList,
	authz.EventsRegistrationRead, authz.EventsExportList, authz.CatalogOfferingRead, authz.CatalogOfferingCancel,
}

// partyPending are actions of the rule whose policy does not apply it yet,
// with the reason. TestOrgSurfaceParty_StaffActsAsMember fails once one of
// them does: drop it from here then.
var partyPending = map[authz.Action]string{}

// staffOnOwnSurface is BE-214's scan: the actions of the rule (the org
// surface of the OpenAPI spec and ownResourceSurface) on which a platform
// role holds a cell for an org it does not belong to. Every one of them is
// asMember-wrapped; a new one must be too (and listed here).
var staffOnOwnSurface = []authz.Action{
	// orgs (pad; sup and pad by break-glass on the member list)
	authz.OrgsOrgUpdate, authz.OrgsOrgClose, authz.OrgsOwnershipTransfer, authz.OrgsMemberList, authz.OrgsMemberRead,
	authz.OrgsMemberRemove, authz.OrgsMemberChangeRole, authz.OrgsInvitationCreate, authz.OrgsInvitationList,
	authz.OrgsInvitationRevoke,
	// audit (dpo, pad), billing, entitlements (pad, fin)
	authz.AuditEventListOrg, authz.BillingInvoiceList, authz.EntitlementsEntitlementList,
	authz.EntitlementsEntitlementRead, authz.EntitlementsUsageList,
	// catalog (pad, fin)
	authz.CatalogOfferingListOrg, authz.CatalogOfferingReviewQueue, authz.CatalogOfferingRead, authz.CatalogOfferingCancel,
	// directory (pad)
	authz.DirectoryClubUpdate,
	// events (mod, sup, pad; sup and pad by break-glass; dpo)
	authz.EventsEventListOrg, authz.EventsEventUpdate, authz.EventsEventCancel, authz.EventsRegistrationList,
	authz.EventsRegistrationRead, authz.EventsExportList,
	// ledger, orders, payments (pad, fin)
	authz.LedgerBalanceRead, authz.LedgerPayoutList, authz.LedgerPayoutRead, authz.OrdersLineListSales,
	authz.PaymentsRefundRequestListSales,
	// outreach (pad)
	authz.OutreachAudienceCount, authz.OutreachDispatchList, authz.OutreachDispatchRead, authz.OutreachDispatchCancel,
	// parties (pad, fin)
	authz.PartiesLegalPartyRead, authz.PartiesLegalPartyCreate, authz.PartiesLegalPartyUpdate, authz.PartiesDocumentUpload,
	authz.PartiesDocumentRead, authz.PartiesPayeeList, authz.PartiesPayeeRead, authz.PartiesBankAccountList,
	authz.PartiesBankAccountSubmit,
	// stats (pad)
	authz.StatsClubRead, authz.StatsCompanyRead, authz.StatsUniversityRead,
	// ingest (pad)
	authz.IngestSuggestionList,
}

// partyRoles are the org roles of each org kind a staff member may hold.
var partyRoles = map[authz.OrgKind][]authz.MemberRole{
	authz.OrgClub:       {authz.MemberMember, authz.MemberClubAdmin},
	authz.OrgUniversity: {authz.MemberUniversityRep, authz.MemberCareerStaff},
	authz.OrgCompany:    {authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner},
}

// allPlatformRoles are the six platform roles (03 §1.1 rows 11–16).
var allPlatformRoles = []authz.PlatformRole{
	authz.PlatformAdmin, authz.PlatformModerator, authz.PlatformSupport, authz.PlatformDPO, authz.PlatformFinanceAdmin,
	authz.PlatformTalentAuditor,
}

// staffWith is an MFA-verified account holding every platform role and ms.
func staffWith(ms ...authz.Membership) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: authztest.ID("party_staff"), SessionID: authztest.ID("party_staff_session"),
		PlatformRoles: slices.Clone(allPlatformRoles), Memberships: ms, StudentStatus: authz.StudentNone, MFAVerified: true}
}

// orgSurfaceActions are the actions of the OpenAPI operations on the org
// surface (x-koro-surface: org; 03 §3.6, §7.6), read from the generated spec.
func orgSurfaceActions(t *testing.T) []authz.Action {
	t.Helper()
	b, err := os.ReadFile("../../openapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	seen := map[authz.Action]bool{}
	for _, item := range spec.Paths {
		for _, v := range item {
			op, ok := v.(map[string]any)
			if !ok || op["x-koro-surface"] != "org" {
				continue
			}
			act, _ := op["x-koro-action"].(string)
			seen[authz.Action(act)] = true
		}
	}
	if !seen[authz.OrgsOwnershipTransfer] || !seen[authz.EventsEventCreate] || len(seen) < 50 {
		t.Fatalf("org surface of the spec = %d actions", len(seen))
	}
	return slices.Sorted(maps.Keys(seen))
}

// partyResources are resources of every type the decision table knows, all
// in org, with every mandatory Attrs key filled, in a few profiles: statuses,
// a sensitive club, a legal party's scopes, a published offering, a
// break-glass reason.
func partyResources(org uuid.UUID, kind authz.OrgKind) []authz.Resource {
	base := map[string]string{
		"org_kind": string(kind), "org_status": "active", "role": string(authz.MemberMember), authz.AttrStatus: "active",
		authz.AttrSensitive: "false", "university_id": authztest.ID("party_university").String(),
		authz.AttrAccessGrant: "active", "discoverable": "true", "minor": "false", "visibility": "public",
		"visibility_university_id": "", authz.AttrEventStaffRole: "none", "event_status": "published",
		"audience_kind": "public", "audience_match": "true", "commercial": "false", "follows_author": "false",
		"company_org_id": org.String(), "kind": "buyer_company", "seller_kind": string(kind),
		authz.AttrOrderSellerOrgIDs: org.String(), authz.AttrOrderLineSellerOrgID: org.String(),
		"recipient_org_id": org.String(), "issuing_mode": "", "purpose": "org_logo", "type": "access",
		authz.AttrAccessReason: "Destek talebinin incelenmesi", authz.AttrSubmittedBy: "", authz.AttrVerifiedBy: "",
		authz.AttrCreatedBy: "", authz.AttrRequestedBy: "", authz.AttrUploadedBy: "", authz.AttrFirstApprover: "",
		authz.AttrOfferingPublished: "false",
	}
	profiles := []map[string]string{
		{},
		{authz.AttrStatus: "draft", authz.AttrSensitive: "true", "scope": authz.PartyScopePayee},
		{authz.AttrStatus: "published", "scope": authz.PartyScopeOrg, authz.AttrOfferingPublished: "true"},
		{authz.AttrStatus: "pending_review", "scope": authz.PartyScopeList},
	}
	types := authz.ResourceTypes()
	var out []authz.Resource
	for _, typ := range slices.Sorted(maps.Keys(types)) {
		if typ == authz.TypeAdminSurface {
			continue // names no org
		}
		for _, p := range profiles {
			attrs := maps.Clone(base)
			maps.Copy(attrs, p)
			id := org
			out = append(out, authz.Resource{Type: typ, ID: id, OrgID: &id, Attrs: attrs})
		}
	}
	return out
}

// TestOrgSurfaceParty_StaffActsAsMember (BE-214, Q-69; T-438): on every
// action of the org surface (the OpenAPI spec's) and of ownResourceSurface,
// on every resource type, the policy gives a platform staff member holding
// every platform role who belongs to the org (each role of each org kind) no
// platform role's path, and exactly the paths it gives the same person
// without the platform roles: only the membership's grants remain. The
// scan: an outsider (no membership) holds a staff cell on exactly
// staffOnOwnSurface.
func TestOrgSurfaceParty_StaffActsAsMember(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	acts := append(orgSurfaceActions(t), ownResourceSurface...)
	outsider := staffWith()
	staffCell := map[authz.Action]bool{}
	broken := map[authz.Action]string{}
	roles := func(gs []authz.Grant, staff bool) []string {
		var out []string
		for _, g := range gs {
			if authz.StaffPath(g) == staff {
				out = append(out, string(g.Role)+map[bool]string{true: "·B"}[g.BreakGlass])
			}
		}
		slices.Sort(out)
		return out
	}
	for _, kind := range []authz.OrgKind{authz.OrgClub, authz.OrgUniversity, authz.OrgCompany} {
		org := authztest.ID("party_" + string(kind))
		for _, r := range partyResources(org, kind) {
			for _, act := range acts {
				if authz.Can(ctx, outsider, act, r) == nil {
					staffCell[act] = true
				}
				for _, role := range partyRoles[kind] {
					party := staffWith(authz.Membership{OrgID: org, OrgKind: kind, Role: role})
					member := party
					member.PlatformRoles = nil
					got := authz.PolicyGrants(party, act, r)
					staff, own := roles(got, true), roles(got, false)
					if want := roles(authz.PolicyGrants(member, act, r), false); (len(staff) > 0 || !slices.Equal(own, want)) &&
						broken[act] == "" {
						broken[act] = fmt.Sprintf("%s of a %s on %s: staff paths %v, member paths %v (want %v)",
							role, kind, r.Type, staff, own, want)
					}
				}
			}
		}
	}
	for _, act := range slices.Sorted(maps.Keys(broken)) {
		if _, ok := partyPending[act]; !ok {
			t.Errorf("%s: a platform staff member who is the org's %s", act, broken[act])
		}
	}
	for act, why := range partyPending {
		if broken[act] == "" {
			t.Errorf("%s binds the party rule now: drop it from partyPending (%s)", act, why)
		}
		delete(staffCell, act)
	}
	if got := slices.Sorted(maps.Keys(staffCell)); !slices.Equal(got, slices.Sorted(slices.Values(staffOnOwnSurface))) {
		t.Errorf("staff cells on an org's own surface = %v\nwant %v", got, slices.Sorted(slices.Values(staffOnOwnSurface)))
	}
}

// TestOrgSurfaceParty_PlatformOrgIsNoPartys (BE-214): the platform org's
// members are the platform staff (staff membership: no grant of its own), so
// the rule takes nothing away there: whatever an outsider's platform roles
// allow on the platform org, a member of it holding them is allowed too, and
// the platform keeps running its own org (profile, members, invitations,
// roles, events, posts, targeting).
func TestOrgSurfaceParty_PlatformOrgIsNoPartys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	platform := authztest.ID("platform_org")
	staff := staffWith(authz.Membership{OrgID: platform, OrgKind: authz.OrgPlatform, Role: authz.MemberStaff})
	outsider := staffWith()
	allowed := map[authz.Action]bool{}
	acts := append(orgSurfaceActions(t), ownResourceSurface...)
	for _, r := range partyResources(platform, authz.OrgPlatform) {
		for _, act := range acts {
			ok := authz.Can(ctx, staff, act, r) == nil
			if !ok && authz.Can(ctx, outsider, act, r) == nil {
				t.Fatalf("%s on %s: the platform org's member lost a staff cell", act, r.Type)
			}
			allowed[act] = allowed[act] || ok
		}
	}
	for _, act := range []authz.Action{
		authz.OrgsOrgUpdate, authz.OrgsMemberList, authz.OrgsMemberRemove, authz.OrgsMemberChangeRole,
		authz.OrgsInvitationCreate, authz.OrgsInvitationRevoke, authz.EventsEventCreate, authz.FeedPostCreate,
		authz.OutreachAudienceCount, authz.CatalogOfferingCreate,
	} {
		if !allowed[act] {
			t.Errorf("%s: the platform org is locked for its own staff", act)
		}
	}
	if !authz.ActsAsStaff(staff, platform, authz.PlatformAdmin) {
		t.Error("ActsAsStaff: the platform org's member is no platform admin there")
	}
}

// TestActsAsStaff (BE-214): a platform role counts on an org the actor does
// not belong to (or on the platform org, or on no org), never on one of its
// own; with no roles named, any platform role counts.
func TestActsAsStaff(t *testing.T) {
	t.Parallel()
	co, other := authztest.ID("company_a"), authztest.ID("company_b")
	pad := persona(t, "platform_admin")
	party := pad
	party.Memberships = []authz.Membership{{OrgID: co, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}
	cases := []struct {
		name  string
		a     authz.Actor
		org   uuid.UUID
		roles []authz.PlatformRole
		want  bool
	}{
		{"outsider", pad, co, []authz.PlatformRole{authz.PlatformAdmin}, true},
		{"party", party, co, []authz.PlatformRole{authz.PlatformAdmin}, false},
		{"party, any role", party, co, nil, false},
		{"party elsewhere", party, other, []authz.PlatformRole{authz.PlatformAdmin}, true},
		{"no org", party, uuid.Nil(), []authz.PlatformRole{authz.PlatformAdmin}, true},
		{"any role", pad, co, nil, true},
		{"another role", pad, co, []authz.PlatformRole{authz.PlatformFinanceAdmin, authz.PlatformDPO}, false},
		{"no platform role", persona(t, "company_admin"), other, nil, false},
		{"anonymous", authz.Anonymous(), co, nil, false},
	}
	for _, tc := range cases {
		if got := authz.ActsAsStaff(tc.a, tc.org, tc.roles...); got != tc.want {
			t.Errorf("%s: ActsAsStaff = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPartyRefused (BE-214): only a refusal the party rule makes reports
// true: a platform admin who is the company's recruiter transferring its
// ownership; not the same refusal of a recruiter without a platform role,
// nor an outsider's refusal for another reason, nor an allowed request, nor
// the moderator's (no cell even as an outsider).
func TestPartyRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	co := orgRes("company_a", authz.OrgCompany, "active")
	join := func(a authz.Actor, role authz.MemberRole) authz.Actor {
		a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: *co.OrgID, OrgKind: authz.OrgCompany, Role: role})
		return a
	}
	refused := func(a authz.Actor, act authz.Action, r authz.Resource) bool {
		return authz.PartyRefused(ctx, a, act, r, authz.Can(ctx, a, act, r))
	}
	pad, mod := persona(t, "platform_admin"), persona(t, "moderator")
	if !refused(join(pad, authz.MemberRecruiter), authz.OrgsOwnershipTransfer, co) {
		t.Error("a party platform admin's transfer is not the party rule's refusal")
	}
	if !refused(join(pad, authz.MemberCompanyAdmin), authz.OrgsOrgVerify, co) {
		t.Error("BE-208's refusal is not the party rule's")
	}
	if refused(persona(t, "company_member"), authz.OrgsOwnershipTransfer, co) {
		t.Error("a recruiter without a platform role was refused by the party rule")
	}
	if refused(join(pad, authz.MemberCompanyOwner), authz.OrgsOwnershipTransfer, co) {
		t.Error("an allowed transfer reported a refusal")
	}
	if refused(join(mod, authz.MemberRecruiter), authz.OrgsOwnershipTransfer, co) {
		t.Error("the moderator has no cell as an outsider either: no party refusal")
	}
	noMFA := join(pad, authz.MemberRecruiter)
	noMFA.MFAVerified = false
	if refused(noMFA, authz.OrgsOwnershipTransfer, co) {
		t.Error("an MFA refusal was reported as the party rule's")
	}
}
