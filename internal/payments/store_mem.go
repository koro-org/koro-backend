package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
)

// MemStore is the in-memory Store of service, wiring and matrix tests (05
// §13.2). It mirrors the rules the contract tests pin on PostgreSQL: the
// status guard of the targeted updates, the frozen columns, the
// uniqueness of an open IBAN, of the default account, of a bank reference
// per account and of a transfer's payment, and the four-eyes CHECKs. It has
// no locks (db.NopTx) and no foreign keys.
type MemStore struct {
	collections map[uuid.UUID]ReceivableCollection
	mu          sync.Mutex
	now         func() time.Time
	accounts    map[uuid.UUID]CollectionAccount
	transfers   map[uuid.UUID]IncomingTransfer
	payments    map[uuid.UUID]Payment
	splits      []Split
	refunds     map[uuid.UUID]Refund
	requests    map[uuid.UUID]RefundRequest // BE-132
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store; now stamps created_at and
// updated_at (a caller's values are ignored, as by the column defaults and
// the touch triggers of gormStore: one clock, BE-069).
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, accounts: map[uuid.UUID]CollectionAccount{}, transfers: map[uuid.UUID]IncomingTransfer{},
		payments: map[uuid.UUID]Payment{}, refunds: map[uuid.UUID]Refund{}}
}

func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

// touch is the updated_at of a row changed now: never before its last
// stamp (a test clock set back).
func (m *MemStore) touch(prev time.Time) time.Time {
	if t := m.stamp(); t.After(prev) {
		return t
	}
	return prev
}

// ---- collection accounts

// InsertAccount implements Store.
func (m *MemStore) InsertAccount(_ context.Context, c CollectionAccount) (CollectionAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.accounts[c.ID]; dup {
		return CollectionAccount{}, fmt.Errorf("payments: insert account %s: duplicate id", c.ID)
	}
	if c.Status != AccountPending {
		return CollectionAccount{}, errors.New("payments: insert account: a new account is pending_approval (collection_accounts_initial_status_chk)")
	}
	if err := m.checkAccount(c); err != nil {
		return CollectionAccount{}, err
	}
	t := m.stamp()
	c.CreatedAt, c.UpdatedAt = t, t
	if c.PartyKind == "" {
		c.PartyKind = PartyKindPlatform
	}
	m.accounts[c.ID] = c
	return c, nil
}

// checkAccount applies the CHECKs and unique indexes of 000103 to c.
func (m *MemStore) checkAccount(c CollectionAccount) error {
	decided := c.Status != AccountPending
	switch {
	case c.ApprovedBy != nil && *c.ApprovedBy == c.CreatedBy:
		return authz.ErrFourEyes // collection_accounts_four_eyes_chk
	case decided != (c.ApprovedBy != nil) || decided != (c.ApprovedAt != nil):
		return fmt.Errorf("payments: account %s: decision columns do not match %s (collection_accounts_decided_chk)", c.ID, c.Status)
	case (c.Status == AccountRetired) != (c.RetiredAt != nil):
		return fmt.Errorf("payments: account %s: retired_at does not match %s (collection_accounts_retired_chk)", c.ID, c.Status)
	case c.IsDefault && c.Status != AccountActive:
		return fmt.Errorf("payments: account %s: only an active account is the default (collection_accounts_default_chk)", c.ID)
	}
	for _, x := range m.accounts {
		if x.ID == c.ID {
			continue
		}
		open := func(s AccountStatus) bool { return s == AccountPending || s == AccountActive }
		if open(x.Status) && open(c.Status) && x.IBAN == c.IBAN {
			return ErrIBANInUse
		}
		if x.IsDefault && x.Status == AccountActive && c.IsDefault && c.Status == AccountActive {
			return ErrInvalidState // collection_accounts_default_uq
		}
	}
	return nil
}

// GetAccount implements Store.
func (m *MemStore) GetAccount(_ context.Context, id uuid.UUID, _ bool) (CollectionAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.accounts[id]
	if !ok {
		return CollectionAccount{}, ErrNotFound
	}
	return c, nil
}

// ListAccounts implements Store.
func (m *MemStore) ListAccounts(context.Context) ([]CollectionAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CollectionAccount, 0, len(m.accounts))
	for _, c := range m.accounts {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b CollectionAccount) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return compareUUID(b.ID, a.ID)
	})
	return out, nil
}

