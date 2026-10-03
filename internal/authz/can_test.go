package authz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/problem"
)

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }

// member returns a user without MFA holding one org role.
func member(kind authz.OrgKind, role authz.MemberRole) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: authztest.ID("member_" + string(role)),
		Memberships: []authz.Membership{{OrgID: authztest.ID("org_" + string(kind)), OrgKind: kind, Role: role}}}
}

// platform returns a user without MFA holding one platform role.
func platform(r authz.PlatformRole) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: authztest.ID("platform_" + string(r)), PlatformRoles: []authz.PlatformRole{r}}
}

// rolePaths returns every role path an actor holds, independent of the
// resource: the grants of a policy that allows everything except public
// access.
func rolePaths(a authz.Actor) []authz.Grant {
	if !a.IsUser() {
		return nil
	}
	gs := []authz.Grant{{Role: authz.RoleSelf}}
	switch {
	case !a.IsVerifiedStudent():
		gs = append(gs, authz.Grant{Role: authz.RoleUserUnverified})
	case a.IsMinor:
		gs = append(gs, authz.Grant{Role: authz.RoleStudentMinor})
	case a.IsVerifiedStudent():
		gs = append(gs, authz.Grant{Role: authz.RoleStudent})
	}
	byPlatform := map[authz.PlatformRole]authz.Role{
		authz.PlatformAdmin: authz.RolePlatformAdmin, authz.PlatformModerator: authz.RoleModerator,
		authz.PlatformSupport: authz.RoleSupport, authz.PlatformDPO: authz.RoleDPO,
		authz.PlatformFinanceAdmin: authz.RoleFinanceAdmin, authz.PlatformTalentAuditor: authz.RoleTalentAuditor,
	}
	for _, r := range a.PlatformRoles {
		gs = append(gs, authz.Grant{Role: byPlatform[r]})
	}
	byMember := map[authz.MemberRole]authz.Role{
		authz.MemberMember: authz.RoleClubMember, authz.MemberClubAdmin: authz.RoleClubAdmin,
		authz.MemberUniversityRep: authz.RoleUniversityRep, authz.MemberCareerStaff: authz.RoleCareerStaff,
		authz.MemberRecruiter: authz.RoleCompanyMember, authz.MemberCompanyAdmin: authz.RoleCompanyAdmin,
		authz.MemberCompanyOwner: authz.RoleCompanyOwner,
	}
	for _, m := range a.Memberships {
		if r, ok := byMember[m.Role]; ok {
			gs = append(gs, authz.Grant{Role: r})
		}
	}
	return gs
}

// allowAll grants every role path of the actor on every visible resource.
var allowAll = authz.Policy{
	Visible: func(authz.Actor, authz.Action, authz.Resource) bool { return true },
	Grants:  func(a authz.Actor, _ authz.Action, _ authz.Resource) []authz.Grant { return rolePaths(a) },
}

func allowAllFor(modules ...string) map[string]authz.Policy {
	ps := map[string]authz.Policy{}
	for _, m := range modules {
		ps[m] = allowAll
	}
	return ps
}

// grantOnly is a visible-everything policy granting exactly gs.
func grantOnly(gs ...authz.Grant) authz.Policy {
	return authz.Policy{
		Visible: func(authz.Actor, authz.Action, authz.Resource) bool { return true },
		Grants:  func(authz.Actor, authz.Action, authz.Resource) []authz.Grant { return gs },
	}
}

var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

func wantErr(t *testing.T, err, want error, internal bool) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("Can() = %v, want nil", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("Can() = %v, want %v", err, want)
	}
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("Can() error %T is not a problem.Error", err)
	}
	d, ok := authz.DenialOf(err)
	if !ok {
		t.Fatalf("Can() = %v carries no Denial", err)
	}
	if d.Internal != internal {
		t.Fatalf("Denial.Internal = %v, want %v (%v)", d.Internal, internal, d)
	}
}

