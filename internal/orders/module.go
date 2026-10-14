// Package orders is the orders module (01 §4.18): a buying company's
// order of one or more offerings, priced on the server (VAT, withholding,
// commission per line with platform/money), the billed party's snapshot,
// the payee and the commerce policy frozen on each line, the bank-transfer
// reference code, the payment window, the delivery flow (the seller marks
// a line, the buyer confirms or disputes it within the dispute window, an
// admin resolves a dispute) and the rights of platform-native lines
// (granted pending at placement, activated when the order is paid).
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6, 06 H-38): /orgs/{org_id}/orders and
//     /orgs/{org_id}/sales are the org's management surface (non-members
//     404). An order is the buyer's; a seller org sees only its own lines
//     and the buyer's title (03 k26, SellerOrderView); anyone else gets 404.
//     The scope columns of a line (buyer, seller, payee, event) come from
//     the order and the offering, never from the request.
//   - 03 k25: only a company admin of an active company orders, billed to a
//     verified buyer_company party of that company (422
//     orders.buyer_party_required); a recruiter never orders (03 §6.4: the
//     company admin's session is MFA verified).
//   - Money (06 H-6, ADR-0014): the request carries offerings and
//     quantities only; every amount comes from catalog (price, VAT rate,
//     VAT class, withholding) and the commerce policy in effect, computed per
//     line (pricing.go) and frozen (orders_frozen, order_lines_frozen).
//     Without a policy in effect there is no order (409
//     catalog.no_commerce_policy; T-10: production has no default rate
//     until the team decides it). Lines of different models are split
//     into one order per model (T-12).
//   - Idempotency-Key (06 H-5): placement is one transaction — offering,
//     policy, payee, stock reservation, the orders and lines, the pending
//     rights and order.placed (O) — and the key is stored in it, so a
//     retry neither orders nor reserves twice.
//   - Payment (01 §4.18 Orders): payments (BE-041) applies captured money
//     in its transaction; partial payments accumulate (T-14) and the order
//     is paid when Koro has collected everything it waits for. Model C
//     (the buyer pays the payee directly, Koro collects its fee): as in
//     model A the order is paid only once all of it is paid (06 H-56), so
//     it is paid when the fee is in and a finance admin confirmed the
//     direct payment (ConfirmDirectPayment; T-18, T-227); its lines are
//     never paid out and a refund through Koro covers only its fee. A paid
//     order activates its rights and fulfils its platform-native lines.
//   - The payment window (T-13, ORDERS_PAYMENT_WINDOW, 7 days): an unpaid
//     order expires (orders.expire_unpaid), its units go back to the
//     offerings and its pending rights are revoked; a cancelled order
//     likewise. An offering cancelled after the order (offering.cancelled)
//     cancels the unpaid orders and queues the undelivered lines of paid
//     ones for refund (RefundsDue; BE-043 refunds them).
//   - Delivery: a line is marked only on a paid order; the dispute window
//     of the frozen policy (dispute_window_days) starts then; the buyer
//     confirms or disputes it inside the window; orders.close_dispute_windows
//     fulfils the rest; a dispute is resolved by a platform or finance
//     admin (delivered, or cancelled for a refund). Fulfilment emits
//     order_line.fulfilled (O) for billing.
//   - Personal data: placed_by_user_id and fulfillment_marked_by name
//     users; orders are financial records (10 years): account erasure
//     retains them (privacy.go).
package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Orders is the contract of payments, ledger and billing (01 §4.18). It
// takes no actor (trusted in-process calls; the caller authorises its own
// operation) and every method joins the caller's transaction.
type Orders interface {
	// ForPayment returns what payments needs to match money to an order.
	ForPayment(ctx context.Context, orderID uuid.UUID) (PayableOrder, error)
	// FindByReference finds an order by the reference code of a transfer
	// description, however it was typed (NormalizeReference); found is
	// false for a malformed or unknown code.
	FindByReference(ctx context.Context, ref string) (PayableOrder, bool, error)
	// ApplyPayment adds captured money to the order (T-14: partial payments
	// accumulate). When Koro has collected everything, a model A, A2 or B
	// order is paid, its rights are active, its platform-native lines
	// fulfilled and the effect carries every line for the splits and the
	// ledger. A model C order collects only Koro's fee here.
	ApplyPayment(ctx context.Context, orderID, paymentID uuid.UUID, amount int64) (PaymentEffect, error)
	// ConfirmDirectPayment pays a model C order whose fee is collected once
	// a finance admin confirmed the buyer's direct payment to the payees
	// (payments direct_confirmed, BE-041; T-18, T-227, 06 H-56); fee not in
	// yet → 409 orders.order_not_paid.
	ConfirmDirectPayment(ctx context.Context, orderID, paymentID uuid.UUID) (PaymentEffect, error)
	// ApplyRefund records a completed refund of the order (a line of it
	// when lineID is set; BE-043).
	ApplyRefund(ctx context.Context, orderID uuid.UUID, lineID *uuid.UUID, amount int64) error
	// PayableLines returns the payee's lines a payout may take as of asOf:
	// paid, fulfilled, their payout hold elapsed, not refunded, not paid out.
	PayableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]PayableLine, error)
	// MarkLinesPaidOut records the payout of the lines (ledger, BE-044):
	// every line must be payable and of the payee of the payout.
	MarkLinesPaidOut(ctx context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID) error
	// GetLine returns an order line or ErrNotFound.
	GetLine(ctx context.Context, lineID uuid.UUID) (Line, error)
	// RefundsDue lists the money that waits for a refund (offering
	// cancelled after payment, dispute resolved with a refund, an order
	// cancelled or expired with money paid); BE-043 opens the refunds.
	RefundsDue(ctx context.Context, limit int) ([]RefundDue, error)
}

