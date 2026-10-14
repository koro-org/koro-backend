package ledger_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// spyTx counts the transactions a service opens.
type spyTx struct{ n atomic.Int64 }

func (s *spyTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	s.n.Add(1)
	return db.NopTx{}.WithTx(ctx, fn)
}

func (s *spyTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	s.n.Add(1)
	return db.NopTx{}.WithSerializableTx(ctx, fn)
}

// noStore is a Store that fails the test on any call.
type noStore struct {
	ledger.Store // nil: every method not listed panics
	t            *testing.T
}

func (s noStore) InsertTxn(context.Context, ledger.Transaction, []ledger.EntryRow) (bool, error) {
	s.t.Error("InsertTxn reached the store")
	return false, nil
}

func (s noStore) ByRef(context.Context, ledger.Kind, ledger.RefType, uuid.UUID) (ledger.Posted, error) {
	s.t.Error("ByRef reached the store")
	return ledger.Posted{}, ledger.ErrNotFound
}

func (s noStore) LockPayees(context.Context, []uuid.UUID) error {
	s.t.Error("LockPayees reached the store")
	return nil
}

func (s noStore) PayeeBalance(context.Context, uuid.UUID) (int64, error) {
	s.t.Error("PayeeBalance reached the store")
	return 0, nil
}

