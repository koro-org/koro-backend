//go:build integration

package orders_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orders"
)

// TestSellerStatus_TwoSellers_Postgres (Q-31, BE-141): on PostgreSQL a
// cart of a booth (seller 1) and a workshop (seller 2) is paid; seller 2's
// line is marked, disputed and refunded in part, so the order is disputed
// and then partially_refunded. Seller 1's getOrder and listOrgSales (the
// GORM store's lines of the page's orders) read paid, and its own mark
// answers in_fulfilment, never the order's states that come from seller 2's
// line; seller 2 reads its own.
func TestSellerStatus_TwoSellers_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	std := cartVAT{rate: 2000, class: "standard"}
	booth := p.cartOffering(t, p.seller, std, 1_000_000, nil)
	workshop := p.cartOffering(t, s2, std, 500_000, nil)
	bctx := p.buyerActor(t)
	buyer, err := authz.ActorFrom(bctx)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := p.create(bctx, line(booth, 1), line(workshop, 1))
	if err != nil || len(vs) != 1 {
		t.Fatalf("CreateOrder: %d orders, %v", len(vs), err)
	}
	v := vs[0]
	if _, err := p.mod.Orders.ApplyPayment(t.Context(), v.Order.ID, uuid.NewV7(), v.Order.PayableAmount); err != nil {
		t.Fatal(err)
	}
	bo, ws := lineOf(t, v, booth), lineOf(t, v, workshop)
	seller1, s1ctx := p.sellerActor(t)
	seller2 := actor(t, "club_admin")
	seller2.UserID = pgUser(t, p.d)
	seller2.Memberships = []authz.Membership{{OrgID: s2.org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}
	s2ctx := ctxOf(t, seller2)

	seen := func(step string, want orders.Status, order orders.Status) {
		t.Helper()
		if o, err := p.mod.Orders.ForPayment(s1ctx, v.Order.ID); err != nil || o.Status != order {
			t.Fatalf("%s: the order is %s (%v), want %s", step, o.Status, err, order)
		}
		sv, err := p.mod.Service.GetOrder(s1ctx, seller1, v.Order.ID)
		if err != nil || !sv.SellerView || sv.SellerStatus != want || len(sv.Lines) != 1 || sv.Lines[0].ID != bo.ID {
			t.Fatalf("%s: seller 1 getOrder = %+v, %v", step, sv, err)
		}
		page, err := p.mod.Service.ListOrgSales(s1ctx, seller1, p.seller.org, orders.SalesQuery{})
		if err != nil || len(page.Items) != 1 || page.Items[0].Line.ID != bo.ID || page.Items[0].SellerStatus != want {
			t.Fatalf("%s: seller 1 listOrgSales = %+v, %v", step, page, err)
		}
	}
	seen("paid", orders.StatusPaid, orders.StatusPaid)
	if lv, err := p.mod.Service.MarkLineFulfilled(s2ctx, seller2, ws.ID, orders.MarkInput{}); err != nil || lv.OrderStatus != orders.StatusInFulfilment {
		t.Fatalf("seller 2 marks = %+v, %v", lv, err)
	}
	if _, err := p.mod.Service.DisputeLine(bctx, buyer, ws.ID, "Atölye yapılmadı"); err != nil {
		t.Fatal(err)
	}
	seen("seller 2's line disputed", orders.StatusPaid, orders.StatusDisputed)
	if err := p.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &ws.ID, ws.PayableAmount/2); err != nil {
		t.Fatal(err)
	}
	seen("seller 2's line refunded in part", orders.StatusPaid, orders.StatusPartiallyRefunded)
	if sv, err := p.mod.Service.GetOrder(s2ctx, seller2, v.Order.ID); err != nil || sv.SellerStatus != orders.StatusPartiallyRefunded {
		t.Fatalf("seller 2 getOrder = %+v, %v", sv, err)
	}
	lv, err := p.mod.Service.MarkLineFulfilled(s1ctx, seller1, bo.ID, orders.MarkInput{})
	if err != nil || !lv.SellerView || lv.OrderStatus != orders.StatusInFulfilment {
		t.Fatalf("seller 1 marks = %+v, %v", lv, err)
	}
	seen("seller 1 marked", orders.StatusInFulfilment, orders.StatusPartiallyRefunded)
}
