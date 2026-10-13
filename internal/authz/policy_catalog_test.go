package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// offeringOf is the Resource the catalog service builds (BE-038): OrgID =
// the seller; Attrs status, university_id, seller_kind, published,
// university_org_id, university_review, created_by.
func offeringOf(seller, status string, published, uniReview bool, creator string) authz.Resource {
	id := authztest.ID(seller)
	uniOrg, uniID := "", ""
	if seller == "club_a" {
		uniOrg, uniID = authztest.ID("uni_a").String(), authztest.UniversityID("uni_a").String()
	}
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return authz.Resource{Type: authz.TypeOffering, ID: uuid.NewV7(), OrgID: &id, Attrs: map[string]string{
		authz.AttrStatus: status, "university_id": uniID, "seller_kind": "org", authz.AttrOfferingPublished: b(published),
		authz.AttrOfferingUniversityOrg: uniOrg, authz.AttrOfferingUniversityReview: b(uniReview),
		authz.AttrCreatedBy: authztest.ID(creator).String(),
	}}
}

// orgOf is an org of /orgs/{org_id}/... (the management surface).
func orgOf(name, kind string) authz.Resource {
	id := authztest.ID(name)
	return authz.Resource{Type: authz.TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{"org_kind": kind, "org_status": "active"}}
}

func serviceOf(accounting bool) authz.Resource {
	v := "false"
	if accounting {
		v = "true"
	}
	return authz.Resource{Type: authz.TypeCatalogService, Attrs: map[string]string{authz.AttrAccountingReview: v}}
}

