package authz

import "uuid"

// Resource types declared by the catalog policy (Policy.Types).
const (
	// TypeOfferingCatalog is the marketplace (browseOfferings): the active
	// offerings every signed-in user may browse. No org, no Attrs.
	TypeOfferingCatalog = "offering_catalog"
	// TypeCatalogService is the Koro service catalogue (listServices) and one
	// service written on the admin surface (adminCreateService,
	// adminUpdateService). Attrs accounting_review: "true" when the write
	// sets accounting_reviewed_at/_ref (03 k23: finance admins only).
	TypeCatalogService = "catalog_service"
)

// Attrs of an offering resource (03 §3.5 status, university_id,
// seller_kind; BE-038 adds the rest). The catalog service derives every
// value from the offering and its seller org (06 H-38), never from the
// request.
const (
	// AttrOfferingPublished is "true" once the offering has been on the
	// marketplace (active, sold_out, closed, or cancelled after approval):
	// every signed-in user may read it. A draft, an offering under review
	// and one cancelled before approval are the seller's (03 §5.14 read:
	// "taslak yalnız satıcı + pad").
	AttrOfferingPublished = "published"
	// AttrOfferingUniversityOrg is the org ID of the seller club's university
	// ("" for a university, the platform or a club without one): the U cell
	// of review_queue and university_decide.
	AttrOfferingUniversityOrg = "university_org_id"
	// AttrOfferingUniversityReview is "true" while the offering waits for its
	// university's decision (requires_university_approval and
	// pending_approval, T-21): the university_rep of AttrOfferingUniversityOrg
	// may then read and decide it.
	AttrOfferingUniversityReview = "university_review"
	// AttrAccountingReview is the TypeCatalogService Attr of 03 k23.
	AttrAccountingReview = "accounting_review"
)

// catalogPolicy is the policy of the catalog module (03 §5.14). Only tickets of
// the catalog module edit this file (BE-001 R5). Written by BE-038:
//
//	catalog.service.list            catalog_service: every signed-in user
//	catalog.service.manage          catalog_service (admin): pad·k23, fin; k23 —
//	                                  accounting_reviewed_* is finance's only
//	catalog.offering.browse         offering_catalog: every signed-in user
//	catalog.offering.read           offering: published → every signed-in user;
//	                                  otherwise the seller's club_admin,
//	                                  university_rep, career_staff (S), pad, and the
//	                                  university_rep of the seller club's university
//	                                  while the offering waits for its decision (U)
//	catalog.offering.list_org       org: cla S, uni S (career_staff too: no k-cs), pad, fin
//	catalog.offering.create         org: cla S, uni S·k-cs, pad k-po
//	catalog.offering.update/submit  offering: cla S, uni S·k-cs, pad k-po
//	catalog.offering.cancel         offering: cla S, uni S·k-cs, pad
//	catalog.offering.review_queue   admin surface: pad; org (a university): uni U·k-cs, pad
//	catalog.offering.decide         offering: pad·k24 (never its creator: the core
//	                                  four-eyes table, Attrs created_by)
//	catalog.offering.university_decide  org (the gate before the offering is read)
//	                                  and offering: uni U·k-cs while the offering
//	                                  waits for its university
//	catalog.policy.list             admin surface: pad, fin
//	catalog.policy.manage           admin surface: pad·R, fin·R (R is the service's, 03 §6.2)
//
// Resources: offering (OrgID = the seller org; Attrs status, university_id,
// seller_kind, published, university_org_id, university_review and, for
// decide, created_by), org (/orgs/{org_id}/offerings and
// /orgs/{org_id}/offering-approvals: the org's management surface, 03 §3.6),
// offering_catalog, catalog_service and admin_surface (403 without a
// platform role before anything is read; 03 §7.2-3).
//
// Visibility (03 §3.6, §7.6): /offerings/{offering_id}/... is a resource
// surface. A published offering is visible to every signed-in user; an
// unpublished one to the seller org's active members, every platform role
// and, while it waits for its university, that university's reps — anyone
// else gets 404 (another org's draft is never probed). A visible offering
// without the grant is 403.
//
// k-cs: the uni cells of create, update, submit, cancel, review_queue and
// university_decide are university_rep only, never career_staff. k-po
// (H-39, T-190): platform_admin creates, edits and submits for the platform
// org only (platformOrg). MFA follows from the role paths (k-mfa: uni, pad,
// fin). The status machine, the org status (03 §2.3) and the payee rules
// are the service's.
func catalogPolicy() Policy {
	return Policy{
		Types: map[string][]string{
			TypeOfferingCatalog: nil,
			TypeCatalogService:  {AttrAccountingReview},
		},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeCatalogService:
				switch act {
				case CatalogServiceList:
					return a.IsUser()
				case CatalogServiceManage:
					return AdminSurfaceVisible(a)
				}
			case TypeOfferingCatalog:
				return act == CatalogOfferingBrowse && a.IsUser()
			case TypeAdminSurface:
				switch act {
				case CatalogOfferingReviewQueue, CatalogPolicyList, CatalogPolicyManage:
					return AdminSurfaceVisible(a)
				}
			case TypeOrg:
				switch act {
				case CatalogOfferingListOrg, CatalogOfferingCreate, CatalogOfferingReviewQueue,
					CatalogOfferingUniversityDecide:
					return OrgSurfaceVisible(a, r.OrgID)
				}
			case TypeOffering:
				switch act {
				case CatalogOfferingRead, CatalogOfferingUpdate, CatalogOfferingSubmit, CatalogOfferingCancel,
					CatalogOfferingDecide, CatalogOfferingUniversityDecide:
					return offeringVisible(a, r)
				}
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			signedIn := []Grant{{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleStudentMinor}}
			pad, fin := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}
			switch r.Type {
			case TypeCatalogService:
				switch act {
				case CatalogServiceList:
					return signedIn
				case CatalogServiceManage:
					if r.Flag(AttrAccountingReview) {
						return []Grant{fin} // k23
					}
					return []Grant{pad, fin}
				}
			case TypeOfferingCatalog:
				if act == CatalogOfferingBrowse {
					return signedIn
				}
			case TypeAdminSurface:
				switch act {
				case CatalogOfferingReviewQueue:
					return []Grant{pad}
				case CatalogPolicyList, CatalogPolicyManage:
					return []Grant{pad, fin}
				}
			case TypeOrg:
				return catalogOrgGrants(a, act, r, pad, fin)
			case TypeOffering:
				return offeringGrants(a, act, r, signedIn, pad)
			}
			return nil
		},
	}
}

