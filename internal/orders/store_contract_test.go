package orders_test

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/orders"
)

// world creates the parent rows a store needs (FKs in PostgreSQL; plain IDs
// in memory): a buying company with its user and verified party, a seller
// club with its payee, an offering of the seller (with its service and the
// policy), an event of an org and a pending payout of lines.
type world struct {
	buyer    func(t *testing.T) buyerRef
	seller   func(t *testing.T) sellerRef
	offering func(t *testing.T, s sellerRef) offeringRef
	event    func(t *testing.T, org uuid.UUID) uuid.UUID
	payout   func(t *testing.T, s sellerRef, lines map[uuid.UUID]int64) uuid.UUID
}

type buyerRef struct{ org, user, party uuid.UUID }
type sellerRef struct{ org, payee uuid.UUID }
type offeringRef struct{ id, service, policy uuid.UUID }

func memWorld() world {
	id := func() uuid.UUID { return uuid.NewV7() }
	return world{
		buyer:    func(*testing.T) buyerRef { return buyerRef{id(), id(), id()} },
		seller:   func(*testing.T) sellerRef { return sellerRef{id(), id()} },
		offering: func(*testing.T, sellerRef) offeringRef { return offeringRef{id(), id(), id()} },
		event:    func(*testing.T, uuid.UUID) uuid.UUID { return id() },
		payout:   func(*testing.T, sellerRef, map[uuid.UUID]int64) uuid.UUID { return id() },
	}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (orders.Store, world) {
		return orders.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// fixture builds an order of one buyer with lines of offerings of sellers,
// with the 02 example amounts (1,000,000 net per unit, 20 % VAT, 10 %
// commission from the seller); mut changes the order before the totals are
// summed (status and running columns only).
type fixture struct {
	o     orders.Order
	lines []orders.Line
}

func newFixture(b buyerRef, placed time.Time, offs []offeringRef, sellers []sellerRef) fixture {
	o := orders.Order{ID: uuid.NewV7(), OrderNo: "KR" + crockford8(), OrgID: b.org, OrgKind: "company", PlacedByUserID: b.user,
		BuyerPartyID: b.party, BuyerSnapshot: orders.BuyerSnapshot{Title: "Örnek A.Ş.", TaxNo: "1234567890"}, ModelCode: orders.ModelA,
		CollectionMethod: orders.CollectBankTransfer, Status: orders.StatusAwaitingPayment, Currency: "TRY",
		ExpiresAt: placed.Add(7 * 24 * time.Hour), PlacedAt: placed, CreatedAt: placed, UpdatedAt: placed}
	f := fixture{o: o}
	for i, off := range offs {
		payee := sellers[i].payee
		l := orders.Line{ID: uuid.NewV7(), OrderID: o.ID, OrgID: b.org, SellerOrgID: sellers[i].org, PayeeID: &payee, OfferingID: off.id,
			ServiceID: off.service, CommercePolicyID: off.policy, TitleSnapshot: "Stant", DeliverablesSnapshot: orders.Deliverables{},
			Quantity: 1, UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000,
			CommissionRateBP: 1000, CommissionBasis: "net_excl_vat", CommissionPayer: "seller", CommissionAmount: 100_000,
			CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, DisputeWindowDays: 7, SellerNetAmount: 1_080_000,
			PayableAmount: 1_200_000, FulfillmentStatus: orders.FulfillPending, CreatedAt: placed.Add(time.Duration(i) * time.Microsecond),
			UpdatedAt: placed}
		f.lines = append(f.lines, l)
		f.o.SubtotalAmount += l.NetAmount
		f.o.VATAmount += l.VATAmount
		f.o.TotalAmount += l.GrossAmount
		f.o.PayableAmount += l.PayableAmount
	}
	f.o.KoroCollectAmount = f.o.PayableAmount
	return f
}

// crockford8 is a random 8-character Crockford body (40 bits).
func crockford8() string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alpha[rand.IntN(len(alpha))]
	}
	return string(b)
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (orders.Store, world)) {
	t.Helper()
	ctx := t.Context()
	insert := func(t *testing.T, s orders.Store, f fixture) {
		t.Helper()
		if err := s.InsertOrder(ctx, f.o, f.lines); err != nil {
			t.Fatalf("InsertOrder: %v", err)
		}
	}
	one := func(t *testing.T, s orders.Store, w world, b buyerRef, sl sellerRef, placed time.Time) fixture {
		t.Helper()
		f := newFixture(b, placed, []offeringRef{w.offering(t, sl)}, []sellerRef{sl})
		insert(t, s, f)
		return f
	}
	pay := func(t *testing.T, s orders.Store, o orders.Order) orders.Order {
		t.Helper()
		at := o.PlacedAt.Add(time.Hour)
		next := o
		next.Status, next.PaidAmount, next.PaidAt = orders.StatusPaid, o.KoroCollectAmount, &at
		got, err := s.UpdateOrder(ctx, next, orders.StatusAwaitingPayment)
		if err != nil {
			t.Fatalf("pay: %v", err)
		}
		return got
	}
	fulfil := func(t *testing.T, s orders.Store, l orders.Line, at time.Time) orders.Line {
		t.Helper()
		via := orders.ViaBuyerConfirmed
		next := l
		next.FulfillmentStatus, next.FulfilledAt, next.FulfilledVia = orders.FulfillFulfilled, &at, &via
		got, err := s.UpdateLine(ctx, next, l.FulfillmentStatus)
		if err != nil {
			t.Fatalf("fulfil: %v", err)
		}
		return got
	}

	t.Run("insert, read and the reference code", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		f := newFixture(b, t0, []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl})
		insert(t, s, f)
		got, err := s.GetOrder(ctx, f.o.ID, false)
		if err != nil || got.OrderNo != f.o.OrderNo || got.PayableAmount != 2_400_000 || got.BuyerSnapshot.Title != "Örnek A.Ş." ||
			got.Status != orders.StatusAwaitingPayment || !got.PlacedAt.Equal(t0) {
			t.Fatalf("GetOrder = %+v, %v", got, err)
		}
		if got, err := s.OrderByNo(ctx, f.o.OrderNo); err != nil || got.ID != f.o.ID {
			t.Fatalf("OrderByNo = %+v, %v", got, err)
		}
		if taken, err := s.OrderNoTaken(ctx, f.o.OrderNo); err != nil || !taken {
			t.Fatalf("OrderNoTaken(own) = %v, %v", taken, err)
		}
		if taken, err := s.OrderNoTaken(ctx, "KR00000000"); err != nil || taken {
			t.Fatalf("OrderNoTaken(free) = %v, %v", taken, err)
		}
		byOrder, err := s.LinesOf(ctx, []uuid.UUID{f.o.ID, uuid.NewV7()})
		if err != nil || len(byOrder[f.o.ID]) != 2 || byOrder[f.o.ID][0].ID != f.lines[0].ID || byOrder[f.o.ID][1].ID != f.lines[1].ID {
			t.Fatalf("LinesOf = %+v, %v", byOrder, err)
		}
		l, err := s.GetLine(ctx, f.lines[0].ID, true)
		if err != nil || l.SellerNetAmount != 1_080_000 || *l.PayeeID != sl.payee || l.DisputeWindowDays != 7 || l.DeliverablesSnapshot == nil {
			t.Fatalf("GetLine = %+v, %v", l, err)
		}
		m, err := s.OrdersByID(ctx, []uuid.UUID{f.o.ID, uuid.NewV7()})
		if err != nil || len(m) != 1 {
			t.Fatalf("OrdersByID = %v, %v", m, err)
		}
		for _, err := range []error{
			func() error { _, err := s.GetOrder(ctx, uuid.NewV7(), false); return err }(),
			func() error { _, err := s.GetLine(ctx, uuid.NewV7(), false); return err }(),
			func() error { _, err := s.OrderByNo(ctx, "KR00000000"); return err }(),
		} {
			wantErr(t, err, orders.ErrNotFound)
		}
	})

	t.Run("targeted updates keep the frozen values", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		f := one(t, s, w, w.buyer(t), w.seller(t), t0)
		next := f.o
		at := t0.Add(time.Hour)
		next.Status, next.PaidAmount, next.PaidAt = orders.StatusPaid, f.o.KoroCollectAmount, &at
		next.PayableAmount, next.OrderNo = 1, "KRZZZZZZZZ" // frozen: never written
		got, err := s.UpdateOrder(ctx, next, orders.StatusAwaitingPayment)
		if err != nil || got.Status != orders.StatusPaid || got.PayableAmount != 1_200_000 || got.OrderNo != f.o.OrderNo || got.PaidAt == nil {
			t.Fatalf("UpdateOrder = %+v, %v", got, err)
		}
		_, err = s.UpdateOrder(ctx, next, orders.StatusAwaitingPayment)
		wantErr(t, err, orders.ErrInvalidState)
		_, err = s.UpdateOrder(ctx, orders.Order{ID: uuid.NewV7()}, orders.StatusAwaitingPayment)
		wantErr(t, err, orders.ErrNotFound)
		over := got
		over.RefundedAmount = got.PaidAmount + 1
		_, err = s.UpdateOrder(ctx, over, orders.StatusPaid)
		wantErr(t, err, orders.ErrRefundExceedsPaid)

		l := f.lines[0]
		marked, by, ends := t0.Add(2*time.Hour), w.buyer(t).user, t0.Add(9*24*time.Hour)
		nl := l
		nl.FulfillmentStatus, nl.FulfillmentMarkedAt, nl.FulfillmentMarkedBy, nl.DisputeWindowEndsAt = orders.FulfillMarked, &marked, &by, &ends
		nl.SellerNetAmount, nl.CommissionRateBP = 1, 0 // frozen
		gl, err := s.UpdateLine(ctx, nl, orders.FulfillPending)
		if err != nil || gl.FulfillmentStatus != orders.FulfillMarked || gl.SellerNetAmount != 1_080_000 || gl.CommissionRateBP != 1000 ||
			gl.DisputeWindowEndsAt == nil || !gl.DisputeWindowEndsAt.Equal(ends) {
			t.Fatalf("UpdateLine = %+v, %v", gl, err)
		}
		_, err = s.UpdateLine(ctx, nl, orders.FulfillPending)
		wantErr(t, err, orders.ErrInvalidState)
		over2 := gl
		over2.RefundedAmount = gl.PayableAmount + 1
		_, err = s.UpdateLine(ctx, over2, orders.FulfillMarked)
		wantErr(t, err, orders.ErrRefundExceedsPaid)
	})

	t.Run("lists: scope, filters and keyset pages", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, other, sl := w.buyer(t), w.buyer(t), w.seller(t)
		var fs []fixture
		for i := range 3 {
			fs = append(fs, one(t, s, w, b, sl, t0.Add(time.Duration(i)*time.Minute)))
		}
		foreign := one(t, s, w, other, w.seller(t), t0.Add(10*time.Minute))
		pay(t, s, fs[0].o)
		page, err := s.ListOrgOrders(ctx, b.org, orders.OrderFilter{}, nil, 2)
		if err != nil || len(page) != 2 || page[0].ID != fs[2].o.ID || page[1].ID != fs[1].o.ID {
			t.Fatalf("page 1 = %v, %v", ids(page), err)
		}
		page, err = s.ListOrgOrders(ctx, b.org, orders.OrderFilter{}, &orders.TimeKey{At: page[1].PlacedAt, ID: page[1].ID}, 2)
		if err != nil || len(page) != 1 || page[0].ID != fs[0].o.ID {
			t.Fatalf("page 2 = %v, %v", ids(page), err)
		}
		if page, _ := s.ListOrgOrders(ctx, b.org, orders.OrderFilter{Status: orders.StatusPaid}, nil, 10); len(page) != 1 || page[0].ID != fs[0].o.ID {
			t.Fatalf("paid = %v", ids(page))
		}
		if page, _ := s.ListOrders(ctx, orders.OrderFilter{OrgID: &other.org}, nil, 10); len(page) != 1 || page[0].ID != foreign.o.ID {
			t.Fatalf("admin by org = %v", ids(page))
		}
		if page, _ := s.ListOrders(ctx, orders.OrderFilter{OrderNo: fs[1].o.OrderNo}, nil, 10); len(page) != 1 || page[0].ID != fs[1].o.ID {
			t.Fatalf("admin by reference = %v", ids(page))
		}
		from, until := t0.Add(time.Minute), t0.Add(2*time.Minute)
		if page, _ := s.ListOrders(ctx, orders.OrderFilter{PlacedFrom: &from, PlacedUntil: &until}, nil, 10); len(page) != 1 || page[0].ID != fs[1].o.ID {
			t.Fatalf("admin by window = %v", ids(page))
		}
		sales, err := s.ListSales(ctx, sl.org, orders.SalesFilter{}, nil, 10)
		if err != nil || len(sales) != 3 || sales[0].OrderID != fs[2].o.ID {
			t.Fatalf("sales = %d, %v", len(sales), err)
		}
		if sales, _ := s.ListSales(ctx, sl.org, orders.SalesFilter{Fulfillment: orders.FulfillMarked}, nil, 10); len(sales) != 0 {
			t.Fatalf("marked sales = %d", len(sales))
		}
		if sales, _ := s.ListSales(ctx, sl.org, orders.SalesFilter{}, &orders.TimeKey{At: sales[0].CreatedAt, ID: sales[0].ID}, 10); len(sales) != 2 {
			t.Fatalf("sales page 2 = %d", len(sales))
		}
		if mine, _ := s.OrdersPlacedBy(ctx, b.user); len(mine) != 3 {
			t.Fatalf("placed by = %d", len(mine))
		}
	})

	t.Run("jobs: expiry and dispute windows", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		old := one(t, s, w, b, sl, t0)
		one(t, s, w, b, sl, t0.Add(48*time.Hour)) // still in its window
		paid := one(t, s, w, b, sl, t0)
		pay(t, s, paid.o)
		due, err := s.DueToExpire(ctx, t0.Add(8*24*time.Hour), 10)
		if err != nil || len(due) != 1 || due[0].ID != old.o.ID {
			t.Fatalf("DueToExpire = %v, %v", ids(due), err)
		}
		l := paid.lines[0]
		marked, ends := t0.Add(2*time.Hour), t0.Add(3*24*time.Hour)
		nl := l
		nl.FulfillmentStatus, nl.FulfillmentMarkedAt, nl.FulfillmentMarkedBy, nl.DisputeWindowEndsAt = orders.FulfillMarked, &marked, &b.user, &ends
		if _, err := s.UpdateLine(ctx, nl, orders.FulfillPending); err != nil {
			t.Fatal(err)
		}
		if ls, err := s.WindowsElapsed(ctx, ends.Add(-time.Second), 10); err != nil || len(ls) != 0 {
			t.Fatalf("WindowsElapsed early = %d, %v", len(ls), err)
		}
		if ls, err := s.WindowsElapsed(ctx, ends, 10); err != nil || len(ls) != 1 || ls[0].ID != l.ID {
			t.Fatalf("WindowsElapsed = %d, %v", len(ls), err)
		}
		if ls, _ := s.LinesMarkedBy(ctx, b.user); len(ls) != 1 {
			t.Fatalf("LinesMarkedBy = %d", len(ls))
		}
	})

	t.Run("offering, buyer units and open commerce", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		off := w.offering(t, sl)
		f1 := newFixture(b, t0, []offeringRef{off}, []sellerRef{sl})
		f1.lines[0].Quantity, f1.lines[0].NetAmount, f1.lines[0].VATAmount, f1.lines[0].GrossAmount = 2, 2_000_000, 400_000, 2_400_000
		f1.lines[0].CommissionAmount, f1.lines[0].CommissionVATAmount, f1.lines[0].SellerNetAmount = 200_000, 40_000, 2_160_000
		f1.lines[0].PayableAmount = 2_400_000
		f1.o.SubtotalAmount, f1.o.VATAmount, f1.o.TotalAmount, f1.o.PayableAmount, f1.o.KoroCollectAmount = 2_000_000, 400_000, 2_400_000, 2_400_000, 2_400_000
		insert(t, s, f1)
		f2 := newFixture(b, t0, []offeringRef{off}, []sellerRef{sl})
		insert(t, s, f2)
		at := t0.Add(time.Hour)
		cancelled := f2.o
		cancelled.Status, cancelled.CancelledAt = orders.StatusCancelled, &at
		if _, err := s.UpdateOrder(ctx, cancelled, orders.StatusAwaitingPayment); err != nil {
			t.Fatal(err)
		}
		if n, err := s.BuyerHeld(ctx, off.id, b.org); err != nil || n != 2 {
			t.Fatalf("BuyerHeld = %d, %v (the cancelled order holds nothing)", n, err)
		}
		if n, _ := s.BuyerHeld(ctx, off.id, w.buyer(t).org); n != 0 {
			t.Fatalf("another buyer holds %d", n)
		}
		open, err := s.OpenOrdersWithOffering(ctx, off.id)
		if err != nil || len(open) != 1 || open[0].ID != f1.o.ID {
			t.Fatalf("OpenOrdersWithOffering = %v, %v", ids(open), err)
		}
		for name, org := range map[string]uuid.UUID{"buyer": b.org, "seller": sl.org} {
			if ok, err := s.HasOpenCommerce(ctx, org); err != nil || !ok {
				t.Fatalf("HasOpenCommerce(%s) = %v, %v", name, ok, err)
			}
		}
		if ok, _ := s.HasOpenCommerce(ctx, uuid.NewV7()); ok {
			t.Fatal("an org without orders has open commerce")
		}
		// Cancel the open one too: nothing open is left on either side.
		c1 := f1.o
		c1.Status, c1.CancelledAt = orders.StatusCancelled, &at
		if _, err := s.UpdateOrder(ctx, c1, orders.StatusAwaitingPayment); err != nil {
			t.Fatal(err)
		}
		for name, org := range map[string]uuid.UUID{"buyer": b.org, "seller": sl.org} {
			if ok, _ := s.HasOpenCommerce(ctx, org); ok {
				t.Fatalf("HasOpenCommerce(%s) after cancellation", name)
			}
		}
	})

	t.Run("payable lines, payout and refunds due", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		f := newFixture(b, t0, []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl})
		f.lines[1].PayoutHoldDays = 3
		insert(t, s, f)
		pay(t, s, f.o)
		done := t0.Add(2 * time.Hour)
		a := fulfil(t, s, f.lines[0], done)
		held := fulfil(t, s, f.lines[1], done)
		ls, err := s.PayableLines(ctx, sl.payee, done)
		if err != nil || len(ls) != 1 || ls[0].ID != a.ID {
			t.Fatalf("PayableLines = %d, %v", len(ls), err)
		}
		ls, _ = s.PayableLines(ctx, sl.payee, done.Add(3*24*time.Hour))
		if len(ls) != 2 {
			t.Fatalf("PayableLines after the hold = %d", len(ls))
		}
		payout := w.payout(t, sl, map[uuid.UUID]int64{a.ID: a.SellerNetAmount})
		n, err := s.MarkLinesPaidOut(ctx, []uuid.UUID{a.ID}, payout, done.Add(time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("MarkLinesPaidOut = %d, %v", n, err)
		}
		if got, _ := s.GetLine(ctx, a.ID, false); got.PayoutID == nil || *got.PayoutID != payout || got.PaidOutAt == nil {
			t.Fatalf("paid out line = %+v", got)
		}
		// billing's commission invoice reads the lines of the payout (BE-042).
		if ls, err := s.LinesOfPayout(ctx, payout); err != nil || len(ls) != 1 || ls[0].ID != a.ID {
			t.Fatalf("LinesOfPayout = %d, %v", len(ls), err)
		}
		if ls, err := s.LinesOfPayout(ctx, uuid.NewV7()); err != nil || len(ls) != 0 {
			t.Fatalf("LinesOfPayout(unknown) = %d, %v", len(ls), err)
		}
		if n, _ := s.MarkLinesPaidOut(ctx, []uuid.UUID{a.ID}, payout, done); n != 0 {
			t.Fatalf("second payout of a line marked %d", n)
		}
		ls, _ = s.PayableLines(ctx, sl.payee, done.Add(3*24*time.Hour))
		if len(ls) != 1 || ls[0].ID != held.ID {
			t.Fatalf("PayableLines after the payout = %d", len(ls))
		}
		if due, err := s.RefundsDue(ctx, 10); err != nil || len(due) != 0 {
			t.Fatalf("RefundsDue = %+v, %v", due, err)
		}
		// A cancelled line of the paid order is due; a partially refunded one only for the rest.
		f2 := newFixture(b, t0, []offeringRef{w.offering(t, sl)}, []sellerRef{sl})
		insert(t, s, f2)
		pay(t, s, f2.o)
		c := f2.lines[0]
		c.FulfillmentStatus, c.RefundedAmount = orders.FulfillCancelled, 200_000
		if _, err := s.UpdateLine(ctx, c, orders.FulfillPending); err != nil {
			t.Fatal(err)
		}
		due, err := s.RefundsDue(ctx, 10)
		if err != nil || len(due) != 1 || *due[0].LineID != c.ID || due[0].Amount != 1_000_000 || due[0].OrgID != b.org {
			t.Fatalf("RefundsDue = %+v, %v", due, err)
		}
	})

	t.Run("collected lines (BE-067)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		paid := newFixture(b, t0, []offeringRef{w.offering(t, sl), w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl, sl})
		insert(t, s, paid)
		pay(t, s, paid.o)
		unpaid := one(t, s, w, b, sl, t0)
		direct := directFixture(newFixture(b, t0, []offeringRef{w.offering(t, sl)}, []sellerRef{sl}))
		insert(t, s, direct)
		pay(t, s, direct.o)
		want := []uuid.UUID{paid.lines[0].ID, paid.lines[1].ID, paid.lines[2].ID}
		slices.SortFunc(want, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		var got []uuid.UUID
		after := uuid.Nil()
		for {
			page, err := s.CollectedLines(ctx, after, false, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range page {
				got = append(got, l.ID)
				if l.ID == unpaid.lines[0].ID || l.ID == direct.lines[0].ID {
					t.Fatalf("CollectedLines returned line %s of an unpaid or model C order", l.ID)
				}
			}
			if len(page) < 2 {
				break
			}
			after = page[len(page)-1].ID
		}
		if !slices.Equal(got, want) {
			t.Fatalf("CollectedLines = %v, want %v (line ID order, paged)", got, want)
		}
		if ls, err := s.CollectedLines(ctx, uuid.Nil(), true, 10); err != nil || len(ls) != 0 {
			t.Fatalf("CollectedLines(platform only) = %d, %v; the fixture's lines all have a payee", len(ls), err)
		}
	})

	t.Run("direct confirmation is a permanent model C fact", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		f := directFixture(newFixture(b, t0, []offeringRef{w.offering(t, sl)}, []sellerRef{sl}))
		insert(t, s, f)
		at := t0.Add(time.Minute)
		next := f.o
		next.DirectConfirmedAt = &at
		got, err := s.UpdateOrder(ctx, next, orders.StatusAwaitingPayment)
		if err != nil || got.DirectConfirmedAt == nil || !got.DirectConfirmedAt.Equal(at) {
			t.Fatalf("confirm=%+v,%v", got, err)
		}
		for _, stamp := range []*time.Time{nil, ptr(at.Add(time.Second))} {
			bad := got
			bad.DirectConfirmedAt = stamp
			if _, err := s.UpdateOrder(ctx, bad, orders.StatusAwaitingPayment); err == nil {
				t.Fatal("confirmation changed")
			}
		}
		got.Status, got.CancelledAt = orders.StatusCancelled, &at
		if kept, err := s.UpdateOrder(ctx, got, orders.StatusAwaitingPayment); err != nil || kept.DirectConfirmedAt == nil {
			t.Fatalf("cancel lost confirmation=%+v,%v", kept, err)
		}
		a := newFixture(b, t0, []offeringRef{w.offering(t, sl)}, []sellerRef{sl})
		insert(t, s, a)
		a.o.DirectConfirmedAt = &at
		if _, err := s.UpdateOrder(ctx, a.o, orders.StatusAwaitingPayment); err == nil {
			t.Fatal("model A confirmed directly")
		}
		late := directFixture(newFixture(b, t0, []offeringRef{w.offering(t, sl)}, []sellerRef{sl}))
		insert(t, s, late)
		late.o.Status, late.o.CancelledAt = orders.StatusCancelled, &at
		if _, err := s.UpdateOrder(ctx, late.o, orders.StatusAwaitingPayment); err != nil {
			t.Fatal(err)
		}
		late.o.DirectConfirmedAt = &at
		if _, err := s.UpdateOrder(ctx, late.o, orders.StatusCancelled); err == nil {
			t.Fatal("cancelled order accepted new confirmation")
		}
	})

	t.Run("model C: never paid out, refunds of the fee only", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		b, sl := w.buyer(t), w.seller(t)
		f := directFixture(newFixture(b, t0, []offeringRef{w.offering(t, sl), w.offering(t, sl)}, []sellerRef{sl, sl}))
		insert(t, s, f)
		pay(t, s, f.o)
		done := t0.Add(2 * time.Hour)
		fulfil(t, s, f.lines[0], done)
		if ls, err := s.PayableLines(ctx, sl.payee, done.Add(time.Hour)); err != nil || len(ls) != 0 {
			t.Fatalf("PayableLines of a model C order = %d, %v", len(ls), err)
		}
		c := f.lines[1]
		c.FulfillmentStatus = orders.FulfillCancelled
		if _, err := s.UpdateLine(ctx, c, orders.FulfillPending); err != nil {
			t.Fatal(err)
		}
		due, err := s.RefundsDue(ctx, 10)
		if err != nil || len(due) != 1 || *due[0].LineID != c.ID || due[0].Amount != 120_000 {
			t.Fatalf("RefundsDue = %+v, %v (the fee Koro holds)", due, err)
		}
		if ok, err := s.HasOpenCommerce(ctx, sl.org); err != nil || !ok {
			t.Fatalf("HasOpenCommerce(seller) with a fee to refund = %v, %v", ok, err)
		}
		c.RefundedAmount = 120_000
		if _, err := s.UpdateLine(ctx, c, orders.FulfillCancelled); err != nil {
			t.Fatal(err)
		}
		if due, _ := s.RefundsDue(ctx, 10); len(due) != 0 {
			t.Fatalf("RefundsDue after the fee refund = %+v", due)
		}
		// The delivered model C line waits for no payout: the seller is free.
		if ok, err := s.HasOpenCommerce(ctx, sl.org); err != nil || ok {
			t.Fatalf("HasOpenCommerce(seller) = %v, %v", ok, err)
		}
	})
}

