package students

import (
	"context"
	"errors"
	"slices"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/orgs"
)

// MemberCard is what a club_admin sees of an active member (04 §V.4): no
// e-mail (T-69), no CV, skills, experience, other clubs, privacy settings,
// birth date or talent value.
type MemberCard struct {
	Member     orgs.MemberView
	Profile    *Profile // nil: the member has no student profile
	University string
	Department string
	// RegistrationsCount is the member's registrations for the club's own
	// events; nil until events provides it (Deps.Registrations).
	RegistrationsCount *int
}

// AdminView is the platform staff view of a student (04 §V.5).
type AdminView struct {
	Account identity.AdminUser
	// Clubs are the student's non-sensitive club memberships (identity's
	// account view hides every club; an unknown club counts as sensitive).
	Clubs []authz.Membership
	View  ProfileView
	// ShowBirthDate: only the dpo column (a request's subject) sees it.
	ShowBirthDate bool
	// Registrations is the event history without answers; nil until
	// events provides it.
	Registrations []RegistrationSummary
	// UniversityName names the active verification's university.
	UniversityName string
}

// AdminAccess is the justification of an admin read: the break-glass
// reason of support and platform_admin (X-Koro-Access-Reason, 10–500
// characters), or for the dpo the ID of an open data subject request of
// the student (03 k11: "gerekçe = başvuru kimliği").
type AdminAccess struct {
	Reason string
}

// registrations returns the bound events reader, or nil.
func (s *service) registrations() Registrations {
	if s.d.Registrations == nil {
		return nil
	}
	return s.d.Registrations()
}

// clubSensitive reports whether orgID is a sensitive club; an org that is
// not a known club counts as sensitive (fail closed, T-240).
func (s *service) clubSensitive(ctx context.Context, orgID uuid.UUID) (known, sensitive bool, err error) {
	c, err := s.d.Catalog.GetClub(ctx, orgID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return false, true, nil
	case err != nil:
		return false, true, err
	}
	return true, c.IsSensitive, nil
}

// memberCardResource is the authz view of a member card (03 §3.6: the
// club's management surface; k10): student_profile of the club with its
// sensitivity. The member is not read before the decision.
func memberCardResource(orgID, membershipID uuid.UUID, sensitive bool) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeStudentProfile, ID: membershipID, OrgID: &org, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": "",
		authz.AttrSensitive: boolString(sensitive),
	}}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// GetMemberCard is getMemberCard (students.profile.read_member_card; 03
// k10, 04 §V.4). Order: the club (an unknown club or a non-club org: 404)
// → authz on the club's surface (a non-member 404, a member without the
// club_admin role 403, platform roles 403 or, in a sensitive club, 404)
// → the membership through orgs (another club's ID, a pending or ended
// membership: 404) → the member's profile facts → the audit record of a
// sensitive club's card.
func (s *service) GetMemberCard(ctx context.Context, a authz.Actor, orgID, membershipID uuid.UUID) (MemberCard, error) {
	known, sensitive, err := s.clubSensitive(ctx, orgID)
	if err != nil {
		return MemberCard{}, err
	}
	if !known {
		return MemberCard{}, ErrNotFound
	}
	if err := authz.Can(ctx, a, authz.StudentsProfileReadMemberCard, memberCardResource(orgID, membershipID, sensitive)); err != nil {
		return MemberCard{}, err
	}
	if s.d.Members == nil {
		return MemberCard{}, errUnavailable
	}
	mv, err := s.d.Members.GetMember(ctx, a, orgID, membershipID, orgs.Access{})
	if err != nil {
		return MemberCard{}, err
	}
	if mv.Membership.OrgID != orgID || mv.Membership.Status != orgs.MemberActive {
		return MemberCard{}, ErrNotFound // k10: a card exists for an active member only
	}
	member := mv.Membership.UserID
	card := MemberCard{Member: mv}
	switch p, err := s.profiles().GetProfile(ctx, member); {
	case err == nil:
		card.Profile = &p
		if card.University, card.Department, err = s.names(ctx, p); err != nil {
			return MemberCard{}, err
		}
	case !errors.Is(err, ErrNotFound):
		return MemberCard{}, err
	}
	if r := s.registrations(); r != nil {
		n, err := r.CountInOrg(ctx, orgID, member)
		if err != nil {
			return MemberCard{}, err
		}
		card.RegistrationsCount = &n
	}
	if sensitive { // k10: every view of a sensitive club's member is audited
		org, id := orgID, membershipID
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			return s.d.Audit.Log(ctx, audit.Event{
				Action: string(authz.StudentsProfileReadMemberCard), Outcome: audit.Success, OrgID: &org, ActorOrgID: &org,
				ResourceType: authz.TypeOrgMember, ResourceID: &id,
				Metadata: map[string]any{audit.MetaSensitive: true, audit.MetaSubjectUserID: member.String()},
			}.By(a))
		})
		if err != nil {
			return MemberCard{}, err
		}
	}
	return card, nil
}