func TestCan_UnknownAction_Denied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	can := authz.CanWith(allowAllFor(authz.Modules()...))
	pad := persona(t, "platform_admin")
	for _, act := range []authz.Action{"", "nope.thing.read", "events.event.fly", "events.event.create.extra"} {
		wantErr(t, authz.Can(ctx, pad, act, adminSurface), authz.ErrForbidden, true)
		wantErr(t, can(ctx, pad, act, adminSurface), authz.ErrForbidden, true)
		wantErr(t, can(ctx, authz.SystemActor("job"), act, adminSurface), authz.ErrForbidden, true)
	}
	// The same core rejects unknown resource types, missing Attrs keys and
	// an invalid (zero) actor, whatever the policy grants.
	wantErr(t, can(ctx, pad, authz.EventsEventRead, authz.Resource{Type: "evnt"}), authz.ErrForbidden, true)
	wantErr(t, can(ctx, pad, authz.EventsEventRead, authz.Resource{Type: authz.TypeEvent, Attrs: map[string]string{"status": "draft"}}),
		authz.ErrForbidden, true)
	wantErr(t, can(ctx, authz.Actor{}, authz.EventsEventList, adminSurface), authz.ErrForbidden, true)
	wantErr(t, can(ctx, pad, authz.EventsEventList, adminSurface), nil, false)
}

func TestCan_DefaultDeny_UnwrittenPolicies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	actors := []authz.Actor{}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		actors = append(actors, p.Actor)
	}
	for _, m := range authz.Modules() {
		if authz.PolicyWritten(m) {
			continue // the module ticket owns its policy tests from now on
		}
		for _, act := range authz.ModuleActions(m) {
			for _, a := range actors {
				err := authz.Can(ctx, a, act, adminSurface)
				if err == nil {
					t.Fatalf("%s as %s: stub policy allowed", act, a)
				}
				if d, _ := authz.DenialOf(err); !errors.Is(err, authz.ErrForbidden) || !d.Internal {
					t.Fatalf("%s as %s: %v, want ErrForbidden (policy not written)", act, a, err)
				}
			}
		}
	}
}

func TestCan_SystemActor_OnlyAllowList(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sysAllowed := map[authz.Action]bool{}
	for _, a := range authz.SystemActions() {
		sysAllowed[a] = true
	}
	// A policy that grants everything to everyone must not widen the
	// system path, and the system path ignores module policies.
	can := authz.CanWith(allowAllFor(authz.Modules()...))
	sys := authz.SystemActor("events.attendance_autofinalize")
	for _, act := range authz.Actions() {
		for name, fn := range map[string]func() error{
			"Can":     func() error { return authz.Can(ctx, sys, act, authz.Resource{}) },
			"CanWith": func() error { return can(ctx, sys, act, authz.Resource{}) },
		} {
			err := fn()
			if sysAllowed[act] {
				wantErr(t, err, nil, false)
			} else if !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("%s: system actor on %s = %v, want ErrForbidden", name, act, err)
			}
		}
	}
	wantErr(t, authz.Can(ctx, authz.SystemActor(""), authz.KvkkRetentionSweep, authz.Resource{}), authz.ErrForbidden, true)

	// System-only actions are never granted to users, whatever the policy.
	pad := persona(t, "platform_admin")
	for _, act := range authz.SystemActions() {
		err := can(ctx, pad, act, adminSurface)
		if act == authz.EventsAttendanceFinalize {
			wantErr(t, err, nil, false) // also an HTTP action (03 §5.6): policy decides
			continue
		}
		wantErr(t, err, authz.ErrForbidden, false)
	}
}

