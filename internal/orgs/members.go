package orgs

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// maxJoinMessage is the length limit of joinClub's message (04 §2.3).
const maxJoinMessage = 300

// Access is the break-glass context of a read (03 §6.3): the
// X-Koro-Access-Reason header. Only support and platform_admin use it, on
// listOrgMembers and getOrgMember.
type Access struct{ Reason string }

// MemberView is a membership with the member's name (04 §2.2 MemberDTO).
type MemberView struct {
	Membership Membership
	FirstName  string
	LastName   string
}

// MemberQuery narrows listOrgMembers (04 §2.2: status, role, sort joined_at).
type MemberQuery struct {
	Statuses []MemberStatus // pending and/or active; empty: both
	Role     authz.MemberRole
	Sort     string // SortMemberNewest (default) or SortMemberJoined
	Limit    int
	After    *MemberAfter
}

// MemberPage is a page of members; Next is the position of the next page.
type MemberPage struct {
	Items []MemberView
	Next  *MemberAfter
	Sort  string
}

// MyMembershipQuery narrows listMyMemberships (04 §2.2: status).
type MyMembershipQuery struct {
	Statuses []MemberStatus // empty: pending and active
	Limit    int
	After    *uuid.UUID
}

// MyMembership is one of the caller's memberships with its org.
type MyMembership struct {
	Membership Membership
	Org        OrgSummary
	LogoURL    string
}

// MyMembershipPage is a page of the caller's memberships.
type MyMembershipPage struct {
	Items []MyMembership
	Next  *uuid.UUID
}

func boolAttr(b bool) string { return strconv.FormatBool(b) }

// memberResource is the authz view of a membership (03 §3.5 org_member:
// OrgID = the org, OwnerUserID = the member; Attrs org_kind, role, status,
// sensitive).
func memberResource(m Membership, sensitive bool, reason string) authz.Resource {
	id, org, owner := m.ID, m.OrgID, m.UserID
	r := authz.Resource{Type: authz.TypeOrgMember, ID: id, OrgID: &org, OwnerUserID: &owner, Attrs: map[string]string{
		"org_kind": string(m.OrgKind), "role": string(m.Role), authz.AttrStatus: string(m.Status),
		authz.AttrSensitive: boolAttr(sensitive),
	}}
	if reason != "" {
		r.Attrs[authz.AttrAccessReason] = reason
	}
	return r
}

// orgSurface is the authz view of an org's member or invitation collection
// (an org resource with the club's sensitivity: 03 k4).
func orgSurface(o Org, sensitive bool, reason string) authz.Resource {
	r := orgResource(o)
	r.Attrs[authz.AttrSensitive] = boolAttr(sensitive)
	if reason != "" {
		r.Attrs[authz.AttrAccessReason] = reason
	}
	return r
}

// clubFacts returns the facts of a club org (found=false: no provider or no
// club row in directory).
func (s *service) clubFacts(ctx context.Context, orgID uuid.UUID) (ClubFacts, bool, error) {
	if s.d.Clubs == nil {
		return ClubFacts{}, false, nil
	}
	return s.d.Clubs(ctx, orgID)
}

// sensitive reports whether the memberships of an org are special-category
// data: a sensitive club (02 §7.3). A club whose facts are unknown counts
// as sensitive (fail closed, T-208).
func (s *service) sensitive(ctx context.Context, orgID uuid.UUID, kind Kind) (bool, error) {
	if kind != KindClub {
		return false, nil
	}
	f, found, err := s.clubFacts(ctx, orgID)
	if err != nil {
		return false, err
	}
	return !found || f.Sensitive, nil
}

// endMember ends one open membership with reason in the transaction carried
// by ctx. In a sensitive club the row is deleted at once (02 §7.3: the
// retention of membership_ended_sensitive is 0; T-83 keeps this out of the
// deferred sweepers). membership.ended follows the commit for a membership
// that was active (a pending request never started one).
func (s *service) endMember(ctx context.Context, m Membership, reason EndReason, sensitive bool, now time.Time) (Membership, error) {
	e, err := s.d.Store.EndMember(ctx, m.OrgID, m.ID, now, reason)
	if err != nil {
		return Membership{}, err
	}
	if sensitive {
		if err := s.d.Store.DeleteMember(ctx, m.OrgID, m.ID); err != nil {
			return Membership{}, err
		}
	}
	if m.Status == MemberActive {
		s.emitEnded(ctx, e, reason)
	}
	return e, nil
}

