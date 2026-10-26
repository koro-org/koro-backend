package orders_test

import (
	"net/http"
	"testing"
	"uuid"
)

// TestSellerView_ModelPaidAtCurrency (BE-127, 03 k26): the seller reads how
// (model_code) and when (paid_at) its order was paid, never the totals or
// the buyer's payment details; its sales lines name their currency.
func TestSellerView_ModelPaidAtCurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.place(t, line(e.offering(t, nil), 1))[0].Order
	app := newHTTP(t, e)
	path := "/api/v1/orders/" + o.ID.String()
	rec, body := call(t, app, "club_admin", http.MethodGet, path, nil, nil)
	if rec.Code != http.StatusOK || body["view"] != "seller" || body["model_code"] != o.ModelCode || body["model_code"] == "" {
		t.Fatalf("seller view: %d %s", rec.Code, rec.Body)
	}
	for _, k := range []string{"paid_at", "total_amount", "payable_amount", "paid_amount", "buyer_snapshot", "expires_at"} {
		if _, ok := body[k]; ok {
			t.Fatalf("unpaid seller view carries %s: %s", k, rec.Body)
		}
	}
	if eff, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), o.PayableAmount); err != nil || !eff.Paid {
		t.Fatalf("ApplyPayment = %+v, %v", eff, err)
	}
	rec, body = call(t, app, "club_admin", http.MethodGet, path, nil, nil)
	if _, ok := body["paid_at"]; rec.Code != http.StatusOK || !ok {
		t.Fatalf("paid seller view: %d %s", rec.Code, rec.Body)
	}
	if _, ok := body["paid_amount"]; ok {
		t.Fatalf("paid seller view carries paid_amount: %s", rec.Body)
	}
	rec, body = call(t, app, "club_admin", http.MethodGet, "/api/v1/orgs/"+org("club_a").String()+"/sales", nil, nil)
	items, _ := body["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["currency"] != o.Currency || o.Currency == "" {
		t.Fatalf("sales: %d %s", rec.Code, rec.Body)
	}
}
