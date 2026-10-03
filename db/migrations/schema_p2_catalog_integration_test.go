//go:build integration

package migrations_test

import (
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_ServiceConstraints (02 §18.1): a platform-native service has an
// entitlement template (and only it), withholding is a valid fraction and
// matches the 'withheld' class, an exempt service names its exemption code.
func TestSchema_ServiceConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	ins := `INSERT INTO services (code, name_tr, category, fulfillment_type, entitlement_template, vat_class, vat_exemption_code,
		vat_withholding_num, vat_withholding_den, default_vat_rate_bp) VALUES ($1, 'Hizmet', 'other', $2, $3, $4, $5, $6, $7, $8)`
	tpl := `{"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}`
	expectFail(t, w.d.Pool, []p1aCase{
		{"platform native without a template", ins, []any{"svc_a", "platform_native", nil, "standard", nil, nil, nil, 2000}, chk, "services_entitlement_template_chk"},
		{"a template on an org-bound service", ins, []any{"svc_a", "org_bound", tpl, "standard", nil, nil, nil, 2000}, chk, "services_entitlement_template_chk"},
		{"a template that is not an object", ins, []any{"svc_a", "platform_native", "[]", "standard", nil, nil, nil, 2000}, chk, "services_entitlement_template_chk"},
		{"withholding numerator only", ins, []any{"svc_a", "org_bound", nil, "withheld", nil, 5, nil, 2000}, chk, "services_withholding_pair_chk"},
		{"withholding above one", ins, []any{"svc_a", "org_bound", nil, "withheld", nil, 11, 10, 2000}, chk, "services_withholding_ratio_chk"},
		{"withheld class without a fraction", ins, []any{"svc_a", "org_bound", nil, "withheld", nil, nil, nil, 2000}, chk, "services_withheld_class_chk"},
		{"a fraction on the standard class", ins, []any{"svc_a", "org_bound", nil, "standard", nil, 5, 10, 2000}, chk, "services_withheld_class_chk"},
		{"exempt without a code", ins, []any{"svc_a", "org_bound", nil, "exempt", nil, nil, nil, 0}, chk, "services_exemption_chk"},
		{"a VAT rate above 100 %", ins, []any{"svc_a", "org_bound", nil, "standard", nil, nil, nil, 10001}, chk, "services_default_vat_rate_bp_chk"},
		{"a code with upper case", ins, []any{"Svc_a", "org_bound", nil, "standard", nil, nil, nil, 2000}, chk, "services_code_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"a platform-native quota", ins, []any{"targeted_post_5", "platform_native", tpl, "standard", nil, nil, nil, 2000}, "", ""},
		{"a withheld service (5/10)", ins, []any{"sponsor_withheld", "org_bound", nil, "withheld", nil, 5, 10, 2000}, "", ""},
		{"an exempt service", ins, []any{"exempt_one", "org_bound", nil, "exempt", "301", nil, nil, 0}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"a duplicate code", ins, []any{"exempt_one", "org_bound", nil, "exempt", "301", nil, nil, 0}, db.CodeUniqueViolation, "services_code_uq"},
		{"app_rw deletes a service", `DELETE FROM services`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_OfferingConstraints (02 §18.2, 03 §6.1): an approved offering
// was approved by someone other than its creator; an org offering on sale
// has a default payee of its own org; sold_out means the stock is gone; the
// university decision columns belong to a decision; only drafts are deleted.
func TestSchema_OfferingConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	seller, service := w.seller(), w.service()
	creator, admin, rep := w.user(), w.user(), w.user()
	defaults := map[string]string{
		"service_id": "$1", "seller_kind": "'org'", "org_id": "$2", "default_payee_id": "$3", "title": "'Stant alanı'",
		"unit_price_amount": "50000", "vat_rate_bp": "2000", "created_by": "$4",
	}
	ins := func(over map[string]string) string { return insertSQL("offerings", defaults, over) }
	args := []any{service, seller.org, seller.payee, creator}
	active := map[string]string{"status": "'active'", "submitted_at": "now()", "approved_by": "$5", "approved_at": "now()"}
	expectFail(t, w.d.Pool, []p1aCase{
		{"approved by its creator", ins(active), append(args, creator), chk, "offerings_approver_chk"},
		{"active without approval", ins(map[string]string{"status": "'active'", "submitted_at": "now()"}), args, chk, "offerings_approved_chk"},
		{"partial: an approver without the time", ins(map[string]string{"approved_by": "$5"}), append(args, admin), chk, "offerings_approved_pair_chk"},
		{"pending approval without submission time", ins(map[string]string{"status": "'pending_approval'"}), args, chk, "offerings_submitted_chk"},
		{"sold out with stock left", ins(cols(active, map[string]string{"status": "'sold_out'", "quantity_total": "5", "quantity_sold": "4"})), append(args, admin), chk, "offerings_sold_out_chk"},
		{"sold more than the stock", ins(map[string]string{"quantity_total": "5", "quantity_sold": "6"}), args, chk, "offerings_stock_chk"},
		{"on sale without a payee", ins(cols(active, map[string]string{"default_payee_id": "NULLIF($3::uuid, $3::uuid)"})), append(args, admin), chk, "offerings_payee_required_chk"},
		{"a platform offering with a payee", ins(map[string]string{"seller_kind": "'platform'"}), args, chk, "offerings_platform_payee_chk"},
		{"a payee of another org", ins(nil), []any{service, w.org("club"), seller.payee, creator}, db.CodeForeignKeyViolation, "offerings_default_payee_id_org_id_fkey"},
		{"university approval required but not tracked", ins(map[string]string{"requires_university_approval": "true"}), args, chk, "offerings_university_required_chk"},
		{"a university decision without the decider", ins(map[string]string{"requires_university_approval": "true", "university_approval_status": "'approved'", "university_approved_at": "now()"}), args, chk, "offerings_university_decision_chk"},
		{"partial: a pending university decision with a decider", ins(map[string]string{"requires_university_approval": "true", "university_approval_status": "'pending'", "university_approved_by": "$5"}), append(args, rep), chk, "offerings_university_decision_chk"},
		{"on sale before the university approved", ins(cols(active, map[string]string{"requires_university_approval": "true", "university_approval_status": "'pending'"})), append(args, admin), chk, "offerings_university_approval_chk"},
		{"cancelled without the time", ins(map[string]string{"status": "'cancelled'"}), args, chk, "offerings_cancelled_chk"},
		{"in kind on sale", ins(cols(active, map[string]string{"compensation_type": "'in_kind'"})), append(args, admin), chk, "offerings_in_kind_chk"},
		{"a zero price", ins(map[string]string{"unit_price_amount": "0"}), args, chk, "offerings_unit_price_amount_chk"},
		{"another currency", ins(map[string]string{"currency": "'EUR'"}), args, chk, "offerings_currency_chk"},
	})
	draft, live := uuid.NewV7(), uuid.NewV7()
	w.exec(ins(map[string]string{"id": "$5"}), service, seller.org, seller.payee, creator, draft)
	w.exec(ins(cols(active, map[string]string{"id": "$6", "requires_university_approval": "true", "university_approval_status": "'approved'",
		"university_approved_by": "$7", "university_approved_at": "now()"})), service, seller.org, seller.payee, creator, admin, live, rep)
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, w.d.Pool, []p1aCase{{"app_rw deletes an active offering", `DELETE FROM offerings WHERE id = $1`, []any{live}, db.CodeRestrictViolation, ""}})
	expectFail(t, owner, []p1aCase{{"the owner deletes an active offering", `DELETE FROM offerings WHERE id = $1`, []any{live}, db.CodeRestrictViolation, ""}})
	w.exec(`DELETE FROM offerings WHERE id = $1`, draft)
	if n := w.count(`SELECT count(*) FROM offerings WHERE id = $1`, draft); n != 0 {
		t.Errorf("the draft offering was not deleted")
	}
	if got := queryString(t, w.d, `SELECT (search_vector @@ websearch_to_tsquery('koro_tr', 'stant'))::text FROM offerings WHERE id = $1`, live); got != "true" {
		t.Errorf("offering search vector does not match its title")
	}
}

// TestSchema_OfferingReservationUnderConcurrency (02 §18.2): the
// reservation is one conditional UPDATE; 25 concurrent buyers of a stock of
// 7 get exactly 7 units and the offering ends sold_out (offerings_stock_chk
// and offerings_sold_out_chk are the second layer).
func TestSchema_OfferingReservationUnderConcurrency(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	seller := w.seller()
	id := w.offering(seller, w.service())
	w.exec(`UPDATE offerings SET quantity_total = 7 WHERE id = $1`, id)
	const reserve = `UPDATE offerings SET quantity_sold = quantity_sold + $2,
		status = CASE WHEN quantity_total IS NOT NULL AND quantity_sold + $2 = quantity_total THEN 'sold_out' ELSE status END
		WHERE id = $1 AND status = 'active' AND (quantity_total IS NULL OR quantity_sold + $2 <= quantity_total) RETURNING id`
	var wg sync.WaitGroup
	got := make([]int64, 25)
	for i := range got {
		wg.Go(func() {
			tag, err := w.d.Pool.Exec(t.Context(), reserve, id, 1)
			if err != nil {
				t.Errorf("reservation: %v", err)
				return
			}
			got[i] = tag.RowsAffected()
		})
	}
	wg.Wait()
	var units int64
	for _, n := range got {
		units += n
	}
	if units != 7 {
		t.Errorf("units reserved = %d, want 7", units)
	}
	if s := queryString(t, w.d, `SELECT status || ' ' || quantity_sold::text FROM offerings WHERE id = $1`, id); s != "sold_out 7" {
		t.Errorf("offering after the race = %q, want sold_out 7", s)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"a careless increment of a sold-out offering", `UPDATE offerings SET quantity_sold = quantity_sold + 1 WHERE id = $1`, []any{id}, db.CodeCheckViolation, "offerings_sold_out_chk"},
		{"an active offering sold past its stock", `UPDATE offerings SET status = 'active', quantity_sold = 8 WHERE id = $1`, []any{id}, db.CodeCheckViolation, "offerings_stock_chk"},
		{"sold out with stock released", `UPDATE offerings SET quantity_sold = 6 WHERE id = $1`, []any{id}, db.CodeCheckViolation, "offerings_sold_out_chk"},
	})
}

// TestCommercePolicies_OverlappingRange_ExclusionViolation (T-43): validity
// periods of one scope cannot overlap — btree_gist exclusion on (scope,
// scope_id, tstzrange), also between concurrent transactions; adjacent
// periods ('[)') and other scopes are fine; a row is changed only by closing
// it (koro_policy_close_only).
func TestCommercePolicies_OverlappingRange_ExclusionViolation(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	admin := w.user()
	ins := `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis, commission_payer,
		valid_from, valid_to, created_by) VALUES ($1, $2, $3, $4, $5, 1000, 'net_excl_vat', $6, $7, $8, $9)`
	org, otherOrg := w.org("club"), w.org("club")
	jan, feb, mar := "2027-01-01T00:00:00Z", "2027-02-01T00:00:00Z", "2027-03-01T00:00:00Z"
	first := uuid.NewV7()
	w.exec(ins, first, "seller_org", org, "A", "bank_transfer", "seller", jan, feb, admin)
	const excl = db.CodeExclusionViolation
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same period", ins, []any{uuid.NewV7(), "seller_org", org, "A", "bank_transfer", "seller", jan, feb, admin}, excl, "commerce_policies_no_overlap_uq"},
		{"an overlapping period", ins, []any{uuid.NewV7(), "seller_org", org, "A", "bank_transfer", "seller", "2027-01-15T00:00:00Z", mar, admin}, excl, "commerce_policies_no_overlap_uq"},
		{"an open period starting earlier", ins, []any{uuid.NewV7(), "seller_org", org, "A", "bank_transfer", "seller", "2026-12-01T00:00:00Z", nil, admin}, excl, "commerce_policies_no_overlap_uq"},
		{"model C collected by Koro", ins, []any{uuid.NewV7(), "seller_org", otherOrg, "C", "bank_transfer", "buyer", jan, nil, admin}, db.CodeCheckViolation, "commerce_policies_model_method_chk"},
		{"model C paid by the seller", ins, []any{uuid.NewV7(), "seller_org", otherOrg, "C", "direct_to_payee", "seller", jan, nil, admin}, db.CodeCheckViolation, "commerce_policies_model_c_payer_chk"},
		{"a default with a scope id", ins, []any{uuid.NewV7(), "default", org, "A", "bank_transfer", "seller", jan, nil, admin}, db.CodeCheckViolation, "commerce_policies_scope_id_chk"},
		{"an org scope without its id", ins, []any{uuid.NewV7(), "seller_org", nil, "A", "bank_transfer", "seller", jan, nil, admin}, db.CodeCheckViolation, "commerce_policies_scope_id_chk"},
		{"an empty period", ins, []any{uuid.NewV7(), "seller_org", otherOrg, "A", "bank_transfer", "seller", jan, jan, admin}, db.CodeCheckViolation, "commerce_policies_validity_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"the adjacent period", ins, []any{uuid.NewV7(), "seller_org", org, "A", "bank_transfer", "seller", feb, nil, admin}, "", ""},
		{"the same period of another org", ins, []any{uuid.NewV7(), "seller_org", otherOrg, "A", "bank_transfer", "seller", jan, feb, admin}, "", ""},
		{"the same period on the service scope", ins, []any{uuid.NewV7(), "service", org, "A", "bank_transfer", "seller", jan, feb, admin}, "", ""},
		{"a model C policy", ins, []any{uuid.NewV7(), "offering", org, "C", "direct_to_payee", "buyer", jan, nil, admin}, "", ""},
	})

	// Concurrent default policies for the same period: exactly one wins.
	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := range results {
		wg.Go(func() {
			_, err := w.d.Pool.Exec(t.Context(), ins, uuid.NewV7(), "default", nil, "A", "bank_transfer", "seller", jan, nil, admin)
			results[i] = db.SQLState(err)
		})
	}
	wg.Wait()
	won := 0
	for _, s := range results {
		switch s {
		case "":
			won++
		case excl:
		default:
			t.Errorf("concurrent default policy: SQLSTATE %s", s)
		}
	}
	if won != 1 {
		t.Errorf("concurrent default policies stored = %d, want 1", won)
	}

	// Change = close the open row + a new row.
	open := queryString(t, w.d, `SELECT id::text FROM commerce_policies WHERE scope = 'default'`)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a new rate in place", `UPDATE commerce_policies SET commission_rate_bp = 500 WHERE id = $1`, []any{open}, db.CodeRestrictViolation, ""},
		{"closing and changing at once", `UPDATE commerce_policies SET commission_rate_bp = 500, valid_to = $2 WHERE id = $1`, []any{open, mar}, db.CodeRestrictViolation, ""},
		{"re-closing a closed row", `UPDATE commerce_policies SET valid_to = $2 WHERE id = $1`, []any{first, mar}, db.CodeRestrictViolation, ""},
		{"app_rw deletes a policy", `DELETE FROM commerce_policies`, nil, db.CodeInsufficientPriv, ""},
	})
	w.mustCommit(
		st(`UPDATE commerce_policies SET valid_to = $2 WHERE id = $1`, open, mar),
		st(ins, uuid.NewV7(), "default", nil, "A", "bank_transfer", "seller", mar, nil, admin))
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"the owner deletes a policy", `DELETE FROM commerce_policies WHERE id = '` + first.String() + `'`, nil, db.CodeRestrictViolation, ""},
		{"the owner truncates (with the referencing tables)", `TRUNCATE commerce_policies CASCADE`, nil, db.CodeRestrictViolation, ""},
	})
}
