package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
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

// Task names of the payout notices (01 §6.2).
const (
	NotifyPayoutCreatedTaskName  = "ledger.notify_payout_created"
	NotifyPayoutApprovedTaskName = "ledger.notify_payout_approved"
	NotifyPayoutPaidTaskName     = "ledger.notify_payout_paid"
	// NotifyPayoutPaidBeforeApprovalTaskName is the security notice of a
	// transfer dated before its approval (00 T-298, BE-092): a second
	// subscription of payout.paid, on the critical queue.
	NotifyPayoutPaidBeforeApprovalTaskName = "ledger.notify_payout_paid_before_approval"
	// NotifyRejectedReceiptTaskName is the file.rejected subscription of
	// the payout receipts (BE-154, T-393).
	NotifyRejectedReceiptTaskName = "ledger.notify_rejected_receipt"
)

// Templates of the payout notices (internal/notify/templates/ledger).
const (
	tmplPayoutCreated  = "ledger.payout_created"
	tmplPayoutApproved = "ledger.payout_approved"
	tmplPayoutPaid     = "ledger.payout_paid"
	// tmplPayoutPaidEarly: preference category security (it cannot be
	// switched off), in-app and e-mail (BE-092).
	tmplPayoutPaidEarly = "ledger.payout_paid_before_approval"
	// tmplReceiptRejected: in-app; the receipt of a paid payout was rejected.
	tmplReceiptRejected = "ledger.payout_receipt_rejected"
)

// Subscribe registers the payout notices (01 §6.2): payout.created (A) →
// the other finance admins, "awaiting approval"; payout.approved (O) → the
// finance admins but the approver, "make the transfer"; payout.paid (O) →
// the payee's contact address (e-mail) and the seller org's admins (in-app,
// with the link to the payout and its receipt); payout.paid of a transfer
// dated before its approval → the security notice to the other finance and
// platform admins (00 T-298, BE-092; critical queue: the payout.paid outbox
// row written in the mark-paid transaction carries it); file.rejected of a
// payout receipt → the seller org's admins and every finance admin (BE-154,
// T-393: the payout keeps the reference, finance uploads it again). billing
// consumes payout.paid itself (BE-042). Nothing without the payout flow.
func (m Module) Subscribe(r *outbox.Registry) {
	if m.svc.d.Payouts == nil {
		return
	}
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicPayoutCreated, NotifyPayoutCreatedTaskName, m.svc.handlePayoutCreated, q)
	r.Subscribe(domainevent.TopicPayoutApproved, NotifyPayoutApprovedTaskName, m.svc.handlePayoutApproved, q)
	r.Subscribe(domainevent.TopicPayoutPaid, NotifyPayoutPaidTaskName, m.svc.handlePayoutPaid, q)
	r.Subscribe(domainevent.TopicPayoutPaid, NotifyPayoutPaidBeforeApprovalTaskName, m.svc.handlePayoutPaidBeforeApproval,
		outbox.Queue(queue.Critical))
	files.SubscribeRejected(r, "ledger", NotifyRejectedReceiptTaskName, m.svc.notifyRejectedReceipt)
}

// notifyRejectedReceipt tells the seller org's club admins and university
// representatives (the payout in their panel) and every finance admin (the
// payout on the admin surface) that the receipt of a payout was rejected
// after its upload: the payout keeps the reference (a financial record,
// BE-151) and finance uploads the receipt again (adminReplacePayoutReceipt).
// A payout that holds another receipt by now (replaced) is a no-op; the
// dedupe keys (the file and the recipient) make a repeated delivery one
// notice, also for an org admin who is a finance admin too.
func (s *service) notifyRejectedReceipt(ctx context.Context, r files.Rejection) error {
	p, ok, err := s.noticePayout(ctx, r.EntityID)
	if err != nil || !ok || p.ReceiptFileID == nil || *p.ReceiptFileID != r.FileID {
		return err
	}
	if s.d.Staff == nil {
		return errors.New("ledger: the finance admin lookup is not wired")
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("ledger: finance admins: %w", err)
	}
	admins, err := s.orgAdmins(ctx, p.OrgID)
	if err != nil {
		return err
	}
	key := r.FileID.String()
	return errors.Join(
		s.notifyInApp(ctx, p, tmplReceiptRejected, key, fins, notify.AdminPayoutLink(p.ID)),
		s.notifyInApp(ctx, p, tmplReceiptRejected, key, admins, notify.OrgPayoutLink(p.OrgID, p.ID)))
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
	link := notify.OrgPayoutLink(p.OrgID, p.ID)
	var errs []error
	errs = append(errs, s.mailPayee(ctx, p))
	admins, err := s.orgAdmins(ctx, p.OrgID)
	if err != nil {
		return err
	}
	errs = append(errs, s.notifyInApp(ctx, p, tmplPayoutPaid, key, admins, link))
	return errors.Join(errs...)
}

