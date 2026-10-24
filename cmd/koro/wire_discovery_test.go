package main

import (
	"fmt"
	"strings"
	"testing"
	"uuid"

	"koro/internal/discovery"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// useMemStore installs the in-memory discovery stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-051.
// Only tickets of the discovery module edit this file (BE-001 R4).
// Discovery owns no store: the seams are the transaction manager of its
// audit records and the shared test token deriver (identity's).
func (w *discoveryWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.deriver, w.keyIDs = testDeriver(), []string{"test1"}
}

// TestWire_DiscoveryFillsAnonResolverAndRoutes (BE-051): discovery builds
// over the students level 2 index, talent's bands, entitlements, orgs and
// audit, fills hooks.anonResolver (messaging) and registers its two
// operations on the org surface; a pseudonym nobody holds resolves to
// nothing, and the pseudonym of the hook is the one of the service.
func TestWire_DiscoveryFillsAnonResolverAndRoutes(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.discovery.svc == nil || c.hooks.anonResolver == nil {
		t.Fatal("discovery did not set its service or hooks.anonResolver")
	}
	company, user := uuid.NewV7(), uuid.NewV7()
	anon, err := c.discovery.svc.AnonID(company, user)
	if err != nil || !discovery.ValidAnonID(anon) {
		t.Fatalf("AnonID = %q, %v", anon, err)
	}
	if id, found, err := c.hooks.anonResolver(t.Context(), company, anon); err != nil || found || id != uuid.Nil() {
		t.Fatalf("resolver for a student without a profile = %v, %v, %v; want not found", id, found, err)
	}
	if _, found, _ := c.hooks.anonResolver(t.Context(), company, "not-a-pseudonym"); found {
		t.Fatal("a malformed pseudonym resolved")
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	paths := app.API.OpenAPI().Paths
	for _, tc := range []struct{ path, id string }{
		{"/api/v1/orgs/{org_id}/discovery/profiles", "searchDiscovery"},
		{"/api/v1/orgs/{org_id}/discovery/profiles/{anon_id}", "getDiscoveryProfile"},
	} {
		p := paths[tc.path]
		if p == nil || p.Get == nil || p.Get.OperationID != tc.id {
			t.Fatalf("%s: operation %s not registered", tc.path, tc.id)
		}
		if fmt.Sprint(p.Get.Extensions["x-koro-surface"]) != "org" {
			t.Fatalf("%s: surface %v", tc.id, p.Get.Extensions["x-koro-surface"])
		}
	}
	if rl := fmt.Sprint(paths["/api/v1/orgs/{org_id}/discovery/profiles"].Get.Extensions["x-koro-rate-limit"]); rl != "search" {
		t.Fatalf("searchDiscovery rate class = %v, want search (04 §1.9)", rl)
	}
}

// Fake keys of the discovery keyring tests (BE-102): counting byte
// sequences, obviously not generated.
const (
	fakeAnonKeyA  = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // 0x00…0x1f
	fakeAnonKeyB  = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=" // 0x20…0x3f
	fakeTokenKeyA = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=" // 0x40…0x5f
	fakeTokenKeyB = "YGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6e3x9fn8=" // 0x60…0x7f
)

// TestWire_DiscoveryAnonKeyring (BE-102, T-329): without the test seam,
// discovery derives its pseudonyms from DISCOVERY_ANON_KEYS (active key
// first), so a rotation of CRYPTO_TOKEN_KEYS renames nobody; a former token
// key may follow the active one (transition window). Unset (dev) it falls
// back to CRYPTO_TOKEN_KEYS as BE-051 did; an active key that is also a
// token key refuses to build, naming ids only.
func TestWire_DiscoveryAnonKeyring(t *testing.T) {
	t.Parallel()
	company, user := uuid.NewV7(), uuid.NewV7()
	build := func(anonKeys, tokenKeys string) (string, error) {
		t.Helper()
		c := newTestContainer(t)
		c.discovery.deriver, c.discovery.keyIDs = nil, nil // the production path
		c.infra.cfg.Discovery = config.Discovery{AnonKeys: config.NewSecret(anonKeys)}
		c.infra.platform.Crypto.TokenKeys = config.NewSecret(tokenKeys)
		if err := c.build(); err != nil {
			return "", err
		}
		return c.discovery.svc.AnonID(company, user)
	}
	derive := func(id, b64 string) string {
		t.Helper()
		secret, err := crypto.ParseKey(b64)
		if err != nil {
			t.Fatal(err)
		}
		d, err := crypto.NewDeriver([]crypto.Key{{ID: id, Secret: secret}})
		if err != nil {
			t.Fatal(err)
		}
		ps, err := discovery.NewPseudonyms(d, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		anon, err := ps.AnonID(company, user)
		if err != nil {
			t.Fatal(err)
		}
		return anon
	}

	rotatedAnon := "anon2:" + fakeAnonKeyB + ",anon1:" + fakeAnonKeyA
	got, err := build(rotatedAnon, "tok1:"+fakeTokenKeyA)
	if err != nil || got != derive("anon2", fakeAnonKeyB) {
		t.Fatalf("pseudonym %q, %v; want the active DISCOVERY_ANON_KEYS key's", got, err)
	}
	if again, err := build(rotatedAnon, "tok2:"+fakeTokenKeyB+",tok1:"+fakeTokenKeyA); err != nil || again != got {
		t.Fatalf("a token rotation renamed the student: %q → %q, %v", got, again, err)
	}
	// Transition: a former token key as the previous discovery key.
	if got, err := build("anon1:"+fakeAnonKeyA+",tok1:"+fakeTokenKeyA, "tok2:"+fakeTokenKeyB); err != nil || got != derive("anon1", fakeAnonKeyA) {
		t.Fatalf("transition ring = %q, %v", got, err)
	}
	// Dev fallback: the token keyring, as before BE-102.
	if got, err := build("", "tok1:"+fakeTokenKeyA); err != nil || got != derive("tok1", fakeTokenKeyA) {
		t.Fatalf("fallback pseudonym %q, %v; want the token key's", got, err)
	}
	// The active key must not be a token key (here under another id).
	_, err = build("anon1:"+fakeTokenKeyA, "tok2:"+fakeTokenKeyB+",tok1:"+fakeTokenKeyA)
	if err == nil || !strings.Contains(err.Error(), "DISCOVERY_ANON_KEYS") || !strings.Contains(err.Error(), `"anon1"`) {
		t.Fatalf("a shared active key built: %v", err)
	}
	if strings.Contains(err.Error(), fakeTokenKeyA[:12]) {
		t.Fatalf("the error leaks key material: %v", err)
	}
	if _, err := build("anon1:c2hvcnQ=", "tok1:"+fakeTokenKeyA); err == nil || strings.Contains(err.Error(), "c2hvcnQ") {
		t.Fatalf("a short key = %v", err)
	}
}
