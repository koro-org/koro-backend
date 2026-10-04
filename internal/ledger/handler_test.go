package ledger_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/ledger"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// newHTTP serves the ledger operations of e with the persona resolver.
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(authztest.Personas()...)})
	if err != nil {
		t.Fatal(err)
	}
	ledger.Register(app.V1, e.svc)
	return app
}

func get(t *testing.T, app *httpx.App, persona string, q url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/ledger/transactions?"+q.Encode(), nil)
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func items(body map[string]any) []map[string]any {
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, x := range raw {
		out[i], _ = x.(map[string]any)
	}
	return out
}

// TestHTTP_ListTransactions: the journal DTO (04 §2.17), filters, date
// bounds in Europe/Istanbul and keyset pages.
func TestHTTP_ListTransactions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payee := uuid.NewV7()
	l := line(payee)
	order := e.paidOrder(t, l)
	// 2026-10-04 23:30 Istanbul is 20:30 UTC: inside the day "2026-10-04".
	late := time.Date(2026, 10, 4, 20, 30, 0, 0, time.UTC)
	payment := uuid.NewV7()
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, payment, 500, late)))
	app := newHTTP(t, e)

	code, body := get(t, app, "finance_admin", url.Values{"kind": {"order_allocated"}})
	its := items(body)
	if code != http.StatusOK || len(its) != 1 {
		t.Fatalf("kind filter: %d %v", code, body)
	}
	it := its[0]
	if it["kind"] != "order_allocated" || it["ref_type"] != "order" || it["ref_id"] != order.String() || it["transaction_id"] == "" {
		t.Fatalf("item %v", it)
	}
	entries, _ := it["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("entries %v", entries)
	}
	var total float64
	sawPayee := false
	for _, x := range entries {
		en, _ := x.(map[string]any)
		amount, _ := en["amount"].(float64)
		total += amount
		if en["currency"] != "TRY" || en["order_line_id"] != l.OrderLineID.String() || en["entry_id"] == "" {
			t.Fatalf("entry %v", en)
		}
		if en["account_code"] == "seller_payable" {
			sawPayee = en["payee_id"] == payee.String() && amount == -1_080_000
		} else if _, ok := en["payee_id"]; ok {
			t.Fatalf("payee on %v", en)
		}
	}
	if total != 0 || !sawPayee {
		t.Fatalf("entries sum %v, payee entry %v", total, sawPayee)
	}

	for _, c := range []struct {
		name string
		q    url.Values
		want int
	}{
		{"ref", url.Values{"ref_id": {payment.String()}}, 1},
		{"day bound includes 23:30 Istanbul", url.Values{"ref_type": {"payment"}, "occurred_from": {"2026-10-04"}, "occurred_to": {"2026-10-04"}}, 2},
		{"next day excludes it", url.Values{"occurred_from": {"2026-10-05"}}, 0},
		{"RFC 3339 end is inclusive", url.Values{"occurred_from": {"2026-10-04T20:30:00Z"}, "occurred_to": {"2026-10-04T20:30:00Z"}}, 1},
	} {
		code, body := get(t, app, "platform_admin", c.q)
		if code != http.StatusOK || len(items(body)) != c.want {
			t.Errorf("%s: %d, %d items, want %d", c.name, code, len(items(body)), c.want)
		}
	}

	// Pages of one.
	seen := 0
	q := url.Values{"limit": {"1"}}
	for range 5 {
		code, body := get(t, app, "finance_admin", q)
		if code != http.StatusOK || len(items(body)) != 1 {
			t.Fatalf("page: %d %v", code, body)
		}
		seen++
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		q.Set("cursor", next)
	}
	if seen != 3 {
		t.Fatalf("visited %d pages, want 3", seen)
	}
	// A cursor of another filter is 422 page.invalid_cursor.
	_, body = get(t, app, "finance_admin", url.Values{"limit": {"1"}})
	next, _ := body["next_cursor"].(string)
	if code, body = get(t, app, "finance_admin", url.Values{"limit": {"1"}, "kind": {"payment_received"}, "cursor": {next}}); code != http.StatusUnprocessableEntity || body["code"] != "page.invalid_cursor" {
		t.Fatalf("foreign cursor: %d %v", code, body)
	}
}

// TestHTTP_ListTransactions_ForbiddenBeforeInvalid: an actor without the
// role gets 403 even for a malformed query (03 §7.2: admin surface before
// validation); staff get 422 with the field. (huma's schema checks —
// format, enum, limit — run before every handler and stay 422.)
func TestHTTP_ListTransactions_ForbiddenBeforeInvalid(t *testing.T) {
	t.Parallel()
	app := newHTTP(t, newEnv(t))
	for _, q := range []url.Values{
		{"occurred_from": {"not-a-date"}},
		{"occurred_to": {"2026-13-01"}},
		{"cursor": {"garbage"}},
		{"occurred_from": {"2026-10-05"}, "occurred_to": {"2026-10-01"}},
	} {
		if code, body := get(t, app, "company_admin", q); code != http.StatusForbidden || body["code"] != "forbidden" {
			t.Errorf("company_admin %v: %d %v", q, code, body)
		}
		if code, body := get(t, app, "moderator", q); code != http.StatusForbidden {
			t.Errorf("moderator %v: %d %v", q, code, body)
		}
		if code, _ := get(t, app, "finance_admin", q); code != http.StatusUnprocessableEntity {
			t.Errorf("finance_admin %v: %d, want 422", q, code)
		}
	}
	if code, _ := get(t, app, "anonymous", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
}
