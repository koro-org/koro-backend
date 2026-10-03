package authz

import (
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"
)

// Absolute bans of 03 §3.4. They apply after every role path, whatever a
// module policy grants, and are owned by the core (module tickets never
// edit this file).

// eyesKey is one Attrs key of a four-eyes rule. required: the key must hold
// a user ID (the earlier step always exists); otherwise an empty value means
// the action is the first step (e.g. the first DSR approval).
type eyesKey struct {
	attr     string
	required bool
}

// fourEyesRules is 03 §6.1 (+ 06 H-17, H-18): the approver must differ from
// every user ID in these keys (Y3). A missing key, or an empty required
// key, denies the decision as a programmer error.
//
// invoice uploaded_by, offering created_by, platform role and transfer
// return requested_by are not in the 03 §3.5 Attrs table yet: the service
// must still pass them (open question recorded by BE-004).
var fourEyesRules = map[Action][]eyesKey{
	PartiesBankAccountVerify:         {{AttrSubmittedBy, true}},
	PartiesBankAccountApprove:        {{AttrSubmittedBy, true}, {AttrVerifiedBy, false}},
	PaymentsCollectionAccountApprove: {{AttrCreatedBy, true}},
	LedgerPayoutApprove:              {{AttrCreatedBy, true}},
	PaymentsRefundApprove:            {{AttrRequestedBy, true}},
	BillingInvoiceVerify:             {{AttrUploadedBy, true}},
	CatalogOfferingDecide:            {{AttrCreatedBy, true}},
	KvkkDsrApprove:                   {{AttrFirstApprover, false}},
	IdentityPlatformRoleGrant:        {{AttrRequestedBy, false}}, // H-17: privileged roles need a second platform_admin
	IdentityPlatformRoleRevoke:       {{AttrRequestedBy, false}},
	PaymentsTransferMarkReturned:     {{AttrRequestedBy, false}}, // H-18
}

// selfForbidden is Y6: nobody grants/revokes their own platform role, resets
// their own MFA, or approves their own student document or club claim. The
// resource owner is the target user; it must be set.
var selfForbidden = map[Action]bool{
	IdentityPlatformRoleGrant:     true,
	IdentityPlatformRoleRevoke:    true,
	IdentityUserResetMfa:          true,
	IdentityEduVerificationReview: true,
	DirectoryClubClaimDecide:      true,
}

// companyRoles may reach student personal data only with level 3 access (Y5).
var companyRoles = map[Role]bool{RoleCompanyMember: true, RoleCompanyAdmin: true, RoleCompanyOwner: true}

// breakGlassRoles are the only roles with B cells (03 §6.3).
var breakGlassRoles = map[Role]bool{RoleSupport: true, RolePlatformAdmin: true}

// Break-glass reason length in characters (03 §6.3).
const (
	minAccessReason = 10
	maxAccessReason = 500
)

// checkResourceBans applies the bans that depend only on the resource:
// Y2 (sensitive membership visibility) and Y6 (self-approval).
func checkResourceBans(a Actor, act Action, r Resource) error {
	// Y2: a sensitive club membership is visible only to the member and the
	// club's club_admin (not even break-glass or platform roles).
	if r.Type == TypeOrgMember && r.Flag(AttrSensitive) {
		self := a.Owns(r)
		admin := r.OrgID != nil && a.HasOrgRole(*r.OrgID, MemberClubAdmin) && !a.IsMinor
		if !self && !admin {
			return deny(ErrNotFound, act, "sensitive club membership (Y2)")
		}
	}
	// Y6: self-approval.
	if selfForbidden[act] {
		if r.OwnerUserID == nil {
			return denyInternal(act, "OwnerUserID (target user) is required (Y6)")
		}
		if a.Owns(r) {
			return deny(ErrForbidden, act, "self-approval (Y6)")
		}
	}
	return nil
}

// holds reports whether the actor's facts back the role path of a grant, so
// a faulty policy can never grant a role the actor does not have. Org scope
// (cell S) stays with the policy: here a membership role anywhere counts.
func holds(a Actor, g Grant, r Resource) bool {
	if g.Role == RolePublic {
		return true
	}
	if !a.IsUser() {
		return false
	}
	hasMember := func(roles ...MemberRole) bool {
		return slices.ContainsFunc(a.Memberships, func(m Membership) bool { return slices.Contains(roles, m.Role) })
	}
	switch g.Role {
	case RoleSelf:
		return a.Owns(r)
	case RoleUserUnverified:
		// An account without an active student verification (03 §1.1 row 2):
		// a verified student, minor or not, never takes the usr column.
		return !a.IsVerifiedStudent()
	case RoleStudent:
		return a.IsVerifiedStudent() && !a.IsMinor
	case RoleStudentMinor:
		return a.IsVerifiedStudent() && a.IsMinor
	case RoleClubMember:
		return hasMember(MemberMember)
	case RoleClubAdmin:
		return hasMember(MemberClubAdmin)
	case RoleEventStaff:
		s := r.Attr(AttrEventStaffRole)
		return s == "scanner" || s == "manager"
	case RoleUniversityRep:
		return hasMember(MemberUniversityRep)
	case RoleCareerStaff:
		return hasMember(MemberCareerStaff)
	case RoleCompanyMember:
		return hasMember(MemberRecruiter)
	case RoleCompanyAdmin:
		return hasMember(MemberCompanyAdmin, MemberCompanyOwner)
	case RoleCompanyOwner:
		return hasMember(MemberCompanyOwner)
	case RoleModerator:
		return a.HasPlatformRole(PlatformModerator)
	case RoleSupport:
		return a.HasPlatformRole(PlatformSupport)
	case RoleDPO:
		return a.HasPlatformRole(PlatformDPO)
	case RolePlatformAdmin:
		return a.HasPlatformRole(PlatformAdmin)
	case RoleFinanceAdmin:
		return a.HasPlatformRole(PlatformFinanceAdmin)
	case RoleTalentAuditor:
		return a.HasPlatformRole(PlatformTalentAuditor)
	}
	return false
}

