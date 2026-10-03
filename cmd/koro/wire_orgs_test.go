package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory orgs stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-017.
// Only tickets of the orgs module edit this file (BE-001 R4).
func (w *orgsWiring) useMemStore() {
	w.store = orgs.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_OrgsProvidesDirectoryPrivacyAndOrgKind pins what orgs binds in
// build: its service, its KVKK provider and entitlements' org kind lookup
// (06 H-40), which answers from the orgs store.
func TestWire_OrgsProvidesDirectoryPrivacyAndOrgKind(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.orgs.svc == nil {
		t.Fatal("orgs service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "orgs" }) {
		t.Fatal("orgs privacy provider not registered")
	}
	if c.entitlements.orgKind == nil {
		t.Fatal("entitlements' org kind lookup is not bound (06 H-40)")
	}
	ctx := t.Context()
	club, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Satranç Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	if kind, found, err := c.entitlements.orgKind(ctx, club); err != nil || !found || kind != "club" {
		t.Fatalf("orgKind(club) = %q, %v, %v", kind, found, err)
	}
	if _, found, err := c.entitlements.orgKind(ctx, uuid.NewV7()); err != nil || found {
		t.Fatalf("orgKind(missing) found=%v err=%v", found, err)
	}
}

// TestWire_OrgsReadsOpenCommerceAtCallTime: a check appended to
// hooks.openCommerce after build (orders and ledger build later) blocks
// closeOrg; the R proof is verified by identity (an account with the
// matrix password).
func TestWire_OrgsReadsOpenCommerceAtCallTime(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	admin, _ := authztest.Get("platform_admin")
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	email := matrixEmail(admin.Name)
	if err := c.identity.store.InsertUser(ctx, identity.User{
		ID: admin.User(), Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		PasswordHash: testPasswordHash(), FirstName: "Ayla", LastName: "Admin", Persona: identity.PersonaPlatform,
		Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	id, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindCompany, Name: "Kapanan Şirket"})
	if err != nil {
		t.Fatal(err)
	}
	var asked []uuid.UUID
	c.hooks.openCommerce = append(c.hooks.openCommerce, func(_ context.Context, orgID uuid.UUID) (bool, error) {
		asked = append(asked, orgID)
		return true, nil
	})
	proof := identity.ReauthProof{Password: matrixReauthPassword}
	if _, err := c.orgs.svc.CloseOrg(ctx, admin.Actor, id, "Pilot sona erdi", proof); !errors.Is(err, orgs.ErrOpenCommerce) {
		t.Fatalf("closeOrg with open commerce = %v, want orgs.open_commerce", err)
	}
	if len(asked) != 1 || asked[0] != id {
		t.Fatalf("open-commerce hook asked for %v", asked)
	}
	c.hooks.openCommerce = nil
	v, err := c.orgs.svc.CloseOrg(ctx, admin.Actor, id, "Pilot sona erdi", proof)
	if err != nil || v.Org.Status != orgs.StatusClosed {
		t.Fatalf("closeOrg without open commerce = %+v, %v", v.Org, err)
	}
}
