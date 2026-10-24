package orgs

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Membership lifecycle and invitations (BE-019) of the in-memory store.

func idLess(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) }

// one returns the first membership of orgID matching match.
func (s *MemStore) one(orgID uuid.UUID, match func(Membership) bool) (Membership, error) {
	return s.find(orgID, match)
}

// GetMember implements Store.
func (s *MemStore) GetMember(_ context.Context, orgID, id uuid.UUID) (Membership, error) {
	return s.one(orgID, func(m Membership) bool { return m.ID == id })
}

// LockMember implements Store.
func (s *MemStore) LockMember(ctx context.Context, orgID, id uuid.UUID) (Membership, error) {
	return s.GetMember(ctx, orgID, id)
}

// LockMemberOfUser implements Store.
func (s *MemStore) LockMemberOfUser(_ context.Context, userID, id uuid.UUID) (Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.members {
		if m.ID == id && m.UserID == userID {
			return m, nil
		}
	}
	return Membership{}, ErrNotFound
}

// LockOpenMember implements Store.
func (s *MemStore) LockOpenMember(_ context.Context, orgID, userID uuid.UUID) (Membership, error) {
	return s.one(orgID, func(m Membership) bool { return m.UserID == userID && open(m) })
}

func (s *MemStore) collect(match func(Membership) bool) []Membership {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Membership
	for _, m := range s.members {
		if match(m) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Membership) int { return idLess(a.ID, b.ID) })
	return out
}

// LockActiveByRoles implements Store.
func (s *MemStore) LockActiveByRoles(_ context.Context, orgID uuid.UUID, roles []authz.MemberRole) ([]Membership, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	return s.collect(func(m Membership) bool {
		return m.OrgID == orgID && m.Status == MemberActive && slices.Contains(roles, m.Role)
	}), nil
}

// LockUserActiveByRoles implements Store.
func (s *MemStore) LockUserActiveByRoles(_ context.Context, userID uuid.UUID, roles []authz.MemberRole) ([]Membership, error) {
	return s.collect(func(m Membership) bool {
		return m.UserID == userID && m.Status == MemberActive && slices.Contains(roles, m.Role)
	}), nil
}

