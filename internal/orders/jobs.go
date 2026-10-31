package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker task names of orders (01 §6.2; handler names "<module>.<verb>_<object>").
const (
	NotifyOrderPlacedTaskName    = "orders.notify_order_placed"
	NotifyOrderCancelledTaskName = "orders.notify_order_cancelled"
	NotifyOrderExpiredTaskName   = "orders.notify_order_expired"
	NotifyDeliveryMarkedTaskName = "orders.notify_delivery_marked"
	NotifyLineFulfilledTaskName  = "orders.notify_line_fulfilled"
	// NotifyRejectedEvidenceTaskName is the file.rejected subscription of
	// the delivery evidence (BE-154, T-393).
	NotifyRejectedEvidenceTaskName = "orders.notify_rejected_evidence"
)

// Notice templates (internal/notify/templates/orders). In-app texts are
// fixed (no data): the deep link carries the order.
const (
	tmplPaymentInstructions = "orders.payment_instructions" // the buyer: Koro IBAN, reference, amount, due date (e-mail + in-app)
	tmplOrderPlaced         = "orders.order_placed"         // the sellers' admins and the finance admins
	tmplOrderCancelled      = "orders.order_cancelled"      // the buyer and the sellers' admins
	tmplOrderExpired        = "orders.order_expired"        // the buyer
	tmplDeliveryMarked      = "orders.delivery_marked"      // the buyer's admins: confirm or dispute
	tmplLineFulfilled       = "orders.line_fulfilled"       // the seller's admins
	// tmplEvidenceRejected: the seller's markers (and finance while the line
	// is disputed): the delivery evidence was rejected after its upload.
	tmplEvidenceRejected = "orders.delivery_evidence_rejected"
)

// Subscribe registers the outbox subscriptions of orders (01 §6.2):
// offering.cancelled → the open orders of the offering (critical, money retries);
// order.placed, .cancelled, .expired, order_line.fulfillment_marked and
// .fulfilled → the notices; file.rejected of a delivery evidence → the
// seller's markers (and finance while the line is disputed; BE-154, T-393:
// the line keeps the reference, the seller uploads it again).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicOfferingCancelled, CancelOfferingOrdersTaskName, m.svc.handleOfferingCancelled,
		outbox.Queue(queue.Critical), outbox.MaxRetry(queue.MoneyMaxRetry))
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicOrderPlaced, NotifyOrderPlacedTaskName, m.svc.handlePlaced, q)
	r.Subscribe(domainevent.TopicOrderCancelled, NotifyOrderCancelledTaskName, m.svc.handleCancelled, q)
	r.Subscribe(domainevent.TopicOrderExpired, NotifyOrderExpiredTaskName, m.svc.handleExpired, q)
	r.Subscribe(domainevent.TopicOrderLineFulfillmentMarked, NotifyDeliveryMarkedTaskName, m.svc.handleMarked, q)
	r.Subscribe(domainevent.TopicOrderLineFulfilled, NotifyLineFulfilledTaskName, m.svc.handleFulfilled, q)
	files.SubscribeRejected(r, evidenceModule, NotifyRejectedEvidenceTaskName, m.svc.notifyRejectedEvidence)
}

// handlePlaced sends the payment instructions to the buyer who placed the
// order (e-mail with Koro's account, the reference code, the amount and
// the due date, and in-app) while it still waits for payment, and tells
// the sellers' admins and the finance admins in-app. Without an active
// collection account the e-mail is left out (logged): the instructions
// stay in the app (getOrderPaymentInstructions).
func (s *service) handlePlaced(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderPlaced](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, lines, ok, err := s.noticeOrder(ctx, p.OrderID)
	if err != nil || !ok {
		return err
	}
	if s.d.Sender == nil || s.d.Staff == nil {
		return errors.New("orders: the order notices are not wired")
	}
	key := e.ID.String()
	var errs []error
	if o.Status == StatusAwaitingPayment {
		errs = append(errs, s.sendInstructions(ctx, o, key))
	}
	for _, seller := range sellersOf(lines) {
		admins, err := s.membersWith(ctx, seller, authz.MemberClubAdmin, authz.MemberUniversityRep)
		if err != nil {
			return err
		}
		errs = append(errs, s.notifyInApp(ctx, o, seller, tmplOrderPlaced, key, admins, notify.OrgSalesLink(seller)))
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("orders: finance admins: %w", err)
	}
	errs = append(errs, s.notifyInApp(ctx, o, o.OrgID, tmplOrderPlaced, key, fins, notify.AdminOrderLink(o.OrderNo)))
	return errors.Join(errs...)
}

// sendInstructions sends the payment instructions to the placer.
func (s *service) sendInstructions(ctx context.Context, o Order, key string) error {
	u := o.PlacedByUserID
	link := notify.OrgOrderPaymentInstructionsLink(o.OrgID, o.ID)
	errs := []error{s.notifyInApp(ctx, o, o.OrgID, tmplPaymentInstructions, key, []uuid.UUID{u}, link)}
	c, ok, err := s.contact(ctx, u)
	if err != nil || !ok {
		return errors.Join(append(errs, err)...)
	}
	acct, err := s.collectionAccount(ctx)
	if errors.Is(err, ErrNoCollectionAccount) {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payment instructions e-mail not sent: no active collection account",
			slog.String("order_id", o.ID.String()))
		return errors.Join(errs...)
	}
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	org := o.OrgID
	_, err = s.d.Sender.Send(ctx, notify.Message{
		UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmplPaymentInstructions, Locale: c.Locale, SourceOrgID: &org,
		DedupeKey: tmplPaymentInstructions + ":" + o.ID.String() + ":email",
		Data: map[string]any{
			"name": c.FirstName, "reference_code": o.OrderNo, "amount": money.Decimal(o.DueAmount()), "currency": o.Currency,
			"iban": acct.IBAN, "holder_name": acct.HolderName, "bank_name": acct.BankName,
			"expires_at": o.ExpiresAt.In(clock.Istanbul()).Format("02.01.2006 15:04"),
		},
	})
	return errors.Join(append(errs, s.sendErr(ctx, tmplPaymentInstructions, o.ID, u, err))...)
}