func TestCan_TalentAbsoluteBan_PlatformAdmin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	subject := authz.Resource{Type: authz.TypeTalentSubject, OwnerUserID: ptr(authztest.ID("student"))}
	deciders := map[string]func(context.Context, authz.Actor, authz.Action, authz.Resource) error{
		"allow all role paths": authz.CanWith(allowAllFor("talent")),
		"explicit platform_admin grant": authz.CanWith(map[string]authz.Policy{"talent": grantOnly(
			authz.Grant{Role: authz.RolePlatformAdmin}, authz.Grant{Role: authz.RoleDPO}, authz.Grant{Role: authz.RoleTalentAuditor})}),
	}
	for name, decide := range deciders {
		for _, act := range authz.ModuleActions("talent") {
			for _, r := range []authz.Resource{adminSurface, subject} {
				for _, p := range []string{"platform_admin", "dpo", "support", "finance_admin", "moderator", "student"} {
					err := decide(ctx, persona(t, p), act, r)
					if !errors.Is(err, authz.ErrForbidden) {
						t.Fatalf("%s: %s as %s = %v, want ErrForbidden (Y1)", name, act, p, err)
					}
				}
				wantErr(t, decide(ctx, persona(t, "talent_auditor"), act, r), nil, false)
			}
		}
	}
	// Two roles at once: platform_admin + talent_auditor is allowed through
	// the talent_auditor path only.
	both := persona(t, "platform_admin")
	both.PlatformRoles = append(both.PlatformRoles, authz.PlatformTalentAuditor)
	wantErr(t, authz.CanWith(allowAllFor("talent"))(ctx, both, authz.TalentReportRead, subject), nil, false)
}

func TestCan_FourEyes_SameUserRejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fin := persona(t, "finance_admin")
	fin2 := authztest.ID("finance_admin_2").String()
	can := authz.CanWith(map[string]authz.Policy{
		"ledger": grantOnly(authz.Grant{Role: authz.RoleFinanceAdmin}),
		"kvkk":   grantOnly(authz.Grant{Role: authz.RoleDPO}, authz.Grant{Role: authz.RolePlatformAdmin}),
		"events": grantOnly(authz.Grant{Role: authz.RoleClubAdmin, FourEyes: []string{"approved_by"}}),
	})
	payout := func(createdBy string) authz.Resource {
		return authz.Resource{Type: authz.TypePayout, Attrs: map[string]string{"status": "pending_approval", "created_by": createdBy}}
	}
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, payout(fin.UserID.String())), authz.ErrFourEyes, false)
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, payout(strings.ToUpper(fin.UserID.String()))), authz.ErrFourEyes, false)
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, payout(fin2)), nil, false)
	// The initiator must be known: an empty or missing key fails closed.
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, payout("")), authz.ErrForbidden, true)
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, authz.Resource{Type: authz.TypeAdminSurface}), authz.ErrForbidden, true)
	wantErr(t, can(ctx, fin, authz.LedgerPayoutApprove, payout("not-a-uuid")), authz.ErrForbidden, true)
	// Four eyes applies to creating the payout? No: only approve is listed.
	wantErr(t, can(ctx, fin, authz.LedgerPayoutCreate, adminSurface), nil, false)

	// Two-step DSR approval: the first approval has no first_approver.
	dpo := persona(t, "dpo")
	dsr := func(first string) authz.Resource {
		return authz.Resource{Type: authz.TypeDSR, OwnerUserID: ptr(authztest.ID("student")),
			Attrs: map[string]string{"type": "profiling_access", "status": "awaiting_second_approval", "first_approver": first}}
	}
	wantErr(t, can(ctx, dpo, authz.KvkkDsrApprove, dsr("")), nil, false)
	wantErr(t, can(ctx, dpo, authz.KvkkDsrApprove, dsr(dpo.UserID.String())), authz.ErrFourEyes, false)
	wantErr(t, can(ctx, dpo, authz.KvkkDsrApprove, dsr(authztest.ID("dpo_2").String())), nil, false)

	// A grant may add its own four-eyes keys.
	cla := persona(t, "club_admin")
	r := authz.Resource{Type: authz.TypeAdminSurface, Attrs: map[string]string{"approved_by": cla.UserID.String()}}
	wantErr(t, can(ctx, cla, authz.EventsEventPublish, r), authz.ErrFourEyes, false)
	r.Attrs["approved_by"] = ""
	wantErr(t, can(ctx, cla, authz.EventsEventPublish, r), nil, false)
	wantErr(t, can(ctx, cla, authz.EventsEventPublish, adminSurface), authz.ErrForbidden, true)
}

