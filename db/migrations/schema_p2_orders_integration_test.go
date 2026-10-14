//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_OrderConstraints (02 §19.1, 06 H-6): the order amounts add up
// (total, payable, Koro / direct split, withholding), money arrives only
// up to what Koro collects and goes back only up to what arrived; the paid
// states carry paid_at and the full payment (06 H-37: paid_at belongs to
// them only); the billed party belongs to the buyer (06 H-38).
func TestSchema_OrderConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	// insert writes an order with one default line; arg replaces the
	// parameters of orderStmt by index (0 id, 1 org, 2 placer, 3 party, 4 no).
	insert := func(name, constraint, state string, over map[string]string, arg map[int]any) {
		t.Helper()
		id := uuid.NewV7()
		o := w.orderStmt(s, id, over)
		for i, v := range arg {
			o.args[i] = v
		}
		line := lineStmt(s, uuid.NewV7(), id, nil)
		if org, ok := arg[1]; ok {
			line.args[2] = org
		}
		expectCommit(t, w, name, state, constraint, o, line)
	}
	for _, c := range []struct {
		name, constraint string
		over             map[string]string
	}{
		{"a total that is not the sum", "orders_total_chk", map[string]string{"total_amount": "1200001", "payable_amount": "1200001", "koro_collect_amount": "1200001"}},
		{"a payable that ignores the withholding", "orders_payable_chk", map[string]string{"vat_withheld_amount": "100000"}},
		{"withholding above the VAT", "orders_withheld_chk", map[string]string{"vat_withheld_amount": "200001", "payable_amount": "999999", "koro_collect_amount": "999999"}},
		{"a split that does not add up", "orders_collect_split_chk", map[string]string{"koro_collect_amount": "1000000"}},
		{"a direct amount outside model C", "orders_direct_model_chk", map[string]string{"koro_collect_amount": "200000", "direct_to_payee_amount": "1000000"}},
		{"model C collected by Koro", "orders_model_method_chk", map[string]string{"model_code": "'C'"}},
		{"paid more than Koro collects", "orders_paid_limit_chk", map[string]string{"paid_amount": "1200001"}},
		{"refunded more than paid", "orders_refunded_limit_chk", map[string]string{"paid_amount": "100", "refunded_amount": "101"}},
		{"paid without paid_at", "orders_paid_chk", map[string]string{"status": "'paid'", "paid_amount": "1200000"}},
		{"partial: awaiting payment with paid_at", "orders_paid_chk", map[string]string{"paid_at": "now()"}},
		{"partial: cancelled with paid_at", "orders_paid_chk", map[string]string{"status": "'cancelled'", "cancelled_at": "now()", "paid_at": "now()"}},
		{"disputed without paid_at", "orders_paid_chk", map[string]string{"status": "'disputed'", "paid_amount": "1200000"}},
		{"paid with a partial payment", "orders_paid_full_chk", map[string]string{"status": "'paid'", "paid_at": "now()", "paid_amount": "1000000"}},
		{"refunded with money left", "orders_refunded_chk", map[string]string{"status": "'refunded'", "paid_at": "now()", "paid_amount": "1200000", "refunded_amount": "1000"}},
		{"partially refunded with nothing refunded", "orders_partially_refunded_chk", map[string]string{"status": "'partially_refunded'", "paid_at": "now()", "paid_amount": "1200000"}},
		{"completed without completed_at", "orders_completed_chk", map[string]string{"status": "'completed'", "paid_at": "now()", "paid_amount": "1200000"}},
		{"cancelled without cancelled_at", "orders_cancelled_chk", map[string]string{"status": "'cancelled'"}},
		{"partial: open with cancelled_at", "orders_cancelled_chk", map[string]string{"cancelled_at": "now()"}},
		{"expires before it is placed", "orders_expires_chk", map[string]string{"expires_at": "now() - interval '1 second'"}},
		{"a snapshot that is not an object", "orders_buyer_snapshot_chk", map[string]string{"buyer_snapshot": "'[]'::jsonb"}},
		{"a negative amount", "orders_vat_amount_chk", map[string]string{"vat_amount": "-1", "total_amount": "999999", "payable_amount": "999999", "koro_collect_amount": "999999"}},
	} {
		insert(c.name, c.constraint, chk, c.over, nil)
	}
	insert("a reference that is not KR + 8", "orders_order_no_chk", chk, nil, map[int]any{4: "KR1234"})
	insert("a reference with I (not Crockford)", "orders_order_no_chk", chk, nil, map[int]any{4: "KR0000000I"})
	other := w.buyer()
	insert("a party of another company", "orders_buyer_party_id_org_id_fkey", db.CodeForeignKeyViolation, nil, map[int]any{3: other.party})
	club := s.seller.org
	clubParty := w.legalParty("buyer_company", "company", &club)
	insert("an org that is not a company", "orders_org_id_org_kind_fkey", db.CodeForeignKeyViolation, nil, map[int]any{1: club, 3: clubParty})

	// The valid life of an order.
	order, _ := w.order(s)
	w.markPaid(order)
	w.exec(`UPDATE orders SET status = 'partially_refunded', refunded_amount = 1000 WHERE id = $1`, order)
	w.exec(`UPDATE orders SET status = 'refunded', refunded_amount = paid_amount WHERE id = $1`, order)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a duplicate reference", `UPDATE orders SET order_no = (SELECT order_no FROM orders WHERE id <> $1 LIMIT 1) WHERE id = $1`, []any{order}, db.CodeRestrictViolation, ""},
		{"app_rw deletes an order", `DELETE FROM orders`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_OrderTotalsMatchLines (06 H-6, BE-034): at commit an order has
// at least one line and its subtotal, VAT, withholding, buyer fee and
// payable amounts are the sums of its lines (deferred constraint trigger
// orders_totals_match_lines), whatever order the rows are written in.
func TestSchema_OrderTotalsMatchLines(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	const name = "orders_totals_match_lines"
	id := uuid.NewV7()
	expectCommit(t, w, "an order without lines", db.CodeCheckViolation, name, w.orderStmt(s, id, nil))
	id = uuid.NewV7()
	expectCommit(t, w, "an empty order of zero without lines", db.CodeCheckViolation, name,
		w.orderStmt(s, id, map[string]string{"subtotal_amount": "0", "vat_amount": "0", "total_amount": "0", "payable_amount": "0", "koro_collect_amount": "0"}))
	id = uuid.NewV7()
	expectCommit(t, w, "a subtotal above the lines", db.CodeCheckViolation, name,
		w.orderStmt(s, id, map[string]string{"subtotal_amount": "2000000", "total_amount": "2200000", "payable_amount": "2200000", "koro_collect_amount": "2200000"}),
		lineStmt(s, uuid.NewV7(), id, nil))
	id = uuid.NewV7()
	expectCommit(t, w, "a buyer fee the lines do not carry", db.CodeCheckViolation, name,
		w.orderStmt(s, id, map[string]string{"buyer_fee_amount": "120000", "total_amount": "1320000", "payable_amount": "1320000", "koro_collect_amount": "1320000"}),
		lineStmt(s, uuid.NewV7(), id, nil))

	// Two lines (the second: 2 × 250,000, buyer pays a 10 % fee): the order
	// row is checked at commit, after its lines were written.
	two := uuid.NewV7()
	second := map[string]string{
		"quantity": "2", "unit_price_amount": "250000", "net_amount": "500000", "vat_amount": "100000", "gross_amount": "600000",
		"commission_payer": "'buyer'", "commission_amount": "50000", "commission_vat_amount": "10000", "buyer_fee_amount": "60000",
		"seller_net_amount": "600000", "payable_amount": "660000",
	}
	w.mustCommit(
		w.orderStmt(s, two, map[string]string{"subtotal_amount": "1500000", "vat_amount": "300000", "buyer_fee_amount": "60000",
			"total_amount": "1860000", "payable_amount": "1860000", "koro_collect_amount": "1860000"}),
		lineStmt(s, uuid.NewV7(), two, nil),
		lineStmt(s, uuid.NewV7(), two, second),
	)
	// A line added to a committed order breaks its totals.
	expectCommit(t, w, "a line appended later", db.CodeCheckViolation, name, lineStmt(s, uuid.NewV7(), two, nil))
}

// TestSchema_OrderLineConstraints (02 §19.2, 06 H-37/H-38): the line amounts
// follow the 02 rules, fulfilment columns belong to their state, a line is
// paid out only to its payee once fulfilled, and the line's buyer, seller and
// payee agree with the order, the offering and the payee rows.
func TestSchema_OrderLineConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	order, line := w.order(s)
	ins := func(over map[string]string) stmt { return lineStmt(s, uuid.NewV7(), order, over) }
	for _, c := range []struct {
		name, constraint string
		over             map[string]string
	}{
		{"net is not quantity × price", "order_lines_net_chk", map[string]string{"net_amount": "999999", "gross_amount": "1199999", "payable_amount": "1199999", "seller_net_amount": "1079999"}},
		{"gross is not net + VAT", "order_lines_gross_chk", map[string]string{"gross_amount": "1200001"}},
		{"withholding numerator only", "order_lines_withholding_pair_chk", map[string]string{"vat_withholding_num": "5"}},
		{"withholding above one", "order_lines_withholding_ratio_chk", map[string]string{"vat_withholding_num": "11", "vat_withholding_den": "10"}},
		{"withheld without a fraction", "order_lines_withheld_chk", map[string]string{"vat_withheld_amount": "100000", "seller_net_amount": "980000", "payable_amount": "1100000"}},
		{"withheld above the VAT", "order_lines_withheld_chk", map[string]string{"vat_withholding_num": "10", "vat_withholding_den": "10", "vat_withheld_amount": "200001",
			"seller_net_amount": "879999", "payable_amount": "999999"}},
		{"a buyer fee when the seller pays", "order_lines_buyer_fee_chk", map[string]string{"buyer_fee_amount": "120000", "payable_amount": "1320000"}},
		{"seller net ignores the commission", "order_lines_seller_net_chk", map[string]string{"seller_net_amount": "1200000"}},
		{"payable ignores the withholding", "order_lines_payable_chk", map[string]string{"vat_withholding_num": "5", "vat_withholding_den": "10", "vat_withheld_amount": "100000", "seller_net_amount": "980000"}},
		{"a commission above the seller's share", "order_lines_seller_net_amount_chk", map[string]string{"commission_amount": "1100000", "commission_vat_amount": "220000", "seller_net_amount": "-120000"}},
		{"refunded above payable", "order_lines_refunded_limit_chk", map[string]string{"refunded_amount": "1200001"}},
		{"fulfilled without the time", "order_lines_fulfilled_chk", map[string]string{"fulfillment_status": "'fulfilled'", "fulfilled_via": "'admin'"}},
		{"partial: pending with fulfilled_at", "order_lines_fulfilled_chk", map[string]string{"fulfilled_at": "now()"}},
		{"partial: disputed with fulfilled_via", "order_lines_fulfilled_chk", map[string]string{"fulfillment_status": "'disputed'", "dispute_reason": "'Yapılmadı'", "fulfilled_via": "'admin'"}},
		{"marked without the dispute window", "order_lines_marked_chk", map[string]string{"fulfillment_status": "'marked'", "fulfillment_marked_at": "now()"}},
		{"disputed without a reason", "order_lines_disputed_chk", map[string]string{"fulfillment_status": "'disputed'"}},
		{"paid out without the time", "order_lines_paid_out_chk", map[string]string{"paid_out_at": "now()"}},
		{"a zero quantity", "order_lines_quantity_chk", map[string]string{"quantity": "0", "net_amount": "0", "vat_amount": "0", "gross_amount": "0",
			"commission_amount": "0", "commission_vat_amount": "0", "seller_net_amount": "0", "payable_amount": "0"}},
		{"a commission rate above 100 %", "order_lines_commission_rate_bp_chk", map[string]string{"commission_rate_bp": "10001"}},
	} {
		expectFail(t, w.d.Pool, []p1aCase{{c.name, ins(c.over).sql, ins(c.over).args, chk, c.constraint}})
	}
	other := w.seller()
	otherBuyer := w.buyer()
	// withArgs replaces lineStmt parameters by index (2 buyer org, 3 seller
	// org, 4 payee).
	withArgs := func(arg map[int]any) (string, []any) {
		l := ins(nil)
		for i, v := range arg {
			l.args[i] = v
		}
		return l.sql, l.args
	}
	payeeSQL, payeeArgs := withArgs(map[int]any{4: other.payee})
	sellerSQL, sellerArgs := withArgs(map[int]any{3: other.org, 4: other.payee})
	buyerSQL, buyerArgs := withArgs(map[int]any{2: otherBuyer.org})
	expectFail(t, w.d.Pool, []p1aCase{
		{"a payee of another seller", payeeSQL, payeeArgs, db.CodeForeignKeyViolation, "order_lines_payee_id_seller_org_id_fkey"},
		{"an offering of another seller", sellerSQL, sellerArgs, db.CodeForeignKeyViolation, "order_lines_offering_id_seller_org_id_fkey"},
		{"a buyer other than the order's", buyerSQL, buyerArgs, db.CodeForeignKeyViolation, "order_lines_order_id_org_id_fkey"},
		{"paid out while pending", `UPDATE order_lines SET payout_id = gen_random_uuid(), paid_out_at = now() WHERE id = $1`, []any{line}, chk, "order_lines_payout_line_chk"},
		{"app_rw deletes a line", `DELETE FROM order_lines`, nil, db.CodeInsufficientPriv, ""},
	})
	// A platform seller's line has no payee and is never paid out.
	platformLine := lineStmt(s, uuid.NewV7(), order, map[string]string{"payee_id": "NULLIF($5::uuid, $5::uuid)", "fulfillment_status": "'fulfilled'",
		"fulfilled_at": "now()", "fulfilled_via": "'auto_platform'", "payout_id": "gen_random_uuid()", "paid_out_at": "now()"})
	expectFail(t, w.d.Pool, []p1aCase{{"a platform line paid out", platformLine.sql, platformLine.args, chk, "order_lines_payout_line_chk"}})
}

