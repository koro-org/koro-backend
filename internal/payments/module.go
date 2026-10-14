// Package payments is the payments module (01 §4.20): Koro's collection
// accounts (the IBANs buyers pay into, four eyes), the incoming bank
// transfers finance records by hand or imports from a bank statement (CSV),
// and the payment capture: a finance admin matches a transfer to an order by
// its reference code, which in one transaction marks the transfer matched,
// captures the payment, applies it to the order (orders.ApplyPayment: the
// order is paid and its rights active once everything is in), writes the
// payment splits, posts the ledger (payment_received, order_allocated) and
// emits payment.captured (01 §6.3 "Havale eşleştirme = ödeme yakalama").
//
// Rules the package enforces:
//
//   - Admin surface (03 §5.16, §7.2-3): every operation is /admin/**; an
//     actor without the role gets 403 before anything is read.
//   - Four eyes (03 §6.1, 06 H-18): a collection account is decided by an
//     admin other than its creator; a transfer's return to its sender is a
//     money exit — requested by one finance admin, approved by another,
//     closed with the EFT receipt (06 H-56: three steps). The database
//     CHECKs are the second layer (collection_accounts_four_eyes_chk,
//     incoming_bank_transfers_return_four_eyes_chk).
//   - Re-authentication (03 §6.2): creating, deciding, retiring and making
//     default a collection account and every step of a return carry a
//     reauth proof (403 identity.reauth_required / reauth_failed).
//   - Capture (T-14): a transfer pays at most what the order still waits
//     for; the rest opens an overpayment refund request (03 k27: the refund
//     itself takes four eyes, BE-043). Partial payments accumulate. The
//     match is idempotent (Idempotency-Key) and serialised on the transfer
//     row: one transfer is captured once. Any failure of the chain — the
//     order, the ledger — rolls the whole match back: the transfer stays
//     unmatched.
//   - Ledger (BE-039 templates, 02 §21.3): a captured transfer posts
//     payment_received; a fully paid order posts order_allocated for its
//     payee lines. Koro's own platform-native lines (payee NULL) have no
//     allocation template yet (ledger TODO T-11): their money stays in
//     buyer_advances, unallocated (T-293). Models B and C have no ledger
//     template (ledger.model_not_enabled): their fee transfers cannot be
//     captured until T-11 decides them.
//   - Model C (06 H-56, T-18, T-227): a finance admin's confirmation that the
//     buyer paid the payees directly records a direct_confirmed payment and
//     pays the order (orders.ConfirmDirectPayment) once Koro's fee is in.
//   - Refunds (BE-043; 03 §6.1 "İade", 02 §20.5): money going back to the
//     buyer leaves in three steps. A finance or platform admin requests it
//     (Idempotency-Key) from a payment: an overpayment (the part of a
//     transfer the order did not wait for, 03 k27; also opened by the match
//     itself), the money of an order cancelled or expired before it was
//     paid in full, or a line of a paid order (the cancelled lines wait in
//     orders.RefundsDue; ListRefundsDue shows them with the payments that
//     can fund them). Every limit counts the requested and approved
//     refunds: the payment's amount, the order's unapplied money, what Koro
//     holds of the line (its buyer fee only in model C). Another finance
//     admin approves it with re-authentication (authz four eyes +
//     refunds_four_eyes_chk, again on the locked row); a finance admin may
//     reject a request. After the EFT any finance admin completes it with
//     the receipt (refund_receipt) and the bank reference: in ONE
//     transaction the refund is completed, the payment's refunded amount
//     grows, orders.ApplyRefund records it on the order (not for an
//     overpayment: that money was never the order's), the ledger posts
//     refund_completed (unallocated money from buyer_advances; a line's
//     shares split against what is left of them, platform/money) and
//     payment.refunded leaves (O). A line in a payout waiting for approval
//     or for its transfer cannot be refunded (409 payments.line_in_payout,
//     checked again after the ledger took the payee's lock); a line already
//     paid out is refunded under the same four eyes and its seller part is
//     claimed back from the payee (payee_receivable; 06 H-59: collected by
//     hand, never offset against a later payout).
//   - Personal data: a transfer's sender may be a natural person (name,
//     IBAN); the rows are financial records (10 years) and are neither
//     deleted nor anonymised on account erasure (privacy.go).
package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Payments is the contract of the higher layers (01 §4.20).
type Payments interface {
	// Get returns a payment or ErrNotFound.
	Get(ctx context.Context, paymentID uuid.UUID) (Payment, error)
}