// catalogOrgGrants are the cells on /orgs/{org_id}/offerings and
// /orgs/{org_id}/offering-approvals (the org's management surface).
func catalogOrgGrants(a Actor, act Action, r Resource, pad, fin Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	org := *r.OrgID
	var gs []Grant
	add := func(ok bool, g Grant) {
		if ok {
			gs = append(gs, g)
		}
	}
	clubAdmin := a.HasOrgRole(org, MemberClubAdmin)
	uniRep := a.HasOrgRole(org, MemberUniversityRep) // k-cs: never career_staff
	university := r.Attr("org_kind") == string(OrgUniversity)
	switch act {
	case CatalogOfferingListOrg:
		add(clubAdmin, Grant{Role: RoleClubAdmin})
		add(uniRep, Grant{Role: RoleUniversityRep})
		add(a.HasOrgRole(org, MemberCareerStaff), Grant{Role: RoleCareerStaff}) // uni S without k-cs
		gs = append(gs, pad, fin)
	case CatalogOfferingCreate:
		add(clubAdmin, Grant{Role: RoleClubAdmin})
		add(uniRep, Grant{Role: RoleUniversityRep})
		add(platformOrg(a, r.OrgID), pad) // k-po
	case CatalogOfferingReviewQueue:
		add(uniRep && university, Grant{Role: RoleUniversityRep}) // U·k-cs
		gs = append(gs, pad)
	case CatalogOfferingUniversityDecide:
		add(uniRep && university, Grant{Role: RoleUniversityRep}) // U·k-cs (the gate)
	}
	return gs
}

// offeringGrants are the cells on one offering (resource surface).
func offeringGrants(a Actor, act Action, r Resource, signedIn []Grant, pad Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	org := *r.OrgID
	var gs []Grant
	add := func(ok bool, g Grant) {
		if ok {
			gs = append(gs, g)
		}
	}
	clubAdmin := a.HasOrgRole(org, MemberClubAdmin)
	uniRep := a.HasOrgRole(org, MemberUniversityRep) // k-cs: never career_staff
	switch act {
	case CatalogOfferingRead:
		if r.Flag(AttrOfferingPublished) {
			return signedIn
		}
		add(clubAdmin, Grant{Role: RoleClubAdmin})
		add(uniRep, Grant{Role: RoleUniversityRep})
		add(a.HasOrgRole(org, MemberCareerStaff), Grant{Role: RoleCareerStaff})
		add(universityReviewer(a, r), Grant{Role: RoleUniversityRep})
		gs = append(gs, pad)
	case CatalogOfferingUpdate, CatalogOfferingSubmit:
		add(clubAdmin, Grant{Role: RoleClubAdmin})
		add(uniRep, Grant{Role: RoleUniversityRep})
		add(platformOrg(a, r.OrgID), pad) // k-po
	case CatalogOfferingCancel:
		add(clubAdmin, Grant{Role: RoleClubAdmin})
		add(uniRep, Grant{Role: RoleUniversityRep})
		gs = append(gs, pad)
	case CatalogOfferingDecide:
		gs = append(gs, pad) // k24: the core refuses the creator (Attrs created_by)
	case CatalogOfferingUniversityDecide:
		add(universityReviewer(a, r), Grant{Role: RoleUniversityRep})
	}
	return gs
}

// universityReviewer reports the U cell on an offering: a university_rep
// (k-cs) of the seller club's university while the offering waits for the
// university's decision.
func universityReviewer(a Actor, r Resource) bool {
	if !r.Flag(AttrOfferingUniversityReview) {
		return false
	}
	uni, err := uuid.Parse(r.Attr(AttrOfferingUniversityOrg))
	return err == nil && a.HasOrgRole(uni, MemberUniversityRep)
}

// offeringVisible is 03 §3.6 for an offering (see catalogPolicy).
func offeringVisible(a Actor, r Resource) bool {
	if !a.IsUser() {
		return false
	}
	if r.Flag(AttrOfferingPublished) {
		return true
	}
	return OrgSurfaceVisible(a, r.OrgID) || universityReviewer(a, r)
}
