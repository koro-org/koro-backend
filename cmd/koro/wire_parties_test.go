package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory parties store used by service, wiring
// and authz matrix tests (05 §13.2). First owner: BE-036.
// Only tickets of the parties module edit this file (BE-001 R4).
// updated_at uses the real clock: these tests do not depend on it.
func (w *partiesWiring) useMemStore() {
	w.store = parties.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_Parties pins what the parties module provides: the service, its
// KVKK provider, the rewrap declaration of legal_parties.tckn_enc, and
// 06 H-54 (a natural-person payee party is refused; cmd/koro never lifts it).
func TestWire_Parties(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.parties.svc == nil {
		t.Fatal("parties service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "parties" }) {
		t.Fatal("parties KVKK provider not registered")
	}
	if !slices.ContainsFunc(c.keys.columns, func(col rewrap.Column) bool {
		return col.Table == "legal_parties" && col.Column == "tckn_enc" && col.PurposePrefix == parties.TCKNSealPurposePrefix &&
			col.PurposeColumn == "id"
	}) {
		t.Fatal("legal_parties.tckn_enc is not declared for crypto.rewrap")
	}
	pad, _ := authztest.Get("platform_admin")
	_, err := c.parties.svc.AdminCreateParty(authz.WithActor(t.Context(), pad.Actor), pad.Actor, parties.PartyInput{
		Kind: parties.KindPayee, LegalType: parties.LegalIndividual, Title: "Ayşe Yılmaz", TCKN: "10000000146",
	})
	if !errors.Is(err, parties.ErrNaturalPersonPayee) {
		t.Fatalf("individual payee party: %v, want parties.natural_person_payee (06 H-54)", err)
	}
}

// TestSeed_BootstrapCommerce_DraftIdempotent pins `koro seed
// bootstrap-commerce` (T-31): the first run writes Koro's party as a draft,
// the second leaves it alone; extra arguments are a usage error.
func TestSeed_BootstrapCommerce_DraftIdempotent(t *testing.T) {
	t.Parallel()
	store := parties.NewMemStore(clock.Real().Now)
	auditStore := audit.NewMemStore()
	au := audit.New(audit.Deps{Store: auditStore, Tx: db.NopTx{}, Clock: clock.Real(), Log: obs.Discard()}).Service
	d := parties.SeederDeps{Store: store, Tx: db.NopTx{}, Audit: au, Clock: clock.Real(), Log: obs.Discard()}
	var out bytes.Buffer
	if err := seedBootstrapCommerce(t.Context(), d, &out); err != nil {
		t.Fatal(err)
	}
	if err := seedBootstrapCommerce(t.Context(), d, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "status draft): created as draft") ||
		!strings.Contains(lines[1], "already present, left unchanged") {
		t.Fatalf("seed output:\n%s", out.String())
	}
	p, err := store.GetPlatform(t.Context())
	if err != nil || p.Status != parties.StatusDraft || p.Kind != parties.KindPlatform {
		t.Fatalf("platform party = %+v %v", p, err)
	}
	if err := runSeed(t.Context(), []string{"bootstrap-commerce", "extra"}, nil, &out); !errors.Is(err, errUsage) {
		t.Fatalf("seed bootstrap-commerce extra: %v, want a usage error", err)
	}
}