// TestCatalogPolicy_Matrix pins 03 §5.14 for the 18 personas (the HTTP
// matrix, testdata/authz/catalog.yaml, checks the same through the API).
func TestCatalogPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	signedIn := func() map[string]error {
		m := map[string]error{"anonymous": forbidden}
		for _, p := range authztest.Personas() {
			if p.Name != "anonymous" {
				m[p.Name] = nil
			}
		}
		return m
	}
	// orgSurface: everyone outside the org 404 (anonymous 403), the org's
	// members and every platform role 403 unless listed.
	orgSurface := func(members []string, allow map[string]error) map[string]error {
		m := map[string]error{"anonymous": forbidden}
		for _, p := range authztest.Personas() {
			if p.Name == "anonymous" {
				continue
			}
			m[p.Name] = notFound
			if p.Actor.HasAnyPlatformRole() {
				m[p.Name] = forbidden
			}
		}
		for _, n := range members {
			m[n] = forbidden
		}
		for k, v := range allow {
			m[k] = v
		}
		return m
	}
	clubA := []string{"club_member", "club_admin"}
	uniA := []string{"university_rep"}
	draftA := offeringOf("club_a", "draft", false, false, "club_admin")
	activeA := offeringOf("club_a", "active", true, false, "club_admin")
	reviewA := offeringOf("club_a", "pending_approval", false, true, "club_admin")
	type cell struct {
		name string
		act  authz.Action
		r    authz.Resource
		want map[string]error
	}
	cells := []cell{
		{"service list", authz.CatalogServiceList, serviceOf(false), signedIn()},
		{"browse", authz.CatalogOfferingBrowse, authz.Resource{Type: authz.TypeOfferingCatalog}, signedIn()},
		{"read a published offering", authz.CatalogOfferingRead, activeA, signedIn()},
		{"read a draft", authz.CatalogOfferingRead, draftA, orgSurface(clubA, map[string]error{"club_admin": nil, "platform_admin": nil})},
		{"read an offering under the university's review", authz.CatalogOfferingRead, reviewA,
			orgSurface(clubA, map[string]error{"club_admin": nil, "platform_admin": nil, "university_rep": nil})},
		{"list a club's offerings", authz.CatalogOfferingListOrg, orgOf("club_a", "club"),
			orgSurface(clubA, map[string]error{"club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"list a university's offerings", authz.CatalogOfferingListOrg, orgOf("uni_a", "university"),
			orgSurface(uniA, map[string]error{"university_rep": nil, "platform_admin": nil, "finance_admin": nil})},
		{"create for a club", authz.CatalogOfferingCreate, orgOf("club_a", "club"), orgSurface(clubA, map[string]error{"club_admin": nil})},
		{"create for a university", authz.CatalogOfferingCreate, orgOf("uni_a", "university"),
			orgSurface(uniA, map[string]error{"university_rep": nil})},
		{"update a draft", authz.CatalogOfferingUpdate, draftA, orgSurface(clubA, map[string]error{"club_admin": nil})},
		{"submit a draft", authz.CatalogOfferingSubmit, draftA, orgSurface(clubA, map[string]error{"club_admin": nil})},
		{"cancel a draft", authz.CatalogOfferingCancel, draftA,
			orgSurface(clubA, map[string]error{"club_admin": nil, "platform_admin": nil})},
		{"decide (created by club_admin)", authz.CatalogOfferingDecide, reviewA,
			orgSurface(clubA, map[string]error{"platform_admin": nil, "university_rep": forbidden})},
		{"university's queue", authz.CatalogOfferingReviewQueue, orgOf("uni_a", "university"),
			orgSurface(uniA, map[string]error{"university_rep": nil, "platform_admin": nil})},
		{"university decision gate", authz.CatalogOfferingUniversityDecide, orgOf("uni_a", "university"),
			orgSurface(uniA, map[string]error{"university_rep": nil})},
		{"university decision", authz.CatalogOfferingUniversityDecide, reviewA,
			orgSurface(clubA, map[string]error{"university_rep": nil})},
	}
	// A published offering's writes: visible to everyone signed in, granted to the seller.
	pub := signedIn()
	for k := range pub {
		if k != "anonymous" {
			pub[k] = forbidden
		}
	}
	pub["club_admin"], pub["platform_admin"] = nil, nil
	cells = append(cells, cell{"cancel a published offering", authz.CatalogOfferingCancel, activeA, pub})
	pubUpdate := map[string]error{}
	for k, v := range pub {
		pubUpdate[k] = v
	}
	pubUpdate["platform_admin"] = forbidden // k-po: not the platform org
	cells = append(cells, cell{"update a published offering (the service refuses non-drafts)", authz.CatalogOfferingUpdate, activeA, pubUpdate})
	for _, c := range cells {
		for _, p := range authztest.Personas() {
			want, ok := c.want[p.Name]
			if !ok {
				t.Fatalf("%s: no expectation for %s", c.name, p.Name)
			}
			err := authz.Can(ctx, p.Actor, c.act, c.r)
			if want == nil && err != nil || want != nil && err == nil {
				t.Errorf("%s: %s: %v, want %v", c.name, p.Name, err, want)
				continue
			}
			if want != nil {
				wantErr(t, err, want, false)
			}
		}
	}
	// Admin surface: 403 before reading for everyone without the role.
	for act, allowed := range map[authz.Action][]string{
		authz.CatalogOfferingReviewQueue: {"platform_admin"},
		authz.CatalogPolicyList:          {"platform_admin", "finance_admin"},
		authz.CatalogPolicyManage:        {"platform_admin", "finance_admin"},
	} {
		for _, p := range authztest.Personas() {
			var want error = forbidden
			for _, n := range allowed {
				if p.Name == n {
					want = nil
				}
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
	// k23: service management by pad and fin; the accountant's review by fin only.
	for _, p := range authztest.Personas() {
		var plain, review error = forbidden, forbidden
		switch p.Name {
		case "platform_admin":
			plain = nil
		case "finance_admin":
			plain, review = nil, nil
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.CatalogServiceManage, serviceOf(false)), plain, false)
		wantErr(t, authz.Can(ctx, p.Actor, authz.CatalogServiceManage, serviceOf(true)), review, false)
	}
}

func TestCatalogPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// k24: the creator of an offering never decides it (core four-eyes table).
	platformDraft := offeringOf("platform_org", "pending_approval", false, false, "platform_admin")
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.CatalogOfferingDecide, platformDraft), authz.ErrFourEyes, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin_2"), authz.CatalogOfferingDecide, platformDraft), nil, false)
	noCreator := platformDraft
	noCreator.Attrs = map[string]string{authz.AttrStatus: "pending_approval", "university_id": "", "seller_kind": "platform",
		authz.AttrOfferingPublished: "false", authz.AttrOfferingUniversityOrg: "", authz.AttrOfferingUniversityReview: "false"}
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin_2"), authz.CatalogOfferingDecide, noCreator), authz.ErrForbidden, true)

	// k-po: platform_admin creates for the platform org it belongs to (kind platform) only.
	pad := persona(t, "platform_admin")
	wantErr(t, authz.Can(ctx, pad, authz.CatalogOfferingCreate, orgOf("platform_org", "platform")), authz.ErrForbidden, false)
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	wantErr(t, authz.Can(ctx, pad, authz.CatalogOfferingCreate, orgOf("platform_org", "platform")), nil, false)
	wantErr(t, authz.Can(ctx, pad, authz.CatalogOfferingCreate, orgOf("club_a", "club")), authz.ErrForbidden, false)

	// k-cs: career_staff lists its university's offerings but never writes or decides.
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone,
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, cs, authz.CatalogOfferingListOrg, orgOf("uni_a", "university")), nil, false)
	wantErr(t, authz.Can(ctx, cs, authz.CatalogOfferingCreate, orgOf("uni_a", "university")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, cs, authz.CatalogOfferingReviewQueue, orgOf("uni_a", "university")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, cs, authz.CatalogOfferingUniversityDecide, offeringOf("club_a", "pending_approval", false, true, "club_admin")),
		authz.ErrNotFound, false) // not a rep: the review is not visible to it

	// The university's review: only while the offering waits for it, only that university's rep.
	rep := persona(t, "university_rep")
	wantErr(t, authz.Can(ctx, rep, authz.CatalogOfferingUniversityDecide, offeringOf("club_a", "pending_approval", false, false, "club_admin")),
		authz.ErrNotFound, false)
	otherRep := rep
	otherRep.Memberships = []authz.Membership{{OrgID: authztest.ID("uni_b"), OrgKind: authz.OrgUniversity, Role: authz.MemberUniversityRep}}
	wantErr(t, authz.Can(ctx, otherRep, authz.CatalogOfferingUniversityDecide, offeringOf("club_a", "pending_approval", false, true, "club_admin")),
		authz.ErrNotFound, false)
	// A university's queue is a university's: a rep of a club org path has no cell.
	wantErr(t, authz.Can(ctx, rep, authz.CatalogOfferingReviewQueue, orgOf("club_a", "club")), authz.ErrNotFound, false)

	// k-mfa: university_rep, platform_admin, finance_admin; club_admin needs none.
	for _, tc := range []struct {
		name string
		act  authz.Action
		r    authz.Resource
		want error
	}{
		{"university_rep", authz.CatalogOfferingCreate, orgOf("uni_a", "university"), authz.ErrMFARequired},
		{"platform_admin", authz.CatalogPolicyManage, adminSurface, authz.ErrMFARequired},
		{"finance_admin", authz.CatalogPolicyList, adminSurface, authz.ErrMFARequired},
		{"club_admin", authz.CatalogOfferingCreate, orgOf("club_a", "club"), nil},
		{"platform_admin", authz.CatalogOfferingBrowse, authz.Resource{Type: authz.TypeOfferingCatalog}, nil}, // signed-in path
	} {
		a := persona(t, tc.name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, tc.act, tc.r), tc.want, false)
	}

	// Y4: a minor is never a club_admin.
	minor := persona(t, "club_admin")
	minor.IsMinor = true
	wantErr(t, authz.Can(ctx, minor, authz.CatalogOfferingCreate, orgOf("club_a", "club")), authz.ErrForbidden, false)

	// The service resource needs its Attrs key (fail closed).
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.CatalogServiceManage, authz.Resource{Type: authz.TypeCatalogService}),
		authz.ErrForbidden, true)
	// Offering actions are bound to their resource types.
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.CatalogOfferingDecide, adminSurface), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.CatalogPolicyManage, orgOf("club_a", "club")), authz.ErrNotFound, false)
}