// TestPost_Unbalanced_RejectedBeforeDB: an unbalanced or malformed Txn is
// rejected by the Go check before a transaction is opened or the store is
// touched (T-44: the trigger is the second line, not the first).
func TestPost_Unbalanced_RejectedBeforeDB(t *testing.T) {
	t.Parallel()
	payee, ref := uuid.NewV7(), uuid.NewV7()
	ok := func(mut func(*ledger.Txn)) ledger.Txn {
		tx := ledger.Txn{Kind: ledger.KindPaymentReceived, RefType: ledger.RefPayment, RefID: ref, Entries: []ledger.Entry{
			{Account: ledger.AccountKoroBank, Amount: 100}, {Account: ledger.AccountBuyerAdvances, Amount: -100},
		}}
		mut(&tx)
		return tx
	}
	for _, c := range []struct {
		name string
		tx   ledger.Txn
		want *problem.Error
		loc  string
	}{
		{"sum is one kuruş off", ok(func(tx *ledger.Txn) { tx.Entries[1].Amount = -99 }), ledger.ErrUnbalanced, "entries"},
		{"sum is positive", ok(func(tx *ledger.Txn) {
			tx.Entries = append(tx.Entries, ledger.Entry{Account: ledger.AccountKoroBank, Amount: 1})
		}), ledger.ErrUnbalanced, "entries"},
		{"one entry", ok(func(tx *ledger.Txn) { tx.Entries = tx.Entries[:1] }), ledger.ErrUnbalanced, "entries"},
		{"no entries", ok(func(tx *ledger.Txn) { tx.Entries = nil }), ledger.ErrUnbalanced, "entries"},
		{"zero amounts", ok(func(tx *ledger.Txn) { tx.Entries[0].Amount, tx.Entries[1].Amount = 0, 0 }), ledger.ErrInvalidTxn, "entries[0].amount"},
		{"MinInt64", ok(func(tx *ledger.Txn) { tx.Entries[0].Amount = math.MinInt64 }), ledger.ErrInvalidTxn, "entries[0].amount"},
		{"overflow", ok(func(tx *ledger.Txn) {
			tx.Entries = []ledger.Entry{
				{Account: ledger.AccountKoroBank, Amount: math.MaxInt64}, {Account: ledger.AccountKoroBank, Amount: 1},
				{Account: ledger.AccountBuyerAdvances, Amount: -math.MaxInt64}, {Account: ledger.AccountBuyerAdvances, Amount: -1},
			}
		}), ledger.ErrInvalidTxn, "entries"},
		{"unknown kind", ok(func(tx *ledger.Txn) { tx.Kind = "gift" }), ledger.ErrInvalidTxn, "kind"},
		{"unknown ref type", ok(func(tx *ledger.Txn) { tx.RefType = "invoice" }), ledger.ErrInvalidTxn, "ref_type"},
		{"ref type does not fit the kind", ok(func(tx *ledger.Txn) { tx.RefType = ledger.RefOrder }), ledger.ErrInvalidTxn, "ref_type"},
		{"adjustment must be manual", ok(func(tx *ledger.Txn) { tx.Kind = ledger.KindAdjustment }), ledger.ErrInvalidTxn, "ref_type"},
		{"nil ref", ok(func(tx *ledger.Txn) { tx.RefID = uuid.Nil() }), ledger.ErrInvalidTxn, "ref_id"},
		{"unknown account", ok(func(tx *ledger.Txn) { tx.Entries[0].Account = "petty_cash" }), ledger.ErrInvalidTxn, "entries[0].account"},
		{"seller_payable without payee", ok(func(tx *ledger.Txn) { tx.Entries[1].Account = ledger.AccountSellerPayable }), ledger.ErrInvalidTxn, "entries[1].payee_id"},
		{"seller_payable with nil payee", ok(func(tx *ledger.Txn) {
			tx.Entries[1].Account, tx.Entries[1].PayeeID = ledger.AccountSellerPayable, ptr(uuid.Nil())
		}), ledger.ErrInvalidTxn, "entries[1].payee_id"},
		{"payee on an account without payees", ok(func(tx *ledger.Txn) { tx.Entries[0].PayeeID = &payee }), ledger.ErrInvalidTxn, "entries[0].payee_id"},
		{"nil order line", ok(func(tx *ledger.Txn) { tx.Entries[0].OrderLineID = ptr(uuid.Nil()) }), ledger.ErrInvalidTxn, "entries[0].order_line_id"},
		{"memo too long", ok(func(tx *ledger.Txn) { tx.Memo = strings.Repeat("ş", 501) }), ledger.ErrInvalidTxn, "memo"},
		{"too many entries", ok(func(tx *ledger.Txn) {
			tx.Entries = nil
			for range 501 {
				tx.Entries = append(tx.Entries, ledger.Entry{Account: ledger.AccountKoroBank, Amount: 1}, ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -1})
			}
		}), ledger.ErrInvalidTxn, "entries"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tx := &spyTx{}
			mod, err := ledger.New(ledger.Deps{Store: noStore{t: t}, Tx: tx, Clock: clock.NewFake(t0), Log: obs.Discard()})
			if err != nil {
				t.Fatal(err)
			}
			id, err := mod.Service.Post(t.Context(), c.tx)
			wantProblem(t, err, c.want)
			if id != uuid.Nil() {
				t.Fatalf("Post returned ID %s with an error", id)
			}
			var pe *problem.Error
			if !errors.As(err, &pe) || len(pe.Fields()) != 1 || pe.Fields()[0].Location != c.loc {
				t.Fatalf("fields of %v, want location %q", err, c.loc)
			}
			if pe.Status() != 422 {
				t.Fatalf("status %d, want 422", pe.Status())
			}
			if n := tx.n.Load(); n != 0 {
				t.Fatalf("%d transactions opened before the check", n)
			}
		})
	}
	// A 500-character memo of multi-byte letters is fine (runes, not bytes).
	e := newEnv(t)
	e.post(t, ok(func(tx *ledger.Txn) { tx.Memo = strings.Repeat("ş", 500) }))
}

