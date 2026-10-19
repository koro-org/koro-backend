//go:build integration

package payments_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/ledger"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
)

type failCollectionAudit struct{ fail *atomic.Bool }

func (a failCollectionAudit) Log(_ context.Context, ev audit.Event) error {
	if a.fail.Load() && ev.Action == string(authz.PaymentsReceivableApprove) {
		return errors.New("synthetic audit failure")
	}
	return nil
}

// PostgreSQL proves cash, ownership, reservation and approval are atomic; an
// aborted last-step audit consumes neither receipt nor idempotency key.
func TestCollection_PostgresRollbackReplayAndConcurrentDecisions(t *testing.T) {
	t.Parallel()
	w := &collectionLine{}
	fail := &atomic.Bool{}
	e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }), func(dep *payments.Deps) { dep.Audit = failCollectionAudit{fail} })
	_, acct := collectionDebt(t, e, w)
	a, b := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	tr := e.collectionSource(t, acct, 540_000)
	c, err := e.svc.CreateCollection(ctxOf(t, a), a, payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line})
	if err != nil {
		t.Fatal(err)
	}
	ctx := idempotency.WithKey(ctxOf(t, b), idempotency.Key{Scope: "pg-collection-approve", Value: "receipt-1", Status: 200})
	fail.Store(true)
	if _, err = e.svc.ApproveCollection(ctx, b, c.ID, good); err == nil {
		t.Fatal("audit failure committed")
	}
	if n := pgCount(t, d, `SELECT count(*) FROM receivable_collections c JOIN incoming_bank_transfers t ON t.id=c.incoming_transfer_id WHERE c.id=$1 AND c.status='requested' AND t.status='collection_requested' AND c.ledger_transaction_id IS NULL`, c.ID); n != 1 {
		t.Fatal("rollback lost reservation")
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE ref_id=$1`, c.ID); n != 0 {
		t.Fatal("rollback posted cash")
	}
	fail.Store(false)
	c, err = e.svc.ApproveCollection(ctx, b, c.ID, good)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := e.svc.ApproveCollection(ctx, b, c.ID, good)
	if err != nil || *replay.LedgerTransactionID != *c.LedgerTransactionID {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	if amount, err := e.ledMod.LineReceivable(t.Context(), w.line); err != nil || amount != 0 {
		t.Fatalf("remaining debt=%d %v", amount, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE ref_id=$1`, c.ID); n != 1 {
		t.Fatalf("cash transactions=%d", n)
	}
	if _, err = d.Pool.Exec(t.Context(), `UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, tr.ID); err == nil {
		t.Fatal("collected transfer escaped ownership")
	}
	if _, err = d.Pool.Exec(t.Context(), `UPDATE receivable_collections SET amount=amount+1 WHERE id=$1`, c.ID); err == nil {
		t.Fatal("receipt snapshot changed")
	}
	if _, err = e.svc.RejectCollection(ctxOf(t, a), a, c.ID, "already collected"); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("reject collected=%v", err)
	}
}

// Distinct receipts compete for one outstanding balance; its payee advisory
// lock serializes request reservations. Only one entire receipt can fit.
func TestCollection_PostgresConcurrentReservationsAndRejectReuse(t *testing.T) {
	t.Parallel()
	w := &collectionLine{}
	e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }))
	_, acct := collectionDebt(t, e, w)
	a, b := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	trs := []payments.IncomingTransfer{e.collectionSource(t, acct, 540_000), e.collectionSource(t, acct, 540_000)}
	cs := make([]payments.ReceivableCollection, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range trs {
		wg.Go(func() {
			<-start
			cs[i], errs[i] = e.svc.CreateCollection(ctxOf(t, a), a, payments.CollectionInput{IncomingTransferID: trs[i].ID, OrderLineID: w.line})
		})
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatal("both receipts reserved same debt")
			}
			winner = i
		} else if !errors.Is(err, payments.ErrCollectionExceedsReceivable) {
			t.Fatalf("loser=%v", err)
		}
	}
	if winner < 0 {
		t.Fatalf("no winner: %v", errs)
	}
	c := cs[winner]
	if _, err := e.svc.RejectCollection(ctxOf(t, b), b, c.ID, "wrong booking"); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.CreateCollection(ctxOf(t, a), a, payments.CollectionInput{IncomingTransferID: trs[winner].ID, OrderLineID: w.line})
	if err != nil {
		t.Fatalf("reclaim=%v", err)
	}
	decisions := make([]error, 2)
	wg = sync.WaitGroup{}
	start = make(chan struct{})
	for i := range decisions {
		wg.Go(func() { <-start; _, decisions[i] = e.svc.ApproveCollection(ctxOf(t, b), b, c.ID, good) })
	}
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range decisions {
		if err == nil {
			successes++
		} else if !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("decision=%v", err)
		}
	}
	if successes != 1 || pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE ref_id=$1`, c.ID) != 1 {
		t.Fatalf("cash duplicate: %v", decisions)
	}
}