func TestCan_MFARequiredRole_WithoutMFA(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	can := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(authz.Grant{Role: authz.RolePlatformAdmin})})
	pad := persona(t, "platform_admin")
	noMFA := pad
	noMFA.MFAVerified = false
	wantErr(t, can(ctx, pad, authz.OrgsOrgVerify, adminSurface), nil, false)
	wantErr(t, can(ctx, noMFA, authz.OrgsOrgVerify, adminSurface), authz.ErrMFARequired, false)

	// Every MFA role (k-mfa, 03 §6.4) and only those, each held by an actor
	// whose session is not MFA verified.
	clubA := authztest.ID("club_a")
	cases := []struct {
		role authz.Role
		mfa  bool
		a    authz.Actor
	}{
		{authz.RoleUniversityRep, true, member(authz.OrgUniversity, authz.MemberUniversityRep)},
		{authz.RoleCareerStaff, false, member(authz.OrgUniversity, authz.MemberCareerStaff)},
		{authz.RoleCompanyAdmin, true, member(authz.OrgCompany, authz.MemberCompanyAdmin)},
		{authz.RoleCompanyAdmin, true, member(authz.OrgCompany, authz.MemberCompanyOwner)},
		{authz.RoleCompanyOwner, true, member(authz.OrgCompany, authz.MemberCompanyOwner)},
		{authz.RoleCompanyMember, false, member(authz.OrgCompany, authz.MemberRecruiter)},
		{authz.RoleClubAdmin, false, member(authz.OrgClub, authz.MemberClubAdmin)},
		{authz.RoleClubMember, false, member(authz.OrgClub, authz.MemberMember)},
		{authz.RoleModerator, true, platform(authz.PlatformModerator)},
		{authz.RoleSupport, true, platform(authz.PlatformSupport)},
		{authz.RoleDPO, true, platform(authz.PlatformDPO)},
		{authz.RolePlatformAdmin, true, platform(authz.PlatformAdmin)},
		{authz.RoleFinanceAdmin, true, platform(authz.PlatformFinanceAdmin)},
		{authz.RoleTalentAuditor, true, platform(authz.PlatformTalentAuditor)},
		{authz.RoleStudent, false, persona(t, "student")},
		{authz.RoleUserUnverified, false, persona(t, "user_unverified")},
	}
	for _, c := range cases {
		one := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(authz.Grant{Role: c.role})})
		err := one(ctx, c.a, authz.OrgsOrgUpdate, authz.Resource{Type: authz.TypeAdminSurface, OrgID: &clubA})
		if c.mfa {
			wantErr(t, err, authz.ErrMFARequired, false)
		} else {
			wantErr(t, err, nil, false)
		}
	}
	// Union (03 §1.3): a non-MFA path of the same actor allows.
	union := authz.CanWith(map[string]authz.Policy{"identity": grantOnly(
		authz.Grant{Role: authz.RolePlatformAdmin}, authz.Grant{Role: authz.RoleSelf})})
	own := authz.Resource{Type: authz.TypeUser, OwnerUserID: ptr(noMFA.UserID)}
	wantErr(t, union(ctx, noMFA, authz.IdentityAccountReadOwn, own), nil, false)
	wantErr(t, union(ctx, noMFA, authz.IdentityAccountReadOwn, adminSurface), authz.ErrMFARequired, false)
	// Action-level M on a non-MFA role.
	m := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(authz.Grant{Role: authz.RoleClubAdmin, MFA: true})})
	wantErr(t, m(ctx, persona(t, "club_admin"), authz.OrgsOrgUpdate, adminSurface), authz.ErrMFARequired, false)
	// Condition order: MFA before four eyes before break-glass.
	order := authz.CanWith(map[string]authz.Policy{"ledger": grantOnly(
		authz.Grant{Role: authz.RoleSupport, BreakGlass: true}, authz.Grant{Role: authz.RoleFinanceAdmin})})
	fin := persona(t, "finance_admin")
	fin.MFAVerified = false
	r := authz.Resource{Type: authz.TypePayout, Attrs: map[string]string{"status": "x", "created_by": fin.UserID.String()}}
	wantErr(t, order(ctx, fin, authz.LedgerPayoutApprove, r), authz.ErrMFARequired, false)
	fin.MFAVerified = true
	wantErr(t, order(ctx, fin, authz.LedgerPayoutApprove, r), authz.ErrFourEyes, false)
}

