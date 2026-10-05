package authz

// TypeLegalPartyDocument is one legal_party_documents row (02 §17.2) for the
// document verification (parties.document.verify). OwnerUserID is unset;
// Attrs uploaded_by (the uploader, who may not verify it) is mandatory.
// Declared by the parties policy (Policy.Types).
const TypeLegalPartyDocument = "legal_party_document"

// TypeLegalPartyOwnership is the ownership correction of a legal party
// (06 H-60, parties.legal_party.verify): deciding that a party does not own
// its tax ID is a platform admin's call (finance admins verify parties but
// do not take a tax ID away). No Attrs. Declared by the parties policy.
const TypeLegalPartyOwnership = "legal_party_ownership"

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
// of the parties module edit this file (BE-001 R5). BE-036 wrote the legal
// party and document actions, BE-037 the payee and bank account actions:
//
//	parties.legal_party.read     org surface: cmm S, cma S, cla S·k21, uni S·k21; pad, fin
//	                             (admin surface: pad, fin)
//	parties.legal_party.create   org surface: cma S·k22; pad, fin (admin surface: pad, fin)
//	parties.legal_party.update   org surface: cma S·k22; pad, fin (admin surface: pad, fin)
//	parties.legal_party.verify   admin surface: pad, fin; the ownership correction
//	                             (legal_party_ownership, 06 H-60): pad only
//	parties.document.upload      org surface: cma S, cla S·k21, uni S·k21; pad, fin
//	                             (admin surface: pad, fin; T-234)
//	parties.document.read        org surface: cma S, cla S·k21, uni S·k21; pad, fin
//	                             (admin surface: pad, fin; T-234)
//	parties.document.verify      admin surface: pad, fin; then on the document:
//	                             pad, fin, never its uploader (four eyes, like 06 H-19)
//	parties.payee.list / read    org surface (payee): cla S, uni S; pad, fin
//	parties.payee.manage/verify  admin surface: pad, fin
//	parties.bank_account.list    org surface (payee): cla S, uni S (masked IBAN); pad, fin
//	                             (admin surface: pad, fin; full IBAN, audited)
//	parties.bank_account.submit  org surface (payee): cla S·R, uni S·R; pad, fin
//	                             (admin surface: pad·R, fin·R); R is the service's (03 §6.2)
//	parties.bank_account.verify  on the account: pad·4, fin·4 (never the submitter: the core
//	                             four-eyes table)
//	parties.bank_account.approve on the account: pad·4·R, fin·4·R (never the submitter nor the
//	                             first eye). The core table demands the eyes' keys for these two
//	                             actions on every resource, so the service's "403 before reading"
//	                             gate of their /admin operations is parties.bank_account.list on
//	                             the admin surface (the same pad, fin cells).
//	parties.bank_account.reject  admin surface: pad, fin
//	parties.bank_account.revoke  admin surface: pad·R, fin·R
//
// Resources: legal_party (03 §3.5: OrgID = the org of the path; Attrs kind
// and scope), legal_party_document (Attrs uploaded_by), legal_party_ownership,
// payee (OrgID = the org of the path; the org's payees as a collection),
// bank_account (Attrs status, submitted_by, verified_by: the earlier eyes)
// and admin_surface (the /admin operations: 403 without a platform role
// before anything is read; 03 §7.2-3).
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
func partiesPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeLegalPartyDocument: {AttrUploadedBy}, TypeLegalPartyOwnership: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				return AdminSurfaceVisible(a)
			case TypeLegalParty:
				switch act {
				case PartiesLegalPartyRead, PartiesLegalPartyCreate, PartiesLegalPartyUpdate,
					PartiesDocumentUpload, PartiesDocumentRead:
					return OrgSurfaceVisible(a, r.OrgID)
				}
			case TypePayee:
				switch act {
				case PartiesPayeeList, PartiesPayeeRead, PartiesBankAccountList, PartiesBankAccountSubmit:
					return OrgSurfaceVisible(a, r.OrgID)
				}
			case TypeLegalPartyDocument:
				return act == PartiesDocumentVerify && a.HasAnyPlatformRole()
			case TypeLegalPartyOwnership:
				return act == PartiesLegalPartyVerify && a.HasAnyPlatformRole()
			case TypeBankAccount:
				return (act == PartiesBankAccountVerify || act == PartiesBankAccountApprove) && a.HasAnyPlatformRole()
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			staff := []Grant{{Role: RolePlatformAdmin}, {Role: RoleFinanceAdmin}}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case PartiesLegalPartyRead, PartiesLegalPartyCreate, PartiesLegalPartyUpdate, PartiesLegalPartyVerify,
					PartiesDocumentUpload, PartiesDocumentRead, PartiesDocumentVerify,
					PartiesPayeeManage, PartiesPayeeVerify, PartiesBankAccountList, PartiesBankAccountSubmit,
					PartiesBankAccountReject, PartiesBankAccountRevoke:
					return staff
				}
				return nil
			case TypeLegalPartyDocument:
				if act != PartiesDocumentVerify {
					return nil
				}
				eyes := []string{AttrUploadedBy}
				return []Grant{{Role: RolePlatformAdmin, FourEyes: eyes}, {Role: RoleFinanceAdmin, FourEyes: eyes}}
			case TypeLegalPartyOwnership:
				if act != PartiesLegalPartyVerify {
					return nil
				}
				return []Grant{{Role: RolePlatformAdmin}} // 06 H-60: a platform admin's decision
			case TypeBankAccount:
				// The eyes: the core table (fourEyesRules) refuses the submitter
				// (verify, approve) and the first eye (approve).
				if act != PartiesBankAccountVerify && act != PartiesBankAccountApprove {
					return nil
				}
				return staff
			case TypePayee:
				return orgPayeeGrants(a, act, r, staff)
			case TypeLegalParty:
				return orgPartyGrants(a, act, r, staff)
			}
			return nil
		},
	}
}

// orgPayeeGrants are the payee cells of the org surface (03 §5.13 S: the
// club admin of the club, the university rep of the university; k-cs).
func orgPayeeGrants(a Actor, act Action, r Resource, staff []Grant) []Grant {
	switch act {
	case PartiesPayeeList, PartiesPayeeRead, PartiesBankAccountList, PartiesBankAccountSubmit:
	default:
		return nil
	}
	if r.OrgID == nil {
		return staff
	}
	gs := staff
	if a.HasOrgRole(*r.OrgID, MemberClubAdmin) {
		gs = append(gs, Grant{Role: RoleClubAdmin})
	}
	if a.HasOrgRole(*r.OrgID, MemberUniversityRep) {
		gs = append(gs, Grant{Role: RoleUniversityRep})
	}
	return gs
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