// TestPost_Replay_ReturnsFirstTransaction: the business key (kind,
// ref_type, ref_id) makes Post idempotent; other entries for the same event
// are a conflict, never a second transaction.
func TestPost_Replay_ReturnsFirstTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payment := uuid.NewV7()
	tx := txn(t)(ledger.PaymentReceived(ledger.ModelA, payment, 1_200_000, t0))
	id := e.post(t, tx)

	// A retry, with the entries in another order and another memo/time.
	again := tx
	again.Entries = []ledger.Entry{tx.Entries[1], tx.Entries[0]}
	again.Memo, again.OccurredAt = "retry", t0.Add(time.Hour)
	if got := e.post(t, again); got != id {
		t.Fatalf("replay = %s, want %s", got, id)
	}
	// Other entries for the same payment.
	other := txn(t)(ledger.PaymentReceived(ledger.ModelA, payment, 1_200_001, t0))
	if _, err := e.svc.Post(t.Context(), other); !errors.Is(err, ledger.ErrRefConflict) {
		t.Fatalf("other entries: %v, want ledger.ref_conflict", err)
	}
	// The same ref under another kind is another event.
	fee := ledger.Txn{Kind: ledger.KindFee, RefType: ledger.RefPayment, RefID: payment, Entries: []ledger.Entry{
		{Account: ledger.AccountBankFeeExpense, Amount: 500}, {Account: ledger.AccountKoroBank, Amount: -500},
	}}
	if got := e.post(t, fee); got == id {
		t.Fatal("a fee of the payment reused the payment's transaction")
	}
	page, err := e.svc.ListTransactions(t.Context(), actor(t, "finance_admin"), ledger.JournalQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("journal = %d items, %v; want 2", len(page.Items), err)
	}
}

// TestPost_StampsHeader: created_by is the HTTP actor (nil for workers),
// occurred_at defaults to the clock and the memo is kept.
func TestPost_StampsHeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	tx := txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 100, time.Time{}))
	tx.Memo = "Havale KR00000001"
	id, err := e.svc.Post(authz.WithActor(t.Context(), fin), tx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.store.ByID(t.Context(), id)
	if err != nil || p.CreatedBy == nil || *p.CreatedBy != fin.UserID || !p.OccurredAt.Equal(t0) ||
		p.Memo == nil || *p.Memo != tx.Memo || len(p.Entries) != 2 {
		t.Fatalf("stored %+v, %v", p, err)
	}
	worker := e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 100, t0.Add(-time.Hour))))
	if p, _ := e.store.ByID(t.Context(), worker); p.CreatedBy != nil || !p.OccurredAt.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("worker post %+v", p)
	}
}

// TestPost_NegativePayeeBalance_Rejected: a payee is never owed less than
// nothing (02 §21; ledger.integrity_check re-checks it).
func TestPost_NegativePayeeBalance_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payee := uuid.NewV7()
	e.paidOrder(t, line(payee)) // payee is owed 1,080,000

	over := txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 1_080_001, t0))
	_, err := e.svc.Post(t.Context(), over)
	wantProblem(t, err, ledger.ErrNegativeBalance)
	if pe, _ := errors.AsType[*problem.Error](err); pe.Status() != 409 {
		t.Fatalf("status %d, want 409", pe.Status())
	}
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("balance after the rejected payout = %d", b)
	}
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 1_000_000, t0)))
	last := txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 80_000, t0))
	lastID := e.post(t, last)
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after paying out everything = %d", b)
	}
	// A replay of the last payout is a replay, not a second debit, even at
	// balance 0 (the key is looked up before the balance).
	if got := e.post(t, last); got != lastID {
		t.Fatalf("replay at balance 0 = %s, want %s", got, lastID)
	}
	// Any further debit overdraws: a payout, or a refund that takes the
	// payee's share back after the payout (BE-043 decides that case).
	_, err = e.svc.Post(t.Context(), txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 1, t0)))
	wantProblem(t, err, ledger.ErrNegativeBalance)
	refund := txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{split(t)(ledger.SplitRefund(line(payee), 1))}, t0))
	if _, err := e.svc.Post(t.Context(), refund); !errors.Is(err, ledger.ErrNegativeBalance) {
		t.Fatalf("refund after payout: %v", err)
	}
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after the rejected debits = %d", b)
	}
}