// directFixture turns a fixture into a model C order (02 §18.3): the buyer
// pays the commission (10 % + 20 % VAT = 120,000 per line) to Koro and the
// gross to the payee directly.
func directFixture(f fixture) fixture {
	f.o.ModelCode, f.o.CollectionMethod = orders.ModelC, orders.CollectDirectToPayee
	f.o.TotalAmount, f.o.PayableAmount, f.o.BuyerFeeAmount = 0, 0, 0
	for i := range f.lines {
		l := &f.lines[i]
		l.CommissionPayer, l.BuyerFeeAmount = "buyer", l.CommissionAmount+l.CommissionVATAmount
		l.SellerNetAmount, l.PayableAmount = l.GrossAmount, l.GrossAmount+l.BuyerFeeAmount
		f.o.BuyerFeeAmount += l.BuyerFeeAmount
		f.o.TotalAmount += l.GrossAmount + l.BuyerFeeAmount
		f.o.PayableAmount += l.PayableAmount
	}
	f.o.KoroCollectAmount, f.o.DirectToPayeeAmount = f.o.BuyerFeeAmount, f.o.PayableAmount-f.o.BuyerFeeAmount
	return f
}

func ids(os []orders.Order) []uuid.UUID {
	out := make([]uuid.UUID, len(os))
	for i, o := range os {
		out[i] = o.ID
	}
	return slices.Clip(out)
}
