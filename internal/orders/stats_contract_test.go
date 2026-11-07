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
// and orders out of the range are left out. And the purchases of a
// company's panel (BE-189): see "purchase stats".
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

	// purchase stats (BE-189): the buyer's orders of the range by status,
	// its seller lines by fulfilment (a line dropped from its unpaid order
	// left out), then its events, latest placement first, each with its
	// orders once by status, its lines by fulfilment and the evidence files
	// of the lines; another buyer's orders and orders out of the range are
	// left out; limit cuts the events, not their count.
	t.Run("purchase stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		b, other, sl, s2 := w.buyer(t), w.buyer(t), w.seller(t), w.seller(t)
		ev1, ev2 := w.event(t, sl.org), w.event(t, s2.org)
		placed := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
		insert := func(f fixture, evs ...*uuid.UUID) fixture {
			t.Helper()
			for i, ev := range evs {
				f.lines[i].EventID = ev
			}
			if err := s.InsertOrder(ctx, f.o, f.lines); err != nil {
				t.Fatalf("InsertOrder: %v", err)
			}
			return f
		}
		pay := func(o orders.Order) {
			t.Helper()
			at := o.PlacedAt.Add(time.Minute)
			next := o
			next.Status, next.PaidAmount, next.PaidAt = orders.StatusPaid, o.KoroCollectAmount, &at
			if _, err := s.UpdateOrder(ctx, next, orders.StatusAwaitingPayment); err != nil {
				t.Fatalf("pay: %v", err)
			}
		}
		// a: paid, two lines of ev1, the first marked with its evidence.
		a := insert(newFixture(b, placed, []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl}), &ev1, &ev1)
		pay(a.o)
		file := w.evidence(t, sl.org)
		marked, by, ends := placed.Add(time.Hour), w.buyer(t).user, placed.Add(8*24*time.Hour)
		ml := a.lines[0]
		ml.FulfillmentStatus, ml.FulfillmentMarkedAt, ml.FulfillmentMarkedBy, ml.DisputeWindowEndsAt = orders.FulfillMarked, &marked, &by, &ends
		ml.FulfillmentEvidenceFileID = &file
		if _, err := s.UpdateLine(ctx, ml, orders.FulfillPending); err != nil {
			t.Fatalf("mark: %v", err)
		}
		// g: unpaid, its ev1 line dropped (BE-134), its other line kept.
		g := insert(newFixture(b, placed.Add(30*time.Minute), []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl}), &ev1)
		keep, drop := g.lines[1], g.lines[0]
		next := g.o
		next.SubtotalAmount, next.VATAmount, next.TotalAmount = keep.NetAmount, keep.VATAmount, keep.GrossAmount
		next.PayableAmount, next.KoroCollectAmount = keep.PayableAmount, keep.PayableAmount
		if _, _, err := s.DropLines(ctx, next, []uuid.UUID{drop.ID}, placed.Add(40*time.Minute)); err != nil {
			t.Fatalf("DropLines: %v", err)
		}
		// bo: awaiting payment, one line of ev1; c: one line without an event.
		insert(newFixture(b, placed.Add(time.Hour), []offeringRef{w.offering(t, sl)}, []sellerRef{sl}), &ev1)
		insert(newFixture(b, placed, []offeringRef{w.offering(t, sl)}, []sellerRef{sl}))
		// f: paid, one line of ev2 fulfilled.
		f := insert(newFixture(b, placed.Add(2*time.Hour), []offeringRef{w.offering(t, s2)}, []sellerRef{s2}), &ev2)
		pay(f.o)
		at, via := placed.Add(3*time.Hour), orders.ViaBuyerConfirmed
		fl := f.lines[0]
		fl.FulfillmentStatus, fl.FulfilledAt, fl.FulfilledVia = orders.FulfillFulfilled, &at, &via
		if _, err := s.UpdateLine(ctx, fl, orders.FulfillPending); err != nil {
			t.Fatalf("fulfil: %v", err)
		}
		// left out: out of the range, another buyer.
		insert(newFixture(b, placed.AddDate(0, -3, 0), []offeringRef{w.offering(t, sl)}, []sellerRef{sl}), &ev1)
		insert(newFixture(other, placed, []offeringRef{w.offering(t, sl)}, []sellerRef{sl}), &ev1)

		from, to := placed.AddDate(0, 0, -7), placed.AddDate(0, 0, 7)
		st, err := s.PurchaseStats(ctx, b.org, from, to, 10)
		if err != nil {
			t.Fatal(err)
		}
		if st.Orders != 5 || st.ByStatus[orders.StatusPaid] != 2 || st.ByStatus[orders.StatusAwaitingPayment] != 3 || len(st.ByStatus) != 2 {
			t.Fatalf("orders = %d %v, want 5 (2 paid, 3 awaiting payment)", st.Orders, st.ByStatus)
		}
		if st.Lines != 6 || st.ByFulfillment[orders.FulfillPending] != 4 || st.ByFulfillment[orders.FulfillMarked] != 1 ||
			st.ByFulfillment[orders.FulfillFulfilled] != 1 || len(st.ByFulfillment) != 3 {
			t.Fatalf("lines = %d %v, want 6 (4 pending, 1 marked, 1 fulfilled)", st.Lines, st.ByFulfillment)
		}
		if st.EventCount != 2 || len(st.Events) != 2 {
			t.Fatalf("events = %d %+v, want 2", st.EventCount, st.Events)
		}
		e2, e1 := st.Events[0], st.Events[1]
		if e2.EventID != ev2 || e2.SellerOrgID != s2.org || e2.Orders != 1 || e2.ByStatus[orders.StatusPaid] != 1 || len(e2.ByStatus) != 1 ||
			e2.Lines != 1 || e2.ByFulfillment[orders.FulfillFulfilled] != 1 || len(e2.ByFulfillment) != 1 || len(e2.EvidenceFileIDs) != 0 ||
			!e2.LastPlacedAt.Equal(placed.Add(2*time.Hour)) {
			t.Fatalf("ev2 = %+v", e2)
		}
		if e1.EventID != ev1 || e1.SellerOrgID != sl.org || e1.Orders != 2 || e1.ByStatus[orders.StatusPaid] != 1 ||
			e1.ByStatus[orders.StatusAwaitingPayment] != 1 || len(e1.ByStatus) != 2 || e1.Lines != 3 ||
			e1.ByFulfillment[orders.FulfillMarked] != 1 || e1.ByFulfillment[orders.FulfillPending] != 2 || len(e1.ByFulfillment) != 2 ||
			len(e1.EvidenceFileIDs) != 1 || e1.EvidenceFileIDs[0] != file || !e1.LastPlacedAt.Equal(placed.Add(time.Hour)) ||
			e1.Evidence != 0 || e1.EvidenceRejected != 0 {
			t.Fatalf("ev1 = %+v", e1)
		}
		cut, err := s.PurchaseStats(ctx, b.org, from, to, 1)
		if err != nil || cut.EventCount != 2 || len(cut.Events) != 1 || cut.Events[0].EventID != ev2 || cut.Orders != 5 {
			t.Fatalf("limit 1 = %+v, %v", cut, err)
		}
		none, err := s.PurchaseStats(ctx, uuid.NewV7(), from, to, 10)
		if err != nil || none.Orders != 0 || none.Lines != 0 || none.EventCount != 0 || none.Events == nil || len(none.Events) != 0 ||
			none.ByStatus == nil || none.ByFulfillment == nil {
			t.Fatalf("unknown buyer = %+v, %v", none, err)
		}
	})
}
