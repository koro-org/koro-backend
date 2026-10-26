package identity

import (
	"sort"
	"time"
	"uuid"

	"koro/internal/authz"
)

// TOTPEnrollmentDTO is the answer of enrollTotp: the only response that
// ever carries the TOTP secret (04 §2.1).
type TOTPEnrollmentDTO struct {
	OTPAuthURI   string `json:"otpauth_uri" doc:"otpauth:// URI for authenticator apps (QR code); shown once"`
	SecretBase32 string `json:"secret_base32" doc:"The TOTP secret for manual entry; shown once"`
}

// RecoveryCodesDTO carries freshly generated recovery codes, shown once.
type RecoveryCodesDTO struct {
	RecoveryCodes []string `json:"recovery_codes" doc:"Ten single-use recovery codes; store them now, they are not shown again"`
}

// AdminUserSummaryDTO is one row of adminQueryUsers (04 §V.5 subset).
type AdminUserSummaryDTO struct {
	UserID      string     `json:"user_id" format:"uuid"`
	Email       string     `json:"email,omitempty"`
	FirstName   string     `json:"first_name,omitempty"`
	LastName    string     `json:"last_name,omitempty"`
	Status      string     `json:"status" enum:"pending_email,active,suspended,erased"`
	MFAEnabled  bool       `json:"mfa_enabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// AdminUserList is the body of adminQueryUsers.
type AdminUserList struct {
	Items      []AdminUserSummaryDTO `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// AdminStudentDTO is the student verification of an account (04 §V.5).
type AdminStudentDTO struct {
	Status       string     `json:"status" enum:"none,verified,lapsed"`
	UniversityID string     `json:"university_id,omitempty" format:"uuid"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// AdminUserDTO is adminGetUser (04 §V.5): no profile content, no club
// membership (a sensitive one must never show), birth date for dpo only.
type AdminUserDTO struct {
	UserID           string             `json:"user_id" format:"uuid"`
	Email            string             `json:"email,omitempty"`
	FirstName        string             `json:"first_name,omitempty"`
	LastName         string             `json:"last_name,omitempty"`
	Status           string             `json:"status" enum:"pending_email,active,suspended,erased"`
	Locale           string             `json:"locale" enum:"tr-TR,en-US"`
	MFAEnabled       bool               `json:"mfa_enabled"`
	CreatedAt        time.Time          `json:"created_at"`
	LastLoginAt      *time.Time         `json:"last_login_at,omitempty"`
	SuspendedAt      *time.Time         `json:"suspended_at,omitempty"`
	SuspensionReason string             `json:"suspension_reason,omitempty"`
	BirthDate        string             `json:"birth_date,omitempty" format:"date" doc:"dpo only"`
	Student          AdminStudentDTO    `json:"student"`
	PlatformRoles    []string           `json:"platform_roles"`
	Memberships      []MembershipDTO    `json:"memberships" doc:"Non-club memberships only"`
	PendingAction    *AccountActionDTO  `json:"pending_action,omitempty" doc:"Set when this call opened (or kept) a four-eyes request instead of acting (06 H-47)"`
	PendingActions   []AccountActionDTO `json:"pending_actions" doc:"Open four-eyes requests against this account (06 H-47): pending and not expired, oldest first. A second platform_admin approves one by repeating its action (adminSuspendUser, adminRevokeUserSessions, adminResetUserMfa); the requester cannot"`
}

// AccountActionDTO is a four-eyes account action request (06 H-47): an
// action against the holder of a privileged platform role waits for a
// second platform_admin, who repeats the same call.
type AccountActionDTO struct {
	RequestID   string    `json:"request_id" format:"uuid"`
	UserID      string    `json:"user_id" format:"uuid"`
	Action      string    `json:"action" enum:"suspend,revoke_sessions,reset_mfa"`
	Status      string    `json:"status" enum:"pending,approved,expired"`
	Reason      string    `json:"reason" doc:"The requester's reason (audited)"`
	RequestedBy string    `json:"requested_by" format:"uuid"`
	RequestedAt time.Time `json:"requested_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func toAccountActionDTO(r *AccountActionRequest) *AccountActionDTO {
	if r == nil {
		return nil
	}
	return &AccountActionDTO{RequestID: r.ID.String(), UserID: r.TargetUserID.String(), Action: string(r.Action), Status: string(r.Status),
		Reason: r.Reason, RequestedBy: r.RequestedBy.String(), RequestedAt: r.RequestedAt, ExpiresAt: r.ExpiresAt}
}

// UserRefDTO names a user in an admin list (no e-mail).
type UserRefDTO struct {
	UserID    string `json:"user_id" format:"uuid"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// PlatformRoleAssignmentDTO is one platform role assignment (02 §5.7 after
// 06 H-17: request and approval columns instead of granted_by).
type PlatformRoleAssignmentDTO struct {
	AssignmentID      string     `json:"assignment_id" format:"uuid"`
	User              UserRefDTO `json:"user"`
	Role              string     `json:"role" enum:"platform_admin,moderator,finance_admin,talent_auditor,support,dpo"`
	Status            string     `json:"status" enum:"pending,active,revoke_pending,revoked,rejected"`
	Reason            string     `json:"reason"`
	RequestedBy       string     `json:"requested_by,omitempty" format:"uuid" doc:"Absent on bootstrap rows"`
	RequestedAt       time.Time  `json:"requested_at"`
	ApprovedBy        string     `json:"approved_by,omitempty" format:"uuid" doc:"The second platform_admin (privileged roles)"`
	ApprovedAt        *time.Time `json:"approved_at,omitempty"`
	GrantedAt         *time.Time `json:"granted_at,omitempty" doc:"The role is in effect since"`
	RejectedBy        string     `json:"rejected_by,omitempty" format:"uuid"`
	RejectedAt        *time.Time `json:"rejected_at,omitempty"`
	RevokeReason      string     `json:"revoke_reason,omitempty"`
	RevokeRequestedBy string     `json:"revoke_requested_by,omitempty" format:"uuid"`
	RevokeRequestedAt *time.Time `json:"revoke_requested_at,omitempty"`
	RevokedBy         string     `json:"revoked_by,omitempty" format:"uuid"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// PlatformRoleList is the body of adminListPlatformRoles.
type PlatformRoleList struct {
	Items      []PlatformRoleAssignmentDTO `json:"items"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

func idText(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func toRoleDTO(a RoleAssignment, u UserSummary) PlatformRoleAssignmentDTO {
	return PlatformRoleAssignmentDTO{
		AssignmentID: a.ID.String(), User: UserRefDTO{UserID: a.UserID.String(), FirstName: u.FirstName, LastName: u.LastName},
		Role: string(a.Role), Status: string(a.Status), Reason: a.Reason, RequestedBy: idText(a.RequestedBy),
		RequestedAt: a.RequestedAt, ApprovedBy: idText(a.ApprovedBy), ApprovedAt: a.ApprovedAt, GrantedAt: a.GrantedAt,
		RejectedBy: idText(a.RejectedBy), RejectedAt: a.RejectedAt, RevokeReason: a.RevokeReason,
		RevokeRequestedBy: idText(a.RevokeRequestedBy), RevokeRequestedAt: a.RevokeRequestedAt,
		RevokedBy: idText(a.RevokedBy), RevokedAt: a.RevokedAt,
	}
}

func toAdminSummaryDTO(u User) AdminUserSummaryDTO {
	return AdminUserSummaryDTO{
		UserID: u.ID.String(), Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Status: string(u.Status),
		MFAEnabled: u.MFAEnabled, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
}

func toAdminUserDTO(v AdminUser) AdminUserDTO {
	u := v.User
	d := AdminUserDTO{
		UserID: u.ID.String(), Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Status: string(u.Status),
		Locale: u.Locale, MFAEnabled: u.MFAEnabled, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
		SuspendedAt: u.SuspendedAt, SuspensionReason: u.SuspensionReason, Student: AdminStudentDTO{Status: "none"},
		PlatformRoles: make([]string, 0, len(v.PlatformRoles)), Memberships: make([]MembershipDTO, 0, len(v.Memberships)),
		PendingActions: make([]AccountActionDTO, len(v.PendingActions)),
	}
	for i := range v.PendingActions {
		d.PendingActions[i] = *toAccountActionDTO(&v.PendingActions[i])
	}
	if v.ShowBirthDate && u.BirthDate != nil {
		d.BirthDate = u.BirthDate.String()
	}
	if v.StudentStatus == authz.StudentLapsed {
		d.Student.Status = string(authz.StudentLapsed)
	}
	if s := v.Student; s != nil {
		va, ex := s.VerifiedAt, s.ExpiresAt
		d.Student = AdminStudentDTO{Status: "verified", UniversityID: s.UniversityID.String(), VerifiedAt: &va, ExpiresAt: &ex}
	}
	for _, r := range v.PlatformRoles {
		d.PlatformRoles = append(d.PlatformRoles, string(r))
	}
	sort.Strings(d.PlatformRoles)
	for _, m := range v.Memberships {
		if m.OrgKind == authz.OrgClub {
			continue // defense in depth: never a club membership (03 Y2)
		}
		d.Memberships = append(d.Memberships, MembershipDTO{OrgID: m.OrgID.String(), OrgKind: string(m.OrgKind), Role: string(m.Role)})
	}
	return d
}
