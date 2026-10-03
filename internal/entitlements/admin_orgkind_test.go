package entitlements_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
)

// TestAdminGrant_OrgMustBeCompany (06 H-40, T-148; BE-017): with the orgs
// kind check bound, a grant goes to a company only: a club or university
// is 422 entitlements.org_not_company, a missing org 404, and a failing
// lookup fails the grant (nothing is written).
func TestAdminGrant_OrgMustBeCompany(t *testing.T) {
	t.Parallel()
	kinds := map[uuid.UUID]string{
		authztest.ID("company_a"): "company", authztest.ID("club_a"): "club", authztest.ID("uni_a"): "university",
	}
	broken := authztest.ID("broken_org")
	clk := clock.NewFake(t0)
	store := entitlements.NewMemStore(clk.Now)
	mod, err := entitlements.New(entitlements.Deps{
		Store: store, Tx: db.NopTx{}, Audit: &auditLog{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		OrgKind: func(_ context.Context, id uuid.UUID) (string, bool, error) {
			if id == broken {
				return "", false, errors.New("lookup failed")
			}
			k, ok := kinds[id]
			return k, ok, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	_, in := adminGrant("company_a")
	for _, tc := range []struct {
		org    uuid.UUID
		status int
		code   string
	}{
		{authztest.ID("club_a"), 422, "entitlements.org_not_company"},
		{authztest.ID("uni_a"), 422, "entitlements.org_not_company"},
		{authztest.ID("missing_org"), 404, "not_found"},
	} {
		_, err := mod.Service.AdminGrant(t.Context(), fin, tc.org, in)
		if st, code := problemOf(err); st != tc.status || code != tc.code {
			t.Fatalf("grant to %s = %d %q (%v), want %d %s", tc.org, st, code, err, tc.status, tc.code)
		}
	}
	if _, err := mod.Service.AdminGrant(t.Context(), fin, broken, in); err == nil {
		t.Fatal("a failing org lookup must fail the grant")
	}
	for _, org := range []uuid.UUID{authztest.ID("club_a"), authztest.ID("uni_a"), broken} {
		rows, err := store.List(t.Context(), org, entitlements.ListFilter{}, nil, 10)
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows of a refused org = %d, %v", len(rows), err)
		}
	}
	e, err := mod.Service.AdminGrant(t.Context(), fin, authztest.ID("company_a"), in)
	if err != nil || e.OrgID != authztest.ID("company_a") {
		t.Fatalf("grant to a company = %+v, %v", e, err)
	}
}
