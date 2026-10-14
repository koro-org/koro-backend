package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/platform/db"
)

// world creates the parent rows a store needs: users, payees and order
// lines of a payee (FKs in PostgreSQL; plain IDs in memory), and runs fn in
// a transaction of the store's database (InsertTxn needs one).
type world struct {
	user  func(t *testing.T) uuid.UUID
	payee func(t *testing.T) uuid.UUID
	line  func(t *testing.T, payee uuid.UUID) uuid.UUID
	tx    db.TxManager
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{user: id, payee: id, line: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() }, tx: db.NopTx{}}
}

// row builds the header and entries of a transaction for InsertTxn.
func row(kind ledger.Kind, ref ledger.RefType, at time.Time, es ...ledger.Entry) (ledger.Transaction, []ledger.EntryRow) {
	h := ledger.Transaction{ID: uuid.NewV7(), Kind: kind, RefType: ref, RefID: uuid.NewV7(), OccurredAt: at, CreatedAt: at}
	rows := make([]ledger.EntryRow, len(es))
	for i, e := range es {
		rows[i] = ledger.EntryRow{ID: uuid.NewV7(), TransactionID: h.ID, Account: e.Account, PayeeID: e.PayeeID,
			OrderLineID: e.OrderLineID, Amount: e.Amount, Currency: ledger.Currency, CreatedAt: at}
	}
	return h, rows
}

// insert writes one transaction in its own database transaction.
func insert(t *testing.T, s ledger.Store, w world, h ledger.Transaction, rows []ledger.EntryRow) (bool, error) {
	t.Helper()
	var ok bool
	err := w.tx.WithTx(t.Context(), func(ctx context.Context) error {
		var err error
		ok, err = s.InsertTxn(ctx, h, rows)
		return err
	})
	return ok, ledger.TranslateError(err)
}