// TestReverse_PostsNegatedAdjustment: a correction is a new transaction
// with every entry negated (append-only); reversing twice returns the
// first reversal, and a reversal may not overdraw a payee.
func TestReverse_PostsNegatedAdjustment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payee := uuid.NewV7()
	l := line(payee)
	order := e.paidOrder(t, l)
	alloc, err := e.store.ByRef(t.Context(), ledger.KindOrderAllocated, ledger.RefOrder, order)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := e.svc.Reverse(t.Context(), alloc.ID, "yanlış tahsis")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.store.ByID(t.Context(), rev)
	if err != nil || p.Kind != ledger.KindAdjustment || p.RefType != ledger.RefManual || p.RefID != alloc.ID ||
		len(p.Entries) != len(alloc.Entries) || p.Memo == nil || *p.Memo != "yanlış tahsis" {
		t.Fatalf("reversal %+v, %v", p, err)
	}
	for _, r := range p.Entries {
		found := false
		for _, o := range alloc.Entries {
			if o.Account == r.Account && o.Amount == -r.Amount && *o.OrderLineID == *r.OrderLineID {
				found = true
			}
		}
		if !found {
			t.Fatalf("reversal entry %+v negates no original entry", r)
		}
	}
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after reversing the allocation = %d", b)
	}
	if again, err := e.svc.Reverse(t.Context(), alloc.ID, "tekrar"); err != nil || again != rev {
		t.Fatalf("second Reverse = %s, %v; want %s", again, err, rev)
	}
	// The original stays as it was (append-only).
	if o, _ := e.store.ByID(t.Context(), alloc.ID); len(o.Entries) != len(alloc.Entries) {
		t.Fatal("the reversed transaction changed")
	}
	if _, err := e.svc.Reverse(t.Context(), uuid.NewV7(), ""); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("Reverse of a missing transaction: %v", err)
	}
	if _, err := e.svc.Reverse(t.Context(), uuid.Nil(), ""); !errors.Is(err, ledger.ErrInvalidTxn) {
		t.Fatalf("Reverse of nil: %v", err)
	}

	// Reversing an allocation that was already paid out would overdraw.
	payee2 := uuid.NewV7()
	order2 := e.paidOrder(t, line(payee2))
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee2, 1_080_000, t0)))
	alloc2, _ := e.store.ByRef(t.Context(), ledger.KindOrderAllocated, ledger.RefOrder, order2)
	if _, err := e.svc.Reverse(t.Context(), alloc2.ID, ""); !errors.Is(err, ledger.ErrNegativeBalance) {
		t.Fatalf("Reverse of a paid-out allocation: %v", err)
	}
}

// TestPayeeBalance_AfterAllocation: the balance is what the allocations
// credited the payee (seller_net), less payouts and refunded shares, and is
// independent per payee.
func TestPayeeBalance_AfterAllocation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p1, p2 := uuid.NewV7(), uuid.NewV7()
	if b := e.balance(t, p1); b != 0 {
		t.Fatalf("balance of a payee without entries = %d", b)
	}
	l1, l2 := line(p1), modelALine(t, p1, 250_000, 2000, 1000, [2]int64{}, true)
	e.paidOrder(t, l1, l2, line(p2))
	want1 := l1.SellerNetAmount + l2.SellerNetAmount
	if b := e.balance(t, p1); b != want1 || want1 != 1_080_000+300_000 {
		t.Fatalf("p1 balance = %d, want %d", b, want1)
	}
	if b := e.balance(t, p2); b != 1_080_000 {
		t.Fatalf("p2 balance = %d", b)
	}
	// A partial payout and a refund of half of l1.
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), p1, 300_000, t0)))
	half := split(t)(ledger.SplitRefund(l1, 600_000))
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{half}, t0)))
	if b := e.balance(t, p1); b != want1-300_000-half.SellerAmount || half.SellerAmount != 540_000 {
		t.Fatalf("p1 balance = %d after payout and refund (refunded share %d)", b, half.SellerAmount)
	}
	if b := e.balance(t, p2); b != 1_080_000 {
		t.Fatalf("p2 balance moved: %d", b)
	}
	if _, err := e.svc.PayeeBalance(t.Context(), uuid.Nil()); !errors.Is(err, ledger.ErrInvalidTxn) {
		t.Fatalf("PayeeBalance(nil): %v", err)
	}
}