// handlePayoutPaidBeforeApproval is the security notice of 00 T-298
// (BE-092). It re-reads the payout: only a paid payout whose transfer is
// dated before its approval (Payout.PaidBeforeApproval, the predicate of
// the audit flag paid_before_approval) is announced; every other
// payout.paid is a no-op. Every platform and finance admin in effect but
// the one who marked it paid gets an in-app notice (fixed text, the link to
// the payout) and an e-mail with the payout, its amount, the masked IBAN,
// the bank reference, the approval and transfer times and the names of the
// approver and the marker. The template's preference category is security:
// nobody can switch it off. The dedupe keys (payout, recipient, channel)
// make a repeated task a no-op.
func (s *service) handlePayoutPaidBeforeApproval(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.PayoutPaid](e)
	if err != nil {
		return queue.Permanent(err)
	}
	p, ok, err := s.noticePayout(ctx, ev.PayoutID)
	if err != nil || !ok || p.Status != StatusPaid || !p.PaidBeforeApproval() {
		return err
	}
	if s.d.Staff == nil {
		return errors.New("ledger: the admin lookup is not wired")
	}
	if s.d.Sender == nil {
		return errors.New("ledger: notify is not wired")
	}
	to, err := s.securityRecipients(ctx, p)
	if err != nil {
		return err
	}
	if len(to) == 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payout paid before approval: no other finance or platform admin to notify",
			slog.String("payout_id", p.ID.String()))
		return nil
	}
	data, err := s.earlyPaidData(ctx, p)
	if err != nil {
		return err
	}
	key := p.ID.String()
	errs := []error{s.notifyInApp(ctx, p, tmplPayoutPaidEarly, key, to, notify.AdminPayoutLink(p.ID))}
	for _, uid := range to {
		errs = append(errs, s.mailStaff(ctx, p, tmplPayoutPaidEarly, key, uid, data))
	}
	return errors.Join(errs...)
}

// securityRecipients are the platform and finance admins in effect (06
// H-33), each once, but the one who marked the payout paid.
func (s *service) securityRecipients(ctx context.Context, p Payout) ([]uuid.UUID, error) {
	pads, err := s.d.Staff.PlatformAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: platform admins: %w", err)
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: finance admins: %w", err)
	}
	var out []uuid.UUID
	for _, id := range slices.Concat(pads, fins) {
		if (p.PaidBy != nil && id == *p.PaidBy) || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// earlyPaidData is the e-mail data of the security notice: the payout, its
// money and IBAN (masked only), the bank reference, the approval and
// transfer times (Türkiye time) and who approved and who marked it paid.
func (s *service) earlyPaidData(ctx context.Context, p Payout) (map[string]any, error) {
	approver, err := s.staffName(ctx, p.ApprovedBy)
	if err != nil {
		return nil, err
	}
	marker, err := s.staffName(ctx, p.PaidBy)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"payout_id": p.ID.String(), "holder_name": p.HolderSnapshot, "amount": money.Decimal(p.Amount),
		"currency": p.Currency, "iban_masked": p.MaskedIBAN(), "bank_reference": strVal(p.BankReference),
		"approved_at": istanbulStamp(p.ApprovedAt), "paid_at": istanbulStamp(p.PaidAt),
		"approved_by": approver, "paid_by": marker,
	}, nil
}

// mailStaff e-mails a staff member the security notice in their language;
// a user without an address (unknown, erased) or no Users port is in-app
// only.
func (s *service) mailStaff(ctx context.Context, p Payout, tmpl, key string, uid uuid.UUID, mail map[string]any) error {
	c, ok, err := s.staffContact(ctx, uid)
	if err != nil || !ok || c.Email == "" {
		return err
	}
	data := maps.Clone(mail)
	data["name"] = c.FirstName
	_, err = s.d.Sender.Send(ctx, notify.Message{
		UserID: &uid, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmpl, Locale: c.Locale, DedupeKey: tmpl + ":" + key + ":" + uid.String() + ":email", Data: data,
	})
	return s.sendErr(ctx, tmpl, p.ID, err)
}

// staffName is a staff member's full name for the security notice; an
// unknown or erased user, a user without a name or no Users port is named
// by ID.
func (s *service) staffName(ctx context.Context, id *uuid.UUID) (string, error) {
	if id == nil {
		return "", nil
	}
	c, ok, err := s.staffContact(ctx, *id)
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(c.FirstName + " " + c.LastName); ok && name != "" {
		return name, nil
	}
	return id.String(), nil
}

// staffContact reads a staff member's contact; an unknown or erased user,
// or no Users port, is not found.
func (s *service) staffContact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("ledger: staff contact: %w", err)
	}
	return c, true, nil
}

// istanbulStamp formats an instant in Türkiye time for an e-mail.
func istanbulStamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(clock.Istanbul()).Format("02.01.2006 15:04")
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
	paidAt := istanbulStamp(p.PaidAt)
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
	return s.notifyInApp(ctx, p, tmpl, key, fins, notify.AdminPayoutLink(p.ID))
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
func (s *service) notifyInApp(ctx context.Context, p Payout, tmpl, key string, to []uuid.UUID, link notify.Link) error {
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
