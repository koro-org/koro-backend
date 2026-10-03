package notify

import (
	"context"
	"encoding/hex"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
)

// SuppressionQuery is adminListEmailSuppressions: one address (exact match
// on its blind index) or a page of the whole list.
type SuppressionQuery struct {
	Email string // "" → every suppression, newest first
	Limit int
	After *SuppressionPosition
}

// SuppressionPage is one page of suppressions.
type SuppressionPage struct {
	Items []Suppression
	Next  *SuppressionPosition
}

// DeliveryQuery is adminListUserDeliveries.
type DeliveryQuery struct {
	Limit int
	After *Position
}

// DeliveryPage is one page of a user's deliveries, newest first.
type DeliveryPage struct {
	Items []Delivery
	Next  *Position
}

// SuppressionID is the API ID of a suppression: the hex of its blind index
// (email_suppressions has no other key; the index reveals nothing without
// the key).
func SuppressionID(emailIndex []byte) string { return hex.EncodeToString(emailIndex) }

// parseSuppressionID decodes an API suppression ID; anything malformed is
// simply not found.
func parseSuppressionID(id string) ([]byte, error) {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != id {
		return nil, ErrSuppressionNotFound
	}
	return b, nil
}

func suppressionResource(access Access) authz.Resource {
	return authz.Resource{Type: authz.TypeEmailSuppression, Attrs: map[string]string{authz.AttrAccessReason: access.Reason}}
}

// breakGlassMeta is the audit metadata of a B access (03 §6.3): every grant
// of these actions is a break-glass cell (support and platform_admin).
func breakGlassMeta(access Access, extra map[string]any) map[string]any {
	m := map[string]any{audit.MetaBreakGlass: true}
	if access.TicketRef != "" {
		m[audit.MetaTicketRef] = access.TicketRef
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// AdminListSuppressions implements Service. The read is audited as a
// break-glass access; the address is used only to derive the index and is
// neither stored nor audited.
func (s *service) AdminListSuppressions(ctx context.Context, a authz.Actor, access Access, q SuppressionQuery) (SuppressionPage, error) {
	if err := authz.Can(ctx, a, authz.NotifySuppressionManage, suppressionResource(access)); err != nil {
		return SuppressionPage{}, err
	}
	var out SuppressionPage
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if q.Email != "" {
			sup, err := s.d.Store.GetSuppression(ctx, s.indexOf(q.Email))
			switch {
			case err == nil:
				out.Items = []Suppression{sup}
			case !errors.Is(err, ErrSuppressionNotFound):
				return err
			}
		} else {
			limit := page.ClampLimit(q.Limit)
			rows, err := s.d.Store.ListSuppressions(ctx, q.After, limit+1)
			if err != nil {
				return err
			}
			rows, more := page.Cut(rows, limit)
			out.Items = rows
			if more {
				last := rows[len(rows)-1]
				out.Next = &SuppressionPosition{At: last.CreatedAt, Index: last.EmailIndex}
			}
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifySuppressionManage), Outcome: audit.Success,
			ResourceType: authz.TypeEmailSuppression, Reason: access.Reason,
			Metadata: breakGlassMeta(access, map[string]any{"op": "list", "by_address": q.Email != "", "returned": len(out.Items)}),
		}.By(a))
	})
	return out, err
}

// AdminDeleteSuppression implements Service: the address may receive e-mail
// again (a mistaken bounce, the user fixed the mailbox).
func (s *service) AdminDeleteSuppression(ctx context.Context, a authz.Actor, access Access, id string) error {
	if err := authz.Can(ctx, a, authz.NotifySuppressionManage, suppressionResource(access)); err != nil {
		return err
	}
	idx, err := parseSuppressionID(id)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetSuppression(ctx, idx)
		if err != nil {
			return err
		}
		if err := s.d.Store.DeleteSuppression(ctx, idx); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifySuppressionManage), Outcome: audit.Success,
			ResourceType: authz.TypeEmailSuppression, Reason: access.Reason,
			Metadata: breakGlassMeta(access, map[string]any{"op": "delete", "suppression_reason": string(cur.Reason),
				"suppression_source": string(cur.Source)}),
		}.By(a))
	})
}

// AdminListUserDeliveries implements Service. The read is audited as a
// break-glass access with the user as subject, so the user sees it in the
// security history and gets the support access notification (03 §6.3,
// 06 H-43).
func (s *service) AdminListUserDeliveries(ctx context.Context, a authz.Actor, access Access, userID uuid.UUID, q DeliveryQuery) (DeliveryPage, error) {
	r := authz.Resource{Type: authz.TypeNotificationDelivery, ID: userID, OwnerUserID: &userID,
		Attrs: map[string]string{authz.AttrAccessReason: access.Reason}}
	if err := authz.Can(ctx, a, authz.NotifyDeliveryList, r); err != nil {
		return DeliveryPage{}, err
	}
	var out DeliveryPage
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		limit := page.ClampLimit(q.Limit)
		rows, err := s.d.Store.ListOfUser(ctx, userID, q.After, limit+1)
		if err != nil {
			return err
		}
		rows, more := page.Cut(rows, limit)
		out.Items = rows
		if more {
			last := rows[len(rows)-1]
			out.Next = &Position{At: last.CreatedAt, ID: last.ID}
		}
		target := userID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyDeliveryList), Outcome: audit.Success,
			ResourceType: authz.TypeUser, ResourceID: &target, Reason: access.Reason,
			Metadata: breakGlassMeta(access, map[string]any{audit.MetaSubjectUserID: userID.String(), "returned": len(rows)}),
		}.By(a))
	})
	return out, err
}
