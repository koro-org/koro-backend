package ledger

import (
	"fmt"
	"time"
	"uuid"

	"koro/internal/orders"
	"koro/internal/platform/money"
)

// Model is a commerce model code (commerce_policies.model_code, 02 §18.3).
// The ledger flow of A2 is the same as A (Koro collects, pays the payee
// out; only the invoice direction differs, 02 §18.3 table).
type Model string

// Commerce models.
const (
	ModelA  Model = "A"  // marketplace: Koro collects, payee invoices the buyer, payout to the payee (MVP)
	ModelA2 Model = "A2" // as A; Koro invoices on the payee's behalf
	ModelB  Model = "B"  // Koro is the only seller, the payee is Koro's supplier
	ModelC  Model = "C"  // the buyer pays the payee directly; Koro collects its fee only
)

// ledgerModelA reports whether m posts with the model A templates, and
// returns ErrModelNotEnabled for the models whose flow T-11 has not decided.
func ledgerModelA(m Model) error {
	switch m {
	case ModelA, ModelA2:
		return nil
	case ModelB, ModelC:
		// TODO(T-11): model B posts the sale as Koro's own revenue
		// (buyer_advances + / platform_sales_revenue − / vat_output −; the
		// entries of Koro's own line in OrderAllocated, BE-067) and the
		// supplier's invoice as cost (cost_of_services + / vat_input + /
		// seller_payable(payee) −); model C keeps no buyer money (the payee
		// is paid directly, a direct_confirmed payment is informational) and
		// posts only Koro's fee (koro_bank + / platform_commission_revenue − /
		// vat_output −; BE-066). The accounts are seeded (000110); the
		// templates come with the T-11 decision.
		return ErrModelNotEnabled
	}
	return invalid("model", fmt.Sprintf("%q is not a commerce model", m))
}

// AllocationLine is the frozen money of one order line (order_lines, 02
// §19.2; computed by orders with platform/money at order time).
//
// A payee line (PayeeID set) is split into the payee's net, Koro's
// commission and the commission's VAT. The identity PayableAmount =
// SellerNetAmount + CommissionAmount + CommissionVATAmount holds for both
// commission payers:
//
//	payer=seller: payable = gross − withheld,              seller_net = payable − commission − commission_vat
//	payer=buyer:  payable = gross − withheld + buyer_fee,  seller_net = gross − withheld, buyer_fee = commission + commission_vat
//
// Koro's own platform-native line (PayeeID nil: seller_kind platform, no
// payee; BE-067, T-293) is Koro's sale: Koro is its seller, so it has no
// seller share (SellerNetAmount 0) and its money is the sale (NetAmount),
// the sale's VAT (VATAmount) less the part the buyer withholds
// (VATWithheldAmount, tevkifat: the buyer pays it to the tax office) and a
// buyer fee when the buyer pays the commission on top (CommissionAmount,
// CommissionVATAmount). A commission the seller pays on Koro's own line is
// Koro's to itself and moves nothing (AllocationOf leaves it out):
//
//	payable = net + vat − withheld + commission + commission_vat
//
// Every amount comes from the line as it was frozen (KDV rate, withholding
// fraction; 06 H-6, E-04): the ledger computes no VAT itself.
type AllocationLine struct {
	OrderLineID         uuid.UUID
	PayeeID             *uuid.UUID // the frozen payee; nil on Koro's own platform-native line
	PayableAmount       int64      // what the buyer pays for the line
	SellerNetAmount     int64      // the payee's share (hakediş); 0 on Koro's own line
	CommissionAmount    int64      // Koro's commission (or buyer fee net); on Koro's own line a buyer fee only
	CommissionVATAmount int64      // VAT on the commission
	// Koro's own line only (PayeeID nil): the sale.
	NetAmount         int64 // the sale excluding VAT → platform_sales_revenue
	VATAmount         int64 // the sale's VAT → vat_output
	VATWithheldAmount int64 // the part of VATAmount the buyer withholds (tevkifat) → vat_output back, its own entry
}