// DefaultAccount implements Store.
func (m *MemStore) DefaultAccount(context.Context) (CollectionAccount, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.accounts {
		if c.IsDefault && c.Status == AccountActive {
			return c, true, nil
		}
	}
	return CollectionAccount{}, false, nil
}

// UpdateAccount implements Store: only the running columns change.
func (m *MemStore) UpdateAccount(_ context.Context, next CollectionAccount, from AccountStatus) (CollectionAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.accounts[next.ID]
	switch {
	case !ok:
		return CollectionAccount{}, ErrNotFound
	case cur.Status != from:
		return CollectionAccount{}, ErrInvalidState
	}
	row := cur
	row.Status, row.IsDefault, row.ApprovedBy, row.ApprovedAt, row.RetiredAt = next.Status, next.IsDefault, next.ApprovedBy, next.ApprovedAt, next.RetiredAt
	if err := m.checkAccount(row); err != nil {
		return CollectionAccount{}, err
	}
	row.UpdatedAt = m.touch(cur.UpdatedAt)
	m.accounts[row.ID] = row
	return row, nil
}

// ---- transfers

// InsertTransfer implements Store.
func (m *MemStore) InsertTransfer(_ context.Context, t IncomingTransfer) (IncomingTransfer, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.transfers[t.ID]; dup {
		return IncomingTransfer{}, false, fmt.Errorf("payments: insert transfer %s: duplicate id", t.ID)
	}
	if t.Status != TransferUnmatched {
		return IncomingTransfer{}, false, errors.New("payments: insert transfer: a new transfer is unmatched (incoming_bank_transfers_initial_status_chk)")
	}
	for _, x := range m.transfers {
		if x.CollectionAccountID == t.CollectionAccountID && x.BankReference == t.BankReference {
			return IncomingTransfer{}, false, nil
		}
	}
	ts := m.stamp()
	t.CreatedAt, t.UpdatedAt = ts, ts
	if t.Currency == "" {
		t.Currency = Currency
	}
	m.transfers[t.ID] = t
	return t, true, nil
}

// GetTransfer implements Store.
func (m *MemStore) GetTransfer(_ context.Context, id uuid.UUID, _ bool) (IncomingTransfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.transfers[id]
	if !ok {
		return IncomingTransfer{}, ErrNotFound
	}
	return t, nil
}