// handleCancelled tells the buyer who placed the order and the admins of
// the sellers of its lines (not of the lines that dropped out before) that
// it was cancelled.
func (s *service) handleCancelled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderCancelled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, lines, ok, err := s.noticeOrder(ctx, p.OrderID)
	if err != nil || !ok {
		return err
	}
	key := e.ID.String()
	errs := []error{s.notifyInApp(ctx, o, o.OrgID, tmplOrderCancelled, key, []uuid.UUID{o.PlacedByUserID}, notify.OrgOrderLink(o.OrgID, o.ID))}
	for _, seller := range sellersOf(liveLines(lines)) { // a dropped line's seller was told when it dropped (BE-134)
		admins, err := s.membersWith(ctx, seller, authz.MemberClubAdmin, authz.MemberUniversityRep)
		if err != nil {
			return err
		}
		errs = append(errs, s.notifyInApp(ctx, o, seller, tmplOrderCancelled, key, admins, notify.OrgSalesLink(seller)))
	}
	return errors.Join(errs...)
}

// handleExpired tells the buyer who placed the order that it expired.
func (s *service) handleExpired(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderExpired](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, _, ok, err := s.noticeOrder(ctx, p.OrderID)
	if err != nil || !ok {
		return err
	}
	return s.notifyInApp(ctx, o, o.OrgID, tmplOrderExpired, e.ID.String(), []uuid.UUID{o.PlacedByUserID}, notify.OrgOrderLink(o.OrgID, o.ID))
}

// handleMarked asks the buyer company's admins to confirm the delivery (or
// dispute it inside the window).
func (s *service) handleMarked(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderLineFulfillmentMarked](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, _, ok, err := s.noticeOrder(ctx, p.OrderID)
	if err != nil || !ok {
		return err
	}
	admins, err := s.membersWith(ctx, o.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
	if err != nil {
		return err
	}
	return s.notifyInApp(ctx, o, o.OrgID, tmplDeliveryMarked, e.ID.String(), admins, notify.OrgOrderLink(o.OrgID, o.ID))
}

// handleFulfilled tells the seller's admins that a line is fulfilled
// (billing plans its invoice from the same event, BE-042).
func (s *service) handleFulfilled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderLineFulfilled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, lines, ok, err := s.noticeOrder(ctx, p.OrderID)
	if err != nil || !ok {
		return err
	}
	i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == p.OrderLineID })
	if i < 0 {
		return nil
	}
	seller := lines[i].SellerOrgID
	admins, err := s.membersWith(ctx, seller, authz.MemberClubAdmin, authz.MemberUniversityRep)
	if err != nil {
		return err
	}
	return s.notifyInApp(ctx, o, seller, tmplLineFulfilled, e.ID.String(), admins, notify.OrgSalesLink(seller))
}

// noticeOrder reads the order of a notice with its lines; a missing one (a
// stale task) is skipped.
func (s *service) noticeOrder(ctx context.Context, id uuid.UUID) (Order, []Line, bool, error) {
	o, lines, err := s.loadOrder(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "order notice: order not found", slog.String("order_id", id.String()))
		return Order{}, nil, false, nil
	case err != nil:
		return Order{}, nil, false, err
	}
	return o, lines, true, nil
}

// membersWith returns the org's active members holding one of roles.
func (s *service) membersWith(ctx context.Context, orgID uuid.UUID, roles ...authz.MemberRole) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("orders: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, roles...)
		if err != nil {
			return nil, fmt.Errorf("orders: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) to every recipient; the
// dedupe key (the event and the recipient) makes a repeated task a no-op.
func (s *service) notifyInApp(ctx context.Context, o Order, sourceOrg uuid.UUID, tmpl, key string, to []uuid.UUID, link notify.Link) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("orders: notify is not wired")
	}
	org := sourceOrg
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &org, LinkPath: link, DedupeKey: tmpl + ":" + key + ":" + u.String(),
			Data: map[string]any{"order_id": o.ID.String()},
		})
		errs = append(errs, s.sendErr(ctx, tmpl, o.ID, u, err))
	}
	return errors.Join(errs...)
}

// sendErr keeps a retryable send error and logs a permanent one (the
// notice cannot be delivered; retrying changes nothing).
func (s *service) sendErr(ctx context.Context, tmpl string, orderID, userID uuid.UUID, err error) error {
	switch {
	case err == nil:
		return nil
	case notify.IsPermanent(err):
		s.d.Log.LogAttrs(ctx, slog.LevelError, "order notice not deliverable", slog.String("template", tmpl),
			slog.String("order_id", orderID.String()), slog.String("user_id", userID.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}

// contact returns the address of a live user; an unknown or erased user,
// or no Users port, is in-app only.
func (s *service) contact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("orders: notice contact: %w", err)
	}
	return c, c.Email != "", nil
}
