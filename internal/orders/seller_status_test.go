package orders_test

import (
	"net/http"
	"strings"
	"testing"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/money"
)

// Q-31 (user decision 2026-10-27; BE-141, T-383): on an order of several
// sellers a seller sees only its own lines' status. Q-32a (T-388): the
// notices of a line dropped from a model C order warn about a payment made
// directly to the seller.

// TestSellerStatus_Rule pins the seller's status of an order from its own
// lines: the whole order's states (unpaid, cancelled, expired, refunded)
// as they are, every other one from the seller's lines by progressStatus's
// rule — never the order's disputed, partially_refunded, in_fulfilment or
// completed when they come from another seller's lines.
func TestSellerStatus_Rule(t *testing.T) {
	t.Parallel()
	l := func(f orders.Fulfillment, refunded int64) orders.Line {
		return orders.Line{FulfillmentStatus: f, PayableAmount: 1000, BuyerFeeAmount: 100, RefundedAmount: refunded}
	}
	dropped := orders.Line{FulfillmentStatus: orders.FulfillCancelled, PayableAmount: 1000, DroppedAt: &t0}
	ful, can, pen := orders.FulfillFulfilled, orders.FulfillCancelled, orders.FulfillPending
	mar, dis := orders.FulfillMarked, orders.FulfillDisputed
	a, c := orders.CollectBankTransfer, orders.CollectDirectToPayee
	for _, tc := range []struct {
		name   string
		method string
		order  orders.Status
		own    []orders.Line
		want   orders.Status
	}{
		{"unpaid", a, orders.StatusAwaitingPayment, []orders.Line{l(pen, 0)}, orders.StatusAwaitingPayment},
		{"unpaid, the seller's every line dropped", a, orders.StatusAwaitingPayment, []orders.Line{dropped}, orders.StatusCancelled},
		{"paid later, the seller's every line dropped", a, orders.StatusCompleted, []orders.Line{dropped}, orders.StatusCancelled},
		{"expired", a, orders.StatusExpired, []orders.Line{l(can, 0)}, orders.StatusExpired},
		{"cancelled", a, orders.StatusCancelled, []orders.Line{l(can, 0)}, orders.StatusCancelled},
		{"all of the order's money went back", a, orders.StatusRefunded, []orders.Line{l(pen, 0)}, orders.StatusRefunded},
		{"another seller's line disputed", a, orders.StatusDisputed, []orders.Line{l(pen, 0)}, orders.StatusPaid},
		{"the seller's line disputed", a, orders.StatusDisputed, []orders.Line{l(dis, 0)}, orders.StatusDisputed},
		{"another seller's line marked", a, orders.StatusInFulfilment, []orders.Line{l(pen, 0)}, orders.StatusPaid},
		{"another seller's line refunded, the seller's marked", a, orders.StatusPartiallyRefunded, []orders.Line{l(mar, 0)}, orders.StatusInFulfilment},
		{"another seller's line refunded, the seller's delivered", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0)}, orders.StatusCompleted},
		{"another seller still delivering", a, orders.StatusInFulfilment, []orders.Line{l(ful, 0), dropped}, orders.StatusCompleted},
		{"the seller's line refunded in part, another pending", a, orders.StatusPartiallyRefunded, []orders.Line{l(pen, 0), l(can, 1000)}, orders.StatusPartiallyRefunded},
		{"the seller's lines delivered or refunded", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(can, 1000)}, orders.StatusCompleted},
		{"the seller's every line refunded", a, orders.StatusPartiallyRefunded, []orders.Line{l(can, 1000), l(pen, 1000)}, orders.StatusRefunded},
		{"delivered, then refunded in full", a, orders.StatusCompleted, []orders.Line{l(ful, 1000)}, orders.StatusRefunded},
		{"completed", a, orders.StatusCompleted, []orders.Line{l(ful, 0)}, orders.StatusCompleted},
		{"model C: the seller's fee refunded", c, orders.StatusPartiallyRefunded, []orders.Line{l(pen, 100)}, orders.StatusRefunded},
		{"model C: one fee of two refunded in part", c, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(pen, 50)}, orders.StatusPartiallyRefunded},
		{"model C: another seller's fee refunded", c, orders.StatusPartiallyRefunded, []orders.Line{l(mar, 0)}, orders.StatusInFulfilment},
	} {
		o := orders.Order{Status: tc.order, CollectionMethod: tc.method}
		if got := orders.SellerStatus(o, tc.own); got != tc.want {
			t.Errorf("%s: order %s → seller %s, want %s", tc.name, tc.order, got, tc.want)
		}
	}
	// An order of one seller whose refunds name its lines reads the same for
	// the seller as for the buyer (progressStatus, the same rule).
	for _, tc := range []struct {
		order orders.Status
		lines []orders.Line
	}{
		{orders.StatusPaid, []orders.Line{l(pen, 0), l(can, 0)}},
		{orders.StatusInFulfilment, []orders.Line{l(ful, 0), l(mar, 0)}},
		{orders.StatusDisputed, []orders.Line{l(ful, 0), l(dis, 0)}},
		{orders.StatusPartiallyRefunded, []orders.Line{l(pen, 0), l(can, 1000)}},
		{orders.StatusCompleted, []orders.Line{l(ful, 500), l(can, 1000)}},
	} {
		o := orders.Order{Status: tc.order, CollectionMethod: a}
		if buyer, seller := orders.ProgressStatus(o, tc.lines), orders.SellerStatus(o, tc.lines); buyer != seller || buyer != tc.order {
			t.Errorf("one seller, %s: buyer %s, seller %s", tc.order, buyer, seller)
		}
	}
}

