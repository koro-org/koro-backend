package authz_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// clubRes is the club resource the directory service builds (03 §3.5:
// OrgID = the club's org; Attrs sensitive, university_id; 06 H-27).
func clubRes(name string, sensitive bool) authz.Resource {
	id := authztest.ID(name)
	s := "false"
	if sensitive {
		s = "true"
	}
	return authz.Resource{Type: authz.TypeClub, ID: id, OrgID: &id, Attrs: map[string]string{
		"sensitive": s, "university_id": authztest.UniversityID("uni_a").String(),
	}}
}

func uniRes(active string) authz.Resource {
	org := authztest.ID("uni_a")
	return authz.Resource{Type: authz.TypeUniversity, ID: authztest.UniversityID("uni_a"), OrgID: &org,
		Attrs: map[string]string{"is_active": active}}
}

var dirCatalog = authz.Resource{Type: authz.TypeDirectoryCatalog}

// claimRes is the club claim resource the directory service builds (03
// §3.5: OrgID = the club's org, OwnerUserID = the claimant, Attrs status,
// university_org_id and approver) for a claim on a uni_a club.
func claimRes(claimant, approver string) authz.Resource {
	return claimResAt(claimant, approver, "uni_a")
}

func claimResAt(claimant, approver, uni string) authz.Resource {
	org, owner := authztest.ID("club_unclaimed"), authztest.ID(claimant)
	return authz.Resource{Type: authz.TypeClubClaim, ID: authztest.ID("claim_pending"), OrgID: &org, OwnerUserID: &owner,
		Attrs: map[string]string{"status": "pending", authz.AttrClaimUniversityOrg: authztest.ID(uni).String(),
			authz.AttrClaimApprover: approver}}
}

// claimGate is the resource of a decision before the claim is read.
var claimGate = authz.Resource{Type: authz.TypeClubClaim, OwnerUserID: new(uuid.Nil()), Attrs: map[string]string{
	"status": "", authz.AttrClaimUniversityOrg: "", authz.AttrClaimApprover: authz.ClaimApproverUnread,
}}

const (
	byRep = authz.ClaimApproverRep
	byPad = authz.ClaimApproverPlatformAdmin
)