// AllocationOf is the allocation line of a line of a paid order (orders'
// frozen amounts): a payee line's shares, or Koro's own sale with its
// buyer fee (BE-067).
func AllocationOf(l orders.PaidLine) AllocationLine {
	a := AllocationLine{OrderLineID: l.LineID, PayableAmount: l.PayableAmount}
	if l.PayeeID != nil {
		payee := *l.PayeeID
		a.PayeeID = &payee
		a.SellerNetAmount, a.CommissionAmount, a.CommissionVATAmount = l.SellerNetAmount, l.CommissionAmount, l.CommissionVATAmount
		return a
	}
	a.NetAmount, a.VATAmount, a.VATWithheldAmount = l.NetAmount, l.VATAmount, l.VATWithheldAmount
	if l.BuyerFeeAmount > 0 { // the buyer paid the commission on top: Koro's fee revenue
		a.CommissionAmount, a.CommissionVATAmount = l.CommissionAmount, l.CommissionVATAmount
	}
	return a
}

// Koro reports Koro's own platform-native line (no payee).
func (l AllocationLine) Koro() bool { return l.PayeeID == nil }

// check validates the shares of a line; i is its index for field errors.
func (l AllocationLine) check(i int) error {
	loc := fmt.Sprintf("lines[%d]", i)
	switch {
	case l.OrderLineID == uuid.Nil():
		return invalid(loc+".order_line_id", "is required")
	case l.PayeeID != nil && *l.PayeeID == uuid.Nil():
		return invalid(loc+".payee_id", "must be a valid ID when set (nil: Koro's own line)")
	case l.PayableAmount < 0 || l.SellerNetAmount < 0 || l.CommissionAmount < 0 || l.CommissionVATAmount < 0 ||
		l.NetAmount < 0 || l.VATAmount < 0 || l.VATWithheldAmount < 0:
		return invalid(loc, "amounts must not be negative")
	}
	parts, what := []int64{l.SellerNetAmount, l.CommissionAmount, l.CommissionVATAmount}, "seller net + commission + commission VAT"
	if l.Koro() {
		switch {
		case l.SellerNetAmount != 0:
			return invalid(loc+".seller_net_amount", "Koro's own line has no seller share")
		case l.VATWithheldAmount > l.VATAmount:
			return invalid(loc+".vat_withheld_amount", "exceeds the VAT")
		}
		parts, what = []int64{l.NetAmount, l.VATAmount, -l.VATWithheldAmount, l.CommissionAmount, l.CommissionVATAmount},
			"net + VAT − withheld VAT + buyer fee"
	} else if l.NetAmount != 0 || l.VATAmount != 0 || l.VATWithheldAmount != 0 {
		return invalid(loc, "a payee line's sale is the payee's: net, VAT and withholding are set on Koro's own line only")
	}
	sum, err := money.Sum(parts...)
	switch {
	case err != nil:
		return invalid(loc, "the shares overflow")
	case sum != l.PayableAmount:
		return invalid(loc, fmt.Sprintf("payable %d is not %s (%d)", l.PayableAmount, what, sum))
	}
	return nil
}

// Shares returns what the allocation of the line credits, per share: a
// payee line's seller net, commission and commission VAT; Koro's own
// line's sale, buyer fee and the VAT it keeps (the sale's after
// withholding plus the fee's). Together they are the payable amount: what
// refunds of the line can take back (SplitRefund, BE-043 RefundedShares).
func (l AllocationLine) Shares() LineShares {
	if l.Koro() {
		return LineShares{Sales: l.NetAmount, Commission: l.CommissionAmount,
			CommissionVAT: l.VATAmount - l.VATWithheldAmount + l.CommissionVATAmount} // bounded by check
	}
	return LineShares{Seller: l.SellerNetAmount, Commission: l.CommissionAmount, CommissionVAT: l.CommissionVATAmount}
}