// TestIntegrityCheck_DetectsNegativeBalance: a payee balance below zero
// written around Post (here straight into the store) is found by the
// nightly check and raises the alarm with the payee and the amount.
func TestIntegrityCheck_DetectsNegativeBalance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	healthy, broken := uuid.NewV7(), uuid.NewV7()
	e.paidOrder(t, line(healthy))
	if rep, err := e.mod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("healthy ledger: %+v, %v", rep, err)
	}
	// Bypass: a balanced debit of a payee that was never credited.
	h := ledger.Transaction{ID: uuid.NewV7(), Kind: ledger.KindAdjustment, RefType: ledger.RefManual, RefID: uuid.NewV7(), OccurredAt: t0}
	rows := []ledger.EntryRow{
		{ID: uuid.NewV7(), TransactionID: h.ID, Account: ledger.AccountSellerPayable, PayeeID: &broken, Amount: 75_000},
		{ID: uuid.NewV7(), TransactionID: h.ID, Account: ledger.AccountKoroBank, Amount: -75_000},
	}
	if ok, err := e.store.InsertTxn(t.Context(), h, rows); err != nil || !ok {
		t.Fatalf("InsertTxn: %v, %v", ok, err)
	}
	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != ledger.FindingNegativeBalance ||
		*rep.Findings[0].PayeeID != broken || *rep.Findings[0].Amount != -75_000 {
		t.Fatalf("findings %+v, want one negative balance of -75000 for %s", rep.Findings, broken)
	}

	// The job raises the alarm for every finding and succeeds. The late
	// recognition of Koro's own sales runs before it (BE-067).
	jobs := e.mod.Jobs()
	if len(jobs) != 2 || jobs[0].Name != ledger.IntegrityJobName || jobs[0].Cron != "15 5 * * *" || jobs[0].Queue != queue.Low ||
		jobs[0].Unique <= 0 || jobs[0].Unique >= 24*time.Hour ||
		jobs[1].Name != ledger.RecognizeJobName || jobs[1].Cron != "5 5 * * *" || jobs[1].Queue != queue.Low ||
		jobs[1].Unique <= 0 || jobs[1].Unique >= 24*time.Hour {
		t.Fatalf("jobs %+v", jobs)
	}
	if err := jobs[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.alarms.all(); len(got) != 1 || got[0].Kind != ledger.FindingNegativeBalance || *got[0].PayeeID != broken {
		t.Fatalf("alarms %+v, want the negative balance", got)
	}
}

// TestIntegrityCheck_PayoutsAgainstLedger: a paid payout needs exactly its
// payout_paid transaction (payee +amount, bank −amount), and a payout_paid
// transaction needs a paid payout (01 §7; the payout flow is BE-044).
func TestIntegrityCheck_PayoutsAgainstLedger(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	store := ledger.NewMemStore(clk.Now)
	e := newEnvWith(t, clk, store, db.NopTx{})
	payee := uuid.NewV7()
	e.paidOrder(t, line(payee))

	good, missing, wrong, unpaid := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, good, payee, 100_000, t0)))
	store.MarkPayoutPaid(good, payee, 100_000)
	store.MarkPayoutPaid(missing, payee, 50_000)
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, wrong, payee, 40_000, t0)))
	store.MarkPayoutPaid(wrong, payee, 45_000)
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, unpaid, payee, 10_000, t0)))

	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, f := range rep.Findings {
		if f.Kind != ledger.FindingPayout {
			t.Fatalf("unexpected finding %+v", f)
		}
		got[*f.PayoutID] = f.Detail
	}
	want := map[uuid.UUID]string{missing: ledger.MismatchMissingTxn, wrong: ledger.MismatchEntries, unpaid: ledger.MismatchNotPaid}
	if len(got) != len(want) {
		t.Fatalf("findings %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("payout %s: %q, want %q", id, got[id], w)
		}
	}
}

// failingStore fails the integrity queries.
type failingStore struct{ ledger.Store }

func (failingStore) Unbalanced(context.Context, int) ([]ledger.UnbalancedTxn, error) {
	return nil, errors.New("connection refused")
}

