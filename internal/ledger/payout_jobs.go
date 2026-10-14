package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Task names of the payout notices (01 §6.2).
const (
	NotifyPayoutCreatedTaskName  = "ledger.notify_payout_created"
	NotifyPayoutApprovedTaskName = "ledger.notify_payout_approved"
	NotifyPayoutPaidTaskName     = "ledger.notify_payout_paid"
)

// Templates of the payout notices (internal/notify/templates/ledger).
const (
	tmplPayoutCreated  = "ledger.payout_created"
	tmplPayoutApproved = "ledger.payout_approved"
	tmplPayoutPaid     = "ledger.payout_paid"
)

// Subscribe registers the payout notices (01 §6.2): payout.created (A) →
// the other finance admins, "awaiting approval"; payout.approved (O) → the
// finance admins but the approver, "make the transfer"; payout.paid (O) →
// the payee's contact address (e-mail) and the seller org's admins (in-app,
// with the link to the payout and its receipt). billing consumes
// payout.paid itself (BE-042). Nothing without the payout flow.
func (m Module) Subscribe(r *outbox.Registry) {
	if m.svc.d.Payouts == nil {
		return
	}
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicPayoutCreated, NotifyPayoutCreatedTaskName, m.svc.handlePayoutCreated, q)
	r.Subscribe(domainevent.TopicPayoutApproved, NotifyPayoutApprovedTaskName, m.svc.handlePayoutApproved, q)
	r.Subscribe(domainevent.TopicPayoutPaid, NotifyPayoutPaidTaskName, m.svc.handlePayoutPaid, q)
}

// handlePayoutCreated asks the other finance admins to approve.
func (s *service) handlePayoutCreated(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.PayoutCreated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	p, ok, err := s.noticePayout(ctx, ev.PayoutID)
	if err != nil || !ok || p.Status != StatusPendingApproval {
		return err
	}
	return s.notifyFinance(ctx, p, tmplPayoutCreated, e.ID.String(), p.CreatedBy)
}

// handlePayoutApproved tells the finance admins (but the approver) that the
// transfer can be made.
func (s *service) handlePayoutApproved(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.PayoutApproved](e)
	if err != nil {
		return queue.Permanent(err)
	}
	p, ok, err := s.noticePayout(ctx, ev.PayoutID)
	if err != nil || !ok || p.Status != StatusApproved || p.ApprovedBy == nil {
		return err
	}
	return s.notifyFinance(ctx, p, tmplPayoutApproved, e.ID.String(), *p.ApprovedBy)
}

// handlePayoutPaid e-mails the payee's contact address (when its legal
// party has one) and tells the seller org's club admins and university
// representatives in-app, with the link to the payout (receipt).
func (s *service) handlePayoutPaid(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.PayoutPaid](e)
	if err != nil {
		return queue.Permanent(err)
	}
	p, ok, err := s.noticePayout(ctx, ev.PayoutID)
	if err != nil || !ok || p.Status != StatusPaid {
		return err
	}
	if s.d.Sender == nil {
		return errors.New("ledger: notify is not wired")
	}
	key := e.ID.String()
	link := "/orgs/" + p.OrgID.String() + "/payouts/" + p.ID.String()
	var errs []error
	errs = append(errs, s.mailPayee(ctx, p))
	admins, err := s.orgAdmins(ctx, p.OrgID)
	if err != nil {
		return err
	}
	errs = append(errs, s.notifyInApp(ctx, p, tmplPayoutPaid, key, admins, link))
	return errors.Join(errs...)
}

// mailPayee sends the payout.paid e-mail to the payee party's address; a
// party without one is in-app only (logged).
func (s *service) mailPayee(ctx context.Context, p Payout) error {
	py, err := s.d.Payees.Get(ctx, p.PayeeID)
	if err != nil {
		return fmt.Errorf("ledger: payee of payout %s: %w", p.ID, err)
	}
	party, err := s.d.Parties.Get(ctx, py.LegalPartyID)
	if err != nil {
		return fmt.Errorf("ledger: party of payout %s: %w", p.ID, err)
	}
	if party.Email == nil || *party.Email == "" {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "payout paid e-mail not sent: the payee has no contact address",
			slog.String("payout_id", p.ID.String()))
		return nil
	}
	org := p.OrgID
	paidAt := ""
	if p.PaidAt != nil {
		paidAt = p.PaidAt.In(clock.Istanbul()).Format("02.01.2006 15:04")
	}
	_, err = s.d.Sender.Send(ctx, notify.Message{
		ToEmail: *party.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmplPayoutPaid, Locale: "tr", SourceOrgID: &org,
		DedupeKey: tmplPayoutPaid + ":" + p.ID.String() + ":email",
		Data: map[string]any{
			"holder_name": p.HolderSnapshot, "amount": money.Decimal(p.Amount), "currency": p.Currency,
			"iban_masked": p.MaskedIBAN(), "bank_reference": strVal(p.BankReference), "paid_at": paidAt,
		},
	})
	return s.sendErr(ctx, tmplPayoutPaid, p.ID, err)
}

// notifyFinance sends an in-app notice to every finance admin but skip.
func (s *service) notifyFinance(ctx context.Context, p Payout, tmpl, key string, skip uuid.UUID) error {
	if s.d.Staff == nil {
		return errors.New("ledger: the finance admin lookup is not wired")
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("ledger: finance admins: %w", err)
	}
	fins = slices.DeleteFunc(slices.Clone(fins), func(id uuid.UUID) bool { return id == skip })
	return s.notifyInApp(ctx, p, tmpl, key, fins, "/admin/payouts/"+p.ID.String())
}

// orgAdmins returns the seller org's active club admins and university
// representatives.
func (s *service) orgAdmins(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("ledger: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, authz.MemberClubAdmin, authz.MemberUniversityRep)
		if err != nil {
			return nil, fmt.Errorf("ledger: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) to every recipient; the
// dedupe key (the event and the recipient) makes a repeated task a no-op.
func (s *service) notifyInApp(ctx context.Context, p Payout, tmpl, key string, to []uuid.UUID, link string) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("ledger: notify is not wired")
	}
	org := p.OrgID
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &org, LinkPath: link, DedupeKey: tmpl + ":" + key + ":" + u.String(),
			Data: map[string]any{"payout_id": p.ID.String()},
		})
		errs = append(errs, s.sendErr(ctx, tmpl, p.ID, err))
	}
	return errors.Join(errs...)
}

// sendErr keeps a retryable send error and logs a permanent one.
func (s *service) sendErr(ctx context.Context, tmpl string, payoutID uuid.UUID, err error) error {
	switch {
	case err == nil:
		return nil
	case notify.IsPermanent(err):
		s.d.Log.LogAttrs(ctx, slog.LevelError, "payout notice not deliverable", slog.String("template", tmpl),
			slog.String("payout_id", payoutID.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}

// noticePayout reads the payout of a notice; a missing one (a stale task)
// is skipped.
func (s *service) noticePayout(ctx context.Context, id uuid.UUID) (Payout, bool, error) {
	if s.d.Payouts == nil {
		return Payout{}, false, nil
	}
	p, err := s.d.Payouts.GetPayout(ctx, id, false)
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payout notice: payout not found", slog.String("payout_id", id.String()))
		return Payout{}, false, nil
	case err != nil:
		return Payout{}, false, err
	}
	return p, true, nil
}