// keepsAdmin is 03 k6: ending or demoting m must not leave its org without
// the role it holds when that role is guarded (the last club_admin; the
// company_owner, whom only a transfer replaces; T-211: platform_admin
// included, the system paths excepted). The org's admin rows are locked, so
// two concurrent demotions cannot both pass.
func (s *service) keepsAdmin(ctx context.Context, m Membership) error {
	if m.Status != MemberActive || !guardedRole(m.OrgKind, m.Role) {
		return nil
	}
	if m.Role == authz.MemberCompanyOwner {
		return ErrLastAdmin
	}
	admins, err := s.d.Store.LockActiveByRoles(ctx, m.OrgID, []authz.MemberRole{m.Role})
	if err != nil {
		return err
	}
	for _, x := range admins {
		if x.ID != m.ID {
			return nil
		}
	}
	return ErrLastAdmin
}

// adultStudent is 03 k5 / T-55 for a user other than the caller: an active
// student verification and 18 or older. Without identity wired nobody
// qualifies (fail closed).
func (s *service) adultStudent(ctx context.Context, userID uuid.UUID) (bool, error) {
	if s.d.Students == nil || s.d.Users == nil {
		return false, nil
	}
	if _, ok, err := s.d.Students.Active(ctx, userID); err != nil || !ok {
		return false, err
	}
	u, err := s.d.Users.Get(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return u.Status == identity.StatusActive && !u.IsMinor, nil
}

// actorAdultStudent is k5 for the caller (acceptOrgInvitation): the Actor
// carries the verification and the age of this request.
func actorAdultStudent(a authz.Actor) bool { return a.IsVerifiedStudent() && !a.IsMinor }

// memberAudit is the audit record of a membership write.
func memberAudit(a authz.Actor, act authz.Action, m Membership, sensitive bool, reason string, meta map[string]any) audit.Event {
	org, id := m.OrgID, m.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["membership_id"] = m.ID.String()
	meta["role"] = string(m.Role)
	if sensitive {
		meta[audit.MetaSensitive] = true
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypeOrgMember, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// names returns the first and last names of users (empty without identity).
func (s *service) names(ctx context.Context, ms []Membership) (map[uuid.UUID]identity.UserSummary, error) {
	if s.d.Users == nil || len(ms) == 0 {
		return map[uuid.UUID]identity.UserSummary{}, nil
	}
	ids := make([]uuid.UUID, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.UserID)
	}
	return s.d.Users.GetMany(ctx, ids)
}

func viewOf(m Membership, users map[uuid.UUID]identity.UserSummary) MemberView {
	u := users[m.UserID]
	if u.Status == identity.StatusErased {
		return MemberView{Membership: m}
	}
	return MemberView{Membership: m, FirstName: u.FirstName, LastName: u.LastName}
}

// staffRead reports whether a reads an org's members through a platform
// role rather than a membership of that org: the B path (03 §6.3).
func staffRead(a authz.Actor, orgID uuid.UUID) bool {
	return !a.MemberOf(orgID) && a.HasAnyPlatformRole()
}

// logRead writes the audit record of an always-audited read (03 §6.5: a
// sensitive club's member list, every break-glass access).
func (s *service) logRead(ctx context.Context, e audit.Event) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Audit.Log(ctx, e) })
}