func TestRefundClose_PostgresNoMoneyAndCompletedFinal(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	fin := actor(t, "finance_admin")
	for _, failed := range []bool{false, true} {
		r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
		closed, err := e.svc.CloseRefund(ctxOf(t, fin), fin, r.ID, failed, "Synthetic EFT stopped", good)
		if err != nil {
			t.Fatal(err)
		}
		if closed.ApprovedBy == nil || closed.ClosedAt == nil || pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE ref_id=$1`, r.ID) != 0 {
			t.Fatalf("closed=%+v", closed)
		}
		if _, err = d.Pool.Exec(t.Context(), `UPDATE refunds SET status='approved',closed_by=NULL,closed_at=NULL,cancel_reason=NULL,failure_reason=NULL WHERE id=$1`, r.ID); err == nil {
			t.Fatal("reopened terminal refund")
		}
	}
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	if _, err := e.completeAs(t, "finance_admin", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CloseRefund(ctxOf(t, fin), fin, r.ID, true, "EFT returned later", good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("completed→failed=%v", err)
	}
	txns, err := e.ledStore.List(t.Context(), ledger.JournalFilter{Kind: ledger.KindRefundCompleted}, nil, 10)
	if err != nil || len(txns) != 1 {
		t.Fatalf("refund transactions=%d %v", len(txns), err)
	}
}

// heldTransfer pauses after the real FOR UPDATE, so the competing operation
// must be observed waiting in PostgreSQL before the collection can commit.
type heldTransfer struct {
	payments.Store
	id              uuid.UUID
	locked, release chan struct{}
	once            sync.Once
}

func (s *heldTransfer) GetTransfer(ctx context.Context, id uuid.UUID, lock bool) (payments.IncomingTransfer, error) {
	tr, err := s.Store.GetTransfer(ctx, id, lock)
	if err == nil && lock && id == s.id {
		s.once.Do(func() {
			close(s.locked)
			select {
			case <-s.release:
			case <-ctx.Done():
			}
		})
	}
	return tr, err
}
func TestCollection_PostgresOwnsTransferAgainstOtherFlows(t *testing.T) {
	t.Parallel()
	for _, flow := range []string{"match", "return", "ignore"} {
		t.Run(flow, func(t *testing.T) {
			t.Parallel()
			w := &collectionLine{}
			hold := &heldTransfer{locked: make(chan struct{}), release: make(chan struct{})}
			e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }), func(dep *payments.Deps) { hold.Store = dep.Store; dep.Store = hold })
			_, acct := collectionDebt(t, e, w)
			a := actor(t, "finance_admin")
			tr := e.collectionSource(t, acct, 540_000)
			unpaid := e.order(t, orderSpec{})
			hold.id = tr.ID
			ctx, cancel := context.WithTimeout(ctxOf(t, a), 10*time.Second)
			defer cancel()
			var release sync.Once
			defer release.Do(func() { close(hold.release) })
			claimed := make(chan error, 1)
			go func() {
				_, err := e.svc.CreateCollection(ctx, a, payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line})
				claimed <- err
			}()
			select {
			case <-hold.locked:
			case err := <-claimed:
				t.Fatalf("collection exited before lock: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			other := make(chan error, 1)
			go func() {
				var err error
				switch flow {
				case "match":
					_, err = e.svc.MatchTransfer(ctx, a, tr.ID, payments.MatchInput{OrderID: unpaid.order.ID, ExpectedOrderNo: unpaid.order.OrderNo})
				case "return":
					_, err = e.svc.RequestReturn(ctx, a, tr.ID, payments.ReturnRequest{Reason: "Synthetic return", TargetIBAN: ibanSender}, good)
				case "ignore":
					_, err = e.svc.IgnoreTransfer(ctx, a, tr.ID, "Synthetic ignore")
				}
				other <- err
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var waiting bool
				if err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%incoming_bank_transfers%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-other:
					t.Fatalf("competing flow did not wait: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-tick.C:
				}
			}
			release.Do(func() { close(hold.release) })
			if err := <-claimed; err != nil {
				t.Fatal(err)
			}
			if err := <-other; !errors.Is(err, payments.ErrInvalidState) {
				t.Fatalf("competing %s=%v", flow, err)
			}
			if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE incoming_transfer_id=$1`, tr.ID); n != 0 {
				t.Fatal("claimed cash also captured as payment")
			}
			if n := pgCount(t, d, `SELECT count(*) FROM incoming_bank_transfers WHERE id=$1 AND status='collection_requested'`, tr.ID); n != 1 {
				t.Fatal("source claim escaped")
			}
		})
	}
}

func TestRefundRequested_PostgresAutomaticNoticeAfterCommitOnly(t *testing.T) {
	t.Parallel()
	fail := &atomic.Bool{}
	fail.Store(true)
	e, _ := newPGEnv(t, withOutbox(func(w outbox.Writer) outbox.Writer {
		return switchOutbox{Writer: w, topic: domainevent.TopicPaymentCaptured, fail: fail}
	}))
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_200_001, p.order.OrderNo, true)
	if _, err := e.match(t, tr, p, ""); err == nil {
		t.Fatal("capture committed with outbox down")
	}
	count := func() int {
		n := 0
		for _, q := range e.queue.Tasks() {
			if q.Type == payments.NotifyRefundRequestedTaskName {
				n++
			}
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("rolled back refund queued %d notices", n)
	}
	fail.Store(false)
	if _, err := e.match(t, tr, p, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("committed overpayment queued %d notices", n)
	}
}
