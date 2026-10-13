package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory catalog stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-038.
// Only tickets of the catalog module edit this file (BE-001 R4).
func (w *catalogWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) catalog.Store { return catalog.NewMemStore(c.infra.clock.Now) }
}

// TestWire_Catalog pins what the catalog module provides and consumes: its
// service and the Offerings contract of orders, the subscriptions of 01
// §6.2 (event.cancelled → the bound offerings; the offering notices) and
// the policy notice task (06 H-17).
func TestWire_Catalog(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.catalog.svc == nil || c.catalog.offerings == nil {
		t.Fatal("catalog service or Offerings not provided")
	}
	for topic, name := range map[string]string{
		domainevent.TopicEventCancelled:    catalog.CancelEventOfferingsTaskName,
		domainevent.TopicOfferingSubmitted: catalog.NotifyOfferingSubmittedTaskName,
		domainevent.TopicOfferingDecided:   catalog.NotifyOfferingDecidedTaskName,
		domainevent.TopicOfferingCancelled: catalog.NotifyOfferingCancelledTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no %s subscription", topic, name)
		}
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(task queue.Task) bool { return task.Name == catalog.NotifyPolicyChangedTaskName }) {
		t.Error("the policy notice task is not registered")
	}
}

// TestSeed_DefaultCommercePolicy pins the catalog step of `koro seed
// bootstrap-commerce` (02 §18.3 MVP seed, T-10): production without
// -commission-rate-bp writes no policy; elsewhere the development default
// (model A, 10 % from the seller, net of VAT) is written once, recorded as
// created by the first platform admin in effect; a second run leaves it
// alone; an explicit rate is taken as given; without a platform admin the
// step fails (seed bootstrap-admin first).
func TestSeed_DefaultCommercePolicy(t *testing.T) {
	t.Parallel()
	pa, pa2 := authztest.ID("platform_admin"), authztest.ID("platform_admin_2")
	first := pa
	if compareUUIDs(pa2, pa) < 0 {
		first = pa2
	}
	seed := func(env string, rate int, admins []uuid.UUID) (catalog.Store, catalogSeed) {
		store := catalog.NewMemStore(clock.Real().Now)
		au := audit.New(audit.Deps{Store: audit.NewMemStore(), Tx: db.NopTx{}, Clock: clock.Real(), Log: obs.Discard()}).Service
		return store, catalogSeed{
			deps:   catalog.SeederDeps{Store: store, Tx: db.NopTx{}, Audit: au, Clock: clock.Real(), Log: obs.Discard()},
			admins: func(context.Context) ([]uuid.UUID, error) { return admins, nil },
			env:    env, rateBP: rate,
		}
	}
	policies := func(store catalog.Store) []catalog.CommercePolicy {
		ps, err := store.ListPolicies(t.Context(), catalog.PolicyFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}

	t.Run("production without a rate writes nothing", func(t *testing.T) {
		t.Parallel()
		store, s := seed(config.EnvProd, -1, []uuid.UUID{pa, pa2})
		var out bytes.Buffer
		if err := seedDefaultCommercePolicy(t.Context(), s, &out); err != nil {
			t.Fatal(err)
		}
		if ps := policies(store); len(ps) != 0 || !strings.Contains(out.String(), "not written") {
			t.Fatalf("policies %+v, output %q", ps, out.String())
		}
	})
	t.Run("development default once", func(t *testing.T) {
		t.Parallel()
		store, s := seed(config.EnvDev, -1, []uuid.UUID{pa, pa2})
		var out bytes.Buffer
		if err := seedDefaultCommercePolicy(t.Context(), s, &out); err != nil {
			t.Fatal(err)
		}
		s.rateBP = 500
		if err := seedDefaultCommercePolicy(t.Context(), s, &out); err != nil {
			t.Fatal(err)
		}
		ps := policies(store)
		if len(ps) != 1 {
			t.Fatalf("policies = %+v", ps)
		}
		p := ps[0]
		if p.Scope != catalog.ScopeDefault || p.ModelCode != catalog.ModelA || p.CollectionMethod != catalog.CollectBankTransfer ||
			p.CommissionRateBP != catalog.DevCommissionRateBP || p.CommissionPayer != catalog.PayerSeller ||
			p.CommissionBasis != catalog.BasisNetExclVAT || p.InvoiceAutomation != catalog.InvoiceManual || p.CreatedBy != first {
			t.Fatalf("default policy = %+v", p)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || !strings.HasSuffix(lines[0], ": created") || !strings.HasSuffix(lines[1], "left unchanged") {
			t.Fatalf("seed output:\n%s", out.String())
		}
	})
	t.Run("an explicit rate in production", func(t *testing.T) {
		t.Parallel()
		store, s := seed(config.EnvProd, 750, []uuid.UUID{pa})
		if err := seedDefaultCommercePolicy(t.Context(), s, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if ps := policies(store); len(ps) != 1 || ps[0].CommissionRateBP != 750 || ps[0].CreatedBy != pa {
			t.Fatalf("policies = %+v", ps)
		}
	})
	t.Run("no platform admin", func(t *testing.T) {
		t.Parallel()
		store, s := seed(config.EnvDev, -1, nil)
		if err := seedDefaultCommercePolicy(t.Context(), s, &bytes.Buffer{}); !errors.Is(err, errNoAdmin) {
			t.Fatalf("err = %v, want errNoAdmin", err)
		}
		if ps := policies(store); len(ps) != 0 {
			t.Fatalf("policies = %+v", ps)
		}
	})
}