// names returns the university and department names of p ("" when the
// catalog no longer has them).
func (s *service) names(ctx context.Context, p Profile) (uni, dept string, err error) {
	switch u, err := s.d.Catalog.GetUniversity(ctx, p.UniversityID); {
	case err == nil:
		uni = u.Name
	case !errors.Is(err, directory.ErrNotFound):
		return "", "", err
	}
	if p.DepartmentID != nil {
		switch d, err := s.d.Catalog.GetDepartment(ctx, *p.DepartmentID); {
		case err == nil:
			dept = d.Name
		case !errors.Is(err, directory.ErrNotFound):
			return "", "", err
		}
	}
	return uni, dept, nil
}

// adminResource is the authz view of the admin student view: the admin
// surface (every user sees it; 403 without a platform role) with the
// target as owner, the break-glass reason and the verified request.
func adminResource(userID uuid.UUID, reason, dsrID string) authz.Resource {
	u := userID
	return authz.Resource{Type: authz.TypeStudentProfile, ID: u, OwnerUserID: &u, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": "",
		authz.AttrAccessReason: reason, authz.AttrDSRID: dsrID,
	}}
}

// openDSR returns the request ID in reason when a is a dpo and the request
// is open with userID as its subject (03 k11); "" otherwise. Nobody else's
// request is read.
func (s *service) openDSR(ctx context.Context, a authz.Actor, userID uuid.UUID, reason string) (string, error) {
	id, isID := requestID(reason)
	if s.d.DSRs == nil || !a.HasPlatformRole(authz.PlatformDPO) || !isID {
		return "", nil
	}
	subject, ok, err := s.d.DSRs.OpenSubject(ctx, a, id)
	if err != nil || !ok || subject != userID {
		return "", err
	}
	return id.String(), nil
}

// requestID reads a data subject request ID from an access reason.
func requestID(reason string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(reason))
	return id, err == nil && id != uuid.Nil()
}

// AdminGetStudentProfile is adminGetStudentProfile
// (students.profile.read_admin; 04 §V.5): support and platform_admin by
// break-glass (X-Koro-Access-Reason, audited, the student is told: 06
// H-43), the dpo for the subject of an open data subject request whose ID
// is the reason (03 k11). Order: the dpo's request (dpo only) → authz
// (admin surface: 403 without a platform role; MFA; the reason) → the
// account (identity; 404) → the stored profile (404 without one) → the
// audit record. Never: CV files, sensitive clubs, form answers, talent;
// the birth date only on the dpo path.
func (s *service) AdminGetStudentProfile(ctx context.Context, a authz.Actor, userID uuid.UUID, access AdminAccess) (AdminView, error) {
	reason := strings.TrimSpace(access.Reason)
	dsrID, err := s.openDSR(ctx, a, userID, reason)
	if err != nil {
		return AdminView{}, err
	}
	if err := authz.Can(ctx, a, authz.StudentsProfileReadAdmin, adminResource(userID, reason, dsrID)); err != nil {
		return AdminView{}, err
	}
	if s.d.Accounts == nil {
		return AdminView{}, errUnavailable
	}
	acct, err := s.d.Accounts.AdminUser(ctx, a, userID)
	if err != nil {
		return AdminView{}, err
	}
	p, err := s.profiles().GetProfile(ctx, userID)
	if err != nil {
		return AdminView{}, err
	}
	v, err := s.view(ctx, p, true)
	if err != nil {
		return AdminView{}, err
	}
	out := AdminView{Account: acct, View: v, ShowBirthDate: dsrID != ""}
	if out.Clubs, err = s.plainClubs(ctx, userID); err != nil {
		return AdminView{}, err
	}
	if acct.Student != nil {
		switch u, err := s.d.Catalog.GetUniversity(ctx, acct.Student.UniversityID); {
		case err == nil:
			out.UniversityName = u.Name
		case !errors.Is(err, directory.ErrNotFound):
			return AdminView{}, err
		}
	}
	if r := s.registrations(); r != nil {
		if out.Registrations, err = r.SummaryOf(ctx, userID); err != nil {
			return AdminView{}, err
		}
	}
	meta := map[string]any{audit.MetaSubjectUserID: userID.String()}
	if dsrID != "" {
		meta["dsr_id"] = dsrID
	} else {
		meta[audit.MetaBreakGlass] = true
	}
	target := userID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.StudentsProfileReadAdmin), Outcome: audit.Success, ResourceType: authz.TypeStudentProfile,
			ResourceID: &target, Reason: reason, Metadata: meta,
		}.By(a))
	})
	if err != nil {
		return AdminView{}, err
	}
	return out, nil
}

// plainClubs returns userID's active club memberships of known,
// non-sensitive clubs (03 Y2: a sensitive club never reaches staff).
func (s *service) plainClubs(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error) {
	ms, err := s.d.Orgs.MembershipsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []authz.Membership
	for _, m := range ms {
		if m.OrgKind != authz.OrgClub {
			continue
		}
		known, sensitive, err := s.clubSensitive(ctx, m.OrgID)
		if err != nil {
			return nil, err
		}
		if known && !sensitive {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b authz.Membership) int { return compareUUID(a.OrgID, b.OrgID) })
	return out, nil
}