// TestDirectoryPolicy_Matrix pins 03 §5.3 (BE-020's 12 actions, BE-022's 5) for the 18
// personas; testdata/authz/directory.yaml checks the same through the API.
func TestDirectoryPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fb, nf := authz.ErrForbidden, authz.ErrNotFound
	only := func(allowed map[string]error, rest error) func(string) error {
		return func(p string) error {
			if e, ok := allowed[p]; ok {
				return e
			}
			return rest
		}
	}
	admin := only(map[string]error{"platform_admin": nil}, fb)
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  authz.Resource
		want func(string) error
	}{
		{"university.list", authz.DirectoryUniversityList, dirCatalog, only(nil, nil)},
		{"university.read active", authz.DirectoryUniversityRead, uniRes("true"), only(nil, nil)},
		{"university.read inactive", authz.DirectoryUniversityRead, uniRes("false"), only(map[string]error{"platform_admin": nil}, nf)},
		{"department.list", authz.DirectoryDepartmentList, uniRes("true"), only(nil, nil)},
		{"skill.list", authz.DirectorySkillList, dirCatalog, only(nil, nil)},
		{"club.list", authz.DirectoryClubList, dirCatalog, only(nil, nil)},
		{"club.read", authz.DirectoryClubRead, clubRes("club_a", false), only(nil, nil)},
		{"club.read sensitive", authz.DirectoryClubRead, clubRes("club_sensitive", true), only(nil, nil)},
		{"club.update club_a", authz.DirectoryClubUpdate, clubRes("club_a", false),
			only(map[string]error{"club_admin": nil, "platform_admin": nil}, fb)},
		{"club.update club_b", authz.DirectoryClubUpdate, clubRes("club_b", false),
			only(map[string]error{"other_club_admin": nil, "platform_admin": nil}, fb)},
		{"club.request_sensitivity", authz.DirectoryClubRequestSensitivity, clubRes("club_a", false),
			only(map[string]error{"club_admin": nil}, fb)},
		{"club.create", authz.DirectoryClubCreate, adminSurface, admin},
		{"club.set_sensitivity", authz.DirectoryClubSetSensitivity, adminSurface, admin},
		{"university.manage", authz.DirectoryUniversityManage, adminSurface, admin},
		{"skill.manage", authz.DirectorySkillManage, adminSurface, admin},
		// k8: adult verified students; the university match is the service's 422.
		{"club_claim.create", authz.DirectoryClubClaimCreate, clubRes("club_unclaimed", false),
			only(map[string]error{"student": nil, "club_member": nil, "club_admin": nil, "other_club_admin": nil,
				"event_staff": nil}, fb)},
		{"club_claim.create sensitive club", authz.DirectoryClubClaimCreate, clubRes("club_sensitive", true),
			only(map[string]error{"student": nil, "club_member": nil, "club_admin": nil, "other_club_admin": nil,
				"event_staff": nil}, fb)},
		{"club_claim.read_own", authz.DirectoryClubClaimReadOwn, claimRes("student", byRep),
			only(map[string]error{"anonymous": fb, "student": nil}, nf)},
		{"club_claim.withdraw", authz.DirectoryClubClaimWithdraw, claimRes("student", byRep),
			only(map[string]error{"anonymous": fb, "student": nil}, nf)},
		{"club_claim.list queue", authz.DirectoryClubClaimList, adminSurface,
			only(map[string]error{"support": nil, "platform_admin": nil}, fb)},
		{"club_claim.list gate", authz.DirectoryClubClaimList, claimGate,
			only(map[string]error{"support": nil, "platform_admin": nil, "university_rep": nil}, fb)},
		{"club_claim.list rep approves", authz.DirectoryClubClaimList, claimRes("student", byRep),
			only(map[string]error{"support": nil, "platform_admin": nil, "university_rep": nil}, fb)},
		{"club_claim.list platform_admin approves", authz.DirectoryClubClaimList, claimRes("student", byPad),
			only(map[string]error{"support": nil, "platform_admin": nil, "university_rep": nf}, fb)},
		{"club_claim.decide gate", authz.DirectoryClubClaimDecide, claimGate,
			only(map[string]error{"platform_admin": nil, "university_rep": nil}, fb)},
		// E-21: the university's rep decides; platform_admin only where there is none (or the club is sensitive).
		{"club_claim.decide rep approves", authz.DirectoryClubClaimDecide, claimRes("student", byRep),
			only(map[string]error{"university_rep": nil}, fb)},
		{"club_claim.decide platform_admin approves", authz.DirectoryClubClaimDecide, claimRes("student", byPad),
			only(map[string]error{"platform_admin": nil, "university_rep": nf}, fb)},
		{"club_claim.decide another university's rep", authz.DirectoryClubClaimDecide, claimResAt("other_club_admin", byRep, "uni_b"),
			only(map[string]error{"university_rep": nf}, fb)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				t.Run(p.Name, func(t *testing.T) {
					want := tc.want(p.Name)
					// Anonymous never passes a non-public action (401 at the
					// middleware; Can says 403).
					if p.Name == "anonymous" && want != nil && !errors.Is(want, nf) {
						want = fb
					}
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res), want, false)
				})
			}
		})
	}
}

func TestDirectoryPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	club := clubRes("club_a", false)
	// A minor club_admin is no club_admin (Y4).
	minorAdmin := persona(t, "club_admin")
	minorAdmin.IsMinor = true
	wantErr(t, authz.Can(ctx, minorAdmin, authz.DirectoryClubUpdate, club), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, minorAdmin, authz.DirectoryClubRequestSensitivity, club), authz.ErrForbidden, false)
	// The platform admin's path needs an MFA session (k-mfa).
	pad := persona(t, "platform_admin")
	pad.MFAVerified = false
	wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubSetSensitivity, adminSurface), authz.ErrMFARequired, false)
	// Missing Attrs are a programmer error (deny).
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.DirectoryClubUpdate,
		authz.Resource{Type: authz.TypeClub, OrgID: club.OrgID}), authz.ErrForbidden, true)
	// A wrong resource type hides the club (safe default).
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.DirectoryClubRead, dirCatalog), authz.ErrNotFound, false)
}