// ListTransfers implements Store.
func (m *MemStore) ListTransfers(_ context.Context, f TransferFilter, after *TransferCursor, limit int) ([]IncomingTransfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(f.Q)
	var out []IncomingTransfer
	for _, t := range m.transfers {
		switch {
		case f.Status != "" && t.Status != f.Status:
		case f.ValueFrom != nil && t.ValueDate.Before(*f.ValueFrom):
		case f.ValueTo != nil && t.ValueDate.After(*f.ValueTo):
		case q != "" && !matchesQ(t, q, f.QRef):
		case after != nil && transferOrder(t, after.ValueDate, after.ID) <= 0:
		default:
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b IncomingTransfer) int { return transferOrder(a, b.ValueDate, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// transferOrder compares t with the key (d, id) in list order: positive
// when t comes after the key (older value date, or the same date and a
// smaller id).
func transferOrder(t IncomingTransfer, d civil.Date, id uuid.UUID) int {
	if c := d.Compare(t.ValueDate); c != 0 {
		return c
	}
	return compareUUID(id, t.ID)
}

func matchesQ(t IncomingTransfer, q, ref string) bool {
	if ref != "" && t.ExtractedOrderNo != nil && *t.ExtractedOrderNo == ref {
		return true
	}
	if strings.Contains(strings.ToLower(t.Description), q) || strings.Contains(strings.ToLower(t.BankReference), q) {
		return true
	}
	return t.SenderName != nil && strings.Contains(strings.ToLower(*t.SenderName), q)
}

// UpdateTransfer implements Store: only the running columns change.
func (m *MemStore) UpdateTransfer(_ context.Context, next IncomingTransfer, from TransferStatus) (IncomingTransfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.transfers[next.ID]
	switch {
	case !ok:
		return IncomingTransfer{}, ErrNotFound
	case cur.Status != from:
		return IncomingTransfer{}, ErrInvalidState
	case cur.ReturnApprovedBy != nil && (!sameID(cur.ReturnRequestedBy, next.ReturnRequestedBy) ||
		!sameStr(cur.ReturnTargetIBAN, next.ReturnTargetIBAN) || !sameID(cur.ReturnApprovedBy, next.ReturnApprovedBy)):
		return IncomingTransfer{}, fmt.Errorf("payments: transfer %s: the approved return is frozen (incoming_bank_transfers_return_frozen)", cur.ID)
	case cur.Status == TransferReturned || cur.Status == TransferCollected:
		return IncomingTransfer{}, fmt.Errorf("payments: transfer %s: a returned transfer is final (incoming_bank_transfers_returned_frozen)", cur.ID)
	}
	row := cur
	row.Status, row.MatchedOrderID, row.MatchedBy, row.MatchedAt, row.IgnoreReason = next.Status, next.MatchedOrderID, next.MatchedBy, next.MatchedAt, next.IgnoreReason
	row.ReturnRequestedBy, row.ReturnRequestedAt, row.ReturnReason, row.ReturnTargetIBAN = next.ReturnRequestedBy, next.ReturnRequestedAt, next.ReturnReason, next.ReturnTargetIBAN
	row.ReturnApprovedBy, row.ReturnApprovedAt = next.ReturnApprovedBy, next.ReturnApprovedAt
	row.ReturnedAt, row.ReturnBankReference, row.ReturnReceiptFileID = next.ReturnedAt, next.ReturnBankReference, next.ReturnReceiptFileID
	if err := checkTransfer(row); err != nil {
		return IncomingTransfer{}, err
	}
	row.UpdatedAt = m.touch(cur.UpdatedAt)
	m.transfers[row.ID] = row
	return row, nil
}

// checkTransfer applies the per-state CHECKs of 000105.
func checkTransfer(t IncomingTransfer) error {
	is := func(ss ...TransferStatus) bool { return slices.Contains(ss, t.Status) }
	all := func(want bool, set ...bool) bool {
		for _, s := range set {
			if s != want {
				return false
			}
		}
		return true
	}
	matched := is(TransferMatched)
	requested := is(TransferReturnRequested, TransferReturnApproved, TransferReturned)
	approved := is(TransferReturnApproved, TransferReturned)
	returned := is(TransferReturned)
	switch {
	case t.ReturnApprovedBy != nil && t.ReturnRequestedBy != nil && *t.ReturnApprovedBy == *t.ReturnRequestedBy:
		return authz.ErrFourEyes // incoming_bank_transfers_return_four_eyes_chk
	case !all(matched, t.MatchedOrderID != nil, t.MatchedBy != nil, t.MatchedAt != nil):
		return fmt.Errorf("payments: transfer %s: incoming_bank_transfers_matched_chk", t.ID)
	case is(TransferIgnored) != (t.IgnoreReason != nil):
		return fmt.Errorf("payments: transfer %s: incoming_bank_transfers_ignored_chk", t.ID)
	case !all(requested, t.ReturnRequestedBy != nil, t.ReturnRequestedAt != nil, t.ReturnReason != nil, t.ReturnTargetIBAN != nil):
		return fmt.Errorf("payments: transfer %s: incoming_bank_transfers_return_request_chk", t.ID)
	case !all(approved, t.ReturnApprovedBy != nil, t.ReturnApprovedAt != nil):
		return fmt.Errorf("payments: transfer %s: incoming_bank_transfers_return_approval_chk", t.ID)
	case !all(returned, t.ReturnedAt != nil, t.ReturnBankReference != nil, t.ReturnReceiptFileID != nil):
		return fmt.Errorf("payments: transfer %s: incoming_bank_transfers_returned_chk", t.ID)
	}
	return nil
}

func sameID(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func sameStr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// UnmatchedBefore implements Store.
func (m *MemStore) UnmatchedBefore(_ context.Context, before time.Time) (UnmatchedSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s UnmatchedSummary
	for _, t := range m.transfers {
		if t.Status != TransferUnmatched || !t.CreatedAt.Before(before) {
			continue
		}
		if s.Count == 0 || t.CreatedAt.Before(s.OldestAt) {
			s.OldestAt = t.CreatedAt
		}
		s.Count++
	}
	return s, nil
}

// TransfersOfUser implements Store.
func (m *MemStore) TransfersOfUser(_ context.Context, userID uuid.UUID) ([]IncomingTransfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	var out []IncomingTransfer
	for _, t := range m.transfers {
		if t.EnteredBy == userID || is(t.MatchedBy) || is(t.ReturnRequestedBy) || is(t.ReturnApprovedBy) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b IncomingTransfer) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// ---- payments, splits, refunds

// InsertPayment implements Store.
func (m *MemStore) InsertPayment(_ context.Context, p Payment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.payments[p.ID]; dup {
		return fmt.Errorf("payments: insert payment %s: duplicate id", p.ID)
	}
	if (p.Provider == ProviderBankTransfer) != (p.IncomingTransferID != nil) {
		return fmt.Errorf("payments: insert payment %s: payments_transfer_chk", p.ID)
	}
	if p.IncomingTransferID != nil {
		for _, x := range m.payments {
			if x.IncomingTransferID != nil && *x.IncomingTransferID == *p.IncomingTransferID {
				return ErrTransferAlreadyMatched // payments_incoming_transfer_id_uq
			}
		}
		if t, ok := m.transfers[*p.IncomingTransferID]; !ok || t.MatchedOrderID == nil || *t.MatchedOrderID != p.OrderID {
			return fmt.Errorf("payments: insert payment %s: the transfer is not matched to the order "+
				"(payments_incoming_transfer_id_order_id_fkey)", p.ID)
		}
	}
	t := m.stamp()
	p.CreatedAt, p.UpdatedAt = t, t
	m.payments[p.ID] = p
	return nil
}

// GetPayment implements Store.
func (m *MemStore) GetPayment(_ context.Context, id uuid.UUID, _ bool) (Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.payments[id]
	if !ok {
		return Payment{}, ErrNotFound
	}
	return p, nil
}

// ListPayments implements Store.
func (m *MemStore) ListPayments(_ context.Context, f PaymentFilter, after *TimeCursor, limit int) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Payment
	for _, p := range m.payments {
		switch {
		case f.OrderID != nil && p.OrderID != *f.OrderID,
			f.Status != "" && p.Status != f.Status,
			after != nil && compareTime(p.CreatedAt, p.ID, after.CreatedAt, after.ID) >= 0:
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Payment) int { return compareTime(b.CreatedAt, b.ID, a.CreatedAt, a.ID) })
	return firstN(out, limit), nil
}

// PaymentsOfOrder implements Store.
func (m *MemStore) PaymentsOfOrder(_ context.Context, orderID uuid.UUID) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Payment
	for _, p := range m.payments {
		if p.OrderID == orderID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Payment) int { return compareTime(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return out, nil
}

// AddPaymentRefund implements Store.
func (m *MemStore) AddPaymentRefund(_ context.Context, paymentID uuid.UUID, amount int64) (Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.payments[paymentID]
	switch {
	case !ok:
		return Payment{}, ErrNotFound
	case p.Status != PaymentCaptured && p.Status != PaymentPartiallyRefunded:
		return Payment{}, ErrInvalidState
	case amount <= 0:
		return Payment{}, errors.New("payments: add payment refund: the amount must be positive")
	case p.RefundedAmount+amount > p.Amount:
		return Payment{}, ErrRefundExceedsPaid // payments_refunded_limit_chk
	}
	p.RefundedAmount += amount
	p.Status = PaymentPartiallyRefunded
	if p.RefundedAmount == p.Amount {
		p.Status = PaymentRefunded
	}
	p.UpdatedAt = m.touch(p.UpdatedAt)
	m.payments[paymentID] = p
	return p, nil
}

// PaymentsCapturedBy implements Store.
func (m *MemStore) PaymentsCapturedBy(_ context.Context, userID uuid.UUID) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Payment
	for _, p := range m.payments {
		if p.CapturedBy != nil && *p.CapturedBy == userID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Payment) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// InsertSplits implements Store.
func (m *MemStore) InsertSplits(_ context.Context, ss []Split) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range ss {
		p, ok := m.payments[s.PaymentID]
		switch {
		case !ok || p.OrderID != s.OrderID:
			return errors.New("payments: insert split: not of the payment's order (payment_splits_payment_id_order_id_fkey)")
		case s.SellerAmount < 0 || s.PlatformAmount < 0 || s.SellerAmount+s.PlatformAmount <= 0:
			return errors.New("payments: insert split: payment_splits_amount_chk")
		}
		for _, x := range m.splits {
			if x.PaymentID == s.PaymentID && x.OrderLineID == s.OrderLineID {
				return errors.New("payments: insert split: payment_splits_payment_line_uq")
			}
		}
	}
	t := m.stamp()
	for _, s := range ss {
		s.CreatedAt, s.UpdatedAt = t, t
		m.splits = append(m.splits, s)
	}
	return nil
}

// SplitsOf implements Store.
func (m *MemStore) SplitsOf(_ context.Context, paymentID uuid.UUID) ([]Split, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Split
	for _, s := range m.splits {
		if s.PaymentID == paymentID {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Split) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// InsertRefund implements Store.
func (m *MemStore) InsertRefund(_ context.Context, r Refund) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.payments[r.PaymentID]
	switch {
	case !ok || p.OrderID != r.OrderID:
		return errors.New("payments: insert refund: not of the payment's order (refunds_payment_id_order_id_fkey)")
	case r.Amount <= 0:
		return errors.New("payments: insert refund: refunds_amount_chk")
	case (r.Method == RefundMethodBankEFT) != (r.TargetIBAN != nil):
		return errors.New("payments: insert refund: refunds_target_chk")
	case r.Status != RefundStatusRequested:
		return errors.New("payments: insert refund: a new refund is requested (refunds_initial_status_chk)")
	}
	if err := checkRefund(r); err != nil {
		return err
	}
	t := m.stamp()
	r.CreatedAt, r.UpdatedAt = t, t
	m.refunds[r.ID] = r
	return nil
}

// GetRefund implements Store.
func (m *MemStore) GetRefund(_ context.Context, id uuid.UUID, _ bool) (Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refunds[id]
	if !ok {
		return Refund{}, ErrNotFound
	}
	return r, nil
}

// ListRefunds implements Store.
func (m *MemStore) ListRefunds(_ context.Context, f RefundFilter, after *TimeCursor, limit int) ([]Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Refund
	for _, r := range m.refunds {
		switch {
		case f.Status != "" && r.Status != f.Status,
			f.OrderID != nil && r.OrderID != *f.OrderID,
			f.PaymentID != nil && r.PaymentID != *f.PaymentID,
			after != nil && compareTime(r.CreatedAt, r.ID, after.CreatedAt, after.ID) >= 0:
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Refund) int { return compareTime(b.CreatedAt, b.ID, a.CreatedAt, a.ID) })
	return firstN(out, limit), nil
}

// RefundsOfOrder implements Store.
func (m *MemStore) RefundsOfOrder(_ context.Context, orderID uuid.UUID) ([]Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Refund
	for _, r := range m.refunds {
		if r.OrderID == orderID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Refund) int { return compareTime(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return out, nil
}

// UpdateRefund implements Store with the guards of 000108: the status
// guard, the frozen columns, the approver once set, a completed refund is
// final, one CHECK per state and the four eyes.
func (m *MemStore) UpdateRefund(_ context.Context, next Refund, from RefundStatus) (Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.refunds[next.ID]
	switch {
	case !ok:
		return Refund{}, ErrNotFound
	case cur.Status != from:
		return Refund{}, ErrInvalidState
	case cur.PaymentID != next.PaymentID || cur.OrderID != next.OrderID || !sameID(cur.OrderLineID, next.OrderLineID) ||
		cur.Amount != next.Amount || cur.Reason != next.Reason || cur.Method != next.Method ||
		!sameStr(cur.TargetIBAN, next.TargetIBAN) || cur.RequestedBy != next.RequestedBy:
		return Refund{}, fmt.Errorf("payments: refund %s: a frozen column changed (refunds_frozen)", next.ID)
	case cur.ApprovedBy != nil && (!sameID(cur.ApprovedBy, next.ApprovedBy) || !sameTime(cur.ApprovedAt, next.ApprovedAt)):
		return Refund{}, fmt.Errorf("payments: refund %s: the approval is frozen (refunds_approval_frozen)", next.ID)
	case cur.Status == RefundStatusFailed && next.Status != cur.Status:
		return Refund{}, ErrInvalidState
	case (next.Status == RefundStatusFailed || next.Status == RefundStatusCancelled) && next.Status != cur.Status && (cur.Status != RefundStatusApproved || next.ClosedBy == nil || next.ClosedAt == nil):
		return Refund{}, ErrInvalidState
	case cur.Status == RefundStatusCompleted || cur.ClosedAt != nil:
		return Refund{}, fmt.Errorf("payments: refund %s: a completed refund is final (refunds_completed_frozen)", next.ID)
	}
	if err := checkRefund(next); err != nil {
		return Refund{}, err
	}
	next.CreatedAt, next.UpdatedAt = cur.CreatedAt, m.touch(cur.UpdatedAt)
	m.refunds[next.ID] = next
	return next, nil
}

// checkRefund applies the per-state CHECKs of 000108 to r.
func checkRefund(r Refund) error {
	approved := r.Status == RefundStatusApproved || r.Status == RefundStatusProcessing || r.Status == RefundStatusCompleted ||
		r.Status == RefundStatusFailed || r.Status == RefundStatusCancelled
	rejected, completed := r.Status == RefundStatusRejected, r.Status == RefundStatusCompleted
	bankStates := r.Status == RefundStatusProcessing || completed || r.Status == RefundStatusFailed
	switch {
	case r.ApprovedBy != nil && *r.ApprovedBy == r.RequestedBy:
		return authz.ErrFourEyes // refunds_four_eyes_chk
	case approved != (r.ApprovedBy != nil) || approved != (r.ApprovedAt != nil):
		return fmt.Errorf("payments: refund %s: approval columns do not match %s (refunds_approved_chk)", r.ID, r.Status)
	case rejected != (r.RejectedBy != nil) || rejected != (r.RejectedAt != nil) || (r.RejectionReason != nil && !rejected):
		return fmt.Errorf("payments: refund %s: rejection columns do not match %s (refunds_rejected_chk)", r.ID, r.Status)
	case completed != (r.CompletedAt != nil) || completed != (r.ReceiptFileID != nil) || (completed && r.BankReference == nil) ||
		(r.BankReference != nil && !bankStates):
		return fmt.Errorf("payments: refund %s: completion columns do not match %s (refunds_completed_chk)", r.ID, r.Status)
	case r.ClosedBy != nil && *r.ClosedBy == r.RequestedBy:
		return authz.ErrFourEyes
	case (r.ClosedAt != nil) != (r.ClosedBy != nil), r.Status == RefundStatusCancelled && r.ClosedAt == nil,
		r.ClosedAt != nil && r.Status != RefundStatusCancelled && r.Status != RefundStatusFailed,
		(r.Status == RefundStatusCancelled) != (r.CancelReason != nil):
		return fmt.Errorf("payments: refund %s: closure columns do not match status", r.ID)
	case (r.Status == RefundStatusFailed) != (r.FailureReason != nil):
		return fmt.Errorf("payments: refund %s: failure_reason does not match %s (refunds_failed_chk)", r.ID, r.Status)
	}
	return nil
}

// RefundsOfUser implements Store.
func (m *MemStore) RefundsOfUser(_ context.Context, userID uuid.UUID) ([]Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	var out []Refund
	for _, r := range m.refunds {
		if r.RequestedBy == userID || is(r.ApprovedBy) || is(r.RejectedBy) || is(r.ClosedBy) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Refund) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// SeedRefund stores r as it is, any status (matrix fixtures and tests that
// need a refund past the request without going through the service); the
// store stamps created_at and updated_at, as on every insert.
func (m *MemStore) SeedRefund(r Refund) error {
	if err := checkRefund(r); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.refunds[r.ID]; dup {
		return fmt.Errorf("payments: seed refund %s: duplicate id", r.ID)
	}
	t := m.stamp()
	r.CreatedAt, r.UpdatedAt = t, t
	m.refunds[r.ID] = r
	return nil
}

// Refunds returns the refund requests (tests).
func (m *MemStore) Refunds() []Refund {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Refund, 0, len(m.refunds))
	for _, r := range m.refunds {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Refund) int { return compareUUID(a.ID, b.ID) })
	return out
}

// Payments returns every payment (tests).
func (m *MemStore) Payments() []Payment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Payment, 0, len(m.payments))
	for _, p := range m.payments {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Payment) int { return compareUUID(a.ID, b.ID) })
	return out
}

// compareUUID orders UUIDs by their bytes (PostgreSQL's uuid order).
func compareUUID(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) }

// compareTime orders (time, id) positions ascending.
func compareTime(at time.Time, a uuid.UUID, bt time.Time, b uuid.UUID) int {
	if c := at.Compare(bt); c != 0 {
		return c
	}
	return compareUUID(a, b)
}

// sameTime reports equal optional instants.
func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// firstN returns at most n rows.
func firstN[T any](rows []T, n int) []T {
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}