func TestCan_DecisionOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	hidden := authz.Policy{
		Visible: func(authz.Actor, authz.Action, authz.Resource) bool { return false },
		Grants:  func(a authz.Actor, _ authz.Action, _ authz.Resource) []authz.Grant { return rolePaths(a) },
	}
	can := authz.CanWith(map[string]authz.Policy{"directory": hidden, "orgs": grantOnly(), "events": grantOnly(authz.Grant{Role: authz.RolePublic})})
	stu := persona(t, "student")
	anon := authz.Anonymous()
	wantErr(t, can(ctx, stu, authz.DirectoryClubRead, adminSurface), authz.ErrNotFound, false)
	wantErr(t, can(ctx, anon, authz.OrgsOrgUpdate, adminSurface), authz.ErrForbidden, false) // anonymous before visibility
	wantErr(t, can(ctx, stu, authz.OrgsOrgUpdate, adminSurface), authz.ErrForbidden, false)  // visible, no grant
	wantErr(t, can(ctx, anon, authz.EventsEventList, adminSurface), nil, false)              // public grant
	wantErr(t, can(ctx, stu, authz.EventsEventList, adminSurface), nil, false)
	// A Visible-less policy hides everything (fail closed).
	nilVis := authz.CanWith(map[string]authz.Policy{"orgs": {Grants: grantOnly(authz.Grant{Role: authz.RoleSelf}).Grants}})
	wantErr(t, nilVis(ctx, stu, authz.OrgsOrgUpdate, adminSurface), authz.ErrNotFound, false)
	// Malformed grants are programmer errors.
	for _, g := range []authz.Grant{{Role: "root"}, {Role: authz.RolePublic, MFA: true}, {Role: authz.RoleClubAdmin, BreakGlass: true}} {
		bad := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(g)})
		wantErr(t, bad(ctx, stu, authz.OrgsOrgUpdate, adminSurface), authz.ErrForbidden, true)
	}
}

func TestCan_BreakGlass_ReasonAndSensitiveClub(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	can := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(
		authz.Grant{Role: authz.RoleSupport, BreakGlass: true}, authz.Grant{Role: authz.RoleClubAdmin})})
	sup := persona(t, "support")
	club := func(sensitive, reason string) authz.Resource {
		return authz.Resource{Type: authz.TypeClub, OrgID: ptr(authztest.ID("club_a")),
			Attrs: map[string]string{"sensitive": sensitive, "university_id": authztest.UniversityID("uni_a").String(), "access_reason": reason}}
	}
	wantErr(t, can(ctx, sup, authz.OrgsMemberList, club("false", "")), authz.ErrBreakGlassReason, false)
	wantErr(t, can(ctx, sup, authz.OrgsMemberList, club("false", "too short")), authz.ErrBreakGlassReason, false)
	wantErr(t, can(ctx, sup, authz.OrgsMemberList, club("false", strings.Repeat("ç", 501))), authz.ErrBreakGlassReason, false)
	wantErr(t, can(ctx, sup, authz.OrgsMemberList, club("false", "Ticket 4711: login loop")), nil, false)
	// Y2: break-glass never reveals sensitive club data → 404, even with a reason.
	wantErr(t, can(ctx, sup, authz.OrgsMemberList, club("true", "Ticket 4711: login loop")), authz.ErrNotFound, false)
	wantErr(t, can(ctx, persona(t, "club_admin"), authz.OrgsMemberList, club("true", "")), nil, false)

	// Y2 on a single sensitive membership: only the member and the club's admin.
	all := authz.CanWith(allowAllFor("orgs"))
	member := func(owner string) authz.Resource {
		return authz.Resource{Type: authz.TypeOrgMember, OrgID: ptr(authztest.ID("club_sensitive")), OwnerUserID: ptr(authztest.ID(owner)),
			Attrs: map[string]string{"org_kind": "club", "role": "member", "status": "active", "sensitive": "true"}}
	}
	wantErr(t, all(ctx, persona(t, "club_member"), authz.OrgsMembershipLeave, member("club_member")), nil, false)
	wantErr(t, all(ctx, persona(t, "club_admin_sensitive"), authz.OrgsMemberRead, member("club_member")), nil, false)
	for _, p := range []string{"platform_admin", "support", "dpo", "club_admin", "student"} {
		wantErr(t, all(ctx, persona(t, p), authz.OrgsMemberRead, member("club_member")), authz.ErrNotFound, false)
	}
}