// entries appends the allocation entries of a checked line to es, each
// naming the line (zero shares left out):
//
//	buyer_advances +payable
//	payee line:     seller_payable(payee) −seller_net
//	Koro's own line: platform_sales_revenue −net / vat_output −vat / vat_output +withheld (tevkifat, its own entry)
//	platform_commission_revenue −commission / vat_output −commission_vat
func (l AllocationLine) entries(es []Entry) []Entry {
	line := ptr(l.OrderLineID)
	es = appendNonZero(es, Entry{Account: AccountBuyerAdvances, OrderLineID: line, Amount: l.PayableAmount})
	if l.Koro() {
		es = appendNonZero(es, Entry{Account: AccountSalesRevenue, OrderLineID: line, Amount: -l.NetAmount})
		es = appendNonZero(es, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: -l.VATAmount})
		es = appendNonZero(es, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: l.VATWithheldAmount})
	} else {
		es = appendNonZero(es, Entry{Account: AccountSellerPayable, PayeeID: ptr(*l.PayeeID), OrderLineID: line, Amount: -l.SellerNetAmount})
	}
	es = appendNonZero(es, Entry{Account: AccountCommissionRevenue, OrderLineID: line, Amount: -l.CommissionAmount})
	return appendNonZero(es, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: -l.CommissionVATAmount})
}

// appendNonZero appends e unless its amount is zero (ledger_entries_amount_chk).
func appendNonZero(es []Entry, e Entry) []Entry {
	if e.Amount == 0 {
		return es
	}
	return append(es, e)
}

// ptr returns a pointer to a copy of id.
func ptr(id uuid.UUID) *uuid.UUID { return &id }

// PaymentReceived is 02 §21.3 payment_received: a captured payment reaches
// Koro's bank and waits in buyer_advances until the order is fully paid
// (DECISIONS-LOG 2026-10-02: partial payments accumulate there).
//
//	koro_bank +amount / buyer_advances −amount
func PaymentReceived(m Model, paymentID uuid.UUID, amount int64, at time.Time) (Txn, error) {
	if err := ledgerModelA(m); err != nil {
		return Txn{}, err
	}
	if amount <= 0 {
		return Txn{}, invalid("amount", "must be positive")
	}
	return Txn{
		Kind: KindPaymentReceived, RefType: RefPayment, RefID: paymentID, OccurredAt: at,
		Entries: []Entry{
			{Account: AccountKoroBank, Amount: amount},
			{Account: AccountBuyerAdvances, Amount: -amount},
		},
	}, nil
}

// OrderAllocated is 02 §21.3 order_allocated: when the order is fully paid
// the advance is split line by line into the payee's payable, Koro's
// commission and the commission's VAT (a buyer fee goes to the same
// accounts) and, on Koro's own platform-native line, into Koro's sale
// revenue and its VAT (BE-067, T-293; model B's sale template without the
// supplier). One transaction per order — payee lines and Koro's lines of a
// mixed order together; every entry names its line.
//
//	per payee line: buyer_advances +payable / seller_payable(payee) −seller_net /
//	                platform_commission_revenue −commission / vat_output −commission_vat
//	per Koro line:  buyer_advances +payable / platform_sales_revenue −net / vat_output −vat /
//	                vat_output +withheld / [platform_commission_revenue −fee / vat_output −fee_vat]
//
// Zero shares are left out (a 0 % commission has no revenue entry); a
// transaction with no money (only free lines) is ErrInvalidTxn.
func OrderAllocated(m Model, orderID uuid.UUID, lines []AllocationLine, at time.Time) (Txn, error) {
	if err := ledgerModelA(m); err != nil {
		return Txn{}, err
	}
	if len(lines) == 0 {
		return Txn{}, invalid("lines", "an order has at least one line")
	}
	t := Txn{Kind: KindOrderAllocated, RefType: RefOrder, RefID: orderID, OccurredAt: at}
	for i, l := range lines {
		if err := l.check(i); err != nil {
			return Txn{}, err
		}
		t.Entries = l.entries(t.Entries)
	}
	if len(t.Entries) == 0 {
		return Txn{}, invalid("lines", "the order moves no money")
	}
	if err := t.validate(); err != nil { // overflow across lines, entry count
		return Txn{}, err
	}
	return t, nil
}

// LateRecognitionMemo is the memo of PlatformSaleRecognized (no personal data).
const LateRecognitionMemo = "late recognition of Koro's own sale paid before its template (BE-067, T-293)"

