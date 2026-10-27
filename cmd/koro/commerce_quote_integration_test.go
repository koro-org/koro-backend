//go:build integration

package main

import (
	"net/http"
	"reflect"
)

// quote previews cart over the whole API stack (BE-136: no Idempotency-Key,
// outside the terms gate, rate class search) and checks that it wrote
// nothing: no new order of the company (the seed has some), no unit sold of
// the offerings.
func (e *commerceEnv) quote(cart map[string]any, offerings ...string) map[string]any {
	e.t.Helper()
	before := e.count(`SELECT count(*) FROM orders WHERE org_id=$1`, e.company)
	out := e.c.do(http.MethodPost, "/api/v1/orgs/"+e.company+"/orders/quote", e.buyer, cart, nil, http.StatusOK)
	if n := e.count(`SELECT count(*) FROM orders WHERE org_id=$1`, e.company); n != before {
		e.t.Fatalf("the quote wrote %d orders", n-before)
	}
	for _, off := range offerings {
		if n := e.count(`SELECT quantity_sold FROM offerings WHERE id=$1`, off); n != 0 {
			e.t.Fatalf("the quote sold %d units of %s", n, off)
		}
	}
	return out
}

// quoteMatches checks that every field of the quote has the same name and
// value in createOrder's answer, order by order and line by line.
func (e *commerceEnv) quoteMatches(quote, placed map[string]any) {
	e.t.Helper()
	qs, ps := quote["orders"].([]any), placed["orders"].([]any)
	if len(qs) != len(ps) {
		e.t.Fatalf("quote %d orders, placement %d", len(qs), len(ps))
	}
	for i := range ps {
		q, p := qs[i].(map[string]any), ps[i].(map[string]any)
		for k, v := range q {
			if k != "lines" && !reflect.DeepEqual(v, p[k]) {
				e.t.Errorf("order %d %s: quote %v, placement %v", i, k, v, p[k])
			}
		}
		ql, pl := q["lines"].([]any), p["lines"].([]any)
		if len(ql) != len(pl) {
			e.t.Fatalf("order %d: quote %d lines, placement %d", i, len(ql), len(pl))
		}
		for j := range pl {
			for k, v := range ql[j].(map[string]any) {
				if w := pl[j].(map[string]any)[k]; !reflect.DeepEqual(v, w) {
					e.t.Errorf("order %d line %d %s: quote %v, placement %v", i, j, k, v, w)
				}
			}
		}
	}
}
