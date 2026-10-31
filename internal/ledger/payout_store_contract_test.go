package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/ledger"
	"koro/internal/platform/db"
)

// payoutSeller is a payee with its seller org and approved account.
type payoutSeller struct {
	payee, org, account uuid.UUID
	iban, holder        string
}

// payoutWorld creates what a payout store needs: users, sellers with an
// approved account, fulfilled lines of a payee (seller net 1,080,000),
// receipts; markPaidOut sets order_lines.payout_id in ctx's transaction
// (orders' part of the mark-paid chain; nothing in memory); rejectFile
// takes a receipt out of use as the scan does (nothing in memory: the
// service checks the file).
type payoutWorld struct {
	user        func(t *testing.T) uuid.UUID
	seller      func(t *testing.T) payoutSeller
	line        func(t *testing.T, payee uuid.UUID) uuid.UUID
	receipt     func(t *testing.T) uuid.UUID
	rejectFile  func(t *testing.T, id uuid.UUID)
	markPaidOut func(ctx context.Context, payout uuid.UUID, lines []uuid.UUID) error
	tx          db.TxManager
}

func memPayoutWorld() payoutWorld {
	n := 0
	return payoutWorld{
		user: func(*testing.T) uuid.UUID { return uuid.NewV7() },
		seller: func(*testing.T) payoutSeller {
			n++
			return payoutSeller{payee: uuid.NewV7(), org: uuid.NewV7(), account: uuid.NewV7(),
				iban: testIBANs[n%len(testIBANs)], holder: "Örnek Vakfı"}
		},
		line:        func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		receipt:     func(*testing.T) uuid.UUID { return uuid.NewV7() },
		rejectFile:  func(*testing.T, uuid.UUID) {},
		markPaidOut: func(context.Context, uuid.UUID, []uuid.UUID) error { return nil },
		tx:          db.NopTx{},
	}
}

// stalePayout is a caller's timestamp the payout stores ignore: created_at
// and updated_at come from one clock, the database's (02 §0.1 [ts], BE-069).
var stalePayout = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// newPayout builds a pending payout of the lines (1,080,000 each).
func newPayout(s payoutSeller, creator uuid.UUID, at time.Time, lines ...uuid.UUID) (ledger.Payout, []ledger.PayoutItem) {
	p := ledger.Payout{ID: uuid.NewV7(), PayeeID: s.payee, OrgID: s.org, BankAccountID: s.account, IBANSnapshot: s.iban,
		HolderSnapshot: s.holder, Currency: ledger.Currency, Status: ledger.StatusPendingApproval, CreatedBy: creator,
		CreatedAt: stalePayout, UpdatedAt: stalePayout}
	items := make([]ledger.PayoutItem, len(lines))
	for i, l := range lines {
		items[i] = ledger.PayoutItem{PayoutID: p.ID, OrderLineID: l, PayeeID: s.payee, Amount: 1_080_000, CreatedAt: at}
		p.Amount += 1_080_000
	}
	return p, items
}