// sellerBody reads path as persona and fails on anything of the other
// seller (hidden: its line, offering, title, org) or of a dispute (the
// disputed status, a reason) or a refund (a status or an amount) in the
// raw answer.
func sellerBody(t *testing.T, app *httpx.App, persona, method, path string, body any, hidden ...string) map[string]any {
	t.Helper()
	rec, out := call(t, app, persona, method, path, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s as %s: %d %s", method, path, persona, rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, h := range append(hidden, `"disputed"`, "dispute_reason", "refund") {
		if strings.Contains(raw, h) {
			t.Fatalf("%s %s as %s leaks %q: %s", method, path, persona, h, raw)
		}
	}
	return out
}

// TestSellerView_TwoSellers_OtherSellersDisputeAndRefundHidden (Q-31): a
// cart of a club_a stand and a club_b workshop is paid; club_b's line is
// marked, disputed, resolved with a refund and refunded, while the order
// goes disputed → paid → partially_refunded → completed. club_a's getOrder,
// listOrgSales and mark answers carry only its own line and the status of
// its own line (paid, in_fulfilment, completed), never club_b's line, a
// dispute or a refund; club_b sees its own (disputed, refunded); the buyer
// and staff see the order's status as before. The sellers' notices carry
// the order's ID only and club_a gets none about club_b's line.
func TestSellerView_TwoSellers_OtherSellersDisputeAndRefundHidden(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payeeB := e.payees.add("club_b", parties.PayeeActive)
	offA := e.offering(t, nil)
	offB := e.offering(t, func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID, o.Title = org("club_b"), &payeeB, "Atölye" })
	v := e.place(t, line(offA, 1), line(offB, 1))[0]
	id := v.Order.ID
	e.pay(t, id)
	lA, lB := lineOf(t, v, offA), lineOf(t, v, offB)
	app := newHTTP(t, e)
	orderPath, salesA, salesB := "/api/v1/orders/"+id.String(), "/api/v1/orgs/"+org("club_a").String()+"/sales",
		"/api/v1/orgs/"+org("club_b").String()+"/sales"
	hidden := []string{lB.ID.String(), offB.String(), "Atölye", org("club_b").String()}

	// seen checks what club_a, club_b and the buyer read now.
	seen := func(step string, a, b, buyer orders.Status) {
		t.Helper()
		if got := e.order(t, id).Status; got != buyer {
			t.Fatalf("%s: order status %s, want %s", step, got, buyer)
		}
		mine := sellerBody(t, app, "club_admin", http.MethodGet, orderPath, nil, hidden...)
		if mine["view"] != "seller" || mine["status"] != string(a) || mine["seller_status"] != string(a) || len(mine["lines"].([]any)) != 1 {
			t.Fatalf("%s: club_a getOrder = %v", step, mine)
		}
		items := sellerBody(t, app, "club_admin", http.MethodGet, salesA, nil, hidden...)["items"].([]any)
		if it := items[0].(map[string]any); len(items) != 1 || it["seller_status"] != string(a) || it["order_status"] != string(a) {
			t.Fatalf("%s: club_a listOrgSales = %v", step, items)
		}
		rec, other := call(t, app, "other_club_admin", http.MethodGet, orderPath, nil, nil)
		if rec.Code != http.StatusOK || other["status"] != string(b) || other["seller_status"] != string(b) || strings.Contains(rec.Body.String(), lA.ID.String()) {
			t.Fatalf("%s: club_b getOrder = %d %s", step, rec.Code, rec.Body)
		}
		rec, sales := call(t, app, "other_club_admin", http.MethodGet, salesB, nil, nil)
		if it := sales["items"].([]any)[0].(map[string]any); rec.Code != http.StatusOK || it["seller_status"] != string(b) {
			t.Fatalf("%s: club_b listOrgSales = %d %s", step, rec.Code, rec.Body)
		}
		for _, p := range []string{"company_admin", "finance_admin"} {
			rec, full := call(t, app, p, http.MethodGet, orderPath, nil, nil)
			if _, ok := full["seller_status"]; rec.Code != http.StatusOK || full["view"] != "buyer" || full["status"] != string(buyer) || ok {
				t.Fatalf("%s: %s getOrder = %d %s", step, p, rec.Code, rec.Body)
			}
		}
		sv, err := e.svc.GetOrder(ctxOf(t, actor(t, "club_admin")), actor(t, "club_admin"), id)
		if err != nil || !sv.SellerView || sv.SellerStatus != a || len(sv.Lines) != 1 || sv.Lines[0].ID != lA.ID {
			t.Fatalf("%s: club_a service view = %+v, %v", step, sv, err)
		}
	}
	seen("paid", orders.StatusPaid, orders.StatusPaid, orders.StatusPaid)

	ob, buyer, fin := actor(t, "other_club_admin"), actor(t, "company_admin"), actor(t, "finance_admin")
	if lv, err := e.svc.MarkLineFulfilled(ctxOf(t, ob), ob, lB.ID, orders.MarkInput{}); err != nil || !lv.SellerView || lv.OrderStatus != orders.StatusInFulfilment {
		t.Fatalf("club_b marks = %+v, %v", lv, err)
	}
	seen("club_b marked", orders.StatusPaid, orders.StatusInFulfilment, orders.StatusInFulfilment)
	if lv, err := e.svc.DisputeLine(ctxOf(t, buyer), buyer, lB.ID, "Atölye yapılmadı"); err != nil || lv.SellerView || lv.OrderStatus != orders.StatusDisputed {
		t.Fatalf("buyer disputes = %+v, %v", lv, err)
	}
	seen("club_b disputed", orders.StatusPaid, orders.StatusDisputed, orders.StatusDisputed)
	if lv, err := e.svc.AdminResolveDispute(ctxOf(t, fin), fin, lB.ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "Atölye yoktu"}); err != nil ||
		lv.SellerView || lv.OrderStatus != orders.StatusPaid {
		t.Fatalf("resolved = %+v, %v", lv, err)
	}
	if err := e.mod.Orders.ApplyRefund(t.Context(), id, &lB.ID, lB.PayableAmount); err != nil {
		t.Fatal(err)
	}
	seen("club_b refunded", orders.StatusPaid, orders.StatusRefunded, orders.StatusPartiallyRefunded)

	// club_a marks its line: it reads its own lines' status back, not the
	// order's partially_refunded.
	marked := sellerBody(t, app, "club_admin", http.MethodPost, "/api/v1/order-lines/"+lA.ID.String()+"/mark-fulfilled",
		map[string]any{"note": "Stant kuruldu"}, hidden...)
	if marked["order_status"] != "in_fulfilment" || marked["seller_status"] != "in_fulfilment" {
		t.Fatalf("club_a mark = %v", marked)
	}
	seen("club_a marked", orders.StatusInFulfilment, orders.StatusRefunded, orders.StatusPartiallyRefunded)
	rec, confirmed := call(t, app, "company_admin", http.MethodPost, "/api/v1/order-lines/"+lA.ID.String()+"/confirm", nil, nil)
	if _, ok := confirmed["seller_status"]; rec.Code != http.StatusOK || confirmed["order_status"] != "completed" || ok {
		t.Fatalf("buyer confirm = %d %s", rec.Code, rec.Body)
	}
	seen("completed", orders.StatusCompleted, orders.StatusRefunded, orders.StatusCompleted)

	// The sellers' notices: fixed texts, the order's ID only; club_a's admin
	// is told of the order and of its own delivery, nothing of club_b's line.
	for _, topic := range []string{domainevent.TopicOrderPlaced, domainevent.TopicOrderLineFulfillmentMarked, domainevent.TopicOrderLineFulfilled} {
		e.deliver(t, topic)
	}
	clubA := actor(t, "club_admin").UserID
	got := map[string]int{}
	for _, m := range e.sender.all() {
		if *m.UserID != clubA {
			continue
		}
		got[m.Template]++
		if *m.SourceOrgID != org("club_a") || len(m.Data) != 1 || m.Data["order_id"] != id.String() || m.LinkPath != notify.OrgSalesLink(org("club_a")) {
			t.Errorf("club_a notice %s = %+v", m.Template, m)
		}
	}
	if len(got) != 2 || got["orders.order_placed"] != 1 || got["orders.line_fulfilled"] != 1 {
		t.Fatalf("club_a notices = %v", got)
	}
}