// Service is the payments service: the cross-module contract plus the HTTP
// use cases of 04 §2.16 (BE-041's part). Every method that takes an actor
// calls authz.Can first (05 §5.3).
type Service interface {
	Payments

	// ListCollectionAccounts is adminListCollectionAccounts.
	ListCollectionAccounts(ctx context.Context, a authz.Actor) ([]CollectionAccount, error)
	// CreateCollectionAccount is adminCreateCollectionAccount (R).
	CreateCollectionAccount(ctx context.Context, a authz.Actor, in AccountInput, proof identity.ReauthProof) (CollectionAccount, error)
	// DecideCollectionAccount is adminApproveCollectionAccount (four eyes, R).
	DecideCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision, proof identity.ReauthProof) (CollectionAccount, error)
	// RetireCollectionAccount is adminRetireCollectionAccount (R).
	RetireCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (CollectionAccount, error)
	// MakeDefaultCollectionAccount is adminMakeDefaultCollectionAccount (R).
	MakeDefaultCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (CollectionAccount, error)

	// ListTransfers is adminListIncomingTransfers.
	ListTransfers(ctx context.Context, a authz.Actor, q TransferQuery) (TransferPage, error)
	// GetTransfer is adminGetIncomingTransfer: the transfer and the orders it
	// may pay (its reference code and amount).
	GetTransfer(ctx context.Context, a authz.Actor, id uuid.UUID) (TransferView, error)
	// CreateTransfer is adminCreateIncomingTransfer.
	CreateTransfer(ctx context.Context, a authz.Actor, in TransferInput) (IncomingTransfer, error)
	// ImportTransfers is adminImportIncomingTransfers (Idempotency-Key).
	ImportTransfers(ctx context.Context, a authz.Actor, in ImportInput) (ImportResult, error)
	// MatchTransfer is adminMatchIncomingTransfer (Idempotency-Key): the
	// payment capture.
	MatchTransfer(ctx context.Context, a authz.Actor, id uuid.UUID, in MatchInput) (MatchResult, error)
	// IgnoreTransfer is adminIgnoreIncomingTransfer.
	IgnoreTransfer(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (IncomingTransfer, error)
	// RequestReturn is adminRequestIncomingTransferReturn (06 H-56 step 1, R).
	RequestReturn(ctx context.Context, a authz.Actor, id uuid.UUID, in ReturnRequest, proof identity.ReauthProof) (IncomingTransfer, error)
	// DecideReturn is adminApproveIncomingTransferReturn (step 2, four eyes, R).
	DecideReturn(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision, proof identity.ReauthProof) (IncomingTransfer, error)
	// MarkReturned is adminMarkIncomingTransferReturned (step 3, R).
	MarkReturned(ctx context.Context, a authz.Actor, id uuid.UUID, in ReturnReceipt, proof identity.ReauthProof) (IncomingTransfer, error)

	// ConfirmDirectPayment is adminConfirmDirectPayment (model C;
	// Idempotency-Key).
	ConfirmDirectPayment(ctx context.Context, a authz.Actor, in DirectInput) (DirectResult, error)

	// ListPayments is adminListPayments (BE-043).
	ListPayments(ctx context.Context, a authz.Actor, q PaymentQuery) (PaymentPage, error)
	// GetPaymentView is adminGetPayment: the payment, its splits and refunds.
	GetPaymentView(ctx context.Context, a authz.Actor, id uuid.UUID) (PaymentView, error)
	// ListRefunds is adminListRefunds.
	ListRefunds(ctx context.Context, a authz.Actor, q RefundQuery) (RefundPage, error)
	// ListRefundsDue is adminListRefundsDue: the money orders says waits for
	// a refund that no open refund covers yet, with the payments that can
	// fund it.
	ListRefundsDue(ctx context.Context, a authz.Actor, limit int) ([]RefundDue, error)
	// CreateRefund is adminCreateRefund (Idempotency-Key): a refund request.
	CreateRefund(ctx context.Context, a authz.Actor, in RefundInput) (Refund, error)
	// ApproveRefund is adminApproveRefund (four eyes, R).
	ApproveRefund(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (Refund, error)
	// RejectRefund is adminRejectRefund.
	RejectRefund(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (Refund, error)
	// CompleteRefund is adminCompleteRefund: the EFT's receipt and bank
	// reference; ledger, order and payment.refunded in one transaction.
	CompleteRefund(ctx context.Context, a authz.Actor, id uuid.UUID, in RefundReceipt) (RefundResult, error)
}

// Deps are the dependencies of the payments module. Store, Tx, Audit,
// Idempotency, Orders, Ledger, Outbox, Queue, Clock and Log are required
// (New returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Audit       AuditLogger
	Idempotency *idempotency.Keeper // the Idempotency-Key of the match, the import and the other POSTs (06 H-5)
	Orders      Orders
	Ledger      Poster
	Outbox      outbox.Writer  // O events in the transaction, A events after it
	Queue       queue.Enqueuer // the collection account change notice (A, no topic)
	Clock       clock.Clock
	Log         *slog.Logger

	// Parties (Koro's party), Files (receipts), Reauth (R operations) and
	// LedgerLines (the payout and share reads of a line refund): nil → the
	// operations that need them fail closed (503).
	Parties     Parties
	Files       Files
	Reauth      Reauthenticator
	LedgerLines LedgerLines

	// Orgs, Sender, Users and Staff serve the notices in the worker; nil
	// Sender or Staff → the notice task fails and is retried; nil Users →
	// in-app only; nil Orgs → the payment notices fail and are retried.
	Orgs   Orgs
	Sender Sender
	Users  Users
	Staff  Staff
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Idempotency", d.Idempotency != nil},
		{"Orders", d.Orders != nil}, {"Ledger", d.Ledger != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
		{"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("payments.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what payments hands to the composition root.
type Module struct {
	Service  Service
	Payments Payments
	// Privacy is the KVKK provider (the staff references of transfers and
	// payments; financial records, retained on erasure).
	Privacy privacy.Provider

	svc *service
}

// New builds the payments module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Payments: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// DefaultAccount is hooks.collectionAccount (orders' payment instructions):
// Koro's active default collection account; found is false when none is.
func (m Module) DefaultAccount(ctx context.Context) (CollectionAccount, bool, error) {
	return m.svc.d.Store.DefaultAccount(ctx)
}

// Jobs returns the scheduled jobs of payments (01 §7): payments.unmatched_digest.
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.digestJob()} }

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

// Get implements Payments.
func (s *service) Get(ctx context.Context, paymentID uuid.UUID) (Payment, error) {
	return s.d.Store.GetPayment(ctx, paymentID, false)
}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// reauth verifies the proof of an R operation (03 §6.2) and returns its
// kind for the audit record; without a verifier the operation fails closed.
func (s *service) reauth(ctx context.Context, a authz.Actor, proof identity.ReauthProof) (string, error) {
	if s.d.Reauth == nil {
		return "", ErrUnavailable
	}
	return s.d.Reauth.Verify(ctx, a.UserID, proof)
}

// logInfo logs a business step (IDs and amounts only, never personal data).
func (s *service) logInfo(ctx context.Context, msg string, attrs ...slog.Attr) {
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
}