// PlatformSaleRecognized is the late allocation of Koro's own platform-native
// line paid before the template existed (BE-067, T-293): its money stayed
// in buyer_advances. It posts the line's order_allocated entries as an
// adjustment naming the line — ref_type manual, ref_id = the order line, so
// the business key makes the recognition idempotent (one per line, whoever
// posts it: ledger.recognize_platform_sales or the line's refund).
func PlatformSaleRecognized(m Model, l AllocationLine, at time.Time) (Txn, error) {
	if err := ledgerModelA(m); err != nil {
		return Txn{}, err
	}
	if !l.Koro() {
		return Txn{}, invalid("lines[0].payee_id", "only Koro's own line is recognised late (a payee line is allocated at payment)")
	}
	if err := l.check(0); err != nil {
		return Txn{}, err
	}
	t := Txn{Kind: KindAdjustment, RefType: RefManual, RefID: l.OrderLineID, OccurredAt: at, Memo: LateRecognitionMemo,
		Entries: l.entries(nil)}
	if len(t.Entries) == 0 {
		return Txn{}, invalid("lines", "the line moves no money")
	}
	if err := t.validate(); err != nil {
		return Txn{}, err
	}
	return t, nil
}

// PayoutPaid is 02 §21.3 payout_paid: the EFT to the payee left Koro's bank.
//
//	seller_payable(payee) +amount / koro_bank −amount
//
// Post rejects it (ErrNegativeBalance) when the payee is owed less.
func PayoutPaid(m Model, payoutID, payeeID uuid.UUID, amount int64, at time.Time) (Txn, error) {
	if err := ledgerModelA(m); err != nil {
		return Txn{}, err
	}
	switch {
	case payeeID == uuid.Nil():
		return Txn{}, invalid("payee_id", "is required")
	case amount <= 0:
		return Txn{}, invalid("amount", "must be positive")
	}
	return Txn{
		Kind: KindPayoutPaid, RefType: RefPayout, RefID: payoutID, OccurredAt: at,
		Entries: []Entry{
			{Account: AccountSellerPayable, PayeeID: ptr(payeeID), Amount: amount},
			{Account: AccountKoroBank, Amount: -amount},
		},
	}, nil
}

// RefundLine is the part of a refund that takes back one allocated line's
// shares (SplitRefund computes a proportional split).
type RefundLine struct {
	OrderLineID uuid.UUID
	PayeeID     uuid.UUID // the line's payee; the zero ID on Koro's own platform-native line (BE-067)
	// SellerAmount comes back from the payee's payable (or is claimed back
	// from the payee when PaidOut); 0 on Koro's own line.
	SellerAmount int64
	// SalesAmount comes back from Koro's sale revenue (platform_sales_revenue):
	// Koro's own line only (BE-067).
	SalesAmount      int64
	CommissionAmount int64 // back from Koro's commission (a buyer fee on Koro's own line)
	// CommissionVATAmount comes back to vat_output: the commission's VAT; on
	// Koro's own line every VAT it kept (the sale's after withholding and a
	// buyer fee's).
	CommissionVATAmount int64
	// PaidOut: the line was already paid out to the payee (06 H-59, T-231).
	// Its seller part cannot come back from seller_payable (the payee
	// balance would turn negative); Koro claims it back from the payee
	// instead: payee_receivable +seller, naming the line.
	PaidOut bool
}

// SplitRefund splits a refund of amount on an allocated line into its
// shares in proportion to the line's allocation (Shares; platform/money
// Allocate: largest remainder, the parts add up to amount exactly). The
// refunds module (BE-043) may build RefundLine itself instead, e.g. to take
// the last partial refund's remainder.
func SplitRefund(l AllocationLine, amount int64) (RefundLine, error) {
	if err := l.check(0); err != nil {
		return RefundLine{}, err
	}
	switch {
	case amount <= 0:
		return RefundLine{}, invalid("amount", "must be positive")
	case amount > l.PayableAmount:
		return RefundLine{}, invalid("amount", "exceeds the line's payable amount")
	}
	sh := l.Shares()
	// A zero weight takes nothing and never a remainder: a payee line splits
	// exactly as over its three shares alone.
	parts, err := money.Allocate(amount, []int64{sh.Seller, sh.Sales, sh.Commission, sh.CommissionVAT})
	if err != nil {
		return RefundLine{}, invalid("amount", "cannot be split: "+err.Error())
	}
	r := RefundLine{
		OrderLineID: l.OrderLineID, SellerAmount: parts[0], SalesAmount: parts[1], CommissionAmount: parts[2], CommissionVATAmount: parts[3],
	}
	if !l.Koro() {
		r.PayeeID = *l.PayeeID
	}
	return r, nil
}