func TestCan_MinorCompanyAndSelfBans(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	all := authz.CanWith(allowAllFor("identity", "directory"))
	// Y4: a minor's club_admin membership grants nothing.
	clubAdminOnly := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(authz.Grant{Role: authz.RoleClubAdmin})})
	minor := persona(t, "club_admin")
	minor.IsMinor = true
	wantErr(t, clubAdminOnly(ctx, minor, authz.OrgsMemberDecide, adminSurface), authz.ErrForbidden, false)
	wantErr(t, clubAdminOnly(ctx, persona(t, "club_admin"), authz.OrgsMemberDecide, adminSurface), nil, false)

	// Y5: company roles reach student data only with level 3 access; 404 otherwise.
	profile := func(grant string) authz.Resource {
		return authz.Resource{Type: authz.TypeStudentProfile, OwnerUserID: ptr(authztest.ID("student_granted")),
			Attrs: map[string]string{"access_grant": grant, "discoverable": "false", "minor": "false"}}
	}
	companies := authz.CanWith(map[string]authz.Policy{"students": grantOnly(
		authz.Grant{Role: authz.RoleCompanyMember}, authz.Grant{Role: authz.RoleCompanyAdmin}, authz.Grant{Role: authz.RoleCompanyOwner})})
	for _, p := range []string{"company_member", "company_admin", "company_owner_a"} {
		wantErr(t, companies(ctx, persona(t, p), authz.StudentsProfileReadCompany, profile("none")), authz.ErrNotFound, false)
		wantErr(t, companies(ctx, persona(t, p), authz.StudentsProfileReadCompany, profile("active")), nil, false)
	}
	wantErr(t, companies(ctx, persona(t, "student"), authz.StudentsProfileReadCompany, profile("active")), authz.ErrForbidden, false)

	// A verified minor never takes the usr path (03 §1.1): min ✗ cells hold.
	usr := authz.CanWith(map[string]authz.Policy{"orgs": grantOnly(authz.Grant{Role: authz.RoleUserUnverified})})
	wantErr(t, usr(ctx, persona(t, "student_minor"), authz.OrgsCompanyCreate, adminSurface), authz.ErrForbidden, false)
	wantErr(t, usr(ctx, persona(t, "user_unverified"), authz.OrgsCompanyCreate, adminSurface), nil, false)
	wantErr(t, usr(ctx, persona(t, "moderator"), authz.OrgsCompanyCreate, adminSurface), nil, false)

	// Y6: no self-approval, whatever the policy grants.
	pad := persona(t, "platform_admin")
	self := authz.Resource{Type: authz.TypePlatformRoleAssignment, OwnerUserID: ptr(pad.UserID),
		Attrs: map[string]string{"role": "finance_admin", "requested_by": ""}}
	wantErr(t, all(ctx, pad, authz.IdentityPlatformRoleGrant, self), authz.ErrForbidden, false)
	other := self
	other.OwnerUserID = ptr(authztest.ID("student"))
	wantErr(t, all(ctx, pad, authz.IdentityPlatformRoleGrant, other), nil, false)
	// H-17: the second admin approves; the requester cannot.
	other.Attrs = map[string]string{"role": "finance_admin", "requested_by": pad.UserID.String()}
	wantErr(t, all(ctx, pad, authz.IdentityPlatformRoleGrant, other), authz.ErrFourEyes, false)
	wantErr(t, all(ctx, pad, authz.IdentityUserResetMfa, authz.Resource{Type: authz.TypeUser, OwnerUserID: ptr(pad.UserID)}), authz.ErrForbidden, false)
	wantErr(t, all(ctx, pad, authz.DirectoryClubClaimDecide, authz.Resource{Type: authz.TypeClubClaim, Attrs: map[string]string{"status": "pending"}}),
		authz.ErrForbidden, true) // the applicant (owner) must be set
}