// TestIntegrityJob_CheckFailureRaisesAlarm: a check that cannot run is an
// alarm too, and the job fails so that the queue retries it.
func TestIntegrityJob_CheckFailureRaisesAlarm(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	e := newEnvWith(t, clk, failingStore{ledger.NewMemStore(clk.Now)}, db.NopTx{})
	if err := e.mod.Jobs()[0].Handle(t.Context()); err == nil {
		t.Fatal("job succeeded without its check")
	}
	if got := e.alarms.all(); len(got) != 1 || got[0].Kind != ledger.FindingCheckFailed || got[0].Detail == "" {
		t.Fatalf("alarms %+v", got)
	}
}

// TestNew_RequiresDeps: a missing dependency stops the process at start.
func TestNew_RequiresDeps(t *testing.T) {
	t.Parallel()
	_, err := ledger.New(ledger.Deps{})
	if err == nil {
		t.Fatal("New without deps succeeded")
	}
	for _, name := range []string{"Store", "Tx", "Clock", "Log"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

// TestJournal_FiltersAndPages: the journal is newest first with keyset
// pages and filters; only pad/fin may read it (403 before anything else).
func TestJournal_FiltersAndPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	var payments []uuid.UUID
	for i := range 5 {
		ref := uuid.NewV7()
		payments = append(payments, ref)
		e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, ref, int64(100*(i+1)), t0.Add(time.Duration(i)*time.Hour))))
	}
	e.paidOrder(t, line(uuid.NewV7()))

	all, err := e.svc.ListTransactions(t.Context(), fin, ledger.JournalQuery{Limit: 100})
	if err != nil || len(all.Items) != 7 || all.Next != nil {
		t.Fatalf("journal %d items, next %v, %v", len(all.Items), all.Next, err)
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i].OccurredAt.After(all.Items[i-1].OccurredAt) {
			t.Fatal("journal is not newest first")
		}
	}
	// Pages of 2 visit every transaction once.
	seen := map[uuid.UUID]bool{}
	q := ledger.JournalQuery{Limit: 2}
	for range 10 {
		p, err := e.svc.ListTransactions(t.Context(), fin, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Fatalf("%s on two pages", it.ID)
			}
			seen[it.ID] = true
		}
		if p.Next == nil {
			break
		}
		q.After = p.Next
	}
	if len(seen) != 7 {
		t.Fatalf("pages visited %d transactions, want 7", len(seen))
	}
	// Filters.
	ref := payments[2]
	from, to := t0.Add(time.Hour), t0.Add(3*time.Hour)
	for _, c := range []struct {
		name string
		f    ledger.JournalFilter
		want int
	}{
		{"kind", ledger.JournalFilter{Kind: ledger.KindOrderAllocated}, 1},
		{"ref type", ledger.JournalFilter{RefType: ledger.RefPayment}, 6},
		{"ref", ledger.JournalFilter{RefID: &ref}, 1},
		{"window", ledger.JournalFilter{Kind: ledger.KindPaymentReceived, From: &from, To: &to}, 2},
		{"nothing", ledger.JournalFilter{Kind: ledger.KindPayoutPaid}, 0},
	} {
		p, err := e.svc.ListTransactions(t.Context(), fin, ledger.JournalQuery{Filter: c.f})
		if err != nil || len(p.Items) != c.want {
			t.Errorf("%s: %d items, %v; want %d", c.name, len(p.Items), err, c.want)
		}
	}
	for _, c := range []struct {
		name string
		f    ledger.JournalFilter
	}{
		{"unknown kind", ledger.JournalFilter{Kind: "gift"}},
		{"unknown ref type", ledger.JournalFilter{RefType: "invoice"}},
		{"empty window", ledger.JournalFilter{From: &to, To: &from}},
	} {
		if _, err := e.svc.ListTransactions(t.Context(), fin, ledger.JournalQuery{Filter: c.f}); !errors.Is(err, problem.ErrValidation) {
			t.Errorf("%s: %v, want validation_failed", c.name, err)
		}
	}
	for _, name := range []string{"company_admin", "moderator", "dpo"} {
		_, err := e.svc.ListTransactions(t.Context(), actor(t, name), ledger.JournalQuery{Filter: ledger.JournalFilter{Kind: "gift"}})
		if !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s: %v, want 403 before validation", name, err)
		}
	}
}
