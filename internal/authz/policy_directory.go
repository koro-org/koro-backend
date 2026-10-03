package authz

import (
	"slices"
	"uuid"
)

// TypeDirectoryCatalog is the public directory lists (universities, skills,
// clubs): no org, no owner, no Attrs. Declared by the directory policy
// (Policy.Types).
const TypeDirectoryCatalog = "directory_catalog"

// directoryPolicy is the policy of the directory module (03 §5.3). Only
// tickets of the directory module edit this file (BE-001 R5). Written by
// BE-020 for its 12 actions and BE-022 for the 5 club claim actions:
//
//	directory.university.list          P (directory_catalog)
//	directory.university.read          P (university); an inactive university
//	directory.department.list            only for platform_admin, else 404
//	directory.skill.list               P (directory_catalog)
//	directory.club.list                P (directory_catalog)
//	directory.club.read                P (club: the catalog is public, 03 §3.6)
//	directory.club.update              S: the club's club_admin; platform_admin
//	                                     (k7 — a sensitive category without the
//	                                     flag — is the service's 422)
//	directory.club.request_sensitivity S: the club's club_admin
//	directory.club.create              platform_admin (admin surface)
//	directory.club.set_sensitivity     platform_admin (admin surface; R in the service)
//	directory.university.manage        platform_admin (admin surface)
//	directory.skill.manage             platform_admin (admin surface)
//	directory.club_claim.create        adult verified students (stu, clm, cla, stf: k8)
//	                                     on a club; the service turns the 403 of
//	                                     an unverified account into
//	                                     students.verification_required and of a
//	                                     minor into directory.minor_restricted,
//	                                     and checks the university (422)
//	directory.club_claim.read_own      O (club_claim of the caller)
//	directory.club_claim.withdraw      O (another user's claim is 404)
//	directory.club_claim.list          support, platform_admin: every claim
//	                                     (admin surface: the queue; club_claim:
//	                                     one claim); university_rep: the claims
//	                                     they approve (see below), others 404
//	directory.club_claim.decide        the claim's approver (E-21): the
//	                                     university_rep of the club's university
//	                                     when that university has one and the
//	                                     club is not sensitive, platform_admin
//	                                     otherwise; Y6 forbids the claimant
//	                                     (core). Before the claim is read (gate:
//	                                     approver "unread", nil owner)
//	                                     platform_admin and any university_rep
//	                                     pass, everyone else 403
//
// Resources (03 §3.5): club (OrgID = the club's org; Attrs sensitive,
// university_id), university (OrgID = its org; optional Attr is_active),
// directory_catalog, admin_surface (403 without a platform role, before
// anything is read; 03 §7.2-3). /clubs/{club_id}/... is a resource surface
// (03 §7.6): the club is visible to everyone, so a signed-in user without
// the role gets 403 (not 404).
//
// Sensitivity (03 Y2) concerns membership, not the club's existence: a
// sensitive club is listed and readable like any other; its member count
// is hidden by the service. A club_admin who is a minor is no club_admin
// (Y4, core).
//
// Club claims (03 §3.5: OrgID = the club's org, OwnerUserID = the
// claimant, Attrs status, university_org_id, approver). The approver is the
// service's fact (it reads orgs): "university_rep" when the club's
// university has an active university_rep other than the claimant and the
// club is not sensitive (a sensitive club's claim reveals special category
// data; it stays with platform staff, 03 Y2), "platform_admin" otherwise,
// "unread" before the claim is read (only with the nil owner of the gate).
// A missing, empty or unknown approver denies (H-27: fail closed). The orchestrator's
// E-21 rule (2026-10-04) supersedes 03 §5.3's platform_admin-only cell.
func directoryPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeDirectoryCatalog: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case DirectoryUniversityList, DirectorySkillList, DirectoryClubList:
				return r.Type == TypeDirectoryCatalog
			case DirectoryUniversityRead, DirectoryDepartmentList:
				return r.Type == TypeUniversity && (r.Attr("is_active") != "false" || a.HasPlatformRole(PlatformAdmin))
			case DirectoryClubRead, DirectoryClubUpdate, DirectoryClubRequestSensitivity, DirectoryClubClaimCreate:
				return r.Type == TypeClub
			case DirectoryClubCreate, DirectoryClubSetSensitivity, DirectoryUniversityManage, DirectorySkillManage:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case DirectoryClubClaimList:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a) || r.Type == TypeClubClaim && claimVisible(a, r)
			case DirectoryClubClaimReadOwn, DirectoryClubClaimWithdraw:
				return r.Type == TypeClubClaim && a.Owns(r)
			case DirectoryClubClaimDecide:
				return r.Type == TypeClubClaim && claimVisible(a, r)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad := Grant{Role: RolePlatformAdmin}
			clubAdmin := r.OrgID != nil && a.HasOrgRole(*r.OrgID, MemberClubAdmin)
			switch act {
			case DirectoryUniversityList, DirectoryUniversityRead, DirectoryDepartmentList, DirectorySkillList,
				DirectoryClubList, DirectoryClubRead:
				return []Grant{{Role: RolePublic}}
			case DirectoryClubUpdate:
				gs := []Grant{pad}
				if clubAdmin {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
				return gs
			case DirectoryClubRequestSensitivity:
				if clubAdmin {
					return []Grant{{Role: RoleClubAdmin}}
				}
				return nil
			case DirectoryClubCreate, DirectoryClubSetSensitivity, DirectoryUniversityManage, DirectorySkillManage:
				return []Grant{pad}
			case DirectoryClubClaimDecide:
				return claimApprover(a, r)
			case DirectoryClubClaimCreate:
				return []Grant{{Role: RoleStudent}} // stu, clm, cla, stf are adult verified students
			case DirectoryClubClaimReadOwn, DirectoryClubClaimWithdraw:
				return []Grant{{Role: RoleSelf}}
			case DirectoryClubClaimList:
				gs := []Grant{{Role: RoleSupport}, pad}
				if r.Type == TypeClubClaim {
					switch r.Attr(AttrClaimApprover) {
					case ClaimApproverUnread, ClaimApproverRep, ClaimApproverPlatformAdmin:
					default:
						return nil // fail closed
					}
					gs = append(gs, claimApprover(a, r)...) // the approving rep reads the claim
				}
				return gs
			}
			return nil
		},
	}
}