// TestSchema_OrderValuesFrozen (06 H-6, ADR-0014): the values frozen at order
// time — parties, model, payee, policy copy and every amount — cannot be
// changed by an UPDATE (23001, also for the owner); the running state and the
// paid / refunded amounts can. A line's payout, once recorded, is final.
func TestSchema_OrderValuesFrozen(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	order, line := w.order(s)
	other := w.seller()
	restrict := db.CodeRestrictViolation
	expectFail(t, w.d.Pool, []p1aCase{
		{"the order total", `UPDATE orders SET subtotal_amount = 1, total_amount = 200001, payable_amount = 200001, koro_collect_amount = 200001 WHERE id = $1`, []any{order}, restrict, ""},
		{"the model", `UPDATE orders SET model_code = 'B' WHERE id = $1`, []any{order}, restrict, ""},
		{"the billed party", `UPDATE orders SET buyer_party_id = $2 WHERE id = $1`, []any{order, w.buyer().party}, restrict, ""},
		{"the buyer snapshot", `UPDATE orders SET buyer_snapshot = '{"title":"Başka"}' WHERE id = $1`, []any{order}, restrict, ""},
		{"the frozen payee", `UPDATE order_lines SET payee_id = $2 WHERE id = $1`, []any{line, other.payee}, restrict, ""},
		{"the commission", `UPDATE order_lines SET commission_amount = 0, commission_vat_amount = 0, seller_net_amount = 1200000 WHERE id = $1`, []any{line}, restrict, ""},
		{"the policy copy", `UPDATE order_lines SET commission_rate_bp = 0 WHERE id = $1`, []any{line}, restrict, ""},
		{"the price", `UPDATE order_lines SET unit_price_amount = 1, net_amount = 1, vat_amount = 0, gross_amount = 1, payable_amount = 1 WHERE id = $1`, []any{line}, restrict, ""},
	})
	w.markPaid(order)
	w.exec(`UPDATE orders SET status = 'in_fulfilment' WHERE id = $1`, order)
	w.exec(`UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(), fulfillment_marked_by = $2,
		dispute_window_ends_at = now() + interval '7 days' WHERE id = $1`, line, w.user())
	w.fulfil(line)
	w.exec(`UPDATE order_lines SET refunded_amount = 1000 WHERE id = $1`, line)
}