// Service is the orders service: the cross-module contract plus the HTTP
// use cases of 04 §2.15. Every method that takes an actor calls authz.Can
// first (05 §5.3).
type Service interface {
	Orders

	// CreateOrder is createOrder (orders.order.create; Idempotency-Key):
	// one order per model of the lines (T-12).
	CreateOrder(ctx context.Context, a authz.Actor, orgID uuid.UUID, in CreateInput) ([]OrderView, error)
	// ListOrgOrders is listOrgOrders (orders.order.list).
	ListOrgOrders(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (OrderPage, error)
	// GetOrder is getOrder (orders.order.read): the buyer's and staff's
	// view, or a seller's view of its own lines (03 k26).
	GetOrder(ctx context.Context, a authz.Actor, id uuid.UUID) (OrderView, error)
	// PaymentInstructions is getOrderPaymentInstructions
	// (orders.order.payment_instructions).
	PaymentInstructions(ctx context.Context, a authz.Actor, id uuid.UUID) (Instructions, error)
	// CancelOrder is cancelOrder (orders.order.cancel): awaiting_payment only.
	CancelOrder(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (OrderView, error)
	// ListOrgSales is listOrgSales (orders.line.list_sales).
	ListOrgSales(ctx context.Context, a authz.Actor, orgID uuid.UUID, q SalesQuery) (SalesPage, error)
	// MarkLineFulfilled is markOrderLineFulfilled (orders.line.mark_fulfilled).
	MarkLineFulfilled(ctx context.Context, a authz.Actor, lineID uuid.UUID, in MarkInput) (LineView, error)
	// ConfirmLine is confirmOrderLine (orders.line.confirm).
	ConfirmLine(ctx context.Context, a authz.Actor, lineID uuid.UUID) (LineView, error)
	// DisputeLine is disputeOrderLine (orders.line.dispute).
	DisputeLine(ctx context.Context, a authz.Actor, lineID uuid.UUID, reason string) (LineView, error)
	// AdminResolveDispute is adminResolveOrderLineDispute
	// (orders.line.resolve_dispute).
	AdminResolveDispute(ctx context.Context, a authz.Actor, lineID uuid.UUID, in ResolveInput) (LineView, error)
	// AdminListOrders is adminListOrders (orders.order.list_admin).
	AdminListOrders(ctx context.Context, a authz.Actor, q ListQuery) (OrderPage, error)
}

// Config is the orders configuration (config.Orders).
type Config struct {
	// PaymentWindow is how long an order waits for its payment (T-13: 7
	// days); 0 → DefaultPaymentWindow.
	PaymentWindow time.Duration
}

// DefaultPaymentWindow is the payment window of T-13.
const DefaultPaymentWindow = 7 * 24 * time.Hour

// Deps are the dependencies of the orders module. Store, Tx, Audit,
// Idempotency, Orgs, Offerings, Parties, Payees, Entitlements, Outbox,
// Clock and Log are required (New returns an error otherwise).
type Deps struct {
	Store        Store
	Tx           db.TxManager
	Audit        AuditLogger
	Idempotency  *idempotency.Keeper // Idempotency-Key of createOrder (required) and the other POSTs (06 H-5)
	Orgs         Orgs
	Offerings    Offerings
	Parties      Parties
	Payees       Payees
	Entitlements Entitlements
	Outbox       outbox.Writer // O events in the transaction, A events after it
	Clock        clock.Clock
	Log          *slog.Logger
	Config       Config

	// CollectionAccount is Koro's account of the payment instructions
	// (hooks.collectionAccount, payments BE-041). nil → 409
	// payments.no_active_collection_account; the order.placed e-mail is
	// left out (logged) and the instructions stay in the app.
	CollectionAccount CollectionAccounts
	// Sender, Users and Staff serve the notices in the worker; nil Sender or
	// Staff → the notice task fails and is retried; nil Users → in-app only.
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
		{"Orgs", d.Orgs != nil}, {"Offerings", d.Offerings != nil}, {"Parties", d.Parties != nil}, {"Payees", d.Payees != nil},
		{"Entitlements", d.Entitlements != nil}, {"Outbox", d.Outbox != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("orders.Deps.%s is missing", c.name))
		}
	}
	if d.Config.PaymentWindow < 0 {
		errs = append(errs, errors.New("orders.Deps.Config.PaymentWindow is negative"))
	}
	return errors.Join(errs...)
}