// RefundCompleted is 02 §21.3 refund_completed: the refund left Koro's
// bank. Money not yet allocated (overpayment, a partially paid order) comes
// back from buyer_advances; an allocated line gives back its shares.
//
//	buyer_advances +unallocated
//	per payee line: seller_payable(payee) +seller / platform_commission_revenue +commission / vat_output +commission_vat
//	                (a paid-out line: payee_receivable +seller instead of seller_payable; 06 H-59)
//	per Koro line:  platform_sales_revenue +sales / platform_commission_revenue +fee / vat_output +vat (BE-067)
//	koro_bank −(unallocated + Σ lines)
//
// Post rejects it (ErrNegativeBalance) when the payee is owed less than the
// seller part of a line that was not paid out.
func RefundCompleted(m Model, refundID uuid.UUID, unallocated int64, lines []RefundLine, at time.Time) (Txn, error) {
	if err := ledgerModelA(m); err != nil {
		return Txn{}, err
	}
	if unallocated < 0 {
		return Txn{}, invalid("unallocated", "must not be negative")
	}
	t := Txn{Kind: KindRefundCompleted, RefType: RefRefund, RefID: refundID, OccurredAt: at}
	t.Entries = appendNonZero(t.Entries, Entry{Account: AccountBuyerAdvances, Amount: unallocated})
	parts := []int64{unallocated}
	for i, l := range lines {
		loc := fmt.Sprintf("lines[%d]", i)
		koro := l.PayeeID == uuid.Nil()
		switch {
		case l.OrderLineID == uuid.Nil():
			return Txn{}, invalid(loc+".order_line_id", "is required")
		case l.SellerAmount < 0 || l.SalesAmount < 0 || l.CommissionAmount < 0 || l.CommissionVATAmount < 0:
			return Txn{}, invalid(loc, "amounts must not be negative")
		case koro && (l.SellerAmount != 0 || l.PaidOut):
			return Txn{}, invalid(loc+".payee_id", "is required: only a payee line has a seller part")
		case !koro && l.SalesAmount != 0:
			return Txn{}, invalid(loc+".sales_amount", "only Koro's own line gives back a sale")
		}
		line := ptr(l.OrderLineID)
		switch {
		case koro:
			t.Entries = appendNonZero(t.Entries, Entry{Account: AccountSalesRevenue, OrderLineID: line, Amount: l.SalesAmount})
		case l.PaidOut:
			t.Entries = appendNonZero(t.Entries, Entry{Account: AccountPayeeReceivable, OrderLineID: line, Amount: l.SellerAmount})
		default:
			t.Entries = appendNonZero(t.Entries, Entry{Account: AccountSellerPayable, PayeeID: ptr(l.PayeeID), OrderLineID: line, Amount: l.SellerAmount})
		}
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountCommissionRevenue, OrderLineID: line, Amount: l.CommissionAmount})
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: l.CommissionVATAmount})
		parts = append(parts, l.SellerAmount, l.SalesAmount, l.CommissionAmount, l.CommissionVATAmount)
	}
	total, err := money.Sum(parts...)
	switch {
	case err != nil:
		return Txn{}, invalid("lines", "the amounts overflow")
	case total <= 0:
		return Txn{}, invalid("lines", "the refund moves no money")
	}
	t.Entries = append(t.Entries, Entry{Account: AccountKoroBank, Amount: -total})
	if err := t.validate(); err != nil {
		return Txn{}, err
	}
	return t, nil
}
