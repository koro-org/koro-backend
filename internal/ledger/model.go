package ledger

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/money"
	"koro/internal/platform/problem"
)

// Kind is ledger_transactions.kind (02 §21.2): the business event a
// transaction records.
type Kind string

// Kinds.
const (
	KindPaymentReceived Kind = "payment_received" // a captured payment reaches Koro's bank
	KindOrderAllocated  Kind = "order_allocated"  // a fully paid order is split into payable, commission and VAT
	KindPayoutPaid      Kind = "payout_paid"      // a payout left Koro's bank
	KindRefundCompleted Kind = "refund_completed" // a refund left Koro's bank
	KindFee             Kind = "fee"              // a bank or PSP fee
	KindAdjustment      Kind = "adjustment"       // a correction: a reversing transaction (Reverse)
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindPaymentReceived, KindOrderAllocated, KindPayoutPaid, KindRefundCompleted, KindFee, KindAdjustment:
		return true
	}
	return false
}

// RefType is ledger_transactions.ref_type: the record the event belongs to.
type RefType string

// Ref types.
const (
	RefPayment RefType = "payment" // payments.id
	RefOrder   RefType = "order"   // orders.id
	RefPayout  RefType = "payout"  // payouts.id
	RefRefund  RefType = "refund"  // refunds.id
	RefManual  RefType = "manual"  // a correction; for a reversal the reversed transaction's ID
)

// Valid reports whether r is a known ref type.
func (r RefType) Valid() bool {
	switch r {
	case RefPayment, RefOrder, RefPayout, RefRefund, RefManual:
		return true
	}
	return false
}

// refTypesOf lists the ref types a kind may carry. The database accepts
// every pair; the ledger narrows them so that the idempotency key (kind,
// ref_type, ref_id) always names the record that caused the event.
var refTypesOf = map[Kind][]RefType{
	KindPaymentReceived: {RefPayment},
	KindOrderAllocated:  {RefOrder},
	KindPayoutPaid:      {RefPayout},
	KindRefundCompleted: {RefRefund},
	KindFee:             {RefPayment, RefRefund, RefPayout, RefManual},
	KindAdjustment:      {RefManual},
}

// Account is a ledger_accounts.code (02 §21.1; seeded by 000110).
type Account string

// Accounts of the chart (000110).
const (
	AccountKoroBank          Account = "koro_bank"
	AccountPSPClearing       Account = "psp_clearing"
	AccountBuyerAdvances     Account = "buyer_advances"
	AccountSellerPayable     Account = "seller_payable"
	AccountRefundPayable     Account = "refund_payable"
	AccountVATOutput         Account = "vat_output"
	AccountVATInput          Account = "vat_input"
	AccountCommissionRevenue Account = "platform_commission_revenue"
	AccountSalesRevenue      Account = "platform_sales_revenue"
	AccountCostOfServices    Account = "cost_of_services"
	AccountPSPFeeExpense     Account = "psp_fee_expense"
	AccountBankFeeExpense    Account = "bank_fee_expense"
)

// Limits of a transaction.
const (
	maxEntries   = 1000 // one transaction: an order has tens of lines, four entries each
	maxMemoRunes = 500  // ledger_transactions_memo_chk
)

// AccountKind is ledger_accounts.kind.
type AccountKind string

// Account kinds.
const (
	Asset     AccountKind = "asset"
	Liability AccountKind = "liability"
	Revenue   AccountKind = "revenue"
	Expense   AccountKind = "expense"
)

// AccountInfo is one row of the chart of accounts.
type AccountInfo struct {
	Code     Account
	Kind     AccountKind
	PerPayee bool // every entry names a payee (and no other entry does)
}

// chart mirrors the seed of 000110; TestChart_MatchesSeed pins it against
// the database.
var chart = []AccountInfo{
	{AccountKoroBank, Asset, false},
	{AccountPSPClearing, Asset, false},
	{AccountBuyerAdvances, Liability, false},
	{AccountSellerPayable, Liability, true},
	{AccountRefundPayable, Liability, false},
	{AccountVATOutput, Liability, false},
	{AccountVATInput, Asset, false},
	{AccountCommissionRevenue, Revenue, false},
	{AccountSalesRevenue, Revenue, false},
	{AccountCostOfServices, Expense, false},
	{AccountPSPFeeExpense, Expense, false},
	{AccountBankFeeExpense, Expense, false},
}

