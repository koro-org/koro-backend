//go:build integration

package migrations_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_EntitlementConstraints (02 §10.1): quota/access shape, the
// quantity bounds (the last line behind the conditional consume UPDATE), the
// validity window, the source and the status ↔ timestamp ties.
func TestSchema_EntitlementConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company := w.org("company")
	admin := w.user()
	src := uuid.NewV7()
	// $1 = org, $2 = source id.
	e := func(name string, over map[string]string, org uuid.UUID, source any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("entitlements", entitlementDefaults, over), []any{org, source}, state, constraint}
	}
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return e(name, over, company, src, db.CodeCheckViolation, constraint)
	}
	nosrc := func(name string, over map[string]string, constraint string) p1aCase {
		return e(name, over, company, nil, db.CodeCheckViolation, constraint)
	}
	unknown := "'" + uuid.NewV7().String() + "'"
	expectFail(t, w.d.Pool, []p1aCase{
		chk("unknown feature", map[string]string{"feature": lit("banner")}, "entitlements_feature_chk"),
		chk("unknown kind", map[string]string{"kind": lit("pass")}, "entitlements_kind_chk"),
		chk("quota of zero", map[string]string{"quantity_total": "0"}, "entitlements_quantity_total_chk"),
		chk("negative usage", map[string]string{"quantity_used": "-1"}, "entitlements_quantity_used_chk"),
		chk("a quota without a total", map[string]string{"quantity_total": "-"}, "entitlements_quota_chk"),
		chk("an access right with a total", map[string]string{"feature": lit("discovery_access"), "kind": lit("access")}, "entitlements_quota_chk"),
		chk("discovery access as a quota", map[string]string{"feature": lit("discovery_access")}, "entitlements_access_feature_chk"),
		chk("a quota feature as an access right", map[string]string{"kind": lit("access"), "quantity_total": "-"}, "entitlements_access_feature_chk"),
		chk("more used than bought", map[string]string{"quantity_used": "11"}, "entitlements_used_total_chk"),
		chk("an empty validity window", map[string]string{"valid_to": "now()"}, "entitlements_validity_chk"),
		chk("a reversed validity window", map[string]string{"valid_to": "now() - interval '1 day'"}, "entitlements_validity_chk"),
		chk("unknown status", map[string]string{"status": lit("paused")}, "entitlements_status_chk"),
		chk("unknown source type", map[string]string{"source_type": lit("gift")}, "entitlements_source_type_chk"),
		nosrc("an order-line right without a source", nil, "entitlements_source_chk"),
		nosrc("a promo right without a source", map[string]string{"source_type": lit("promo")}, "entitlements_source_chk"),
		chk("an admin grant without the admin", map[string]string{"source_type": lit("admin_grant")}, "entitlements_granted_by_chk"),
		chk("active without an activation time", map[string]string{"activated_at": "-"}, "entitlements_activated_chk"),
		chk("exhausted without an activation time", map[string]string{"status": lit("exhausted"), "quantity_used": "10", "activated_at": "-"}, "entitlements_activated_chk"),
		chk("revoked without a revocation time", map[string]string{"status": lit("revoked")}, "entitlements_revoked_chk"),
		chk("a revocation time on an active right", map[string]string{"revoked_at": "now()"}, "entitlements_revoked_chk"),
		chk("revoke reason of 301 characters", map[string]string{"status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit(repeat("a", 301))}, "entitlements_revoke_reason_chk"),
		e("unknown org", nil, uuid.NewV7(), src, db.CodeForeignKeyViolation, "entitlements_org_id_fkey"),
		e("unknown admin", map[string]string{"source_type": lit("admin_grant"), "granted_by": unknown}, company, src, db.CodeForeignKeyViolation, "entitlements_granted_by_fkey"),
	})

	fresh := func() uuid.UUID { return uuid.NewV7() }
	expectOK(t, w.d.Pool, []p1aCase{
		e("active order-line quota", nil, company, src, "", ""),
		e("pending right (not yet activated)", map[string]string{"status": lit("pending"), "activated_at": "-", "feature": lit("contact_request")}, company, fresh(), "", ""),
		e("discovery access", map[string]string{"feature": lit("discovery_access"), "kind": lit("access"), "quantity_total": "-"}, company, fresh(), "", ""),
		e("fully used and exhausted", map[string]string{"feature": lit("targeted_dispatch"), "status": lit("exhausted"), "quantity_used": "10"}, company, fresh(), "", ""),
		e("revoked right", map[string]string{"source_type": lit("promo"), "status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("refund")}, company, fresh(), "", ""),
	})
	// An admin grant needs the admin and no source; any number of them may exist.
	for range 2 {
		w.exec(`INSERT INTO entitlements (org_id, feature, kind, quantity_total, valid_from, valid_to, status, source_type, granted_by, activated_at)
			VALUES ($1, 'targeted_post', 'quota', 5, now(), now() + interval '7 days', 'active', 'admin_grant', $2, now())`, company, admin)
	}
	// A source grants a feature once: the order line cannot be fulfilled twice,
	// but it may grant a different feature.
	expectFail(t, w.d.Pool, []p1aCase{
		e("the same order line and feature twice", nil, company, src, db.CodeUniqueViolation, "entitlements_source_uq"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		e("the same order line, another feature", map[string]string{"feature": lit("targeted_dispatch")}, company, src, "", ""),
	})
	// The second layer behind the conditional consume UPDATE: over-consuming
	// is rejected by the table itself.
	id := w.entitlement(company, nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"consume more than the total", `UPDATE entitlements SET quantity_used = quantity_used + 11 WHERE id = $1`, []any{id}, db.CodeCheckViolation, "entitlements_used_total_chk"},
	})
	w.exec(`UPDATE entitlements SET quantity_used = 6 WHERE id = $1`, id)
	expectFail(t, w.d.Pool, []p1aCase{
		{"lower the total below the usage", `UPDATE entitlements SET quantity_total = 5 WHERE id = $1`, []any{id}, db.CodeCheckViolation, "entitlements_used_total_chk"},
	})
}

// TestSchema_EntitlementUsagesAppendOnly (02 §10.2, §0.5): the usage ledger
// takes inserts only. app_rw has no UPDATE/DELETE/TRUNCATE grant and the
// koro_forbid_mutation triggers stop even the owner; a usage has at most one
// reversal and a reversal is the negative of a consumption.
func TestSchema_EntitlementUsagesAppendOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company := w.org("company")
	user := w.user()
	ent := w.entitlement(company, nil)
	use, undo, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	ref := uuid.NewV7()
	w.exec(`INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id, actor_user_id)
		VALUES ($1, $2, $3, 1, 'post', $4, $5)`, use, ent, company, ref, user)
	// A reversal carries the negative quantity and may repeat the reference.
	w.exec(`INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id, reversal_of)
		VALUES ($1, $2, $3, -1, 'post', $4, $5)`, undo, ent, company, ref, use)

	// Layer 1: no grant for app_rw.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw update", `UPDATE entitlement_usages SET quantity = 5 WHERE id = $1`, []any{use}, db.CodeInsufficientPriv, ""},
		{"app_rw delete", `DELETE FROM entitlement_usages WHERE id = $1`, []any{use}, db.CodeInsufficientPriv, ""},
		{"app_rw truncate", `TRUNCATE entitlement_usages`, nil, db.CodeInsufficientPriv, ""},
	})
	// Layer 2: the triggers hold against the table owner too.
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"owner update", `UPDATE entitlement_usages SET quantity = 5 WHERE id = $1`, []any{use}, db.CodeRestrictViolation, ""},
		{"owner update to the same value", `UPDATE entitlement_usages SET quantity = quantity WHERE id = $1`, []any{undo}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM entitlement_usages WHERE id = $1`, []any{undo}, db.CodeRestrictViolation, ""},
		// posts, outreach_dispatches and contact_requests refer to the usages (000066–000077),
		// the post children refer to posts (post_dropped_media too, 000166) and the
		// conversations (with their children) to contact_requests (000169), so a plain
		// TRUNCATE stops at the FK (0A000).
		{"owner truncate", `TRUNCATE entitlement_usages, posts, post_media, post_dropped_media, post_reactions, post_saves,
			post_hides, outreach_dispatches, contact_requests, conversations, conversation_messages, conversation_user_states`,
			nil, db.CodeRestrictViolation, ""},
		{"owner truncate cascade", `TRUNCATE entitlement_usages CASCADE`, nil, db.CodeRestrictViolation, ""},
	})
	if n := w.count(`SELECT count(*) FROM entitlement_usages WHERE entitlement_id = $1`, ent); n != 2 {
		t.Fatalf("usages = %d, want 2: the ledger changed", n)
	}
	if got := queryStrings(t, w, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'entitlement_usages'::regclass AND NOT tgisinternal ORDER BY tgname`); !slices.Equal(got, []string{"entitlement_usages_immutable", "entitlement_usages_no_truncate"}) {
		t.Errorf("triggers on entitlement_usages = %v", got)
	}

	// The shape rules of 02 §10.2.
	ins := `INSERT INTO entitlement_usages (entitlement_id, org_id, quantity, ref_type, ref_id, reversal_of) VALUES ($1, $2, $3, $4, $5, $6)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"a consumption of zero", ins, []any{ent, company, 0, "post", uuid.NewV7(), nil}, db.CodeCheckViolation, "entitlement_usages_quantity_chk"},
		{"a consumption pointing at a usage", ins, []any{ent, company, 1, "post", uuid.NewV7(), use}, db.CodeCheckViolation, "entitlement_usages_reversal_chk"},
		{"a negative quantity that reverses nothing", ins, []any{ent, company, -1, "post", uuid.NewV7(), nil}, db.CodeCheckViolation, "entitlement_usages_reversal_chk"},
		{"unknown reference type", ins, []any{ent, company, 1, "banner", uuid.NewV7(), nil}, db.CodeCheckViolation, "entitlement_usages_ref_type_chk"},
		{"the same reference consumed twice", ins, []any{ent, company, 1, "post", ref, nil}, db.CodeUniqueViolation, "entitlement_usages_ref_uq"},
		{"a second reversal of one usage", ins, []any{ent, company, -1, "post", ref, use}, db.CodeUniqueViolation, "entitlement_usages_reversal_uq"},
		{"unknown entitlement", ins, []any{other, company, 1, "post", uuid.NewV7(), nil}, db.CodeForeignKeyViolation, "entitlement_usages_entitlement_id_fkey"},
		{"unknown org", ins, []any{ent, other, 1, "post", uuid.NewV7(), nil}, db.CodeForeignKeyViolation, "entitlement_usages_org_id_fkey"},
		{"reversal of an unknown usage", ins, []any{ent, company, -1, "post", uuid.NewV7(), other}, db.CodeForeignKeyViolation, "entitlement_usages_reversal_of_fkey"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"another reference", ins, []any{ent, company, 2, "dispatch", ref, nil}, "", ""}, // the key is (ref_type, ref_id)
	})
	// An entitlement that has usages cannot be deleted (NO ACTION): the usages
	// are immutable, so the entitlement stays until their purge (T-120).
	expectFail(t, owner, []p1aCase{
		{"an entitlement with usages", `DELETE FROM entitlements WHERE id = $1`, []any{ent}, db.CodeForeignKeyViolation, ""},
	})
	w.exec(`DELETE FROM entitlements WHERE id = $1`, w.entitlement(company, nil)) // one without usages goes
}