// payoutStoreContract is the contract both payout stores pass (05 §13.3).
func payoutStoreContract(t *testing.T, newStores func(t *testing.T) (ledger.PayoutStore, payoutWorld)) {
	in := func(t *testing.T, w payoutWorld, fn func(ctx context.Context) error) error {
		t.Helper()
		return ledger.TranslatePayout(w.tx.WithTx(t.Context(), fn))
	}
	insert := func(t *testing.T, s ledger.PayoutStore, w payoutWorld, p ledger.Payout, items []ledger.PayoutItem) error {
		t.Helper()
		return in(t, w, func(ctx context.Context) error {
			_, err := s.InsertPayout(ctx, p, items)
			return err
		})
	}
	transition := func(t *testing.T, s ledger.PayoutStore, w payoutWorld, next ledger.Payout, from ledger.PayoutStatus) (ledger.Payout, error) {
		t.Helper()
		var out ledger.Payout
		err := in(t, w, func(ctx context.Context) error {
			var err error
			out, err = s.TransitionPayout(ctx, next, from)
			return err
		})
		return out, err
	}
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	t.Run("lifecycle", func(t *testing.T) {
		t.Parallel()
		s, w := newStores(t)
		sel := w.seller(t)
		creator, approver := w.user(t), w.user(t)
		l1, l2 := w.line(t, sel.payee), w.line(t, sel.payee)
		p, items := newPayout(sel, creator, at, l1, l2)
		if err := insert(t, s, w, p, items); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got, err := s.GetPayout(t.Context(), p.ID, false)
		if err != nil || got.Status != ledger.StatusPendingApproval || got.Amount != 2_160_000 || got.IBANSnapshot != sel.iban ||
			got.OrgID != sel.org || got.CreatedBy != creator {
			t.Fatalf("get %+v, %v", got, err)
		}
		// The store stamps both columns on insert, at once (BE-069).
		if got.CreatedAt.IsZero() || got.CreatedAt.Equal(stalePayout) || !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Fatalf("insert stamps created_at %v, updated_at %v; want one store stamp", got.CreatedAt, got.UpdatedAt)
		}
		its, err := s.PayoutItems(t.Context(), p.ID)
		if err != nil || len(its) != 2 {
			t.Fatalf("items %+v, %v", its, err)
		}
		held, err := s.LinesInPayouts(t.Context(), []uuid.UUID{l1, l2, uuid.NewV7()})
		if err != nil || len(held) != 2 || held[l1] != p.ID {
			t.Fatalf("held %v, %v", held, err)
		}
		if open, err := s.HasOpenPayouts(t.Context(), sel.org); err != nil || !open {
			t.Fatalf("open %v, %v", open, err)
		}
		if a, err := s.PayoutAmounts(t.Context(), sel.payee); err != nil || a.Pending != 2_160_000 || a.PaidOut != 0 {
			t.Fatalf("amounts %+v, %v", a, err)
		}
		// four eyes in the store too (payouts_four_eyes_chk)
		self := got
		now := at.Add(time.Hour)
		self.Status, self.ApprovedBy, self.ApprovedAt = ledger.StatusApproved, &creator, &now
		if _, err := transition(t, s, w, self, ledger.StatusPendingApproval); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("approval by the creator: %v", err)
		}
		next := got
		next.Status, next.ApprovedBy, next.ApprovedAt = ledger.StatusApproved, &approver, &now
		if _, err := transition(t, s, w, next, ledger.StatusApproved); !errors.Is(err, ledger.ErrInvalidState) {
			t.Fatalf("transition from the wrong state: %v", err)
		}
		approved, err := transition(t, s, w, next, ledger.StatusPendingApproval)
		if err != nil || approved.Status != ledger.StatusApproved || *approved.ApprovedBy != approver ||
			!approved.CreatedAt.Equal(got.CreatedAt) || approved.UpdatedAt.Before(approved.CreatedAt) {
			t.Fatalf("approve %+v, %v", approved, err)
		}
		// paid: every state column at once, the lines marked in the same transaction
		ref, receipt, paidAt := "EFT-1", w.receipt(t), now.Add(time.Hour)
		paid := approved
		paid.Status, paid.PaidBy, paid.PaidAt, paid.BankReference, paid.ReceiptFileID = ledger.StatusPaid, &approver, &paidAt, &ref, &receipt
		err = in(t, w, func(ctx context.Context) error {
			if err := w.markPaidOut(ctx, p.ID, []uuid.UUID{l1, l2}); err != nil {
				return err
			}
			_, err := s.TransitionPayout(ctx, paid, ledger.StatusApproved)
			return err
		})
		if err != nil {
			t.Fatalf("paid: %v", err)
		}
		if a, _ := s.PayoutAmounts(t.Context(), sel.payee); a.Pending != 0 || a.PaidOut != 2_160_000 {
			t.Fatalf("amounts after paid %+v", a)
		}
		if open, _ := s.HasOpenPayouts(t.Context(), sel.org); open {
			t.Fatal("a paid payout is open")
		}
		by, err := s.PayoutsBy(t.Context(), approver)
		if err != nil || len(by) != 1 || by[0].ID != p.ID {
			t.Fatalf("payouts by the approver %+v, %v", by, err)
		}
		cancel := paid
		cancelledAt, why := paidAt, "late"
		cancel.Status, cancel.CancelledAt, cancel.CancelReason = ledger.StatusCancelled, &cancelledAt, &why
		if _, err := transition(t, s, w, cancel, ledger.StatusPaid); err == nil {
			t.Fatal("a paid payout was cancelled")
		}
		// The receipt of a paid payout is replaced only from the receipt it
		// holds (BE-154), once the scan rejected it.
		replace := func(cur, next uuid.UUID) (ledger.Payout, error) {
			var out ledger.Payout
			err := in(t, w, func(ctx context.Context) error {
				var err error
				out, err = s.ReplaceReceipt(ctx, p.ID, cur, next)
				return err
			})
			return out, err
		}
		fresh := w.receipt(t)
		if _, err := replace(uuid.NewV7(), fresh); !errors.Is(err, ledger.ErrInvalidState) {
			t.Fatalf("replace from another receipt: %v", err)
		}
		w.rejectFile(t, receipt)
		got, err = replace(receipt, fresh)
		if err != nil || got.ReceiptFileID == nil || *got.ReceiptFileID != fresh || got.Status != ledger.StatusPaid ||
			*got.BankReference != ref {
			t.Fatalf("replace %+v, %v", got, err)
		}
		if _, err := replace(receipt, w.receipt(t)); !errors.Is(err, ledger.ErrInvalidState) {
			t.Fatalf("replace of the replaced receipt: %v", err)
		}
		if _, err := s.ReplaceReceipt(t.Context(), uuid.NewV7(), receipt, fresh); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("replace on a missing payout: %v", err)
		}
	})

	t.Run("line in one live payout", func(t *testing.T) {
		t.Parallel()
		s, w := newStores(t)
		sel := w.seller(t)
		u := w.user(t)
		l := w.line(t, sel.payee)
		p, items := newPayout(sel, u, at, l)
		if err := insert(t, s, w, p, items); err != nil {
			t.Fatal(err)
		}
		q, qItems := newPayout(sel, u, at, l)
		if err := insert(t, s, w, q, qItems); !errors.Is(err, ledger.ErrLineNotPayable) {
			t.Fatalf("second payout of the line: %v", err)
		}
		// Cancelled: the items go, the line is free again.
		why, now := "wrong line", at.Add(time.Minute)
		c := p
		c.Status, c.CancelledAt, c.CancelReason = ledger.StatusCancelled, &now, &why
		err := in(t, w, func(ctx context.Context) error {
			if _, err := s.TransitionPayout(ctx, c, ledger.StatusPendingApproval); err != nil {
				return err
			}
			n, err := s.DeleteItems(ctx, p.ID)
			if err == nil && n != 1 {
				t.Errorf("deleted %d items", n)
			}
			return err
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if held, _ := s.LinesInPayouts(t.Context(), []uuid.UUID{l}); len(held) != 0 {
			t.Fatalf("held after the cancellation %v", held)
		}
		q, qItems = newPayout(sel, u, at.Add(time.Hour), l)
		if err := insert(t, s, w, q, qItems); err != nil {
			t.Fatalf("payout of the released line: %v", err)
		}
		// The items of a live payout are never deleted.
		if err := in(t, w, func(ctx context.Context) error { _, err := s.DeleteItems(ctx, q.ID); return err }); err == nil {
			t.Fatal("deleted the items of a pending payout")
		}
	})

	t.Run("items sum to the amount", func(t *testing.T) {
		t.Parallel()
		s, w := newStores(t)
		sel := w.seller(t)
		p, items := newPayout(sel, w.user(t), at, w.line(t, sel.payee))
		p.Amount++
		if err := insert(t, s, w, p, items); !errors.Is(err, ledger.ErrInvalidTxn) {
			t.Fatalf("items not summing to the amount: %v", err)
		}
		if _, err := s.GetPayout(t.Context(), p.ID, false); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("the payout stayed: %v", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		s, w := newStores(t)
		a, b := w.seller(t), w.seller(t)
		u := w.user(t)
		var ids []uuid.UUID
		for i := range 3 {
			p, items := newPayout(a, u, at.Add(time.Duration(i)*time.Minute), w.line(t, a.payee))
			if err := insert(t, s, w, p, items); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, p.ID)
		}
		pb, itemsB := newPayout(b, u, at, w.line(t, b.payee))
		if err := insert(t, s, w, pb, itemsB); err != nil {
			t.Fatal(err)
		}
		page1, err := s.ListPayouts(t.Context(), ledger.PayoutFilter{OrgID: &a.org}, nil, 2)
		if err != nil || len(page1) != 2 || page1[0].ID != ids[2] || page1[1].ID != ids[1] {
			t.Fatalf("page 1 %v, %v", page1, err)
		}
		last := page1[1]
		page2, err := s.ListPayouts(t.Context(), ledger.PayoutFilter{OrgID: &a.org},
			&ledger.PayoutCursor{CreatedAt: last.CreatedAt, ID: last.ID}, 2)
		if err != nil || len(page2) != 1 || page2[0].ID != ids[0] {
			t.Fatalf("page 2 %v, %v", page2, err)
		}
		pending, err := s.ListPayouts(t.Context(), ledger.PayoutFilter{PayeeID: &b.payee, Status: ledger.StatusPendingApproval}, nil, 10)
		if err != nil || len(pending) != 1 || pending[0].ID != pb.ID {
			t.Fatalf("by payee and status %v, %v", pending, err)
		}
		none, err := s.ListPayouts(t.Context(), ledger.PayoutFilter{OrgID: &a.org, Status: ledger.StatusPaid}, nil, 10)
		if err != nil || len(none) != 0 {
			t.Fatalf("paid %v, %v", none, err)
		}
	})
}

func TestPayoutStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	payoutStoreContract(t, func(*testing.T) (ledger.PayoutStore, payoutWorld) {
		return ledger.NewMemStore(func() time.Time { return t0 }), memPayoutWorld()
	})
}
