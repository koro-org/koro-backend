package notify

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
)

// InAppQuery is listMyNotifications.
type InAppQuery struct {
	UnreadOnly bool
	Limit      int       // 1–100 (page.ClampLimit)
	After      *Position // nil: first page
}

// InAppPage is one page of in-app notifications, newest first.
type InAppPage struct {
	Items []InAppNotification
	Next  *Position // set when another page exists
}

// inAppResource is the authz view of a user's in-app inbox (03 §3.5).
func inAppResource(owner uuid.UUID, id uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeNotification, ID: id, OwnerUserID: &owner}
}

// ListMine implements Service.
func (s *service) ListMine(ctx context.Context, a authz.Actor, q InAppQuery) (InAppPage, error) {
	if err := authz.Can(ctx, a, authz.NotifyInAppListOwn, inAppResource(a.UserID, uuid.Nil())); err != nil {
		return InAppPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListInApp(ctx, a.UserID, q.UnreadOnly, q.After, limit+1)
	if err != nil {
		return InAppPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := InAppPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &Position{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// MarkRead implements Service. Only the owner's notification is found
// (another user's ID is 404, 03 §7.2); marking a read notification again is
// a no-op.
func (s *service) MarkRead(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.NotifyInAppMarkRead, inAppResource(a.UserID, id)); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.MarkInAppRead(ctx, a.UserID, id, s.d.Clock.Now()); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyInAppMarkRead), Outcome: audit.Success,
			ResourceType: authz.TypeNotification, ResourceID: &id,
		}.By(a))
	})
}

// MarkAllRead implements Service: every unread notification of the actor,
// never another user's.
func (s *service) MarkAllRead(ctx context.Context, a authz.Actor) error {
	if err := authz.Can(ctx, a, authz.NotifyInAppMarkRead, inAppResource(a.UserID, uuid.Nil())); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := s.d.Store.MarkAllInAppRead(ctx, a.UserID, s.d.Clock.Now())
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyInAppMarkRead), Outcome: audit.Success,
			ResourceType: authz.TypeNotification, Metadata: map[string]any{"all": true, "changed": n},
		}.By(a))
	})
}