// ListMembers implements Service. Order: the org (404) → authz (404/403,
// break-glass reason) → the page → the audit record (sensitive club or
// break-glass) → names.
func (s *service) ListMembers(ctx context.Context, a authz.Actor, orgID uuid.UUID, q MemberQuery, access Access) (MemberPage, error) {
	o, err := s.d.Store.GetOrg(ctx, orgID)
	if err != nil {
		return MemberPage{}, err
	}
	sens, err := s.sensitive(ctx, o.ID, o.Kind)
	if err != nil {
		return MemberPage{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsMemberList, orgSurface(o, sens, access.Reason)); err != nil {
		return MemberPage{}, err
	}
	for _, st := range q.Statuses {
		if st != MemberPending && st != MemberActive {
			return MemberPage{}, fieldErr("query.status", "must be pending or active")
		}
	}
	if q.Role != "" && !roleFitsKind(o.Kind, q.Role) {
		return MemberPage{}, fieldErr("query.role", "is not a role of this organization")
	}
	sort := SortMemberNewest
	if q.Sort == SortMemberJoined {
		sort = SortMemberJoined
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListMembers(ctx, o.ID, MemberFilter{Statuses: q.Statuses, Role: q.Role, Sort: sort}, q.After, limit+1)
	if err != nil {
		return MemberPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	if bg := staffRead(a, o.ID); sens || bg {
		meta := map[string]any{"count": len(rows)}
		reason := ""
		if sens {
			meta[audit.MetaSensitive] = true
		}
		if bg {
			meta[audit.MetaBreakGlass], reason = true, access.Reason
		}
		if err := s.logRead(ctx, auditEvent(a, authz.OrgsMemberList, o.ID, reason, meta)); err != nil {
			return MemberPage{}, err
		}
	}
	users, err := s.names(ctx, rows)
	if err != nil {
		return MemberPage{}, err
	}
	out := MemberPage{Items: make([]MemberView, len(rows)), Sort: sort}
	for i, m := range rows {
		out.Items[i] = viewOf(m, users)
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &MemberAfter{Since: last.since(), ID: last.ID}
	}
	return out, nil
}

// openMember loads an open membership of orgID (an ended or missing one,
// or one of another org, is 404: 06 H-38) with its org and sensitivity.
func (s *service) openMember(ctx context.Context, orgID, id uuid.UUID, lock bool) (Org, Membership, bool, error) {
	get := s.d.Store.GetOrg
	member := s.d.Store.GetMember
	if lock {
		get, member = s.d.Store.LockOrg, s.d.Store.LockMember
	}
	o, err := get(ctx, orgID)
	if err != nil {
		return Org{}, Membership{}, false, err
	}
	m, err := member(ctx, o.ID, id)
	if err != nil {
		return Org{}, Membership{}, false, err
	}
	if m.Status == MemberEnded {
		return Org{}, Membership{}, false, ErrNotFound
	}
	sens, err := s.sensitive(ctx, o.ID, o.Kind)
	if err != nil {
		return Org{}, Membership{}, false, err
	}
	return o, m, sens, nil
}

// GetMember implements Service.
func (s *service) GetMember(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, access Access) (MemberView, error) {
	o, m, sens, err := s.openMember(ctx, orgID, id, false)
	if err != nil {
		return MemberView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsMemberRead, memberResource(m, sens, access.Reason)); err != nil {
		return MemberView{}, err
	}
	if bg := staffRead(a, o.ID); sens || bg {
		meta := map[string]any{}
		reason := ""
		if bg {
			meta[audit.MetaBreakGlass], meta[audit.MetaSubjectUserID], reason = true, m.UserID.String(), access.Reason
		}
		if err := s.logRead(ctx, memberAudit(a, authz.OrgsMemberRead, m, sens, reason, meta)); err != nil {
			return MemberView{}, err
		}
	}
	users, err := s.names(ctx, []Membership{m})
	if err != nil {
		return MemberView{}, err
	}
	return viewOf(m, users), nil
}

// memberView reads a membership (any status) for a response.
func (s *service) memberView(ctx context.Context, m Membership) (MemberView, error) {
	users, err := s.names(ctx, []Membership{m})
	if err != nil {
		return MemberView{}, err
	}
	return viewOf(m, users), nil
}

// decide is the shared flow of approve and reject: in one transaction the
// org and the request are locked, authz (S: club_admin), the org status,
// the pending status, then fn.
func (s *service) decide(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, op string,
	fn func(ctx context.Context, o Org, m Membership, sens bool) (Membership, error)) (MemberView, error) {
	hash, err := idempotency.Hash(struct {
		Op      string
		Org, ID uuid.UUID
	}{op, orgID, id})
	if err != nil {
		return MemberView{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Membership, error) {
		o, m, sens, err := s.openMember(ctx, orgID, id, true)
		if err != nil {
			return Membership{}, err
		}
		if err := authz.Can(ctx, a, authz.OrgsMemberDecide, memberResource(m, sens, "")); err != nil {
			return Membership{}, err
		}
		if err := writable(a, o); err != nil {
			return Membership{}, err
		}
		if m.Status != MemberPending {
			return Membership{}, ErrMemberState
		}
		return fn(ctx, o, m, sens)
	})
	if err != nil {
		return MemberView{}, err
	}
	return s.memberView(ctx, out)
}

// ApproveJoinRequest implements Service.
func (s *service) ApproveJoinRequest(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (MemberView, error) {
	return s.decide(ctx, a, orgID, id, "approve", func(ctx context.Context, o Org, m Membership, sens bool) (Membership, error) {
		act, err := s.d.Store.ActivateMember(ctx, o.ID, m.ID, s.now(), a.UserID)
		if err != nil {
			return Membership{}, err
		}
		if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberDecide, act, sens, "",
			map[string]any{"decision": "approved"})); err != nil {
			return Membership{}, err
		}
		s.emitStarted(ctx, act)
		return act, nil
	})
}

// RejectJoinRequest implements Service. The request ends (rejected); in a
// sensitive club it is deleted at once.
func (s *service) RejectJoinRequest(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (MemberView, error) {
	return s.decide(ctx, a, orgID, id, "reject", func(ctx context.Context, o Org, m Membership, sens bool) (Membership, error) {
		e, err := s.endMember(ctx, m, EndRejected, sens, s.now())
		if err != nil {
			return Membership{}, err
		}
		return e, s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberDecide, e, sens, "",
			map[string]any{"decision": "rejected"}))
	})
}

// RemoveMember implements Service: an active membership ends (removed);
// the last club_admin and the company_owner stay (409 orgs.last_admin).
func (s *service) RemoveMember(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maxReason {
		return fieldErr("body.reason", "must be at most 500 characters")
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Org, ID uuid.UUID
		Reason  string
	}{"remove", orgID, id, reason})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		o, m, sens, err := s.openMember(ctx, orgID, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.OrgsMemberRemove, memberResource(m, sens, "")); err != nil {
			return uuid.Nil(), err
		}
		if err := writable(a, o); err != nil {
			return uuid.Nil(), err
		}
		if m.Status != MemberActive {
			return uuid.Nil(), ErrMemberState // a pending request is rejected, not removed
		}
		if err := s.keepsAdmin(ctx, m); err != nil {
			return uuid.Nil(), err
		}
		e, err := s.endMember(ctx, m, EndRemoved, sens, s.now())
		if err != nil {
			return uuid.Nil(), err
		}
		if mfaRole(m.Role) {
			s.revokeAfterCommit(ctx, m.UserID)
		}
		return e.ID, s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberRemove, e, sens, reason, nil))
	})
	return err
}

