package authz

import "uuid"

// Resource is what an action is performed on (01 §3.1). The calling service
// fills it before Can (03 §3.3): IDs come from the loaded record, never from
// the request alone, and Attrs carries the facts authz cannot read itself.
// Every Attrs value is a string ("true"/"false", UUID text).
type Resource struct {
	Type        string
	ID          uuid.UUID
	OrgID       *uuid.UUID // owning org, when there is one
	OwnerUserID *uuid.UUID // owning user, when there is one
	Attrs       map[string]string
}

// Attr returns Attrs[key] ("" when absent).
func (r Resource) Attr(key string) string { return r.Attrs[key] }

// Flag reports whether Attrs[key] is "true".
func (r Resource) Flag(key string) bool { return r.Attrs[key] == "true" }

// Resource types of 03 §3.5. A module may declare further types in its
// policy (Policy.Types); any other type is denied (03 §3.2-0).
const (
	TypeUser                   = "user"
	TypeSession                = "session"
	TypeEduVerification        = "edu_verification"
	TypePlatformRoleAssignment = "platform_role_assignment"
	TypeOrg                    = "org"
	TypeOrgMember              = "org_member"
	TypeOrgInvitation          = "org_invitation"
	TypeClub                   = "club"
	TypeClubClaim              = "club_claim"
	TypeUniversity             = "university"
	TypeStudentProfile         = "student_profile"
	TypeStudentCV              = "student_cv"
	TypeAccessGrant            = "access_grant"
	TypeConsentRecord          = "consent_record"
	TypeNoticeVersion          = "notice_version"
	TypeEvent                  = "event"
	TypeEventForm              = "event_form"
	TypeRegistration           = "registration"
	TypeTicket                 = "ticket"
	TypeCheckin                = "checkin"
	TypeEventExport            = "event_export"
	TypePost                   = "post"
	TypeContentReport          = "content_report"
	TypeSegment                = "segment"
	TypeDispatch               = "dispatch"
	TypeContactRequest         = "contact_request"
	TypeAnonProfile            = "anon_profile"
	TypeEntitlement            = "entitlement"
	TypeLegalParty             = "legal_party"
	TypePayee                  = "payee"
	TypeBankAccount            = "bank_account"
	TypeCollectionAccount      = "collection_account"
	TypeOffering               = "offering"
	TypeOrder                  = "order"
	TypeOrderLine              = "order_line"
	TypeIncomingTransfer       = "incoming_transfer"
	TypePayment                = "payment"
	TypeLedger                 = "ledger"
	TypeRefund                 = "refund"
	TypeReceivableCollection   = "receivable_collection"
	TypePayout                 = "payout"
	TypeInvoice                = "invoice"
	TypeNotification           = "notification"
	TypePushToken              = "push_token"
	TypeNotificationPref       = "notification_pref"
	TypeFile                   = "file"
	TypeAuditLog               = "audit_log"
	TypeDSR                    = "dsr"
	TypeTalentSubject          = "talent_subject"
	TypeAdminSurface           = "admin_surface" // platform management surface: visible to everyone (03 §3.6)
)

// Attrs keys read by the core (03 §3.5; the full per-type list is
// coreTypes). Module policies may read any key their service fills.
const (
	AttrStatus         = "status"
	AttrSensitive      = "sensitive"        // "true": sensitive club data (03 Y2); never logged
	AttrAccessGrant    = "access_grant"     // "active" | "none": level 3 company access (03 Y5)
	AttrAccessReason   = "access_reason"    // X-Koro-Access-Reason of a break-glass request (03 §6.3)
	AttrEventStaffRole = "event_staff_role" // "scanner" | "manager" | "none"
	AttrSubmittedBy    = "submitted_by"     // four eyes (03 §6.1): user IDs of earlier steps
	AttrVerifiedBy     = "verified_by"
	AttrCreatedBy      = "created_by"
	AttrRequestedBy    = "requested_by"
	AttrUploadedBy     = "uploaded_by"
	AttrFirstApprover  = "first_approver"
)

// coreTypes maps every 03 §3.5 resource type to its mandatory Attrs keys.
// A missing key denies the decision (programmer error). A key may be present
// with an empty value when the fact does not exist yet (e.g. verified_by
// before verification).
var coreTypes = map[string][]string{
	TypeUser:                   nil,
	TypeSession:                nil,
	TypeEduVerification:        {AttrStatus},
	TypePlatformRoleAssignment: {"role"},
	TypeOrg:                    {"org_kind", "org_status"},
	TypeOrgMember:              {"org_kind", "role", AttrStatus, AttrSensitive},
	TypeOrgInvitation:          {"org_kind", "role"},
	TypeClub:                   {AttrSensitive, "university_id"},
	TypeClubClaim:              {AttrStatus},
	TypeUniversity:             nil,
	TypeStudentProfile:         {AttrAccessGrant, "discoverable", "minor"},
	TypeStudentCV:              {AttrAccessGrant},
	TypeAccessGrant:            nil,
	TypeConsentRecord:          nil,
	TypeNoticeVersion:          nil,
	TypeEvent:                  {AttrStatus, "visibility", "visibility_university_id", AttrSensitive, AttrEventStaffRole},
	TypeEventForm:              {"event_status"},
	TypeRegistration:           {AttrEventStaffRole, AttrStatus},
	TypeTicket:                 nil,
	TypeCheckin:                {AttrEventStaffRole},
	TypeEventExport:            nil,
	TypePost:                   {AttrStatus, "audience_kind", "audience_match", "commercial", "follows_author"},
	TypeContentReport:          nil,
	TypeSegment:                nil,
	TypeDispatch:               nil,
	TypeContactRequest:         {AttrStatus},
	TypeAnonProfile:            {"company_org_id"},
	TypeEntitlement:            nil,
	TypeLegalParty:             {"kind"},
	TypePayee:                  nil,
	TypeBankAccount:            {AttrStatus, AttrSubmittedBy, AttrVerifiedBy},
	TypeCollectionAccount:      {AttrStatus, AttrCreatedBy},
	TypeOffering:               {AttrStatus, "university_id", "seller_kind"},
	TypeOrder:                  {"seller_org_ids"},
	TypeOrderLine:              {"seller_org_id"},
	TypeIncomingTransfer:       nil,
	TypePayment:                nil,
	TypeLedger:                 nil,
	TypeRefund:                 {AttrStatus, AttrRequestedBy},
	TypePayout:                 {AttrStatus, AttrCreatedBy},
	TypeInvoice:                {"recipient_org_id"},
	TypeNotification:           nil,
	TypePushToken:              nil,
	TypeNotificationPref:       nil,
	TypeFile:                   {"purpose"},
	TypeAuditLog:               nil,
	TypeDSR:                    {"type", AttrStatus, AttrFirstApprover},
	TypeTalentSubject:          nil,
	TypeAdminSurface:           nil,
}

// OrgResource is the TypeOrg resource of an org with its mandatory Attrs
// (coreTypes: org_kind, org_status). Modules build it from the org row they
// loaded (orgs.Org, orgs.OrgSummary), never from the request.
func OrgResource(id uuid.UUID, kind, status string) Resource {
	return Resource{Type: TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{
		"org_kind": kind, "org_status": status,
	}}
}
