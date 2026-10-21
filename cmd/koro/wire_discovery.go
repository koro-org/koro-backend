package main

import (
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
//	deriver  nil → a crypto.Deriver over CRYPTO_TOKEN_KEYS (pseudonyms and
//	         session seeds: the "discovery.anon_id" / "discovery.session_seed"
//	         subkeys); tests preset the shared test deriver with keyIDs
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
		ring, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
		if err != nil {
			return fmt.Errorf("discovery: CRYPTO_TOKEN_KEYS: %w", err)
		}
		if deriver, err = crypto.NewDeriver(ring); err != nil {
			return fmt.Errorf("discovery: CRYPTO_TOKEN_KEYS: %w", err)
		}
		keyIDs = make([]string, len(ring))
		for i, k := range ring {
			keyIDs[i] = k.ID
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

// tasks: none (discovery owns no table, event or job).
func (w *discoveryWiring) tasks(c *container) {}

// routes: searchDiscovery and getDiscoveryProfile (04 §2.10); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *discoveryWiring) routes(c *container, api huma.API) { discovery.Register(api, w.svc) }
