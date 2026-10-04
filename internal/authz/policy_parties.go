package authz

// TypeLegalPartyDocument is one legal_party_documents row (02 §17.2) for the
// document verification (parties.document.verify). OwnerUserID is unset;
// Attrs uploaded_by (the uploader, who may not verify it) is mandatory.
// Declared by the parties policy (Policy.Types).
const TypeLegalPartyDocument = "legal_party_document"

// Scopes of a legal_party resource on an org surface (Attrs "scope"; BE-036
// reading of 03 k21/k22). The service derives the scope from the rows,
// never from the request (06 H-38).
const (
	// PartyScopeOrg: the party belongs to the org itself (legal_parties.org_id
	// = the org: a company's buyer party).
	PartyScopeOrg = "org"
	// PartyScopePayee: the party is the legal side of one of the org's
	// payees (payees.org_id = the org, not closed: a club's foundation, 03 k21).
	PartyScopePayee = "payee"
	// PartyScopeList: the org's collection (listOrgLegalParties); every row
	// is authorised again with its own scope.
	PartyScopeList = "list"
)

// partiesPolicy is the policy of the parties module (03 §5.13). Only tickets
// of the parties module edit this file (BE-001 R5). Written by BE-036 for its
// 7 actions:
//
//	parties.legal_party.read     org surface: cmm S, cma S, cla S·k21, uni S·k21; pad, fin
//	                             (admin surface: pad, fin)
//	parties.legal_party.create   org surface: cma S·k22; pad, fin (admin surface: pad, fin)
//	parties.legal_party.update   org surface: cma S·k22; pad, fin (admin surface: pad, fin)
//	parties.legal_party.verify   admin surface: pad, fin
//	parties.document.upload      org surface: cma S, cla S·k21, uni S·k21; pad, fin
//	parties.document.read        org surface: cma S, cla S·k21, uni S·k21; pad, fin
//	parties.document.verify      admin surface: pad, fin; then on the document:
//	                             pad, fin, never its uploader (four eyes, like 06 H-19)
//
// Resources: legal_party (03 §3.5: OrgID = the org of the path; Attrs kind
// and scope), legal_party_document (Attrs uploaded_by) and admin_surface
// (the /admin operations: 403 without a platform role before anything is
// read; 03 §7.2-3).
//
// Org surface (03 §3.6, §7.6): visible to the org's active members (403
// without a role) and to every platform role; anyone else gets 404.
//
// k21 (club_admin, university_rep): only a party linked to the org by a
// payee (scope payee) or the org's collection (scope list), never a party
// the org owns itself. k22 (company_admin/owner): create and update only a
// buyer_company party of their own company (scope org). k-cs: the uni
// column is university_rep, never career_staff. MFA follows from the role
// paths (k-mfa: uni, cma, pad, fin).
//
// The payee and bank account actions belong to BE-037: until then they are
// visible and granted to no role path (403).
func partiesPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeLegalPartyDocument: {AttrUploadedBy}},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case PartiesLegalPartyRead, PartiesLegalPartyCreate, PartiesLegalPartyUpdate,
				PartiesDocumentUpload, PartiesDocumentRead:
				switch r.Type {
				case TypeAdminSurface:
					return act != PartiesDocumentUpload && act != PartiesDocumentRead && AdminSurfaceVisible(a)
				case TypeLegalParty:
					return OrgSurfaceVisible(a, r.OrgID)
				}
				return false
			case PartiesLegalPartyVerify:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case PartiesDocumentVerify:
				switch r.Type {
				case TypeAdminSurface:
					return AdminSurfaceVisible(a)
				case TypeLegalPartyDocument:
					return a.HasAnyPlatformRole()
				}
				return false
			}
			return true // BE-037's actions: no grant below → 403
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			staff := []Grant{{Role: RolePlatformAdmin}, {Role: RoleFinanceAdmin}}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case PartiesLegalPartyRead, PartiesLegalPartyCreate, PartiesLegalPartyUpdate, PartiesLegalPartyVerify,
					PartiesDocumentVerify:
					return staff
				}
				return nil
			case TypeLegalPartyDocument:
				if act != PartiesDocumentVerify {
					return nil
				}
				eyes := []string{AttrUploadedBy}
				return []Grant{{Role: RolePlatformAdmin, FourEyes: eyes}, {Role: RoleFinanceAdmin, FourEyes: eyes}}
			case TypeLegalParty:
				return orgPartyGrants(a, act, r, staff)
			}
			return nil
		},
	}
}

// orgPartyGrants are the org-surface cells of 03 §5.13 (S, k21, k22). A
// resource without an org is nobody's surface but the staff's.
func orgPartyGrants(a Actor, act Action, r Resource, staff []Grant) []Grant {
	switch act {
	case PartiesLegalPartyRead, PartiesLegalPartyCreate, PartiesLegalPartyUpdate, PartiesDocumentUpload, PartiesDocumentRead:
	default:
		return nil
	}
	if r.OrgID == nil {
		return staff
	}
	org, scope := *r.OrgID, r.Attr("scope")
	payeeSide := scope == PartyScopePayee || scope == PartyScopeList // k21
	ownBuyer := scope == PartyScopeOrg && r.Attr("kind") == "buyer_company"
	gs := staff
	add := func(ok bool, role Role) {
		if ok {
			gs = append(gs, Grant{Role: role})
		}
	}
	companyAdmin := a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner)
	clubAdmin := a.HasOrgRole(org, MemberClubAdmin)
	uniRep := a.HasOrgRole(org, MemberUniversityRep) // k-cs: not career_staff
	switch act {
	case PartiesLegalPartyRead:
		add(a.HasOrgRole(org, MemberRecruiter), RoleCompanyMember)
		add(companyAdmin, RoleCompanyAdmin)
		add(clubAdmin && payeeSide, RoleClubAdmin)
		add(uniRep && payeeSide, RoleUniversityRep)
	case PartiesLegalPartyCreate, PartiesLegalPartyUpdate:
		add(companyAdmin && ownBuyer, RoleCompanyAdmin) // k22
	case PartiesDocumentUpload, PartiesDocumentRead:
		add(companyAdmin, RoleCompanyAdmin)
		add(clubAdmin && payeeSide, RoleClubAdmin)
		add(uniRep && payeeSide, RoleUniversityRep)
	default:
		return nil
	}
	return gs
}
