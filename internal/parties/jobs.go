package parties

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker task names of the bank account notices (01 §6.2: parties
// consumes its own payee_bank_account.* events).
const (
	NotifySubmittedTaskName = "parties.notify_bank_account_submitted"
	NotifyApprovedTaskName  = "parties.notify_bank_account_approved"
	NotifyRejectedTaskName  = "parties.notify_bank_account_rejected"
	// NotifyRevokedTaskName is a plain task (A, queue.AfterCommit): 01 §6.2
	// has no topic for a revocation (doc patch: payee_bank_account.revoked).
	NotifyRevokedTaskName = "parties.notify_bank_account_revoked"
)

// Notice templates (internal/notify/templates/parties; transactional,
// security preference: they cannot be switched off). The e-mail carries the
// masked IBAN so an org admin recognises an account they did not enter;
// in-app notices carry IDs only.
const (
	tmplPending  = "parties.bank_account_pending"  // finance: an eye is needed (data: stage)
	tmplChanged  = "parties.bank_account_changed"  // payee org admins: a new IBAN was submitted
	tmplApproved = "parties.bank_account_approved" // payee org admins + finance: the IBAN money goes to changed
	tmplRejected = "parties.bank_account_rejected" // the submitter
	tmplRevoked  = "parties.bank_account_revoked"  // payee org admins + finance: payouts stop
)

// accountNotice is the payload of NotifyRevokedTaskName (IDs only).
type accountNotice struct {
	BankAccountID uuid.UUID `json:"bank_account_id"`
	PayeeID       uuid.UUID `json:"payee_id"`
}

// Subscribe registers the outbox subscriptions of parties (01 §6.2):
// payee_bank_account.submitted, .approved and .rejected → the notices;
// file.rejected of a document or a bank account's evidence → the people
// who upload it again (BE-154b).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicPayeeBankAccountSubmitted, NotifySubmittedTaskName, m.svc.handleSubmitted,
		outbox.Queue(queue.Critical))
	r.Subscribe(domainevent.TopicPayeeBankAccountApproved, NotifyApprovedTaskName, m.svc.handleApproved,
		outbox.Queue(queue.Critical))
	r.Subscribe(domainevent.TopicPayeeBankAccountRejected, NotifyRejectedTaskName, m.svc.handleRejected,
		outbox.Queue(queue.Default))
	files.SubscribeRejected(r, documentModule, NotifyRejectedEvidenceTaskName, m.svc.notifyRejectedEvidence)
}

// Tasks returns the plain task handlers of parties for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: NotifyRevokedTaskName, Handle: m.svc.handleRevoked}}
}

// handleSubmitted tells every finance admin that an account waits for an
// eye (stage pending_verification: the first eye; pending_approval: the
// second) and, for a new submission (an org's, or an admin's that starts
// at pending_approval), every admin of the payee's org: an IBAN change
// they did not make is how an account takeover redirects the money.
func (s *service) handleSubmitted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PayeeBankAccountSubmitted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	b, py, err := s.noticeSubject(ctx, p.BankAccountID)
	if err != nil || b.ID == uuid.Nil() {
		return err
	}
	stage := BankAccountStatus(p.Stage)
	if !stage.Pending() {
		return queue.Permanent(fmt.Errorf("parties: submitted notice: unknown stage %q", p.Stage))
	}
	fin, err := s.financeAdmins(ctx)
	if err != nil {
		return err
	}
	data := map[string]any{"stage": p.Stage}
	key := "parties.bank_account_pending:" + b.ID.String() + ":" + p.Stage
	errs := []error{s.notifyAll(ctx, b, tmplPending, key, fin, notify.AdminBankAccountsLink(), data)}
	if stage == AccountPendingVerification || b.SubmittedByAdmin {
		admins, err := s.orgAdmins(ctx, py.OrgID)
		if err != nil {
			return err
		}
		errs = append(errs, s.notifyAll(ctx, b, tmplChanged, "parties.bank_account_changed:"+b.ID.String(), admins,
			notify.OrgPayeeLink(py.OrgID, py.ID), nil))
	}
	return errors.Join(errs...)
}

// handleApproved sends the security notice of an approval: the IBAN money
// goes to changed (payee org admins and every finance admin).
func (s *service) handleApproved(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PayeeBankAccountApproved](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.notifyOrgAndFinance(ctx, p.BankAccountID, tmplApproved)
}

// handleRevoked sends the notice of a revocation (payouts stop).
func (s *service) handleRevoked(ctx context.Context, payload []byte) error {
	var n accountNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.BankAccountID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("parties: revoked notice payload: %w", errors.Join(err, errors.New("bank_account_id required"))))
	}
	return s.notifyOrgAndFinance(ctx, n.BankAccountID, tmplRevoked)
}