func mustInsert(t *testing.T, s ledger.Store, w world, h ledger.Transaction, rows []ledger.EntryRow) {
	t.Helper()
	if ok, err := insert(t, s, w, h, rows); err != nil || !ok {
		t.Fatalf("InsertTxn %s: %v, %v", h.Kind, ok, err)
	}
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (ledger.Store, world)) {
	t.Helper()

	t.Run("insert once, read by ref and ID", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		payee := w.payee(t)
		ln := w.line(t, payee)
		user := w.user(t)
		h, rows := row(ledger.KindOrderAllocated, ledger.RefOrder, t0,
			ledger.Entry{Account: ledger.AccountBuyerAdvances, OrderLineID: &ln, Amount: 1_200_000},
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &payee, OrderLineID: &ln, Amount: -1_080_000},
			ledger.Entry{Account: ledger.AccountCommissionRevenue, OrderLineID: &ln, Amount: -100_000},
			ledger.Entry{Account: ledger.AccountVATOutput, OrderLineID: &ln, Amount: -20_000},
		)
		memo := "Sipariş KR00000001"
		h.Memo, h.CreatedBy = &memo, &user
		mustInsert(t, s, w, h, rows)

		for name, get := range map[string]func() (ledger.Posted, error){
			"ByRef": func() (ledger.Posted, error) { return s.ByRef(t.Context(), h.Kind, h.RefType, h.RefID) },
			"ByID":  func() (ledger.Posted, error) { return s.ByID(t.Context(), h.ID) },
		} {
			p, err := get()
			if err != nil || p.ID != h.ID || p.Kind != h.Kind || p.RefType != h.RefType || p.RefID != h.RefID ||
				!p.OccurredAt.Equal(t0) || p.Memo == nil || *p.Memo != memo || p.CreatedBy == nil || *p.CreatedBy != user ||
				p.CreatedAt.IsZero() || len(p.Entries) != 4 {
				t.Fatalf("%s = %+v, %v", name, p, err)
			}
			for i, e := range p.Entries {
				want := rows[i]
				if e.ID != want.ID || e.TransactionID != h.ID || e.Account != want.Account || e.Amount != want.Amount ||
					e.Currency != "TRY" || *e.OrderLineID != ln || (e.PayeeID != nil) != (want.PayeeID != nil) ||
					!e.CreatedAt.Equal(p.CreatedAt) {
					t.Fatalf("%s entry %d = %+v, want %+v", name, i, e, want)
				}
			}
		}

		// The same business key again writes nothing.
		dup, dupRows := row(h.Kind, h.RefType, t0,
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: 1}, ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -1})
		dup.RefID = h.RefID
		for i := range dupRows {
			dupRows[i].TransactionID = dup.ID
		}
		if ok, err := insert(t, s, w, dup, dupRows); err != nil || ok {
			t.Fatalf("duplicate key: inserted %v, %v", ok, err)
		}
		if p, _ := s.ByRef(t.Context(), h.Kind, h.RefType, h.RefID); p.ID != h.ID || len(p.Entries) != 4 {
			t.Fatalf("duplicate changed the transaction: %+v", p)
		}
		if _, err := s.ByID(t.Context(), dup.ID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("ByID of the duplicate: %v", err)
		}
		if _, err := s.ByRef(t.Context(), ledger.KindPayoutPaid, ledger.RefPayout, h.RefID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("ByRef of another kind: %v", err)
		}
	})

	t.Run("the store refuses what the trigger refuses", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		payee := w.payee(t)
		for _, c := range []struct {
			name string
			es   []ledger.Entry
			want error
		}{
			{"unbalanced", []ledger.Entry{{Account: ledger.AccountKoroBank, Amount: 100}, {Account: ledger.AccountBuyerAdvances, Amount: -99}}, ledger.ErrUnbalanced},
			{"one entry", []ledger.Entry{{Account: ledger.AccountKoroBank, Amount: 100}}, ledger.ErrUnbalanced},
			{"per-payee account without payee", []ledger.Entry{{Account: ledger.AccountSellerPayable, Amount: 100}, {Account: ledger.AccountKoroBank, Amount: -100}}, ledger.ErrInvalidTxn},
			{"payee on another account", []ledger.Entry{{Account: ledger.AccountKoroBank, PayeeID: &payee, Amount: 100}, {Account: ledger.AccountBuyerAdvances, Amount: -100}}, ledger.ErrInvalidTxn},
		} {
			h, rows := row(ledger.KindAdjustment, ledger.RefManual, t0, c.es...)
			if ok, err := insert(t, s, w, h, rows); !errors.Is(err, c.want) || ok {
				t.Errorf("%s: inserted %v, %v; want %v", c.name, ok, err, c.want)
			}
			if _, err := s.ByID(t.Context(), h.ID); !errors.Is(err, ledger.ErrNotFound) {
				t.Errorf("%s: the rejected transaction was stored (%v)", c.name, err)
			}
		}
	})

	t.Run("payee balance", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		p1, p2 := w.payee(t), w.payee(t)
		credit := func(p uuid.UUID, amount int64) {
			h, rows := row(ledger.KindAdjustment, ledger.RefManual, t0,
				ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: amount},
				ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p, Amount: -amount})
			mustInsert(t, s, w, h, rows)
		}
		credit(p1, 1_080_000)
		credit(p1, 20_000)
		credit(p2, 5)
		h, rows := row(ledger.KindPayoutPaid, ledger.RefPayout, t0,
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p1, Amount: 100_000},
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: -100_000})
		mustInsert(t, s, w, h, rows)
		for p, want := range map[uuid.UUID]int64{p1: 1_000_000, p2: 5, uuid.NewV7(): 0} {
			if b, err := s.PayeeBalance(t.Context(), p); err != nil || b != want {
				t.Errorf("PayeeBalance = %d, %v; want %d", b, err, want)
			}
		}
		if err := w.tx.WithTx(t.Context(), func(ctx context.Context) error {
			return s.LockPayees(ctx, []uuid.UUID{p1, p2})
		}); err != nil {
			t.Fatalf("LockPayees: %v", err)
		}
		if neg, err := s.NegativePayeeBalances(t.Context(), 10); err != nil || len(neg) != 0 {
			t.Fatalf("NegativePayeeBalances = %+v, %v", neg, err)
		}
		// A debit without a credit (written around Post) is negative.
		h, rows = row(ledger.KindAdjustment, ledger.RefManual, t0,
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p2, Amount: 7},
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: -7})
		mustInsert(t, s, w, h, rows)
		neg, err := s.NegativePayeeBalances(t.Context(), 10)
		if err != nil || len(neg) != 1 || neg[0].PayeeID != p2 || neg[0].Balance != -2 {
			t.Fatalf("NegativePayeeBalances = %+v, %v", neg, err)
		}
		if bad, err := s.Unbalanced(t.Context(), 10); err != nil || len(bad) != 0 {
			t.Fatalf("Unbalanced = %+v, %v", bad, err)
		}
		if late, err := s.LateEntries(t.Context(), 10); err != nil || len(late) != 0 {
			t.Fatalf("LateEntries = %+v, %v", late, err)
		}
	})

	t.Run("refunded shares of a line", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		p := w.payee(t)
		l1, l2 := w.line(t, p), w.line(t, p)
		// The allocation is not a refund: it does not count.
		h, rows := row(ledger.KindOrderAllocated, ledger.RefOrder, t0,
			ledger.Entry{Account: ledger.AccountBuyerAdvances, OrderLineID: &l1, Amount: 1_200_000},
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p, OrderLineID: &l1, Amount: -1_080_000},
			ledger.Entry{Account: ledger.AccountCommissionRevenue, OrderLineID: &l1, Amount: -100_000},
			ledger.Entry{Account: ledger.AccountVATOutput, OrderLineID: &l1, Amount: -20_000})
		mustInsert(t, s, w, h, rows)
		// A partial refund from the payable, a later one claimed back from
		// the payee after a payout (06 H-59), one of another line.
		h, rows = row(ledger.KindRefundCompleted, ledger.RefRefund, t0,
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p, OrderLineID: &l1, Amount: 540_000},
			ledger.Entry{Account: ledger.AccountCommissionRevenue, OrderLineID: &l1, Amount: 50_000},
			ledger.Entry{Account: ledger.AccountVATOutput, OrderLineID: &l1, Amount: 10_000},
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: -600_000})
		mustInsert(t, s, w, h, rows)
		h, rows = row(ledger.KindRefundCompleted, ledger.RefRefund, t0,
			ledger.Entry{Account: ledger.AccountPayeeReceivable, OrderLineID: &l1, Amount: 1},
			ledger.Entry{Account: ledger.AccountCommissionRevenue, OrderLineID: &l1, Amount: 2},
			ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: 7},
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: -10})
		mustInsert(t, s, w, h, rows)
		h, rows = row(ledger.KindRefundCompleted, ledger.RefRefund, t0,
			ledger.Entry{Account: ledger.AccountSellerPayable, PayeeID: &p, OrderLineID: &l2, Amount: 3},
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: -3})
		mustInsert(t, s, w, h, rows)
		got, err := s.RefundedShares(t.Context(), l1)
		if err != nil || got != (ledger.LineShares{Seller: 540_001, Commission: 50_002, CommissionVAT: 10_000}) || got.Total() != 600_003 {
			t.Fatalf("RefundedShares(l1) = %+v, %v", got, err)
		}
		if got, err := s.RefundedShares(t.Context(), w.line(t, p)); err != nil || got != (ledger.LineShares{}) {
			t.Fatalf("RefundedShares(new line) = %+v, %v", got, err)
		}
	})

	t.Run("journal list", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		var ids []uuid.UUID
		for i := range 4 {
			kind, ref := ledger.KindPaymentReceived, ledger.RefPayment
			if i == 3 {
				kind, ref = ledger.KindFee, ledger.RefManual
			}
			h, rows := row(kind, ref, t0.Add(time.Duration(i)*time.Minute),
				ledger.Entry{Account: ledger.AccountKoroBank, Amount: 10}, ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -10})
			mustInsert(t, s, w, h, rows)
			ids = append(ids, h.ID)
		}
		// Two transactions at the same instant are ordered by ID.
		h, rows := row(ledger.KindPaymentReceived, ledger.RefPayment, t0.Add(2*time.Minute),
			ledger.Entry{Account: ledger.AccountKoroBank, Amount: 10}, ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -10})
		mustInsert(t, s, w, h, rows)
		want := []uuid.UUID{ids[3], h.ID, ids[2], ids[1], ids[0]}

		var got []uuid.UUID
		var after *ledger.JournalCursor
		for range 10 {
			page, err := s.List(t.Context(), ledger.JournalFilter{}, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page {
				if len(p.Entries) != 2 {
					t.Fatalf("%s has %d entries", p.ID, len(p.Entries))
				}
				got = append(got, p.ID)
			}
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			after = &ledger.JournalCursor{OccurredAt: last.OccurredAt, ID: last.ID}
		}
		if len(got) != len(want) {
			t.Fatalf("pages %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order %v, want %v", got, want)
			}
		}
		from, to := t0.Add(time.Minute), t0.Add(3*time.Minute)
		for _, c := range []struct {
			name string
			f    ledger.JournalFilter
			want int
		}{
			{"kind", ledger.JournalFilter{Kind: ledger.KindFee}, 1},
			{"ref type", ledger.JournalFilter{RefType: ledger.RefPayment}, 4},
			{"ref", ledger.JournalFilter{RefID: &h.RefID}, 1},
			{"window [from, to)", ledger.JournalFilter{From: &from, To: &to}, 3},
		} {
			page, err := s.List(t.Context(), c.f, nil, 100)
			if err != nil || len(page) != c.want {
				t.Errorf("%s: %d, %v; want %d", c.name, len(page), err, c.want)
			}
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (ledger.Store, world) {
		return ledger.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}