// Chart returns the chart of accounts in seed order.
func Chart() []AccountInfo { return slices.Clone(chart) }

// accountInfo returns the chart row of a.
func accountInfo(a Account) (AccountInfo, bool) {
	i := slices.IndexFunc(chart, func(c AccountInfo) bool { return c.Code == a })
	if i < 0 {
		return AccountInfo{}, false
	}
	return chart[i], true
}

// Entry is one line of a transaction (01 §4.19): amount in kuruş, debit +,
// credit −. PayeeID is set exactly on per-payee accounts (seller_payable);
// OrderLineID ties an entry to the order line it is about (the database
// checks that a payee named next to a line is the line's own payee).
type Entry struct {
	Account     Account
	PayeeID     *uuid.UUID
	OrderLineID *uuid.UUID
	Amount      int64
}

// Txn is a balanced transaction to post (01 §4.19). (Kind, RefType, RefID)
// is its idempotency key: posting the same key again returns the first
// transaction.
type Txn struct {
	Kind    Kind
	RefType RefType
	RefID   uuid.UUID
	// OccurredAt is when the business event happened (the capture, the EFT);
	// zero means now.
	OccurredAt time.Time
	// Memo is an optional note without personal data (≤ 500 characters).
	Memo    string
	Entries []Entry
}

// Total returns the sum of the debits (the positive amounts) of t, the
// amount the transaction moves.
func (t Txn) Total() int64 {
	var s int64
	for _, e := range t.Entries {
		if e.Amount > 0 {
			s += e.Amount // validate bounds the sum
		}
	}
	return s
}

// invalid builds a 422 ledger.invalid_transaction naming the offending field.
func invalid(location, message string) error {
	return ErrInvalidTxn.WithFields(problem.FieldError{Location: location, Message: message})
}

// validate checks t before anything reaches the database (TestPost_
// Unbalanced_RejectedBeforeDB): known kind, ref and accounts, the payee
// rule in both directions, at least two non-zero entries and a zero sum
// computed with platform/money (overflow is an error, 06 H-6).
func (t Txn) validate() error {
	switch {
	case !t.Kind.Valid():
		return invalid("kind", "is not a known transaction kind")
	case !t.RefType.Valid():
		return invalid("ref_type", "is not a known reference type")
	case !slices.Contains(refTypesOf[t.Kind], t.RefType):
		return invalid("ref_type", fmt.Sprintf("does not fit kind %s", t.Kind))
	case t.RefID == uuid.Nil():
		return invalid("ref_id", "is required")
	case utf8.RuneCountInString(t.Memo) > maxMemoRunes:
		return invalid("memo", "must be at most 500 characters")
	case len(t.Entries) > maxEntries:
		return invalid("entries", fmt.Sprintf("must be at most %d", maxEntries))
	case len(t.Entries) < 2:
		return ErrUnbalanced.WithFields(problem.FieldError{Location: "entries", Message: "a transaction has at least two entries"})
	}
	amounts := make([]int64, len(t.Entries))
	for i, e := range t.Entries {
		loc := fmt.Sprintf("entries[%d]", i)
		info, ok := accountInfo(e.Account)
		switch {
		case !ok:
			return invalid(loc+".account", "is not an account of the chart")
		case info.PerPayee && (e.PayeeID == nil || *e.PayeeID == uuid.Nil()):
			return invalid(loc+".payee_id", "is required on a per-payee account")
		case !info.PerPayee && e.PayeeID != nil:
			return invalid(loc+".payee_id", "must be empty on an account without payees")
		case e.OrderLineID != nil && *e.OrderLineID == uuid.Nil():
			return invalid(loc+".order_line_id", "must be a valid ID when set")
		case e.Amount == 0:
			return invalid(loc+".amount", "must not be zero")
		case e.Amount == math.MinInt64:
			return invalid(loc+".amount", "is out of range")
		}
		amounts[i] = e.Amount
	}
	sum, err := money.Sum(amounts...)
	if err != nil {
		return invalid("entries", "the amounts overflow")
	}
	if sum != 0 {
		return ErrUnbalanced.WithFields(problem.FieldError{Location: "entries", Message: fmt.Sprintf("entries sum to %d, not zero", sum)})
	}
	// The debits alone must fit too (Total, balances).
	var debits []int64
	for _, a := range amounts {
		if a > 0 {
			debits = append(debits, a)
		}
	}
	if _, err := money.Sum(debits...); err != nil {
		return invalid("entries", "the amounts overflow")
	}
	return nil
}