// handleRejected tells the submitter that the account was rejected (the
// reason is on the account; it is not mailed).
func (s *service) handleRejected(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PayeeBankAccountRejected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	b, py, err := s.noticeSubject(ctx, p.BankAccountID)
	if err != nil || b.ID == uuid.Nil() {
		return err
	}
	link := notify.OrgPayeeLink(py.OrgID, py.ID)
	if b.SubmittedByAdmin {
		link = notify.AdminBankAccountsLink()
	}
	return s.notifyAll(ctx, b, tmplRejected, "parties.bank_account_rejected:"+b.ID.String(), []uuid.UUID{b.SubmittedBy}, link, nil)
}

func (s *service) notifyOrgAndFinance(ctx context.Context, accountID uuid.UUID, tmpl string) error {
	b, py, err := s.noticeSubject(ctx, accountID)
	if err != nil || b.ID == uuid.Nil() {
		return err
	}
	fin, err := s.financeAdmins(ctx)
	if err != nil {
		return err
	}
	admins, err := s.orgAdmins(ctx, py.OrgID)
	if err != nil {
		return err
	}
	key := tmpl + ":" + b.ID.String()
	return errors.Join(
		s.notifyAll(ctx, b, tmpl, key, admins, notify.OrgPayeeLink(py.OrgID, py.ID), nil),
		s.notifyAll(ctx, b, tmpl, key, fin, notify.AdminBankAccountsLink(), nil),
	)
}

// noticeSubject reads the account and its payee; a missing one (never
// deleted: a stale task) is skipped with a zero account.
func (s *service) noticeSubject(ctx context.Context, id uuid.UUID) (BankAccount, Payee, error) {
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "bank account notice: account not found", slog.String("bank_account_id", id.String()))
		return BankAccount{}, Payee{}, nil
	case err != nil:
		return BankAccount{}, Payee{}, err
	}
	py, err := s.d.Store.GetPayee(ctx, b.PayeeID, false)
	if err != nil {
		return BankAccount{}, Payee{}, err
	}
	return b, py, nil
}

// financeAdmins returns every finance admin in effect (06 H-33).
func (s *service) financeAdmins(ctx context.Context) ([]uuid.UUID, error) {
	if s.d.FinanceAdmins == nil {
		return nil, errors.New("parties: the finance admin lookup is not wired")
	}
	ids, err := s.d.FinanceAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("parties: finance admins: %w", err)
	}
	return ids, nil
}

// orgAdmins returns the active club_admin and university_rep members of a
// payee's seller org (career_staff is not a rep, k-cs).
func (s *service) orgAdmins(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	return s.membersWith(ctx, orgID, authz.MemberClubAdmin, authz.MemberUniversityRep)
}

// notifyAll sends tmpl to every recipient: in-app (IDs only) and, when the
// address resolves, e-mail (the masked IBAN). The dedupe keys make a
// repeated task a no-op per recipient and channel; a permanent failure for
// one recipient is logged and skipped, anything else is retried.
func (s *service) notifyAll(ctx context.Context, b BankAccount, tmpl, key string, to []uuid.UUID, link notify.Link, data map[string]any) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("parties: notify is not wired")
	}
	org := b.OrgID
	var errs []error
	for _, uid := range to {
		u := uid
		msgs := []notify.Message{{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &org, LinkPath: link, DedupeKey: key + ":" + u.String() + ":in_app",
			Data: map[string]any{"bank_account_id": b.ID.String(), "payee_id": b.PayeeID.String()},
		}}
		c, ok, err := s.contact(ctx, u)
		if err != nil {
			errs = append(errs, err) // retried: the in-app notice is deduplicated
		}
		if ok {
			mail := map[string]any{"iban_masked": b.MaskedIBAN(), "name": c.FirstName}
			for k, v := range data {
				mail[k] = v
			}
			msgs = append(msgs, notify.Message{
				UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
				Template: tmpl, Locale: c.Locale, Data: mail, DedupeKey: key + ":" + u.String() + ":email",
			})
		}
		for _, m := range msgs {
			_, err := s.d.Sender.Send(ctx, m)
			switch {
			case err == nil:
			case notify.IsPermanent(err):
				s.d.Log.LogAttrs(ctx, slog.LevelError, "bank account notice not deliverable", slog.String("template", tmpl),
					slog.String("bank_account_id", b.ID.String()), slog.String("user_id", u.String()),
					slog.String("channel", string(m.Channel)), slog.String("err", err.Error()))
			default:
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// contact returns the address of a live user; an unknown or erased user,
// or no Users port, is in-app only. Any other lookup error is returned (the
// task is retried: a security notice must not lose its e-mail silently).
func (s *service) contact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("parties: notice contact: %w", err)
	}
	return c, c.Email != "", nil
}
