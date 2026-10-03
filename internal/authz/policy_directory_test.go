package authz_test

import (
	"errors"
	"testing"

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

// TestDirectoryPolicy_Matrix pins 03 §5.3 (BE-020's 12 actions) for the 18
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
	// BE-022's claim actions stay denied until written.
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.DirectoryClubClaimList, adminSurface), authz.ErrForbidden, false)
}