// TestSchema_OrderLineEventOrgFK (06 H-55, BE-040): an order line is bound
// only to an event of its seller org — order_lines_event_id_seller_org_id_fkey
// (event_id, seller_org_id) → events (id, org_id); a line without an event
// is free (MATCH SIMPLE). Mutation test: with the former single-column FK
// (event_id → events (id)) the same cross-org line commits, so the
// composite FK is what refuses it.
func TestSchema_OrderLineEventOrgFK(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	own, foreign := w.event(s.seller.org, w.user(), nil), w.event(w.org("club"), w.user(), nil)
	withEvent := func(ev uuid.UUID) []stmt {
		order := uuid.NewV7()
		return []stmt{w.orderStmt(s, order, nil), lineStmt(s, uuid.NewV7(), order, map[string]string{"event_id": "'" + ev.String() + "'::uuid"})}
	}
	const fk = "order_lines_event_id_seller_org_id_fkey"
	expectCommit(t, w, "an event of another org", db.CodeForeignKeyViolation, fk, withEvent(foreign)...)
	expectCommit(t, w, "an unknown event", db.CodeForeignKeyViolation, fk, withEvent(uuid.NewV7())...)
	w.mustCommit(withEvent(own)...)
	w.order(s) // a line without an event

	// Mutation: the former single-column FK lets the cross-org line in.
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `ALTER TABLE order_lines DROP CONSTRAINT `+fk+`,
		ADD CONSTRAINT order_lines_event_id_fkey FOREIGN KEY (event_id) REFERENCES events (id)`); err != nil {
		t.Fatal(err)
	}
	if state, c := w.commit(stmts(t, withEvent(foreign)...)); state != "" {
		t.Fatalf("mutant (event_id → events (id)): cross-org line SQLSTATE %q %s, want it stored (the mutant must survive "+
			"only without the composite FK)", state, c)
	}
}

