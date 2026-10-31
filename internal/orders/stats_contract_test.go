package orders_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/orders"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (orders.Store, world) {
		return orders.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// statsContract is the sales counts of a seller's panel (BE-163): the
// orders placed in a range that hold a line of the seller, each once (two
// lines of the seller in one order), by status; another seller's orders
// and orders out of the range are left out.
func statsContract(t *testing.T, newStore func(t *testing.T) (orders.Store, world)) {
	t.Helper()
	t.Run("sales stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		b, sl, other := w.buyer(t), w.seller(t), w.seller(t)
		placed := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
		insert := func(f fixture) {
			t.Helper()
			if err := s.InsertOrder(ctx, f.o, f.lines); err != nil {
				t.Fatalf("InsertOrder: %v", err)
			}
		}
		insert(newFixture(b, placed, []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl}))
		insert(newFixture(b, placed.Add(time.Hour), []offeringRef{w.offering(t, sl), w.offering(t, other)}, []sellerRef{sl, other}))
		insert(newFixture(b, placed.AddDate(0, -3, 0), []offeringRef{w.offering(t, sl)}, []sellerRef{sl}))
		insert(newFixture(b, placed, []offeringRef{w.offering(t, other)}, []sellerRef{other}))
		st, err := s.SalesStats(ctx, sl.org, placed.AddDate(0, 0, -7), placed.AddDate(0, 0, 7))
		if err != nil {
			t.Fatal(err)
		}
		if st.Orders != 2 || st.ByStatus[orders.StatusAwaitingPayment] != 2 || len(st.ByStatus) != 1 {
			t.Fatalf("sales stats = %+v, want 2 awaiting payment", st)
		}
		if none, err := s.SalesStats(ctx, uuid.NewV7(), placed.AddDate(0, 0, -7), placed.AddDate(0, 0, 7)); err != nil || none.Orders != 0 {
			t.Fatalf("unknown seller = %+v, %v", none, err)
		}
	})
}