// Module is what orders hands to the composition root.
type Module struct {
	Service Service
	// Orders is the contract of payments, ledger and billing (01 §4.18).
	Orders Orders
	// Privacy is the KVKK provider (the orders a user placed, the lines a
	// user marked; financial records, retained on erasure).
	Privacy privacy.Provider

	svc *service
}

// New builds the orders module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	if d.Config.PaymentWindow == 0 {
		d.Config.PaymentWindow = DefaultPaymentWindow
	}
	s := &service{d: d}
	return Module{Service: s, Orders: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// BuyerHeld is catalog's per-buyer count (catalog.Deps.BuyerHeld): the units
// of the offering the buyer holds on open orders, read in the caller's
// transaction under the offering's row lock.
func (m Module) BuyerHeld(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error) {
	return m.svc.d.Store.BuyerHeld(ctx, offeringID, buyerOrgID)
}

// OpenCommerce is the closeOrg check of hooks.openCommerce: an org with an
// open order (as the buyer) or a line still to deliver, refund or pay out
// (as the seller) cannot be closed.
func (m Module) OpenCommerce(ctx context.Context, orgID uuid.UUID) (bool, error) {
	return m.svc.d.Store.HasOpenCommerce(ctx, orgID)
}

// Jobs returns the scheduled jobs of orders (01 §7): orders.expire_unpaid
// and orders.close_dispute_windows.
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.expireJob(), m.svc.closeWindowsJob()} }

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
