package identity

import (
	"sort"
	"time"

	"koro/internal/consent"
)

// SessionDTO is a created session (04 §2.1). token is present for mobile
// clients only, exactly once; web clients get the __Host-koro_session
// cookie instead. For an MFA account the login answers mfa_required with a
// challenge and no session (03 §6.4-1).
type SessionDTO struct {
	UserID        string     `json:"user_id,omitempty" format:"uuid"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty" doc:"Absolute expiry of the session (or of the MFA challenge)"`
	IdleExpiresAt *time.Time `json:"idle_expires_at,omitempty" doc:"Expiry if the session stays idle; slides with use"`
	Token         string     `json:"token,omitempty" doc:"Opaque bearer token (mobile clients only; shown once)"`
	MFARequired   bool       `json:"mfa_required,omitempty" doc:"true: complete the login with completeMfaLogin"`
	ChallengeID   string     `json:"challenge_id,omitempty" format:"uuid"`
}

// MySessionDTO is one live session of the caller (IP and user agent are
// never returned; 04 §2.1).
type MySessionDTO struct {
	SessionID   string    `json:"session_id" format:"uuid"`
	ClientKind  string    `json:"client_kind" enum:"web,mobile"`
	DeviceLabel string    `json:"device_label,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Current     bool      `json:"current" doc:"The session of this request"`
}

// MySessionList is the body of listMySessions.
type MySessionList struct {
	Items      []MySessionDTO `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// MembershipDTO is one active org membership of the caller (sensitive clubs
// included: only the owner sees them, 04 §V.1).
type MembershipDTO struct {
	OrgID   string `json:"org_id" format:"uuid"`
	OrgKind string `json:"org_kind" enum:"club,university,company,platform,ngo"`
	Role    string `json:"role"`
}

// StudentDTO is the student verification state of the caller.
type StudentDTO struct {
	Status       string `json:"status" enum:"none,verified,lapsed"`
	UniversityID string `json:"university_id,omitempty" format:"uuid"`
}

// MeDTO is the identity part of getMe (04 §V.1). Student profile sections
// are added by the students module (BE-022).
type MeDTO struct {
	UserID          string          `json:"user_id" format:"uuid"`
	Email           string          `json:"email"`
	FirstName       string          `json:"first_name"`
	LastName        string          `json:"last_name"`
	Locale          string          `json:"locale" enum:"tr-TR,en-US"`
	SignupPersona   string          `json:"signup_persona" enum:"student,company,university,club,platform"`
	MFAEnabled      bool            `json:"mfa_enabled"`
	EmailVerifiedAt *time.Time      `json:"email_verified_at,omitempty"`
	BirthDate       string          `json:"birth_date,omitempty" format:"date"`
	IsMinor         bool            `json:"is_minor"`
	Student         StudentDTO      `json:"student"`
	Memberships     []MembershipDTO `json:"memberships"`
	PlatformRoles   []string        `json:"platform_roles"`
	Capabilities    []string        `json:"capabilities" doc:"Menu hints only; every request is authorized on its own"`
}

func toMeDTO(m Me) MeDTO {
	d := MeDTO{
		UserID: m.UserID.String(), Email: m.Email, FirstName: m.FirstName, LastName: m.LastName, Locale: m.Locale,
		SignupPersona: string(m.Persona), MFAEnabled: m.MFAEnabled, EmailVerifiedAt: m.EmailVerifiedAt, IsMinor: m.IsMinor,
		Student:     StudentDTO{Status: string(m.StudentStatus)},
		Memberships: make([]MembershipDTO, 0, len(m.Memberships)), PlatformRoles: make([]string, 0, len(m.PlatformRoles)),
		Capabilities: make([]string, 0, len(m.Capabilities)),
	}
	if d.Student.Status == "" {
		d.Student.Status = "none"
	}
	if m.UniversityID != nil {
		d.Student.UniversityID = m.UniversityID.String()
	}
	if m.BirthDate != nil {
		d.BirthDate = m.BirthDate.String()
	}
	for _, x := range m.Memberships {
		d.Memberships = append(d.Memberships, MembershipDTO{OrgID: x.OrgID.String(), OrgKind: string(x.OrgKind), Role: string(x.Role)})
	}
	for _, r := range m.PlatformRoles {
		d.PlatformRoles = append(d.PlatformRoles, string(r))
	}
	sort.Strings(d.PlatformRoles)
	for _, c := range m.Capabilities {
		d.Capabilities = append(d.Capabilities, string(c))
	}
	return d
}

// reauthProof reads the shared "reauth" body member (04 §1.11; the schema
// is consent's ReauthDTO, one OpenAPI component for every R operation).
func reauthProof(r *consent.ReauthDTO) ReauthProof {
	if r == nil {
		return ReauthProof{}
	}
	return ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}
