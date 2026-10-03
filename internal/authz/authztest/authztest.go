// Package authztest provides the 18 personas of the authz matrix (03 §1.1,
// §7.3), their helper users and org fixtures with deterministic IDs, and a
// test ActorResolver that maps persona tokens to actors without identity.
//
// It is imported only by tests (cmd/koro matrix harness, module service and
// policy tests). Fixture IDs are UUIDv5 of a fixed namespace and the fixture
// name (03 §7.3: "uuid.NewSHA1(ns, name)"), so YAML files never contain
// UUIDs and every module computes the same ID for the same name.
package authztest

import (
	"crypto/sha1" //nolint:gosec // UUIDv5 (RFC 9562 §5.5) is defined with SHA-1; no security use
	"slices"
	"uuid"

	"koro/internal/authz"
)

// Namespace is the UUIDv5 namespace of every matrix fixture ID.
var Namespace = uuid.MustParse("6b6f726f-0004-4a5a-8d1e-617574687a74")

// ID returns the deterministic ID of a fixture name: UUIDv5(Namespace, name).
// Org fixtures use the org name ("club_a"); the universities row of a
// university org uses "<org>_university" ("uni_a_university").
func ID(name string) uuid.UUID { return NameUUID(Namespace, name) }

// NameUUID returns the RFC 9562 version 5 UUID of name in namespace ns.
func NameUUID(ns uuid.UUID, name string) uuid.UUID {
	h := sha1.New() //nolint:gosec // see the import
	h.Write(ns[:])
	h.Write([]byte(name))
	var u uuid.UUID
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u
}

// UniversityID returns the universities.id of a university org fixture.
func UniversityID(org string) uuid.UUID { return ID(org + "_university") }

// Org is an org fixture shared by the personas (03 §7.3 "Org'lar"). The
// orgs and directory modules seed these rows with these IDs in their
// matrix fixtures.
type Org struct {
	Name       string
	Kind       authz.OrgKind
	University string // owning university org fixture (clubs)
	Sensitive  bool   // clubs.is_sensitive
	Claimed    bool   // clubs with an active club_admin
}

// Orgs returns the org fixtures of 03 §7.3.
func Orgs() []Org {
	return []Org{
		{Name: "platform_org", Kind: authz.OrgPlatform},
		{Name: "uni_a", Kind: authz.OrgUniversity},
		{Name: "uni_b", Kind: authz.OrgUniversity},
		{Name: "club_a", Kind: authz.OrgClub, University: "uni_a", Claimed: true},
		{Name: "club_b", Kind: authz.OrgClub, University: "uni_b", Claimed: true},
		{Name: "club_sensitive", Kind: authz.OrgClub, University: "uni_a", Sensitive: true, Claimed: true},
		{Name: "club_unclaimed", Kind: authz.OrgClub, University: "uni_a"},
		{Name: "company_a", Kind: authz.OrgCompany},
		{Name: "company_b", Kind: authz.OrgCompany},
	}
}

// Persona is a matrix persona or a helper user. Token is its bearer token
// for the test resolver ("" for anonymous).
type Persona struct {
	Name   string
	Helper bool // helper user (resource owner / first eye), not a matrix persona
	Token  string
	Actor  authz.Actor
}

// User returns the user ID of the persona (uuid.Nil for anonymous).
func (p Persona) User() uuid.UUID { return p.Actor.UserID }

// PersonaNames are the 18 matrix personas in report order (03 §1.1, §7.5).
var PersonaNames = []string{
	"anonymous", "user_unverified", "student", "student_minor", "club_member", "club_admin",
	"other_club_admin", "event_staff", "university_rep", "company_member", "company_admin",
	"other_company_member", "moderator", "support", "dpo", "platform_admin", "finance_admin",
	"talent_auditor",
}

// HelperNames are the helper users of 03 §7.3 (owners and first eyes).
var HelperNames = []string{
	"club_admin_sensitive", "company_owner_a", "event_manager_a", "student_b", "student_granted",
	"finance_admin_2", "platform_admin_2", "dpo_2",
}

// Personas returns the 18 personas in report order.
func Personas() []Persona {
	out := make([]Persona, len(PersonaNames))
	for i, n := range PersonaNames {
		out[i], _ = Get(n)
	}
	return out
}

// Helpers returns the helper users.
func Helpers() []Persona {
	out := make([]Persona, len(HelperNames))
	for i, n := range HelperNames {
		out[i], _ = Get(n)
	}
	return out
}

