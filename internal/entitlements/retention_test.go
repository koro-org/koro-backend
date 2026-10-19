package entitlements_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/entitlements"
	"koro/internal/platform/privacy"
)

// TestPrivacy_UsagesExportedAndRetained (T-196, 06 H-49; BE-057): the
// usages a person made are exported; erasure keeps them (append-only
// commercial record, tombstone reference) and says why.
func TestPrivacy_UsagesExportedAndRetained(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	total := 5
	ent := entitlements.Entitlement{ID: uuid.NewV7(), OrgID: uuid.NewV7(), Feature: entitlements.FeatureTargetedPost,
		Kind: entitlements.KindQuota, QuantityTotal: &total, ValidFrom: t0, ValidTo: t0.AddDate(0, 1, 0), Status: entitlements.StatusActive,
		SourceType: entitlements.SourceAdminGrant, GrantedBy: &e.admin, ActivatedAt: &t0}
	if err := e.store.Insert(ctx, ent); err != nil {
		t.Fatal(err)
	}
	person := uuid.NewV7()
	if _, ok, err := e.store.InsertUsage(ctx, entitlements.Usage{ID: uuid.NewV7(), EntitlementID: ent.ID, Quantity: 1,
		RefType: entitlements.RefPost, RefID: uuid.NewV7(), ActorUserID: &person, CreatedAt: t0}); !ok || err != nil {
		t.Fatalf("InsertUsage = %v, %v", ok, err)
	}
	if e.mod.Privacy.Name() != "entitlements" {
		t.Fatalf("Name() = %q", e.mod.Privacy.Name())
	}
	sec, err := e.mod.Privacy.Export(ctx, person)
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["entitlement_id"] != ent.ID.String() || sec.Items[0]["ref_type"] != "post" {
		t.Fatalf("Export = %+v, %v", sec, err)
	}
	for range 2 { // idempotent
		res, err := e.mod.Privacy.Erase(ctx, person)
		if err != nil || res.Retained != 1 || res.Deleted != 0 || res.RetainedReason == "" {
			t.Fatalf("Erase = %+v, %v", res, err)
		}
	}
	if sec, _ := e.mod.Privacy.Export(ctx, uuid.NewV7()); len(sec.Items) != 0 {
		t.Fatalf("Export of a stranger = %+v", sec)
	}
}

// TestSweeper_EntitlementOther (entitlement_other, 2 years after the end;
// BE-057): unapproved, the sweep counts; approved, the ended admin grant
// goes with its usages, an order right and a young grant stay.
func TestSweeper_EntitlementOther(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	total := 5
	grant := func(src entitlements.SourceType, ended time.Time) entitlements.Entitlement {
		t.Helper()
		from := ended.AddDate(0, -1, 0)
		x := entitlements.Entitlement{ID: uuid.NewV7(), OrgID: uuid.NewV7(), Feature: entitlements.FeatureTargetedPost,
			Kind: entitlements.KindQuota, QuantityTotal: &total, ValidFrom: from, ValidTo: ended, Status: entitlements.StatusExpired,
			SourceType: src, GrantedBy: &e.admin, ActivatedAt: &from}
		if src != entitlements.SourceAdminGrant {
			id := uuid.NewV7()
			x.SourceID = &id
		}
		if src == entitlements.SourceOrderLine {
			x.GrantedBy = nil
		}
		if err := e.store.Insert(ctx, x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	old := grant(entitlements.SourceAdminGrant, t0.AddDate(-3, 0, 0))
	if _, ok, err := e.store.InsertUsage(ctx, entitlements.Usage{ID: uuid.NewV7(), EntitlementID: old.ID, Quantity: 1,
		RefType: entitlements.RefPost, RefID: uuid.NewV7(), CreatedAt: old.ValidFrom}); !ok || err != nil {
		t.Fatalf("InsertUsage = %v, %v", ok, err)
	}
	order := grant(entitlements.SourceOrderLine, t0.AddDate(-3, 0, 0))
	young := grant(entitlements.SourcePromo, t0.AddDate(-1, 0, 0))
	p := privacy.RetentionPolicy{Code: entitlements.PolicyOther, Retention: t0.Sub(t0.AddDate(-2, 0, 0))}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, t0); err != nil || res.Count != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	p.LegalApproved = true
	if res, err := e.mod.Sweeper.Sweep(ctx, p, t0); err != nil || res.Count != 2 || res.Table != "entitlements" {
		t.Fatalf("sweep = %+v, %v; want the grant and its usage", res, err)
	}
	if _, err := e.store.Get(ctx, old.OrgID, old.ID); err == nil {
		t.Fatal("the ended admin grant survived")
	}
	for _, x := range []entitlements.Entitlement{order, young} {
		if _, err := e.store.Get(ctx, x.OrgID, x.ID); err != nil {
			t.Fatalf("grant %s was purged: %v", x.SourceType, err)
		}
	}
}