// TestLineDroppedNotice_ModelC_DirectPaymentWarning (Q-32a): a club_b line
// dropped from an unpaid model C cart tells the buyer — in-app and by
// e-mail, in Turkish and English — to ask the seller for the refund of a
// direct payment (the seller, the line's direct amount, the fee still due
// to Koro), and club_b's admins in-app and by e-mail to refund a direct
// payment they received (the reference, the buyer's title, the line's
// direct amount; nothing of the other line). A model A drop keeps its
// notices without the warning.
func TestLineDroppedNotice_ModelC_DirectPaymentWarning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.payees.add("club_b", parties.PayeeActive)
	aOff := e.offering(t, nil)
	bOff := e.offering(t, func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID, o.Title = org("club_b"), &b, "Atölye" })
	for _, id := range []uuid.UUID{aOff, bOff} {
		e.addPolicy(t, catalog.ScopeOffering, &id, modelCPolicy)
	}
	v := e.place(t, line(aOff, 2), line(bOff, 1))[0]
	e.cancelAs(t, "other_club_admin", bOff)
	e.runDropNotices(t)
	o := e.order(t, v.Order.ID)
	// Line B: 1,000,000 + 20 % VAT = 1,200,000 to the payee; line A's fee
	// (10 % of 2,000,000 + 20 % VAT) is what Koro still waits for.
	if o.KoroCollectAmount != 240_000 || o.Status != orders.StatusAwaitingPayment || !o.DirectToPayee() {
		t.Fatalf("model C order after the drop = %+v", o)
	}
	buyer, seller := actor(t, "company_admin").UserID, actor(t, "other_club_admin").UserID
	var buyerMail, sellerMail notify.Message
	var buyerInApp, sellerInApp int
	for _, m := range e.sender.all() {
		switch {
		case m.Template == "orders.line_dropped_direct" && m.Channel == notify.ChannelEmail && *m.UserID == buyer:
			buyerMail = m
		case m.Template == "orders.line_dropped_direct" && m.Channel == notify.ChannelInApp && *m.UserID == buyer:
			buyerInApp++
			if m.LinkPath != notify.OrderLink(o.ID) {
				t.Errorf("buyer link = %v", m.LinkPath)
			}
		case m.Template == "orders.sale_line_dropped_direct" && m.Channel == notify.ChannelEmail && *m.UserID == seller:
			sellerMail = m
		case m.Template == "orders.sale_line_dropped_direct" && m.Channel == notify.ChannelInApp && *m.UserID == seller:
			sellerInApp++
			if m.LinkPath != notify.OrgSalesLink(org("club_b")) || *m.SourceOrgID != org("club_b") {
				t.Errorf("seller in-app = %+v", m)
			}
		default:
			t.Errorf("unexpected notice %s (%s) to %s", m.Template, m.Channel, m.UserID)
		}
	}
	direct := money.Decimal(1_200_000)
	if buyerInApp != 1 || sellerInApp != 1 || buyerMail.Data["direct_amount"] != direct || buyerMail.Data["seller_name"] != "Org club_b" ||
		buyerMail.Data["amount"] != money.Decimal(240_000) || buyerMail.Data["koro_due"] != true || buyerMail.Data["line_title"] != "Atölye" {
		t.Fatalf("buyer notices: in-app %d, e-mail %+v", buyerInApp, buyerMail.Data)
	}
	want := map[string]any{"name": "other_club_admin", "reference_code": o.OrderNo, "line_title": "Atölye",
		"buyer_title": o.BuyerSnapshot.Title, "direct_amount": direct, "currency": "TRY"}
	if *sellerMail.SourceOrgID != org("club_b") || len(sellerMail.Data) != len(want) {
		t.Fatalf("seller e-mail = %+v", sellerMail)
	}
	for k, w := range want {
		if sellerMail.Data[k] != w {
			t.Errorf("seller e-mail %s = %v, want %v", k, sellerMail.Data[k], w)
		}
	}

	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	warn := map[string]map[string]string{
		"orders.line_dropped_direct": {"tr": "Bu kalem için satıcıya doğrudan ödeme yaptıysanız iadeyi satıcıdan isteyin",
			"en": "If you already paid the seller directly for this line, ask the seller for the refund"},
		"orders.sale_line_dropped_direct": {"tr": "Bu kalem için alıcıdan doğrudan ödeme aldıysanız iade edin",
			"en": "If you received a direct payment from the buyer for this line, refund it"},
	}
	data := map[string]map[string]any{"orders.line_dropped_direct": buyerMail.Data, "orders.sale_line_dropped_direct": sellerMail.Data}
	for id, byLoc := range warn {
		if meta, ok := tpl.Meta(id); !ok || meta.Category != notify.CategoryTransactional || meta.Preference != notify.PrefOrdersPayments ||
			!meta.Supports(notify.ChannelEmail) || !meta.Supports(notify.ChannelInApp) {
			t.Fatalf("%s meta = %+v, %v", id, meta, ok)
		}
		for loc, w := range byLoc {
			r, err := tpl.Render(id, loc, notify.RenderInput{Data: data[id]})
			if err != nil {
				t.Fatalf("Render(%s, %s): %v", id, loc, err)
			}
			for _, s := range []string{w, direct, "Atölye", o.OrderNo} {
				if !strings.Contains(r.Text, s) || !strings.Contains(r.HTML, s) {
					t.Errorf("Render(%s, %s) misses %q:\n%s", id, loc, s, r.Text)
				}
			}
			if n, ok := tpl.Notice(id, loc); !ok || !strings.Contains(n.Body, w) {
				t.Errorf("Notice(%s, %s) = %+v: misses the warning", id, loc, n)
			}
		}
	}
	// The buyer's e-mail names the seller to ask; the seller's names the buyer.
	if r, _ := tpl.Render("orders.line_dropped_direct", "tr", notify.RenderInput{Data: buyerMail.Data}); !strings.Contains(r.Text, "Org club_b") ||
		!strings.Contains(r.Text, money.Decimal(240_000)) {
		t.Errorf("buyer e-mail:\n%s", r.Text)
	}
	if r, _ := tpl.Render("orders.sale_line_dropped_direct", "tr", notify.RenderInput{Data: sellerMail.Data}); !strings.Contains(r.Text, o.BuyerSnapshot.Title) {
		t.Errorf("seller e-mail:\n%s", r.Text)
	}
	// Model A notices carry no direct payment warning.
	for _, id := range []string{"orders.line_dropped", "orders.sale_line_dropped"} {
		for loc, w := range map[string]string{"tr": "doğrudan ödeme", "en": "directly"} {
			if n, _ := tpl.Notice(id, loc); strings.Contains(n.Body, w) {
				t.Errorf("model A notice %s (%s) warns: %s", id, loc, n.Body)
			}
		}
	}
}
