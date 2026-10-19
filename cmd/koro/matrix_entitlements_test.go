package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
)

// matrixFixtures seeds fixtures/entitlements.yaml (03 §7.3) into the
// in-memory store installed by useMemStore: rights first, then usages.
func (w *entitlementsWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx, now := context.Background(), w.clk.Now()
	from, to := now.Add(-24*time.Hour), now.Add(30*24*time.Hour)
	admin := authztest.ID("platform_admin")
	all := fx.Of("entitlements")
	for _, f := range all {
		if f.Kind != "entitlement" {
			continue
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		org, err := fx.Ref(f, "org")
		if err != nil {
			return err
		}
		feature, _ := f.Fields["feature"].(string)
		e := entitlements.Entitlement{
			ID: id, OrgID: org, Feature: entitlements.Feature(feature), Kind: entitlements.Feature(feature).Kind(),
			ValidFrom: from, ValidTo: to, Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant,
			GrantedBy: &admin, ActivatedAt: &from,
		}
		if q, ok := f.Fields["quantity"].(int); ok {
			e.QuantityTotal = &q
		}
		if src, _ := f.Fields["source"].(string); src == string(entitlements.SourceOrderLine) {
			line := authztest.ID(f.Name + "_line")
			e.SourceType, e.SourceID, e.GrantedBy = entitlements.SourceOrderLine, &line, nil
		}
		if err := w.store.Insert(ctx, e); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	for _, f := range all {
		switch f.Kind {
		case "entitlement":
			continue
		case "usage":
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		entID, err := fx.Ref(f, "entitlement")
		if err != nil {
			return err
		}
		actor, err := fx.Ref(f, "actor")
		if err != nil {
			return err
		}
		ent, err := w.store.Lock(ctx, entID)
		if err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
		if _, ok, err := w.store.TryConsume(ctx, ent.OrgID, entID, 1, now); err != nil || !ok {
			return fmt.Errorf("fixture %s: consume: ok=%v err=%w", f.Name, ok, err)
		}
		_, ok, err := w.store.InsertUsage(ctx, entitlements.Usage{
			ID: id, EntitlementID: entID, Quantity: 1, RefType: ent.Feature.RefType(), RefID: authztest.ID(f.Name + "_ref"),
			ActorUserID: &actor, CreatedAt: now,
		})
		if err != nil || !ok {
			return fmt.Errorf("fixture %s: usage: ok=%v err=%w", f.Name, ok, err)
		}
	}
	return nil
}

// matrixHook runs the entitlements scenario hooks (the consumers, feed,
// outreach, messaging and discovery, come with later tickets):
//
//	entitlements.reset {org}                        revoke every right of the org
//	entitlements.consume {org, feature}             one unit must be consumed
//	entitlements.expect_not_entitled {org, feature} consuming (quota) or using (access)
//	                                                must fail with 403 entitlements.not_entitled
func (w *entitlementsWiring) matrixHook(fx *matrixFixtures, name string, args map[string]any) error {
	ctx := context.Background()
	ref, _ := args["org"].(string)
	org, err := fx.ID(ref)
	if err != nil {
		return err
	}
	feature := entitlements.Feature(fmt.Sprint(args["feature"]))
	switch name {
	case "reset":
		rows, err := w.store.List(ctx, org, entitlements.ListFilter{}, nil, 1000)
		if err != nil {
			return err
		}
		for _, e := range rows {
			if _, err := w.store.RevokeOne(ctx, e.ID, w.clk.Now(), "matrix reset"); err != nil && !errors.Is(err, entitlements.ErrNotFound) {
				return err
			}
		}
		return nil
	case "consume":
		_, err := w.svc.Consume(ctx, org, feature, 1, entitlements.Ref{Type: feature.RefType(), ID: uuid.NewV7()})
		return err
	case "expect_not_entitled":
		if feature.Kind() == entitlements.KindAccess {
			av, err := w.svc.Available(ctx, org, feature)
			if err != nil {
				return err
			}
			if av.Entitled() {
				return fmt.Errorf("%s still entitled to %s: %+v", ref, feature, av)
			}
			return nil
		}
		_, err := w.svc.Consume(ctx, org, feature, 1, entitlements.Ref{Type: feature.RefType(), ID: uuid.NewV7()})
		if !errors.Is(err, entitlements.ErrNoEntitlement) {
			return fmt.Errorf("consume %s for %s: %w, want entitlements.not_entitled", feature, ref, err)
		}
		return nil
	}
	return fmt.Errorf("unknown entitlements hook %q", name)
}