// TestDirectoryPolicy_ClubClaims pins the club claim edges (03 §5.3 k8, Y4,
// Y6, k-mfa; BE-022).
func TestDirectoryPolicy_ClubClaims(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pad := persona(t, "platform_admin")
	rep := persona(t, "university_rep")
	// Y6: nobody decides their own claim, whatever the policy grants.
	wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubClaimDecide, claimRes("platform_admin", byPad)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, rep, authz.DirectoryClubClaimDecide, claimRes("university_rep", byRep)), authz.ErrForbidden, false)
	// The gate value is only valid before the claim is read (nil owner): a
	// read claim that forgot its approver grants nothing (fail closed).
	wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubClaimDecide, claimRes("student", authz.ClaimApproverUnread)),
		authz.ErrForbidden, false)
	for _, approver := range []string{"", "someone"} {
		wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubClaimDecide, claimRes("student", approver)), authz.ErrForbidden, false)
		wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubClaimList, claimRes("student", approver)), authz.ErrForbidden, false)
	}
	noApprover := claimRes("student", byPad)
	delete(noApprover.Attrs, authz.AttrClaimApprover)
	wantErr(t, authz.Can(ctx, pad, authz.DirectoryClubClaimDecide, noApprover), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "support"), authz.DirectoryClubClaimList, noApprover), authz.ErrForbidden, false)
	// A rep holds no grant on a malformed university.
	badUni := claimRes("student", byRep)
	badUni.Attrs[authz.AttrClaimUniversityOrg] = "not-a-uuid"
	wantErr(t, authz.Can(ctx, rep, authz.DirectoryClubClaimDecide, badUni), authz.ErrNotFound, false)
	// The claimant (an owner without the decide grant) cannot decide either.
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.DirectoryClubClaimDecide, claimRes("student", byPad)),
		authz.ErrForbidden, false)
	// k-mfa: the platform roles and university_rep need an MFA session.
	noMFA := pad
	noMFA.MFAVerified = false
	wantErr(t, authz.Can(ctx, noMFA, authz.DirectoryClubClaimDecide, claimRes("student", byPad)), authz.ErrMFARequired, false)
	repNoMFA := rep
	repNoMFA.MFAVerified = false
	wantErr(t, authz.Can(ctx, repNoMFA, authz.DirectoryClubClaimDecide, claimRes("student", byRep)), authz.ErrMFARequired, false)
	sup := persona(t, "support")
	sup.MFAVerified = false
	wantErr(t, authz.Can(ctx, sup, authz.DirectoryClubClaimList, adminSurface), authz.ErrMFARequired, false)
	// Y4: a minor never claims (403; the service says directory.minor_restricted).
	wantErr(t, authz.Can(ctx, persona(t, "student_minor"), authz.DirectoryClubClaimCreate, clubRes("club_unclaimed", false)),
		authz.ErrForbidden, false)
	// A lapsed verification is no student (students.verification_required in the service).
	lapsed := persona(t, "student")
	lapsed.StudentStatus = authz.StudentLapsed
	wantErr(t, authz.Can(ctx, lapsed, authz.DirectoryClubClaimCreate, clubRes("club_unclaimed", false)), authz.ErrForbidden, false)
	// A claim without its status Attr is a programmer error (deny).
	noAttr := claimRes("student", byRep)
	noAttr.Attrs = nil
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.DirectoryClubClaimWithdraw, noAttr), authz.ErrForbidden, true)
	// Another user's claim is invisible to its non-owner (404), even a club admin.
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.DirectoryClubClaimWithdraw, claimRes("student", byRep)),
		authz.ErrNotFound, false)
}
