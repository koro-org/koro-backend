package ledger

import (
	"fmt"
	"time"
	"uuid"

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
		// (buyer_advances + / platform_sales_revenue − / vat_output −) and
		// the supplier's invoice as cost (cost_of_services + / vat_input + /
		// seller_payable(payee) −); model C keeps no buyer money (the payee
		// is paid directly, a direct_confirmed payment is informational) and
		// posts only Koro's fee (koro_bank + / platform_commission_revenue − /
		// vat_output −). The accounts are seeded (000110); the templates come
		// with the T-11 decision.
		return ErrModelNotEnabled
	}
	return invalid("model", fmt.Sprintf("%q is not a commerce model", m))
}

// AllocationLine is the frozen money of one order line (order_lines, 02
// §19.2; computed by orders with platform/money at order time). The
// identity PayableAmount = SellerNetAmount + CommissionAmount +
// CommissionVATAmount holds for both commission payers:
//
//	payer=seller: payable = gross − withheld,              seller_net = payable − commission − commission_vat
//	payer=buyer:  payable = gross − withheld + buyer_fee,  seller_net = gross − withheld, buyer_fee = commission + commission_vat
type AllocationLine struct {
	OrderLineID         uuid.UUID
	PayeeID             *uuid.UUID // the frozen payee; model A lines always have one
	PayableAmount       int64      // what the buyer pays for the line
	SellerNetAmount     int64      // the payee's share (hakediş)
	CommissionAmount    int64      // Koro's commission (or buyer fee net)
	CommissionVATAmount int64      // VAT on the commission
}

// check validates the shares of a line; i is its index for field errors.
func (l AllocationLine) check(i int) error {
	loc := fmt.Sprintf("lines[%d]", i)
	switch {
	case l.OrderLineID == uuid.Nil():
		return invalid(loc+".order_line_id", "is required")
	case l.PayeeID == nil || *l.PayeeID == uuid.Nil():
		// TODO(T-11): a platform-native line (Koro sells its own feature,
		// payee NULL) is a model B sale; it has no model A template.
		return invalid(loc+".payee_id", "a model A line has a payee")
	case l.PayableAmount < 0 || l.SellerNetAmount < 0 || l.CommissionAmount < 0 || l.CommissionVATAmount < 0:
		return invalid(loc, "amounts must not be negative")
	}
	sum, err := money.Sum(l.SellerNetAmount, l.CommissionAmount, l.CommissionVATAmount)
	switch {
	case err != nil:
		return invalid(loc, "the shares overflow")
	case sum != l.PayableAmount:
		return invalid(loc, fmt.Sprintf("payable %d is not seller net + commission + commission VAT (%d)", l.PayableAmount, sum))
	}
	return nil
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
// accounts). One transaction per order; every entry names its line.
//
//	per line: buyer_advances +payable / seller_payable(payee) −seller_net /
//	          platform_commission_revenue −commission / vat_output −commission_vat
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
		line := ptr(l.OrderLineID)
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountBuyerAdvances, OrderLineID: line, Amount: l.PayableAmount})
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountSellerPayable, PayeeID: ptr(*l.PayeeID), OrderLineID: line, Amount: -l.SellerNetAmount})
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountCommissionRevenue, OrderLineID: line, Amount: -l.CommissionAmount})
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: -l.CommissionVATAmount})
	}
	if len(t.Entries) == 0 {
		return Txn{}, invalid("lines", "the order moves no money")
	}
	if err := t.validate(); err != nil { // overflow across lines, entry count
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
	OrderLineID         uuid.UUID
	PayeeID             uuid.UUID
	SellerAmount        int64 // back from the payee's payable (or claimed back from the payee when PaidOut)
	CommissionAmount    int64 // back from Koro's commission
	CommissionVATAmount int64 // back from the commission's VAT
	// PaidOut: the line was already paid out to the payee (06 H-59, T-231).
	// Its seller part cannot come back from seller_payable (the payee
	// balance would turn negative); Koro claims it back from the payee
	// instead: payee_receivable +seller, naming the line.
	PaidOut bool
}

// SplitRefund splits a refund of amount on an allocated line into its
// shares in proportion to the line's allocation (platform/money Allocate:
// largest remainder, the parts add up to amount exactly). The refunds
// module (BE-043) may build RefundLine itself instead, e.g. to take the
// last partial refund's remainder.
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
	parts, err := money.Allocate(amount, []int64{l.SellerNetAmount, l.CommissionAmount, l.CommissionVATAmount})
	if err != nil {
		return RefundLine{}, invalid("amount", "cannot be split: "+err.Error())
	}
	return RefundLine{
		OrderLineID: l.OrderLineID, PayeeID: *l.PayeeID,
		SellerAmount: parts[0], CommissionAmount: parts[1], CommissionVATAmount: parts[2],
	}, nil
}

// RefundCompleted is 02 §21.3 refund_completed: the refund left Koro's
// bank. Money not yet allocated (overpayment, a partially paid order) comes
// back from buyer_advances; an allocated line gives back its shares.
//
//	buyer_advances +unallocated
//	per line: seller_payable(payee) +seller / platform_commission_revenue +commission / vat_output +commission_vat
//	          (a paid-out line: payee_receivable +seller instead of seller_payable; 06 H-59)
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
		switch {
		case l.OrderLineID == uuid.Nil():
			return Txn{}, invalid(loc+".order_line_id", "is required")
		case l.PayeeID == uuid.Nil():
			return Txn{}, invalid(loc+".payee_id", "is required")
		case l.SellerAmount < 0 || l.CommissionAmount < 0 || l.CommissionVATAmount < 0:
			return Txn{}, invalid(loc, "amounts must not be negative")
		}
		line := ptr(l.OrderLineID)
		seller := Entry{Account: AccountSellerPayable, PayeeID: ptr(l.PayeeID), OrderLineID: line, Amount: l.SellerAmount}
		if l.PaidOut {
			seller = Entry{Account: AccountPayeeReceivable, OrderLineID: line, Amount: l.SellerAmount}
		}
		t.Entries = appendNonZero(t.Entries, seller)
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountCommissionRevenue, OrderLineID: line, Amount: l.CommissionAmount})
		t.Entries = appendNonZero(t.Entries, Entry{Account: AccountVATOutput, OrderLineID: line, Amount: l.CommissionVATAmount})
		parts = append(parts, l.SellerAmount, l.CommissionAmount, l.CommissionVATAmount)
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
