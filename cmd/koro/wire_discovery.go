package main

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/discovery"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/students"
)

// discoveryWiring wires the discovery module (L9; 01 §4.16). First owner: BE-051.
// Only tickets of the discovery module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	tx       nil → the app_rw pool's TxManager (the audit record of every
//	         search and read); tests preset db.NopTx
//	deriver  nil → a crypto.Deriver over discovery's keyring,
//	         DISCOVERY_ANON_KEYS (pseudonyms and session seeds: the
//	         "discovery.anon_id" / "discovery.session_seed" subkeys; BE-102,
//	         T-329). Unset (dev only: config requires it in staging/prod) →
//	         CRYPTO_TOKEN_KEYS as before BE-102, with a warning. Its active key
//	         must not be a CRYPTO_TOKEN_KEYS key (separate lifecycles); tests
//	         preset the shared test deriver with keyIDs
//
// Consumed (built in lower layers): c.students.svc (Discoverable and its
// bulk side students.DiscoveryIndex, BE-051), c.talent.mod.Ranking (bands),
// c.entitlements.svc (discovery_access), c.orgs.svc (company status) and
// c.audit.svc.
//
// Provided, set in build: svc (discovery.Service) and hooks.anonResolver
// (messaging, BE-056: company-scoped anon_id → discoverable student).
// No task: discovery owns no table and no job.
type discoveryWiring struct {
	tx      db.TxManager
	deriver *crypto.Deriver
	keyIDs  []string // the deriver's keyring ids, active first (with deriver)

	// late is the container, kept for the matrix fixtures (a discoverable
	// student is set up through the students service).
	late *container

	svc discovery.Service // set by build
}

func (w *discoveryWiring) build(c *container) error {
	tx := w.tx
	if tx == nil && c.infra.db != nil {
		tx = c.infra.db.Tx
	}
	if tx == nil {
		return errors.New("discovery: no transaction manager (database not opened)")
	}
	deriver, keyIDs := w.deriver, w.keyIDs
	if deriver == nil {
		var err error
		if deriver, keyIDs, err = discoveryDeriver(c); err != nil {
			return err
		}
	}
	pseudonyms, err := discovery.NewPseudonyms(deriver, keyIDs)
	if err != nil {
		return err
	}
	// The level 2 index is a method set of the students service next to its
	// Discoverable contract (BE-051); without it there is no discovery.
	idx, ok := c.students.svc.(interface {
		students.Discoverable
		students.DiscoveryIndex
	})
	if !ok {
		return errors.New("discovery: students does not offer the level 2 index (students.DiscoveryIndex)")
	}
	if c.talent.mod.Ranking == nil || c.entitlements.svc == nil || c.orgs.svc == nil || c.audit.svc == nil {
		return errors.New("discovery: talent, entitlements, orgs or audit is not built")
	}
	mod, err := discovery.New(discovery.Deps{
		Students: idx, Ranking: c.talent.mod.Ranking, Entitlements: c.entitlements.svc, Orgs: c.orgs.svc,
		Audit: c.audit.svc, Tx: tx, Pseudonyms: pseudonyms, Log: c.infra.log.With("module", "discovery"),
	})
	if err != nil {
		return err
	}
	w.svc, w.late = mod.Service, c
	c.hooks.anonResolver = mod.Service.Resolve
	return nil
}

// discoveryDeriver builds the deriver of the pseudonyms and session seeds
// over DISCOVERY_ANON_KEYS (BE-102). Its active key must differ from every
// CRYPTO_TOKEN_KEYS key: otherwise a token rotation would still rename the
// students. A token key may only follow it, as the previous key of a
// transition window (the pseudonyms of before BE-102 keep resolving). Error
// messages name key ids, never key material.
func discoveryDeriver(c *container) (*crypto.Deriver, []string, error) {
	anon, err := c.infra.cfg.Discovery.AnonKeyring()
	if err != nil {
		return nil, nil, fmt.Errorf("discovery: DISCOVERY_ANON_KEYS: %w", err)
	}
	tokens, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
	source := "DISCOVERY_ANON_KEYS"
	switch {
	case len(anon) == 0 && err != nil:
		return nil, nil, fmt.Errorf("discovery: CRYPTO_TOKEN_KEYS: %w", err)
	case len(anon) == 0:
		// Dev only (config refuses staging/prod without the keyring).
		c.infra.log.Warn("discovery: DISCOVERY_ANON_KEYS is not set; pseudonyms derive from CRYPTO_TOKEN_KEYS " +
			"(development fallback, T-329)")
		anon, source = tokens, "CRYPTO_TOKEN_KEYS"
	case err == nil:
		for _, t := range tokens {
			if subtle.ConstantTimeCompare(anon[0].Secret, t.Secret) == 1 {
				return nil, nil, fmt.Errorf("discovery: DISCOVERY_ANON_KEYS: the active key %q is also a CRYPTO_TOKEN_KEYS key "+
					"(T-329: generate a separate one; a token key may only follow it)", anon[0].ID)
			}
		}
	}
	d, err := crypto.NewDeriver(anon)
	if err != nil {
		return nil, nil, fmt.Errorf("discovery: %s: %w", source, err)
	}
	ids := make([]string, len(anon))
	for i, k := range anon {
		ids[i] = k.ID
	}
	return d, ids, nil
}

// tasks: none (discovery owns no table, event or job).
func (w *discoveryWiring) tasks(c *container) {}

// routes: searchDiscovery and getDiscoveryProfile (04 §2.10); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *discoveryWiring) routes(c *container, api huma.API) { discovery.Register(api, w.svc) }
