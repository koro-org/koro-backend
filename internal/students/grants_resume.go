package students

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/idempotency"
)

// The suspension window of level 3 grants (BE-088; user decisions
// 2026-10-23: Q-03/T-331, Q-04/T-332). A grant suspended by a lapsed
// verification (BE-077) stays open for the window: the student, verified
// again, may resume it (an explicit consent act: the company's name is on
// the grant list, the request names the grant); once the window ran out
// the grant can no longer be resumed (409 students.resume_window_expired)
// and students.expire_suspended_grants revokes it. Only a new contact
// request of the company, accepted, gives access again after that.
//
// Boundary: a grant suspended at S may be resumed while now ≤ S + window
// and is revoked by the first run with now > S + window.

// DefaultSuspensionWindow is the suspension window without configuration
// (Q-03: 90 days).
const DefaultSuspensionWindow = 90 * 24 * time.Hour

// suspensionWindow is the configured window (the default when unset).
func (s *service) suspensionWindow() time.Duration {
	if s.d.SuspensionWindow > 0 {
		return s.d.SuspensionWindow
	}
	return DefaultSuspensionWindow
}

// resumableUntil is the last moment g may be resumed (nil: g is not
// suspended).
func (s *service) resumableUntil(g AccessGrant) *time.Time {
	if g.Status() != GrantSuspended {
		return nil
	}
	until := g.SuspendedAt.Add(s.suspensionWindow())
	return &until
}

// ResumeMyAccessGrant implements Service (students.access_grant.resume,
// BE-088). Order: authz (the O cell of verified students; an account
// without an active verification 403 students.verification_required) →
// the verification read now (the same 403 when it ended since the session
// was resolved) → under the optional Idempotency-Key, in one transaction:
// the student's grant lock, the grant (another student's or an unknown one:
// 404), its state (not suspended: 409 students.invalid_state; the window
// ran out: 409 students.resume_window_expired), the resume and its audit
// record (the student is the actor; the company is not told). The answer
// names the company: the consent is given to it.
func (s *service) ResumeMyAccessGrant(ctx context.Context, a authz.Actor, grantID uuid.UUID) (GrantView, error) {
	err := authz.Can(ctx, a, authz.StudentsAccessGrantResume, ownGrantResource(a, grantID))
	switch {
	case err != nil && errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent():
		return GrantView{}, ErrVerificationRequired
	case err != nil:
		return GrantView{}, err
	}
	switch verified, err := s.verifiedAmong(ctx, []uuid.UUID{a.UserID}); {
	case err != nil:
		return GrantView{}, err
	case !verified[a.UserID]:
		return GrantView{}, ErrVerificationRequired
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		User  uuid.UUID
		Grant uuid.UUID
	}{"resume_grant", a.UserID, grantID})
	if err != nil {
		return GrantView{}, err
	}
	g, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (AccessGrant, error) {
		var out AccessGrant
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			if err := s.d.Store.LockGrants(ctx, a.UserID); err != nil {
				return err
			}
			g, err := s.d.Store.GetGrant(ctx, a.UserID, grantID)
			if err != nil {
				return err
			}
			now := s.now()
			switch until := s.resumableUntil(g); {
			case until == nil:
				return ErrInvalidState
			case now.After(*until):
				return ErrResumeExpired
			}
			// Grant ends the suspended grants of a company before it gives a
			// new one, so an active grant of the same company next to this
			// one is a broken invariant: refuse rather than hold two.
			switch _, err := s.d.Store.ActiveGrant(ctx, a.UserID, g.CompanyOrgID); {
			case err == nil:
				return ErrInvalidState
			case !errors.Is(err, ErrNotFound):
				return err
			}
			suspendedAt := *g.SuspendedAt
			if out, err = s.d.Store.ResumeGrant(ctx, a.UserID, grantID, now); err != nil {
				return err
			}
			ev := grantEvent(string(authz.StudentsAccessGrantResume), out, map[string]any{
				"suspended_at": suspendedAt.UTC().Format(time.RFC3339Nano),
			})
			return s.d.Audit.Log(ctx, ev.By(a))
		})
		return out, err
	})
	if err != nil {
		return GrantView{}, err
	}
	names, err := s.orgNames(ctx, []uuid.UUID{g.CompanyOrgID})
	if err != nil {
		return GrantView{}, err
	}
	return GrantView{Grant: g, CompanyName: names[g.CompanyOrgID]}, nil
}