// Transaction is one row of ledger_transactions (02 §21.2; append-only).
type Transaction struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Kind       Kind       `gorm:"column:kind"`
	RefType    RefType    `gorm:"column:ref_type"`
	RefID      uuid.UUID  `gorm:"type:uuid;column:ref_id"`
	OccurredAt time.Time  `gorm:"column:occurred_at"`
	Memo       *string    `gorm:"column:memo"`
	CreatedBy  *uuid.UUID `gorm:"type:uuid;column:created_by"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Transaction) TableName() string { return "ledger_transactions" }

// EntryRow is one row of ledger_entries (02 §21.3; append-only).
type EntryRow struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey"`
	TransactionID uuid.UUID  `gorm:"type:uuid;column:transaction_id"`
	Account       Account    `gorm:"column:account_code"`
	PayeeID       *uuid.UUID `gorm:"type:uuid;column:payee_id"`
	OrderLineID   *uuid.UUID `gorm:"type:uuid;column:order_line_id"`
	Amount        int64      `gorm:"column:amount"`
	Currency      string     `gorm:"column:currency"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (EntryRow) TableName() string { return "ledger_entries" }

// Entry returns the posted form of the row.
func (r EntryRow) Entry() Entry {
	return Entry{Account: r.Account, PayeeID: r.PayeeID, OrderLineID: r.OrderLineID, Amount: r.Amount}
}

// Posted is a stored transaction with its entries (in ID order).
type Posted struct {
	Transaction
	Entries []EntryRow
}

// Currency of every amount (ledger_entries_currency_chk).
const Currency = "TRY"

// sameEntries reports whether two entry lists are equal as multisets: an
// idempotent replay must post exactly what the first call posted.
func sameEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.SortFunc(x, compareEntries)
	slices.SortFunc(y, compareEntries)
	return slices.EqualFunc(x, y, func(p, q Entry) bool { return compareEntries(p, q) == 0 })
}

func compareEntries(p, q Entry) int {
	return cmp.Or(
		cmp.Compare(p.Account, q.Account),
		compareID(p.PayeeID, q.PayeeID),
		compareID(p.OrderLineID, q.OrderLineID),
		cmp.Compare(p.Amount, q.Amount),
	)
}

func compareID(a, b *uuid.UUID) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return bytes.Compare(a[:], b[:])
}

// payeeDebits returns, per payee, how much t reduces what Koro owes the
// payee (the positive net amount on per-payee accounts), in payee ID
// order. Credits and payees whose net is not a debit are left out.
func payeeDebits(t Txn) []payeeAmount {
	net := map[uuid.UUID]int64{}
	for _, e := range t.Entries {
		if info, _ := accountInfo(e.Account); info.PerPayee && e.PayeeID != nil {
			net[*e.PayeeID] += e.Amount // bounded by validate
		}
	}
	var out []payeeAmount
	for id, amt := range net {
		if amt > 0 {
			out = append(out, payeeAmount{PayeeID: id, Amount: amt})
		}
	}
	slices.SortFunc(out, func(a, b payeeAmount) int { return bytes.Compare(a.PayeeID[:], b.PayeeID[:]) })
	return out
}

// payeeAmount is an amount of one payee.
type payeeAmount struct {
	PayeeID uuid.UUID
	Amount  int64
}