// TestSchema_OrderLinePolicyWindows (02 §18.3, BE-040): the dispute window
// and the payout hold of the policy in effect are frozen on the line, within
// the policy's ranges (0–60 and 0–90 days).
func TestSchema_OrderLinePolicyWindows(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	chk := db.CodeCheckViolation
	for _, c := range []struct{ name, constraint, col, val string }{
		{"a dispute window above 60 days", "order_lines_dispute_window_days_chk", "dispute_window_days", "61"},
		{"a negative dispute window", "order_lines_dispute_window_days_chk", "dispute_window_days", "-1"},
		{"a payout hold above 90 days", "order_lines_payout_hold_days_chk", "payout_hold_days", "91"},
	} {
		order := uuid.NewV7()
		expectCommit(t, w, c.name, chk, c.constraint, w.orderStmt(s, order, nil),
			lineStmt(s, uuid.NewV7(), order, map[string]string{c.col: c.val}))
	}
	order := uuid.NewV7()
	line := uuid.NewV7()
	w.mustCommit(w.orderStmt(s, order, nil), lineStmt(s, line, order, map[string]string{"dispute_window_days": "14", "payout_hold_days": "3"}))
	if got := queryString(t, w.d, `SELECT dispute_window_days || '/' || payout_hold_days FROM order_lines WHERE id = $1`, line); got != "14/3" {
		t.Fatalf("windows = %s, want 14/3", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"the dispute window", `UPDATE order_lines SET dispute_window_days = 0 WHERE id = $1`, []any{line}, db.CodeRestrictViolation, ""},
		{"the payout hold", `UPDATE order_lines SET payout_hold_days = 0 WHERE id = $1`, []any{line}, db.CodeRestrictViolation, ""},
	})
}