// checkNewRole validates the target role of a role change or a grant
// (02 §7.3 CHECK, 03 k5): it exists for the org's kind and is not
// company_owner (only a transfer moves ownership).
func checkNewRole(k Kind, r authz.MemberRole) error {
	if r == authz.MemberCompanyOwner || r == authz.MemberStaff || !roleFitsKind(k, r) {
		return ErrRoleNotAllowed
	}
	return nil
}

// ChangeMemberRole implements Service: the old row ends (role_changed) and
// a new active row with the role starts (02 §7.3: the history is kept; in
// a sensitive club the old row is deleted). The member's sessions close
// when an MFA role is gained or lost (03 §2.2-6).
func (s *service) ChangeMemberRole(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, role authz.MemberRole) (MemberView, error) {
	hash, err := idempotency.Hash(struct {
		Op      string
		Org, ID uuid.UUID
		Role    authz.MemberRole
	}{"role", orgID, id, role})
	if err != nil {
		return MemberView{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Membership, error) {
		o, m, sens, err := s.openMember(ctx, orgID, id, true)
		if err != nil {
			return Membership{}, err
		}
		if err := authz.Can(ctx, a, authz.OrgsMemberChangeRole, memberResource(m, sens, "")); err != nil {
			return Membership{}, err
		}
		if err := writable(a, o); err != nil {
			return Membership{}, err
		}
		if m.Status != MemberActive {
			return Membership{}, ErrMemberState
		}
		if err := checkNewRole(o.Kind, role); err != nil {
			return Membership{}, err
		}
		if role == m.Role {
			return Membership{}, ErrRoleUnchanged
		}
		if err := s.keepsAdmin(ctx, m); err != nil {
			return Membership{}, err
		}
		if role == authz.MemberClubAdmin {
			ok, err := s.adultStudent(ctx, m.UserID)
			if err != nil {
				return Membership{}, err
			}
			if !ok {
				return Membership{}, ErrClubAdminNeedsAdultStudent
			}
		}
		now, by := s.now(), a.UserID
		old, err := s.endMember(ctx, m, EndRoleChanged, sens, now)
		if err != nil {
			return Membership{}, err
		}
		next, err := s.d.Store.InsertMember(ctx, Membership{
			ID: uuid.NewV7(), OrgID: o.ID, UserID: m.UserID, Role: role, Status: MemberActive, Source: SourceAdmin,
			JoinedAt: &now, ApprovedBy: &by,
		})
		if err != nil {
			return Membership{}, err
		}
		if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberChangeRole, next, sens, "", map[string]any{
			"previous_membership_id": old.ID.String(), "previous_role": string(old.Role),
		})); err != nil {
			return Membership{}, err
		}
		s.emitStarted(ctx, next)
		if mfaRole(old.Role) || mfaRole(role) {
			s.revokeAfterCommit(ctx, m.UserID)
		}
		return next, nil
	})
	if err != nil {
		return MemberView{}, err
	}
	return s.memberView(ctx, out)
}

