package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

// alarms records the findings an integrity run raises.
type alarms struct {
	mu   sync.Mutex
	list []ledger.Finding
}

func (a *alarms) raise(_ context.Context, f ledger.Finding) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, f)
}

func (a *alarms) all() []ledger.Finding {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ledger.Finding(nil), a.list...)
}

// env is a ledger module over a store (in memory by default).
type env struct {
	clk    *clock.Fake
	store  ledger.Store
	alarms *alarms
	mod    ledger.Module
	svc    ledger.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvWith(t, clk, ledger.NewMemStore(clk.Now), db.NopTx{})
}

func newEnvWith(t *testing.T, clk *clock.Fake, store ledger.Store, tx db.TxManager) *env {
	t.Helper()
	e := &env{clk: clk, store: store, alarms: &alarms{}}
	mod, err := ledger.New(ledger.Deps{Store: store, Tx: tx, Clock: clk, Log: obs.Discard(), Alarm: e.alarms.raise})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// post posts t and fails the test on error.
func (e *env) post(t *testing.T, tx ledger.Txn) uuid.UUID {
	t.Helper()
	id, err := e.svc.Post(t.Context(), tx)
	if err != nil {
		t.Fatalf("Post %s/%s: %v", tx.Kind, tx.RefID, err)
	}
	return id
}

// balance returns the payee balance and fails the test on error.
func (e *env) balance(t *testing.T, payee uuid.UUID) int64 {
	t.Helper()
	b, err := e.svc.PayeeBalance(t.Context(), payee)
	if err != nil {
		t.Fatalf("PayeeBalance: %v", err)
	}
	return b
}

// txn, split and amt unwrap a template or platform/money result and fail
// the test on error: txn(t)(ledger.PaymentReceived(...)).
func txn(t *testing.T) func(ledger.Txn, error) ledger.Txn { return unwrap[ledger.Txn](t) }

func split(t *testing.T) func(ledger.RefundLine, error) ledger.RefundLine {
	return unwrap[ledger.RefundLine](t)
}

func amt(t *testing.T) func(int64, error) int64 { return unwrap[int64](t) }

func unwrap[T any](t *testing.T) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// line is the 02 §19.2 example line: 1,000,000 net + 200,000 VAT, 10 %
// commission from the seller (100,000 + 20,000 VAT): payable 1,200,000,
// seller net 1,080,000.
func line(payee uuid.UUID) ledger.AllocationLine {
	return ledger.AllocationLine{
		OrderLineID: uuid.NewV7(), PayeeID: &payee, PayableAmount: 1_200_000,
		SellerNetAmount: 1_080_000, CommissionAmount: 100_000, CommissionVATAmount: 20_000,
	}
}

// paidOrder posts the payment and the allocation of an order of the lines
// and returns the order ID.
func (e *env) paidOrder(t *testing.T, lines ...ledger.AllocationLine) uuid.UUID {
	t.Helper()
	var total int64
	for _, l := range lines {
		total += l.PayableAmount
	}
	order := uuid.NewV7()
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), total, t0)))
	e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, lines, t0)))
	return order
}

// sum returns the sum of the entry amounts.
func sum(es []ledger.Entry) int64 {
	var s int64
	for _, e := range es {
		s += e.Amount
	}
	return s
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ptr[T any](v T) *T { return &v }