// filterGrants drops the grants the actor's facts do not back (holds) and
// those an absolute ban voids: Y1 (talent), Y2 (break-glass on sensitive
// data), Y4 (minor club_admin) and Y5 (company access to student data
// without level 3 access). Malformed grants are programmer errors. When
// nothing is left, the error is ErrNotFound if a hiding ban (Y2, Y5) removed
// a grant, ErrForbidden otherwise.
func filterGrants(a Actor, act Action, r Resource, in []Grant) ([]Grant, error) {
	out := make([]Grant, 0, len(in))
	reason, hide := "no role path grants the action", false
	for _, g := range in {
		switch {
		case !knownRoles[g.Role]:
			return nil, denyInternal(act, "grant with unknown role "+string(g.Role))
		case g.Role == RolePublic && (g.MFA || g.BreakGlass || len(g.FourEyes) > 0):
			return nil, denyInternal(act, "a public grant carries no condition")
		case g.BreakGlass && !breakGlassRoles[g.Role]:
			return nil, denyInternal(act, "break-glass grant for role "+string(g.Role))
		case !holds(a, g, r):
			// not this actor's role path
		case act.Module() == "talent" && g.Role != RoleTalentAuditor:
			reason = "talent actions are talent_auditor only (Y1)"
		case g.BreakGlass && r.Flag(AttrSensitive):
			reason, hide = "break-glass never reveals sensitive club data (Y2)", true
		case g.Role == RoleClubAdmin && a.IsMinor:
			reason = "a minor cannot act as club_admin (Y4)"
		case companyRoles[g.Role] && (r.Type == TypeStudentProfile || r.Type == TypeStudentCV) &&
			r.Attr(AttrAccessGrant) != "active":
			reason, hide = "company roles need level 3 access to student data (Y5)", true
		default:
			out = append(out, g)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if hide {
		return nil, deny(ErrNotFound, act, reason)
	}
	return nil, deny(ErrForbidden, act, reason)
}

// checkFourEyesKeys validates the Attrs of a four-eyes action before any
// grant is evaluated (fail closed on a service that forgot a key).
func checkFourEyesKeys(act Action, r Resource) error {
	for _, k := range fourEyesRules[act] {
		v, ok := r.Attrs[k.attr]
		switch {
		case !ok:
			return denyInternal(act, "Attrs key "+k.attr+" is required (four eyes, 03 §6.1)")
		case k.required && v == "":
			return denyInternal(act, "Attrs key "+k.attr+" must hold the initiator (four eyes, 03 §6.1)")
		case v != "":
			if _, err := uuid.Parse(v); err != nil {
				return denyInternal(act, "Attrs key "+k.attr+" is not a UUID")
			}
		}
	}
	return nil
}

// condition ranks follow 03 §3.2-5: MFA → four eyes → break-glass.
const (
	rankOK = iota
	rankMFA
	rankFourEyes
	rankBreakGlass
	rankInternal
)

// checkConditions evaluates one grant's conditions.
func checkConditions(a Actor, act Action, r Resource, g Grant) (int, error) {
	if g.Role == RolePublic {
		return rankOK, nil
	}
	if (mfaRoles[g.Role] || g.MFA) && !a.MFAVerified {
		return rankMFA, deny(ErrMFARequired, act, "role path "+string(g.Role)+" requires an MFA-verified session")
	}
	keys := make([]string, 0, len(fourEyesRules[act])+len(g.FourEyes))
	for _, k := range fourEyesRules[act] {
		keys = append(keys, k.attr)
	}
	for _, k := range g.FourEyes {
		v, ok := r.Attrs[k]
		if !ok {
			return rankInternal, denyInternal(act, "Attrs key "+k+" is required by the grant (four eyes)")
		}
		if _, err := uuid.Parse(v); v != "" && err != nil {
			return rankInternal, denyInternal(act, "Attrs key "+k+" is not a UUID")
		}
		keys = append(keys, k)
	}
	for _, k := range keys {
		if v := r.Attrs[k]; v != "" && uuid.MustParse(v) == a.UserID {
			return rankFourEyes, deny(ErrFourEyes, act, "actor is the "+k+" user")
		}
	}
	if g.BreakGlass {
		n := utf8.RuneCountInString(strings.TrimSpace(r.Attr(AttrAccessReason)))
		if n < minAccessReason || n > maxAccessReason {
			return rankBreakGlass, deny(ErrBreakGlassReason, act, "break-glass needs a 10–500 character access reason")
		}
	}
	return rankOK, nil
}
