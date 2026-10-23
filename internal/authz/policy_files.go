package authz

import "uuid"

// filesPolicy is the policy of the files module (03 §5.20). Only tickets of
// the files module edit this file (BE-001 R5). Written by BE-008.
//
// Resource (03 §3.5): Type file, OrgID = files.org_id, OwnerUserID =
// uploaded_by (the uploader; on files.upload.create the actor itself),
// Attrs purpose.
//
// Visibility (03 §3.6 "Dosya"): on the files endpoints only the uploader
// sees an existing file. A new upload for an org (org_logo, …) is an org
// surface: members of the org and platform roles see it, everyone else 404.
//
// files.upload.create follows k30 (purpose → roles; no mapping = 403):
//
//	avatar, cv                 verified students (stu, min; clm/cla/stf are students)
//	edu_document               every signed-in user
//	org_logo                   S: club_admin, university_rep, company_admin/owner; pad (orgs.org.update)
//	event_cover                S: club_admin, university_rep; pad k-po (events.event.create)
//	post_media                 S: club_admin, university_rep, recruiter, company_admin/owner; pad k-po (feed.post.create)
//	club_claim_evidence        adult verified students (directory.club_claim.create, k8)
//	party_document,
//	bank_account_evidence      S: club_admin, university_rep, company_admin/owner; fin; pad
//	transfer/payout/refund_receipt  fin
//	invoice_pdf                fin; S: club_admin, university_rep (billing.invoice.upload, k29)
//	delivery_evidence          S: club_admin, university_rep of the seller org (k-cs); pad
//	                           (orders.line.mark_fulfilled; BE-093, T-292)
//	event_export, dsr_export   nobody (PutGenerated only)
//
// "university_rep" never includes career_staff (k-cs). k-po: platform_admin
// acts for the platform org only, i.e. an org the actor belongs to with
// kind platform. MFA (k-mfa) follows from the role paths.
func filesPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			if act == FilesUploadCreate {
				return a.IsUser() && (r.OrgID == nil || OrgSurfaceVisible(a, r.OrgID))
			}
			return a.Owns(r)
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case FilesUploadComplete, FilesFileDeleteOwn:
				return []Grant{{Role: RoleSelf}}
			case FilesUploadCreate:
				return uploadGrants(a, r)
			}
			return nil
		},
	}
}

// File purposes as authz sees them (02 §3.1; files owns the list).
const (
	filePurposeAvatar        = "avatar"
	filePurposeCV            = "cv"
	filePurposeEduDocument   = "edu_document"
	filePurposeOrgLogo       = "org_logo"
	filePurposeEventCover    = "event_cover"
	filePurposePostMedia     = "post_media"
	filePurposeClubClaim     = "club_claim_evidence"
	filePurposePartyDocument = "party_document"
	filePurposeBankEvidence  = "bank_account_evidence"
	filePurposeTransferRcpt  = "transfer_receipt"
	filePurposePayoutRcpt    = "payout_receipt"
	filePurposeRefundRcpt    = "refund_receipt"
	filePurposeInvoicePDF    = "invoice_pdf"
	filePurposeDeliveryProof = "delivery_evidence"
	fileOrgAdmins            = "admins"    // club_admin, university_rep, company_admin/owner
	fileOrgEventers          = "eventers"  // club_admin, university_rep
	fileOrgPosters           = "posters"   // club_admin, university_rep, recruiter, company_admin/owner
	fileOrgInvoiceUploaders  = "invoicers" // club_admin, university_rep
	fileOrgSellers           = "sellers"   // club_admin, university_rep (the markers of an order line)
)

// orgGrants returns the S grants the actor holds in orgID for one org role
// set of k30.
func orgGrants(a Actor, orgID *uuid.UUID, set string) []Grant {
	if orgID == nil {
		return nil
	}
	type pair struct {
		member MemberRole
		role   Role
	}
	pairs := []pair{{MemberClubAdmin, RoleClubAdmin}, {MemberUniversityRep, RoleUniversityRep}}
	switch set {
	case fileOrgAdmins:
		pairs = append(pairs, pair{MemberCompanyAdmin, RoleCompanyAdmin}, pair{MemberCompanyOwner, RoleCompanyOwner})
	case fileOrgPosters:
		pairs = append(pairs, pair{MemberRecruiter, RoleCompanyMember},
			pair{MemberCompanyAdmin, RoleCompanyAdmin}, pair{MemberCompanyOwner, RoleCompanyOwner})
	}
	var out []Grant
	for _, p := range pairs {
		if a.HasOrgRole(*orgID, p.member) {
			out = append(out, Grant{Role: p.role})
		}
	}
	return out
}

// platformOrg reports k-po: orgID is a platform-kind org the actor belongs to.
func platformOrg(a Actor, orgID *uuid.UUID) bool {
	if orgID == nil || !a.IsUser() {
		return false
	}
	for _, m := range a.Memberships {
		if m.OrgID == *orgID && m.OrgKind == OrgPlatform {
			return true
		}
	}
	return false
}

func uploadGrants(a Actor, r Resource) []Grant {
	students := []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
	fin := Grant{Role: RoleFinanceAdmin}
	pad := Grant{Role: RolePlatformAdmin}
	switch r.Attr("purpose") {
	case filePurposeAvatar, filePurposeCV:
		return students
	case filePurposeEduDocument:
		return append(students, Grant{Role: RoleUserUnverified})
	case filePurposeOrgLogo:
		return append(orgGrants(a, r.OrgID, fileOrgAdmins), pad)
	case filePurposeEventCover:
		gs := orgGrants(a, r.OrgID, fileOrgEventers)
		if platformOrg(a, r.OrgID) {
			gs = append(gs, pad)
		}
		return gs
	case filePurposePostMedia:
		gs := orgGrants(a, r.OrgID, fileOrgPosters)
		if platformOrg(a, r.OrgID) {
			gs = append(gs, pad)
		}
		return gs
	case filePurposeClubClaim:
		return []Grant{{Role: RoleStudent}}
	case filePurposePartyDocument, filePurposeBankEvidence:
		return append(orgGrants(a, r.OrgID, fileOrgAdmins), fin, pad)
	case filePurposeTransferRcpt, filePurposePayoutRcpt, filePurposeRefundRcpt:
		return []Grant{fin}
	case filePurposeInvoicePDF:
		return append(orgGrants(a, r.OrgID, fileOrgInvoiceUploaders), fin)
	case filePurposeDeliveryProof:
		// Whoever may mark the seller's lines delivered (orders.line.mark_fulfilled):
		// the seller org's club_admin and university_rep, and platform_admin.
		return append(orgGrants(a, r.OrgID, fileOrgSellers), pad)
	}
	return nil // event_export, dsr_export and unknown purposes: 403
}
