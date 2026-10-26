package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/money"
	"koro/internal/platform/queue"
)

// NotifyRefundRequestTaskName is the plain task (A, queue.AfterCommit) of a
// refund request's change (BE-132): 01 §6.2 has no topic for it (R6).
const NotifyRefundRequestTaskName = "payments.notify_refund_request"

// Refund request notice templates (internal/notify/templates/payments;
// transactional, preference orders_payments). In-app texts are fixed; the
// e-mails carry the order number, the line's title, the amount and, for a
// rejection, finance's reason — never the IBAN or the buyer's explanation.
const (
	tmplRequestCreated   = "payments.refund_request_created"   // finance admins (in-app)
	tmplRequestReceived  = "payments.refund_request_received"  // the seller's admins (e-mail + in-app)
	tmplRequestInReview  = "payments.refund_request_in_review" // the buyer's admins (in-app)
	tmplRequestAccepted  = "payments.refund_request_accepted"  // the buyer's admins (e-mail + in-app)
	tmplRequestRejected  = "payments.refund_request_rejected"  // the buyer's admins (e-mail + in-app)
	tmplRequestClosed    = "payments.refund_request_closed"    // the seller's admins: accepted, rejected, withdrawn (e-mail + in-app)
	tmplRequestWithdrawn = "payments.refund_request_withdrawn" // the finance admin who reviewed it (in-app)
)

// requestNotice is the task payload: the request and the change it tells.
type requestNotice struct {
	RequestID uuid.UUID           `json:"refund_request_id"`
	Change    RefundRequestStatus `json:"change"`
}

// notifyRequest queues the notices of change after the transaction commits.
func (s *service) notifyRequest(ctx context.Context, id uuid.UUID, change RefundRequestStatus) {
	s.d.Queue.AfterCommit(ctx, NotifyRefundRequestTaskName, requestNotice{RequestID: id, Change: change},
		NotifyRefundRequestTaskName+":"+id.String()+":"+string(change))
}

// handleRefundRequest sends the notices of a request's change once it is
// the request's state (a stale change sends nothing; a new request is told
// while it waits):
//
//	open       finance admins (in-app) and the seller's admins
//	in_review  the buyer's admins
//	accepted   the buyer's and the seller's admins
//	rejected   the buyer's admins (with the reason) and the seller's
//	withdrawn  the seller's admins and the finance admin who reviewed it
func (s *service) handleRefundRequest(ctx context.Context, raw []byte) error {
	var n requestNotice
	if err := json.Unmarshal(raw, &n); err != nil {
		return queue.Permanent(err)
	}
	if n.RequestID == uuid.Nil() || !n.Change.Valid() {
		return queue.Permanent(errors.New("payments: refund request notice requires refund_request_id and a change"))
	}
	r, err := s.d.Store.GetRefundRequest(ctx, n.RequestID, false)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	case n.Change == RequestOpen && !r.Status.Pending(), n.Change != RequestOpen && r.Status != n.Change:
		return nil
	}
	if s.d.Orgs == nil {
		return errors.New("payments: the org directory is not wired")
	}
	key := r.ID.String() + ":" + string(n.Change)
	ids := map[string]any{"refund_request_id": r.ID.String(), "order_id": r.OrderID.String()}
	mail := func(extra map[string]any) map[string]any {
		m := map[string]any{"order_no": r.OrderNo, "line_title": r.LineTitle, "amount": money.Decimal(r.Amount),
			"currency": r.Currency, "change": string(n.Change)}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	buyerLink, sellerLink := notify.OrgRefundRequestLink(r.OrgID, r.ID), notify.OrgSalesRefundRequestLink(r.SellerOrgID, r.ID)
	buyers := func() ([]uuid.UUID, error) {
		return s.membersWith(ctx, r.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
	}
	sellers := func() ([]uuid.UUID, error) {
		return s.membersWith(ctx, r.SellerOrgID, authz.MemberClubAdmin, authz.MemberUniversityRep)
	}
	var errs []error
	send := func(tmpl string, who func() ([]uuid.UUID, error), link notify.Link, m map[string]any) {
		to, err := who()
		if err != nil {
			errs = append(errs, err)
			return
		}
		errs = append(errs, s.notifyInApp(ctx, tmpl, key, to, link, ids, m))
	}
	switch n.Change {
	case RequestOpen:
		send(tmplRequestCreated, func() ([]uuid.UUID, error) { return s.financeAdmins(ctx) }, notify.AdminRefundRequestLink(r.ID), nil)
		send(tmplRequestReceived, sellers, sellerLink, mail(nil))
	case RequestInReview:
		send(tmplRequestInReview, buyers, buyerLink, nil)
	case RequestAccepted:
		send(tmplRequestAccepted, buyers, buyerLink, mail(nil))
		send(tmplRequestClosed, sellers, sellerLink, mail(nil))
	case RequestRejected:
		send(tmplRequestRejected, buyers, buyerLink, mail(map[string]any{"reason": str(r.RejectionReason)}))
		send(tmplRequestClosed, sellers, sellerLink, mail(nil))
	case RequestWithdrawn:
		send(tmplRequestClosed, sellers, sellerLink, mail(nil))
		if r.ReviewedBy != nil {
			reviewer := *r.ReviewedBy
			send(tmplRequestWithdrawn, func() ([]uuid.UUID, error) { return []uuid.UUID{reviewer}, nil },
				notify.AdminRefundRequestLink(r.ID), nil)
		}
	}
	return errors.Join(errs...)
}

// financeAdmins lists the finance admins in effect (06 H-33).
func (s *service) financeAdmins(ctx context.Context) ([]uuid.UUID, error) {
	if s.d.Staff == nil {
		return nil, errors.New("payments: finance staff is not wired")
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: finance admins: %w", err)
	}
	return fins, nil
}