// userResource is the caller's own account (orgs.membership.list_own).
func userResource(a authz.Actor) authz.Resource {
	id := a.UserID
	return authz.Resource{Type: authz.TypeUser, ID: id, OwnerUserID: &id}
}

// ListMyMemberships implements Service: the caller's own rows, sensitive
// clubs included (only the owner sees them).
func (s *service) ListMyMemberships(ctx context.Context, a authz.Actor, q MyMembershipQuery) (MyMembershipPage, error) {
	if err := authz.Can(ctx, a, authz.OrgsMembershipListOwn, userResource(a)); err != nil {
		return MyMembershipPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListUserMemberships(ctx, a.UserID, q.Statuses, q.After, limit+1)
	if err != nil {
		return MyMembershipPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	ids := make([]uuid.UUID, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.OrgID)
	}
	orgs, err := s.d.Store.GetOrgs(ctx, ids)
	if err != nil {
		return MyMembershipPage{}, err
	}
	byID := make(map[uuid.UUID]Org, len(orgs))
	for _, o := range orgs {
		byID[o.ID] = o
	}
	out := MyMembershipPage{Items: make([]MyMembership, len(rows))}
	for i, m := range rows {
		o := byID[m.OrgID]
		out.Items[i] = MyMembership{Membership: m, Org: o.Summary(), LogoURL: s.logoURL(ctx, o)}
	}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// LeaveMembership implements Service: the caller's pending request is
// withdrawn or the active membership ends (left); the last club_admin and
// the company_owner cannot leave (409 orgs.last_admin). Leaving is a
// personal action: it works in a suspended org too.
func (s *service) LeaveMembership(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	hash, err := idempotency.Hash(struct {
		Op string
		ID uuid.UUID
	}{"leave", id})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		m, err := s.d.Store.LockMemberOfUser(ctx, a.UserID, id)
		if err != nil {
			return uuid.Nil(), err
		}
		sens, err := s.sensitive(ctx, m.OrgID, m.OrgKind)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.OrgsMembershipLeave, memberResource(m, sens, "")); err != nil {
			return uuid.Nil(), err
		}
		if m.Status == MemberEnded {
			return uuid.Nil(), ErrMemberState
		}
		if err := s.keepsAdmin(ctx, m); err != nil {
			return uuid.Nil(), err
		}
		e, err := s.endMember(ctx, m, EndLeft, sens, s.now())
		if err != nil {
			return uuid.Nil(), err
		}
		if mfaRole(m.Role) {
			s.revokeAfterCommit(ctx, m.UserID)
		}
		return e.ID, s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMembershipLeave, e, sens, "", nil))
	})
	return err
}

