package orders_test

import (
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/orders"
)

// TestLists_ByEvent pins the event filter of listOrgOrders and listOrgSales
// (BE-199): only the orders (lines) of the event, cursor pages within the
// filter, an unknown event an empty list.
func TestLists_ByEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := uuid.NewV7()
	withEvent := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal, o.EventID = nil, &ev })
	plain := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	var mine []uuid.UUID
	for range 2 {
		mine = append(mine, e.place(t, line(withEvent, 1))[0].Order.ID)
		e.clk.Advance(time.Minute)
	}
	e.place(t, line(plain, 1))

	a := actor(t, "company_member")
	p, err := e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{EventID: &ev, Limit: 1})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != mine[1] || p.Next == nil {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	p, err = e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{EventID: &ev, Limit: 1, After: p.Next})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != mine[0] || p.Next != nil {
		t.Fatalf("second page = %+v, %v", p, err)
	}
	unknown := uuid.NewV7()
	if p, err := e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{EventID: &unknown}); err != nil || len(p.Items) != 0 {
		t.Fatalf("unknown event = %+v, %v", p, err)
	}

	club := actor(t, "club_admin")
	s, err := e.svc.ListOrgSales(ctxOf(t, club), club, org("club_a"), orders.SalesQuery{EventID: &ev})
	if err != nil || len(s.Items) != 2 {
		t.Fatalf("sales of the event = %+v, %v", s, err)
	}
	for _, it := range s.Items {
		if it.Line.EventID == nil || *it.Line.EventID != ev {
			t.Fatalf("sales of the event holds %+v", it.Line)
		}
	}
	if s, err := e.svc.ListOrgSales(ctxOf(t, club), club, org("club_a"), orders.SalesQuery{EventID: &unknown}); err != nil || len(s.Items) != 0 {
		t.Fatalf("sales of an unknown event = %+v, %v", s, err)
	}
}

// TestLists_ByEventHTTP pins the query parameter: a malformed event_id is
// 422, and the cursor carries the filter (a cursor of the unfiltered list
// is refused on the filtered one).
func TestLists_ByEventHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := uuid.NewV7()
	off := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal, o.EventID = nil, &ev })
	for range 2 {
		e.place(t, line(off, 1))
		e.clk.Advance(time.Minute)
	}
	app := newHTTP(t, e)
	orgs := "/api/v1/orgs/" + org("company_a").String() + "/orders"
	sales := "/api/v1/orgs/" + org("club_a").String() + "/sales"

	for persona, path := range map[string]string{"company_admin": orgs + "?event_id=nope", "club_admin": sales + "?event_id=nope"} {
		if rec, _ := call(t, app, persona, http.MethodGet, path, nil, nil); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422", path, rec.Code)
		}
	}

	rec, body := call(t, app, "company_admin", http.MethodGet, orgs+"?event_id="+ev.String(), nil, nil)
	if items, _ := body["items"].([]any); rec.Code != http.StatusOK || len(items) != 2 {
		t.Fatalf("by event = %d %v", rec.Code, body)
	}
	rec, body = call(t, app, "company_admin", http.MethodGet, orgs+"?limit=1", nil, nil)
	next, _ := body["next_cursor"].(string)
	if rec.Code != http.StatusOK || next == "" {
		t.Fatalf("unfiltered page = %d %v", rec.Code, body)
	}
	rec, body = call(t, app, "company_admin", http.MethodGet, orgs+"?event_id="+ev.String()+"&cursor="+next, nil, nil)
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "page.invalid_cursor" {
		t.Fatalf("foreign cursor = %d %v, want 422 page.invalid_cursor", rec.Code, body)
	}
	rec, body = call(t, app, "club_admin", http.MethodGet, sales+"?event_id="+ev.String(), nil, nil)
	if items, _ := body["items"].([]any); rec.Code != http.StatusOK || len(items) != 2 {
		t.Fatalf("sales by event = %d %v", rec.Code, body)
	}
}