// Get returns the persona or helper user called name.
func Get(name string) (Persona, bool) {
	spec, ok := specs()[name]
	if !ok {
		return Persona{}, false
	}
	p := Persona{Name: name, Helper: slices.Contains(HelperNames, name)}
	if name == "anonymous" {
		p.Actor = authz.Anonymous()
		return p, true
	}
	p.Token = "persona-" + name
	a := authz.Actor{
		Kind:          authz.KindUser,
		UserID:        ID(name),
		SessionID:     ID(name + "_session"),
		StudentStatus: authz.StudentNone,
		IsMinor:       spec.minor,
		PlatformRoles: slices.Clone(spec.platform),
	}
	if spec.uni != "" {
		a.StudentStatus = authz.StudentVerified
		u := UniversityID(spec.uni)
		a.UniversityID = &u
	}
	for _, m := range spec.members {
		a.Memberships = append(a.Memberships, authz.Membership{OrgID: ID(m.org), OrgKind: orgKind(m.org), Role: m.role})
	}
	// Sessions of MFA-required roles are MFA verified (03 §7.3).
	a.MFAVerified = len(a.PlatformRoles) > 0 || slices.ContainsFunc(a.Memberships, func(m authz.Membership) bool {
		return m.Role == authz.MemberUniversityRep || m.Role == authz.MemberCompanyAdmin || m.Role == authz.MemberCompanyOwner
	})
	p.Actor = a
	return p, true
}

type member struct {
	org  string
	role authz.MemberRole
}

type spec struct {
	uni      string // verified student of this university org ("" = not a student)
	minor    bool
	members  []member
	platform []authz.PlatformRole
}

// specs is 03 §7.3 "Personalar (18)" and "Yardımcı kullanıcılar". Event
// staff roles are not memberships: the events service passes them in
// Attrs (event_a: event_staff scanner, event_manager_a manager).
func specs() map[string]spec {
	return map[string]spec{
		"anonymous":            {},
		"user_unverified":      {},
		"student":              {uni: "uni_a"},
		"student_minor":        {uni: "uni_a", minor: true},
		"club_member":          {uni: "uni_a", members: []member{{"club_a", authz.MemberMember}, {"club_sensitive", authz.MemberMember}}},
		"club_admin":           {uni: "uni_a", members: []member{{"club_a", authz.MemberClubAdmin}}},
		"other_club_admin":     {uni: "uni_b", members: []member{{"club_b", authz.MemberClubAdmin}}},
		"event_staff":          {uni: "uni_a"},
		"university_rep":       {members: []member{{"uni_a", authz.MemberUniversityRep}}},
		"company_member":       {members: []member{{"company_a", authz.MemberRecruiter}}},
		"company_admin":        {members: []member{{"company_a", authz.MemberCompanyAdmin}}},
		"other_company_member": {members: []member{{"company_b", authz.MemberCompanyAdmin}}},
		"moderator":            {platform: []authz.PlatformRole{authz.PlatformModerator}},
		"support":              {platform: []authz.PlatformRole{authz.PlatformSupport}},
		"dpo":                  {platform: []authz.PlatformRole{authz.PlatformDPO}},
		"platform_admin":       {platform: []authz.PlatformRole{authz.PlatformAdmin}},
		"finance_admin":        {platform: []authz.PlatformRole{authz.PlatformFinanceAdmin}},
		"talent_auditor":       {platform: []authz.PlatformRole{authz.PlatformTalentAuditor}},

		"club_admin_sensitive": {uni: "uni_a", members: []member{{"club_sensitive", authz.MemberClubAdmin}}},
		"company_owner_a":      {members: []member{{"company_a", authz.MemberCompanyOwner}}},
		"event_manager_a":      {uni: "uni_a"},
		"student_b":            {uni: "uni_a"},
		"student_granted":      {uni: "uni_a"},
		"finance_admin_2":      {platform: []authz.PlatformRole{authz.PlatformFinanceAdmin}},
		"platform_admin_2":     {platform: []authz.PlatformRole{authz.PlatformAdmin}},
		"dpo_2":                {platform: []authz.PlatformRole{authz.PlatformDPO}},
	}
}

func orgKind(name string) authz.OrgKind {
	for _, o := range Orgs() {
		if o.Name == name {
			return o.Kind
		}
	}
	panic("authztest: unknown org fixture " + name)
}