// clubResource is the authz view of a club (03 §3.5: Attrs sensitive,
// university_id).
func clubResource(o Org, f ClubFacts) authz.Resource {
	id := o.ID
	return authz.Resource{Type: authz.TypeClub, ID: id, OrgID: &id, Attrs: map[string]string{
		authz.AttrSensitive: boolAttr(f.Sensitive), "university_id": f.UniversityID.String(),
	}}
}

// JoinClub implements Service. Order: the club (404: no such org, not a
// club, not public, or no directory facts — T-77) → authz (403
// students.verification_required for an account without a student
// verification, k-uv) → the org status → message → one open membership per
// user (409 orgs.already_member) → open: active + membership.started;
// approval: pending. The universities of the club and the student need not
// match (T-64). A minor may join (role member).
func (s *service) JoinClub(ctx context.Context, a authz.Actor, clubID uuid.UUID, message string) (Membership, error) {
	o, err := s.d.Store.GetOrg(ctx, clubID)
	if err != nil {
		return Membership{}, err
	}
	if o.Kind != KindClub || o.Status == StatusClosed || o.Status == StatusPendingReview {
		return Membership{}, ErrNotFound
	}
	facts, found, err := s.clubFacts(ctx, o.ID)
	if err != nil {
		return Membership{}, err
	}
	if !found {
		return Membership{}, ErrNotFound
	}
	if err := authz.Can(ctx, a, authz.OrgsMemberJoin, clubResource(o, facts)); err != nil {
		if errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent() {
			return Membership{}, ErrVerificationRequired
		}
		return Membership{}, err
	}
	if o.Status == StatusSuspended {
		return Membership{}, ErrOrgSuspended
	}
	// The message has no column (02 §7.3): it is validated and dropped
	// (T-205); the club_admin sees the request itself.
	if utf8.RuneCountInString(strings.TrimSpace(message)) > maxJoinMessage {
		return Membership{}, fieldErr("body.message", "must be at most 300 characters")
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Club uuid.UUID
	}{"join", o.ID})
	if err != nil {
		return Membership{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Membership, error) {
		if _, err := s.d.Store.LockOrg(ctx, o.ID); err != nil {
			return Membership{}, err
		}
		switch _, err := s.d.Store.LockOpenMember(ctx, o.ID, a.UserID); {
		case err == nil:
			return Membership{}, ErrAlreadyMember
		case !errors.Is(err, ErrNotFound):
			return Membership{}, err
		}
		now := s.now()
		m := Membership{ID: uuid.NewV7(), OrgID: o.ID, UserID: a.UserID, Role: authz.MemberMember, Source: SourceJoinRequest,
			RequestedAt: &now}
		if facts.JoinPolicy == JoinOpen {
			m.Status, m.JoinedAt = MemberActive, &now
		} else {
			m.Status = MemberPending
		}
		m, err := s.d.Store.InsertMember(ctx, m)
		if err != nil {
			return Membership{}, err
		}
		if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberJoin, m, facts.Sensitive, "",
			map[string]any{authz.AttrStatus: string(m.Status)})); err != nil {
			return Membership{}, err
		}
		if m.Status == MemberActive {
			s.emitStarted(ctx, m)
		}
		return m, nil
	})
	return out, err
}