func TestPolicies_TableIsConsistent(t *testing.T) {
	t.Parallel()
	if err := authz.NewEngineErr(nil); err != nil {
		t.Fatalf("real policy table rejected: %v", err)
	}
	dupCore := map[string]authz.Policy{"events": {Types: map[string][]string{authz.TypeEvent: nil}}}
	if err := authz.NewEngineErr(dupCore); err == nil {
		t.Fatal("a policy redeclaring a 03 §3.5 type must be rejected")
	}
	dup := map[string]authz.Policy{
		"events": {Types: map[string][]string{"event_series": nil}},
		"feed":   {Types: map[string][]string{"event_series": nil}},
	}
	if err := authz.NewEngineErr(dup); err == nil {
		t.Fatal("two policies declaring the same type must be rejected")
	}
	// A module-declared type is known, with its mandatory Attrs.
	ctx := t.Context()
	can := authz.CanWith(map[string]authz.Policy{"events": {
		Types:   map[string][]string{"event_series": {"status"}},
		Visible: allowAll.Visible, Grants: allowAll.Grants,
	}})
	stu := persona(t, "student")
	wantErr(t, can(ctx, stu, authz.EventsEventRead, authz.Resource{Type: "event_series"}), authz.ErrForbidden, true)
	wantErr(t, can(ctx, stu, authz.EventsEventRead, authz.Resource{Type: "event_series", Attrs: map[string]string{"status": "x"}}), nil, false)
	if len(authz.CoreTypes()) != 49 {
		t.Fatalf("03 §3.5 lists 49 resource types, core has %d", len(authz.CoreTypes()))
	}
}

func TestActor_NeverSerialised(t *testing.T) {
	t.Parallel()
	a := persona(t, "club_member")
	secret := authztest.ID("club_sensitive").String()
	if _, err := json.Marshal(a); err == nil {
		t.Fatal("json.Marshal(Actor) must fail")
	}
	if _, err := json.Marshal(struct{ A authz.Actor }{a}); err == nil {
		t.Fatal("json.Marshal of a struct holding an Actor must fail")
	}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(f, a); strings.Contains(s, secret) {
			t.Fatalf("%s leaks memberships: %s", f, s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", slog.Any("actor", a))
	if strings.Contains(buf.String(), secret) || !strings.Contains(buf.String(), a.UserID.String()) {
		t.Fatalf("log output: %s", buf.String())
	}
}

func TestActorFrom_Context(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if _, err := authz.ActorFrom(ctx); !errors.Is(err, problem.ErrUnauthenticated) {
		t.Fatalf("ActorFrom(empty) = %v, want unauthenticated", err)
	}
	if a := authz.ActorOrAnonymous(ctx); a.Kind != authz.KindAnonymous {
		t.Fatalf("ActorOrAnonymous(empty) = %v", a)
	}
	stu := persona(t, "student")
	ctx = authz.WithActor(ctx, stu)
	if a, err := authz.ActorFrom(ctx); err != nil || a.UserID != stu.UserID {
		t.Fatalf("ActorFrom = %v, %v", a, err)
	}
	if a := authz.ActorOrAnonymous(ctx); a.UserID != stu.UserID {
		t.Fatalf("ActorOrAnonymous = %v", a)
	}
}
