package events

import (
	"context"
	"errors"
	"strings"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// staffAggType is the audit resource type of a staff assignment.
const staffAggType = "event_staff"

// maxEmail bounds the address of addEventStaff (RFC 5321 path limit).
const maxEmail = 254

// staffChangeable: staff are managed while the event is a draft or
// published; a cancelled or completed event is final (409).
func staffChangeable(e Event) error {
	if e.Status != StatusDraft && e.Status != StatusPublished {
		return ErrInvalidState
	}
	return nil
}

func checkRole(r StaffRole) error {
	if !r.Valid() {
		return problem.Field("body.role", "must be scanner or manager")
	}
	return nil
}

// AddStaff implements Service. The account of email must exist and be
// active (404 otherwise: invitations are phase 2, 04 §2.6); the staff row
// takes org_id from the events row (06 H-38). The address is never logged
// or audited: the audit record names the account.
func (s *service) AddStaff(ctx context.Context, a authz.Actor, eventID uuid.UUID, email string, role StaffRole) (StaffView, error) {
	email = strings.TrimSpace(email)
	if email == "" || len(email) > maxEmail || !strings.Contains(email, "@") {
		return StaffView{}, problem.Field("body.email", "must be an e-mail address")
	}
	if err := checkRole(role); err != nil {
		return StaffView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		Email string
		Role  StaffRole
	}{"add_staff", eventID, strings.ToLower(email), role})
	if err != nil {
		return StaffView{}, err
	}
	st, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Staff, error) {
		e, _, err := s.locked(ctx, a, authz.EventsStaffManage, eventID)
		if err != nil {
			return Staff{}, err
		}
		if err := staffChangeable(e); err != nil {
			return Staff{}, err
		}
		userID, err := s.activeAccount(ctx, email)
		if err != nil {
			return Staff{}, err
		}
		st, err := s.d.Store.InsertStaff(ctx, Staff{EventID: e.ID, UserID: userID, OrgID: e.OrgID, Role: role, InvitedBy: a.UserID})
		if err != nil {
			return Staff{}, err
		}
		return st, s.d.Audit.Log(ctx, auditEvent(a, authz.EventsStaffManage, staffAggType, e.ID, e.OrgID, "",
			map[string]any{"op": "add", "user_id": userID.String(), "role": string(role)}))
	})
	if err != nil {
		return StaffView{}, err
	}
	vs, err := s.staffViews(ctx, []Staff{st})
	if err != nil {
		return StaffView{}, err
	}
	return vs[0], nil
}

// activeAccount returns the active account of an address or ErrNotFound.
func (s *service) activeAccount(ctx context.Context, email string) (uuid.UUID, error) {
	if s.d.Users == nil {
		return uuid.Nil(), ErrNotFound
	}
	id, found, err := s.d.Users.FindByEmail(ctx, email)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !found:
		return uuid.Nil(), ErrNotFound
	}
	u, err := s.d.Users.Get(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return uuid.Nil(), ErrNotFound
	case err != nil:
		return uuid.Nil(), err
	case u.Status != identity.StatusActive:
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

// UpdateStaff implements Service: the role of a staff member (404 when the
// user is not staff of the event).
func (s *service) UpdateStaff(ctx context.Context, a authz.Actor, eventID, userID uuid.UUID, role StaffRole) (StaffView, error) {
	if err := checkRole(role); err != nil {
		return StaffView{}, err
	}
	var st Staff
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, _, err := s.locked(ctx, a, authz.EventsStaffManage, eventID)
		if err != nil {
			return err
		}
		if err := staffChangeable(e); err != nil {
			return err
		}
		if st, err = s.d.Store.UpdateStaffRole(ctx, e.OrgID, e.ID, userID, role); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsStaffManage, staffAggType, e.ID, e.OrgID, "",
			map[string]any{"op": "update", "user_id": userID.String(), "role": string(role)}))
	})
	if err != nil {
		return StaffView{}, err
	}
	vs, err := s.staffViews(ctx, []Staff{st})
	if err != nil {
		return StaffView{}, err
	}
	return vs[0], nil
}

// RemoveStaff implements Service. Staff may be removed in any status (a
// door scanner leaves even after the event); open WebSocket connections
// close within 60 seconds once BE-054 adds the live board.
func (s *service) RemoveStaff(ctx context.Context, a authz.Actor, eventID, userID uuid.UUID) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, _, err := s.locked(ctx, a, authz.EventsStaffManage, eventID)
		if err != nil {
			return err
		}
		if err := s.d.Store.DeleteStaff(ctx, e.OrgID, e.ID, userID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsStaffManage, staffAggType, e.ID, e.OrgID, "",
			map[string]any{"op": "remove", "user_id": userID.String()}))
	})
}