// Club claim Attrs (directory; 03 §3.5 extended by BE-022).
const (
	// AttrClaimUniversityOrg is the org ID of the claimed club's university
	// ("" before the claim is read).
	AttrClaimUniversityOrg = "university_org_id"
	// AttrClaimApprover is who decides the claim: ClaimApproverRep,
	// ClaimApproverPlatformAdmin, or ClaimApproverUnread before the claim is
	// read (the gate, whose owner is the nil UUID).
	AttrClaimApprover = "approver"
)

// Approvers of a club claim (E-21).
const (
	ClaimApproverRep           = "university_rep"
	ClaimApproverPlatformAdmin = "platform_admin"
	ClaimApproverUnread        = "unread"
)

// claimApprover returns the grant of the claim's approver (decide; also the
// rep's read of one claim). A missing or unknown approver grants nothing.
func claimApprover(a Actor, r Resource) []Grant {
	switch r.Attr(AttrClaimApprover) {
	case ClaimApproverUnread: // gate: the claim is not read yet
		if r.OwnerUserID == nil || *r.OwnerUserID != uuid.Nil() {
			return nil // a read claim always names its approver
		}
		gs := []Grant{{Role: RolePlatformAdmin}}
		if repAnywhere(a) {
			gs = append(gs, Grant{Role: RoleUniversityRep})
		}
		return gs
	case ClaimApproverPlatformAdmin:
		return []Grant{{Role: RolePlatformAdmin}}
	case ClaimApproverRep:
		if claimRep(a, r) {
			return []Grant{{Role: RoleUniversityRep}}
		}
	}
	return nil
}

// claimRep reports whether a is a university_rep of the claim's university
// and that rep approves the claim.
func claimRep(a Actor, r Resource) bool {
	if r.Attr(AttrClaimApprover) != ClaimApproverRep {
		return false
	}
	uni, err := uuid.Parse(r.Attr(AttrClaimUniversityOrg))
	return err == nil && a.HasOrgRole(uni, MemberUniversityRep)
}

// repAnywhere reports whether a holds university_rep in some org.
func repAnywhere(a Actor) bool {
	return a.IsUser() && slices.ContainsFunc(a.Memberships, func(m Membership) bool { return m.Role == MemberUniversityRep })
}

// claimVisible is the admin view of a claim: like the admin surface every
// user sees it (403 without a grant), except that a university_rep without
// a platform role sees only the claims they approve (another university's
// or a sensitive club's claim is 404) once the claim is read.
func claimVisible(a Actor, r Resource) bool {
	if r.OwnerUserID == nil || !AdminSurfaceVisible(a) {
		return false
	}
	if r.Attr(AttrClaimApprover) == ClaimApproverUnread || a.HasAnyPlatformRole() || !repAnywhere(a) {
		return true
	}
	return claimRep(a, r)
}
