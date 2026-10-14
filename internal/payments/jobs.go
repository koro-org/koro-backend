package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Job and task names of payments (01 §6.2, §7; "<module>.<verb>_<object>").
const (
	DigestJobName                 = "payments.unmatched_digest"
	NotifyAccountApprovedTaskName = "payments.notify_collection_account_approved"
	NotifyPaymentCapturedTaskName = "payments.notify_payment_captured"
	NotifyUnmatchedTaskName       = "payments.notify_unmatched_transfers"
	// NotifyAccountChangedTaskName is a plain task (A, queue.AfterCommit):
	// 01 §6.2 has no topic for a retired or newly default account.
	NotifyAccountChangedTaskName = "payments.notify_collection_account_changed"
)

// digest schedule (01 §7: every working day at 09:30 Europe/Istanbul).
const (
	digestCron   = "30 9 * * 1-5"
	digestUnique = 12 * time.Hour
	// UnmatchedAge is how long a transfer waits unmatched before the digest
	// reports it (01 §7: 24 hours).
	UnmatchedAge = 24 * time.Hour
)

// Notice templates (internal/notify/templates/payments; transactional).
// In-app texts are fixed; data carry IDs only. The collection account
// notices are security notices (preference security: they cannot be
// switched off) and their e-mail shows the IBAN masked.
const (
	tmplAccountApproved = "payments.collection_account_approved" // platform + finance admins (e-mail + in-app)
	tmplAccountChanged  = "payments.collection_account_changed"  // platform + finance admins (e-mail + in-app)
	tmplPaymentReceived = "payments.payment_received"            // the buyer's admins (in-app)
	tmplOrderPaid       = "payments.order_paid"                  // the sellers' admins (in-app)
	tmplUnmatched       = "payments.unmatched_transfers"         // finance admins (in-app)
)

// Subscribe registers the outbox subscriptions of payments (01 §6.2):
// collection_account.approved → the security notice (critical);
// payment.captured → the buyer's and the sellers' notices;
// incoming_transfer.unmatched → the finance digest.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicCollectionAccountApproved, NotifyAccountApprovedTaskName, m.svc.handleAccountApproved,
		outbox.Queue(queue.Critical))
	r.Subscribe(domainevent.TopicPaymentCaptured, NotifyPaymentCapturedTaskName, m.svc.handlePaymentCaptured,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicIncomingTransferUnmatched, NotifyUnmatchedTaskName, m.svc.handleUnmatched,
		outbox.Queue(queue.Default))
}

// Tasks returns the plain task handlers of payments for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: NotifyAccountChangedTaskName, Handle: m.svc.handleAccountChanged}}
}

// digestJob is payments.unmatched_digest (01 §7): the unmatched transfers
// recorded more than 24 hours ago are reported to finance
// (incoming_transfer.unmatched, A). Nothing to report → no event.
func (s *service) digestJob() queue.Job {
	return queue.Job{
		Name: DigestJobName, Cron: digestCron, Queue: queue.Default, Unique: digestUnique,
		Handle: func(ctx context.Context) error {
			_, err := s.RunDigest(ctx)
			return err
		},
	}
}

// RunDigest runs payments.unmatched_digest once and returns what it
// reported.
func (s *service) RunDigest(ctx context.Context) (UnmatchedSummary, error) {
	sum, err := s.d.Store.UnmatchedBefore(ctx, s.d.Clock.Now().Add(-UnmatchedAge))
	if err != nil || sum.Count == 0 {
		return sum, err
	}
	env, err := domainevent.New(domainevent.IncomingTransferUnmatched{Count: sum.Count, OldestAt: sum.OldestAt.UTC()},
		aggregateUnmatched, uuid.NewV7())
	if err != nil {
		return sum, err
	}
	if err := s.d.Outbox.AfterCommit(ctx, env); err != nil {
		return sum, err
	}
	s.logInfo(ctx, "unmatched transfers reported", slog.Int("count", sum.Count), slog.Time("oldest_at", sum.OldestAt))
	return sum, nil
}

// RunDigest runs payments.unmatched_digest once (tests, operations).
func (m Module) RunDigest(ctx context.Context) (UnmatchedSummary, error) { return m.svc.RunDigest(ctx) }

// handleUnmatched tells every finance admin that transfers wait for a match.
func (s *service) handleUnmatched(ctx context.Context, e domainevent.Envelope) error {
	if _, err := domainevent.Decode[domainevent.IncomingTransferUnmatched](e); err != nil {
		return queue.Permanent(err)
	}
	if s.d.Staff == nil {
		return errors.New("payments: the finance admin lookup is not wired")
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("payments: finance admins: %w", err)
	}
	return s.notifyInApp(ctx, tmplUnmatched, e.ID.String(), fins, "/admin/incoming-transfers?status=unmatched", nil, nil)
}

// handleAccountApproved sends the security notice of an approved
// collection account to every platform and finance admin (01 §6.2, 03
// §6.1 "güvenlik bildirimi").
func (s *service) handleAccountApproved(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.CollectionAccountApproved](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.notifyAccount(ctx, p.CollectionAccountID, tmplAccountApproved, "approved", e.ID.String())
}