// update applies fn to membership id of orgID when cond holds.
func (s *MemStore) update(orgID, id uuid.UUID, cond func(Membership) bool, fn func(Membership) Membership) (Membership, error) {
	if orgID == uuid.Nil() {
		return Membership{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.members {
		if m.ID == id && m.OrgID == orgID && cond(m) {
			next := fn(m)
			if err := checkMember(next); err != nil {
				return Membership{}, err
			}
			next.UpdatedAt = s.stamp(m.UpdatedAt)
			s.members[i] = next
			return next, nil
		}
	}
	return Membership{}, ErrNotFound
}

// ActivateMember implements Store.
func (s *MemStore) ActivateMember(_ context.Context, orgID, id uuid.UUID, now time.Time, approvedBy uuid.UUID) (Membership, error) {
	return s.update(orgID, id, func(m Membership) bool { return m.Status == MemberPending }, func(m Membership) Membership {
		by := approvedBy
		m.Status, m.JoinedAt, m.ApprovedBy = MemberActive, &now, &by
		return m
	})
}

// DeleteMember implements Store.
func (s *MemStore) DeleteMember(_ context.Context, orgID, id uuid.UUID) error {
	if orgID == uuid.Nil() {
		return db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members = slices.DeleteFunc(s.members, func(m Membership) bool { return m.ID == id && m.OrgID == orgID })
	return nil
}

// DeleteEndedMembers implements Store.
func (s *MemStore) DeleteEndedMembers(_ context.Context, orgID uuid.UUID, limit int) (int, error) {
	if orgID == uuid.Nil() {
		return 0, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	s.members = slices.DeleteFunc(s.members, func(m Membership) bool {
		if n < limit && m.OrgID == orgID && m.Status == MemberEnded {
			n++
			return true
		}
		return false
	})
	return n, nil
}

func statusSet(sts []MemberStatus) []MemberStatus {
	if len(sts) == 0 {
		return []MemberStatus{MemberPending, MemberActive}
	}
	return sts
}

// ListMembers implements Store.
func (s *MemStore) ListMembers(_ context.Context, orgID uuid.UUID, f MemberFilter, after *MemberAfter, limit int) ([]Membership, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	sts := statusSet(f.Statuses)
	joined := f.Sort == SortMemberJoined
	out := s.collect(func(m Membership) bool {
		return m.OrgID == orgID && slices.Contains(sts, m.Status) && (f.Role == "" || m.Role == f.Role)
	})
	slices.SortFunc(out, func(a, b Membership) int {
		if joined {
			if c := a.since().Compare(b.since()); c != 0 {
				return c
			}
			return idLess(a.ID, b.ID)
		}
		return idLess(b.ID, a.ID)
	})
	if after != nil {
		out = slices.DeleteFunc(out, func(m Membership) bool {
			if joined {
				c := m.since().Compare(after.Since)
				return c < 0 || c == 0 && idLess(m.ID, after.ID) <= 0
			}
			return idLess(m.ID, after.ID) >= 0
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ActiveMembershipsOfUser implements Store.
func (s *MemStore) ActiveMembershipsOfUser(_ context.Context, userID uuid.UUID) ([]Membership, error) {
	return s.collect(func(m Membership) bool { return m.UserID == userID && m.Status == MemberActive }), nil
}

// EndedMemberships implements Store.
func (s *MemStore) EndedMemberships(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	for _, m := range s.collect(func(m Membership) bool { return m.Status == MemberEnded && slices.Contains(ids, m.ID) }) {
		out = append(out, m.ID)
	}
	return out, nil
}

// ListUserMemberships implements Store.
func (s *MemStore) ListUserMemberships(_ context.Context, userID uuid.UUID, statuses []MemberStatus, after *uuid.UUID, limit int) ([]Membership, error) {
	sts := statusSet(statuses)
	out := s.collect(func(m Membership) bool {
		return m.UserID == userID && slices.Contains(sts, m.Status) && (after == nil || idLess(m.ID, *after) < 0)
	})
	slices.Reverse(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ActiveMemberIDs implements Store.
func (s *MemStore) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var out []uuid.UUID
	for _, m := range s.collect(func(m Membership) bool { return m.OrgID == orgID && m.Status == MemberActive }) {
		out = append(out, m.UserID)
	}
	slices.SortFunc(out, idLess)
	return out, nil
}

// ActiveByRole implements Store.
func (s *MemStore) ActiveByRole(_ context.Context, role authz.MemberRole, after uuid.UUID, limit int) ([]Membership, error) {
	out := s.collect(func(m Membership) bool {
		return m.Status == MemberActive && m.Role == role && idLess(m.ID, after) > 0
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// checkInvitation applies the CHECK constraints of org_invitations.
func checkInvitation(i Invitation) error {
	switch {
	case !invitableRole(i.OrgKind, i.Role):
		return errCheck
	case utf8.RuneCountInString(i.Email) > 254, len(i.TokenHash) != 32:
		return errCheck
	case (i.AcceptedAt == nil) != (i.AcceptedUserID == nil):
		return errCheck
	case i.AcceptedAt != nil && i.RevokedAt != nil:
		return errCheck
	}
	return nil
}

// InsertInvitation implements Store.
func (s *MemStore) InsertInvitation(_ context.Context, inv Invitation) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.orgs[inv.OrgID]
	if !ok {
		return Invitation{}, ErrNotFound
	}
	inv.OrgKind = parent.Kind // 06 H-38
	if err := checkInvitation(inv); err != nil {
		return Invitation{}, err
	}
	for _, x := range s.invites {
		switch {
		case x.ID == inv.ID, bytes.Equal(x.TokenHash, inv.TokenHash):
			return Invitation{}, problem.ErrConflict
		case x.OrgID == inv.OrgID && x.EmailNormalized == inv.EmailNormalized && x.Open(): // org_invitations_open_uq
			return Invitation{}, ErrInvitationExists
		}
	}
	inv.CreatedAt = s.stamp(time.Time{})
	inv.TokenHash = bytes.Clone(inv.TokenHash)
	s.invites = append(s.invites, inv)
	return inv, nil
}

func (s *MemStore) findInvite(match func(Invitation) bool) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.invites {
		if match(i) {
			return i, nil
		}
	}
	return Invitation{}, ErrNotFound
}

// GetInvitation implements Store.
func (s *MemStore) GetInvitation(_ context.Context, orgID, id uuid.UUID) (Invitation, error) {
	if orgID == uuid.Nil() {
		return Invitation{}, db.ErrMissingOrgScope
	}
	return s.findInvite(func(i Invitation) bool { return i.ID == id && i.OrgID == orgID })
}

// LockInvitationByTokenHash implements Store.
func (s *MemStore) LockInvitationByTokenHash(_ context.Context, hash []byte) (Invitation, error) {
	return s.findInvite(func(i Invitation) bool { return bytes.Equal(i.TokenHash, hash) })
}

// LockOpenInvitationTo implements Store.
func (s *MemStore) LockOpenInvitationTo(_ context.Context, orgID uuid.UUID, emailNormalized string) (Invitation, error) {
	if orgID == uuid.Nil() {
		return Invitation{}, db.ErrMissingOrgScope
	}
	return s.findInvite(func(i Invitation) bool {
		return i.OrgID == orgID && i.EmailNormalized == emailNormalized && i.Open()
	})
}

func (s *MemStore) updateInvite(orgID, id uuid.UUID, fn func(Invitation) Invitation) (Invitation, error) {
	if orgID == uuid.Nil() {
		return Invitation{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, i := range s.invites {
		if i.ID == id && i.OrgID == orgID && i.Open() {
			next := fn(i)
			if err := checkInvitation(next); err != nil {
				return Invitation{}, err
			}
			s.invites[k] = next
			return next, nil
		}
	}
	return Invitation{}, ErrNotFound
}

// CloseInvitation implements Store.
func (s *MemStore) CloseInvitation(_ context.Context, orgID, id uuid.UUID, at time.Time) (Invitation, error) {
	return s.updateInvite(orgID, id, func(i Invitation) Invitation {
		i.RevokedAt = &at
		return i
	})
}

// AcceptInvitation implements Store.
func (s *MemStore) AcceptInvitation(_ context.Context, orgID, id, userID uuid.UUID, now time.Time) (Invitation, error) {
	return s.updateInvite(orgID, id, func(i Invitation) Invitation {
		u := userID
		i.AcceptedAt, i.AcceptedUserID = &now, &u
		return i
	})
}

func (s *MemStore) invitesWhere(match func(Invitation) bool) []Invitation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Invitation
	for _, i := range s.invites {
		if match(i) {
			out = append(out, i)
		}
	}
	slices.SortFunc(out, func(a, b Invitation) int { return idLess(a.ID, b.ID) })
	return out
}

// ListOpenInvitations implements Store.
func (s *MemStore) ListOpenInvitations(_ context.Context, orgID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Invitation, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	out := s.invitesWhere(func(i Invitation) bool {
		return i.OrgID == orgID && i.Open() && !i.Expired(now) && (after == nil || idLess(i.ID, *after) < 0)
	})
	slices.Reverse(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CloseExpiredInvitations implements Store.
func (s *MemStore) CloseExpiredInvitations(_ context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, i := range s.invites {
		if n < limit && i.Open() && i.Expired(now) {
			at := i.ExpiresAt
			s.invites[k].RevokedAt = &at
			n++
		}
	}
	return n, nil
}

// closedAt is COALESCE(accepted_at, revoked_at, expires_at).
func (i Invitation) closedAt() time.Time {
	switch {
	case i.AcceptedAt != nil:
		return *i.AcceptedAt
	case i.RevokedAt != nil:
		return *i.RevokedAt
	}
	return i.ExpiresAt
}

// PurgeExpired implements Store.
func (s *MemStore) PurgeExpired(_ context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		at time.Time
		id uuid.UUID
	}
	var rows []row
	switch policy {
	case PolicyMembershipEnded:
		for _, m := range s.members {
			if m.Status == MemberEnded && m.EndedAt != nil && m.EndedAt.Before(cutoff) {
				rows = append(rows, row{*m.EndedAt, m.ID})
			}
		}
	case PolicyOrgInvitation:
		for _, i := range s.invites {
			if i.closedAt().Before(cutoff) {
				rows = append(rows, row{i.closedAt(), i.ID})
			}
		}
	default:
		return 0, fmt.Errorf("orgs: no retention purge for policy %q", policy)
	}
	if dryRun {
		return len(rows), nil
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Or(a.at.Compare(b.at), bytes.Compare(a.id[:], b.id[:])) })
	doomed := map[uuid.UUID]bool{}
	for _, r := range rows[:min(len(rows), max(limit, 0))] {
		doomed[r.id] = true
	}
	if policy == PolicyMembershipEnded {
		s.members = slices.DeleteFunc(s.members, func(m Membership) bool { return doomed[m.ID] })
	} else {
		s.invites = slices.DeleteFunc(s.invites, func(i Invitation) bool { return doomed[i.ID] })
	}
	return len(doomed), nil
}

func ofUser(userID uuid.UUID, emailNormalized string) func(Invitation) bool {
	return func(i Invitation) bool {
		return i.AcceptedUserID != nil && *i.AcceptedUserID == userID ||
			emailNormalized != "" && i.EmailNormalized == emailNormalized
	}
}

// InvitationsOfUser implements Store.
func (s *MemStore) InvitationsOfUser(_ context.Context, userID uuid.UUID, emailNormalized string) ([]Invitation, error) {
	return s.invitesWhere(ofUser(userID, emailNormalized)), nil
}

// DeleteInvitationsOfUser implements Store.
func (s *MemStore) DeleteInvitationsOfUser(_ context.Context, userID uuid.UUID, emailNormalized string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	match, n := ofUser(userID, emailNormalized), 0
	s.invites = slices.DeleteFunc(s.invites, func(i Invitation) bool {
		if match(i) {
			n++
			return true
		}
		return false
	})
	return n, nil
}