// handleAccountChanged sends the security notice of a retired or newly
// default collection account.
func (s *service) handleAccountChanged(ctx context.Context, payload []byte) error {
	var n accountNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.CollectionAccountID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("payments: account notice payload: %w", errors.Join(err, errors.New("collection_account_id required"))))
	}
	if n.Change != changeMadeDefault && n.Change != changeRetired {
		return queue.Permanent(fmt.Errorf("payments: account notice: unknown change %q", n.Change))
	}
	return s.notifyAccount(ctx, n.CollectionAccountID, tmplAccountChanged, n.Change, n.CollectionAccountID.String()+":"+n.Change)
}

// notifyAccount sends a collection account notice (in-app and e-mail) to
// every platform and finance admin in effect.
func (s *service) notifyAccount(ctx context.Context, id uuid.UUID, tmpl, change, key string) error {
	c, err := s.d.Store.GetAccount(ctx, id, false)
	switch {
	case isNotFound(err):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "collection account notice: account not found", slog.String("collection_account_id", id.String()))
		return nil
	case err != nil:
		return err
	}
	if s.d.Staff == nil {
		return errors.New("payments: the admin lookup is not wired")
	}
	pads, err := s.d.Staff.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("payments: platform admins: %w", err)
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("payments: finance admins: %w", err)
	}
	to := slices.Clone(pads)
	for _, u := range fins {
		if !slices.Contains(to, u) {
			to = append(to, u)
		}
	}
	mail := map[string]any{"label": c.Label, "iban_masked": c.MaskedIBAN(), "change": change, "status": string(c.Status)}
	return s.notifyInApp(ctx, tmpl, key, to, "/admin/collection-accounts", map[string]any{"collection_account_id": c.ID.String()}, mail)
}

// handlePaymentCaptured tells the buying company's admins that the money
// arrived and, once the order is paid, the sellers' admins that delivery
// may start.
func (s *service) handlePaymentCaptured(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PaymentCaptured](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if s.d.Orgs == nil {
		return errors.New("payments: the org directory is not wired")
	}
	o, err := s.d.Orders.ForPayment(ctx, p.OrderID)
	if isNotFound(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payment notice: order not found", slog.String("order_id", p.OrderID.String()))
		return nil
	}
	if err != nil {
		return err
	}
	ids := map[string]any{"order_id": o.ID.String(), "payment_id": p.PaymentID.String()}
	buyers, err := s.membersWith(ctx, o.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
	if err != nil {
		return err
	}
	key := e.ID.String()
	errs := []error{s.notifyInApp(ctx, tmplPaymentReceived, key, buyers, "/orders/"+o.ID.String(), ids, nil)}
	if !p.OrderPaid {
		return errors.Join(errs...)
	}
	splits, err := s.d.Store.SplitsOf(ctx, p.PaymentID)
	if err != nil {
		return err
	}
	var sellers []uuid.UUID
	for _, sp := range splits {
		l, err := s.d.Orders.GetLine(ctx, sp.OrderLineID)
		if err != nil {
			return err
		}
		if l.PayeeID != nil && !slices.Contains(sellers, l.SellerOrgID) {
			sellers = append(sellers, l.SellerOrgID)
		}
	}
	for _, seller := range sellers {
		admins, err := s.membersWith(ctx, seller, authz.MemberClubAdmin, authz.MemberUniversityRep)
		if err != nil {
			return err
		}
		errs = append(errs, s.notifyInApp(ctx, tmplOrderPaid, key, admins, "/orgs/"+seller.String()+"/sales", ids, nil))
	}
	return errors.Join(errs...)
}

// membersWith returns the org's active members holding one of roles.
func (s *service) membersWith(ctx context.Context, orgID uuid.UUID, roles ...authz.MemberRole) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("payments: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, roles...)
		if err != nil {
			return nil, fmt.Errorf("payments: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) to every recipient and,
// when mail is set, the e-mail of the template to those with an address.
// The dedupe key (the event and the recipient) makes a repeated task a
// no-op.
func (s *service) notifyInApp(ctx context.Context, tmpl, key string, to []uuid.UUID, link string, ids, mail map[string]any) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("payments: notify is not wired")
	}
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			LinkPath: link, DedupeKey: tmpl + ":" + key + ":" + u.String() + ":in_app", Data: ids,
		})
		errs = append(errs, s.sendErr(ctx, tmpl, u, err))
		if mail == nil {
			continue
		}
		c, ok, err := s.contact(ctx, u)
		if err != nil || !ok {
			errs = append(errs, err)
			continue
		}
		data := map[string]any{"name": c.FirstName}
		for k, v := range mail {
			data[k] = v
		}
		_, err = s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional, Template: tmpl,
			Locale: c.Locale, DedupeKey: tmpl + ":" + key + ":" + u.String() + ":email", Data: data,
		})
		errs = append(errs, s.sendErr(ctx, tmpl, u, err))
	}
	return errors.Join(errs...)
}

// sendErr keeps a retryable send error and logs a permanent one.
func (s *service) sendErr(ctx context.Context, tmpl string, userID uuid.UUID, err error) error {
	switch {
	case err == nil:
		return nil
	case notify.IsPermanent(err):
		s.d.Log.LogAttrs(ctx, slog.LevelError, "payments notice not deliverable", slog.String("template", tmpl),
			slog.String("user_id", userID.String()), slog.String("err", err.Error()))
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
		return identity.UserContact{}, false, fmt.Errorf("payments: notice contact: %w", err)
	}
	return c, c.Email != "", nil
}
