package parties

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// SeedJob is the audit system actor of `koro seed bootstrap-commerce`.
const SeedJob = "parties.bootstrap_commerce"

// Placeholder data of the seeded platform party (T-31): Koro's legal entity
// is not founded yet, so the row is a draft that a finance or platform
// admin completes (adminUpdateLegalParty) and verifies before any invoice
// names it.
const (
	PlatformDraftTitle = "Koro"
	platformDraftType  = LegalCompany
)

// SeederDeps are the dependencies of the seeder (all required).
type SeederDeps struct {
	Store Store
	Tx    db.TxManager
	Audit AuditLogger
	Clock clock.Clock
	Log   *slog.Logger
}

// Seeder writes the draft platform party (00 T-31). It is reachable from
// the CLI only (`koro seed bootstrap-commerce`), never over HTTP.
type Seeder struct{ d SeederDeps }

// NewSeeder returns the seeder; a missing dependency is an error.
func NewSeeder(d SeederDeps) (*Seeder, error) {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil}} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("parties.SeederDeps.%s is missing", c.name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Seeder{d: d}, nil
}

// EnsurePlatform writes the platform party as a draft when there is none and
// returns it with created=true; when one exists (any status, filled in or
// not) it is returned unchanged with created=false, so running the seed
// again never overwrites an admin's data. The write is audited.
func (sd *Seeder) EnsurePlatform(ctx context.Context) (p LegalParty, created bool, err error) {
	err = sd.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, created, err = sd.d.Store.InsertPlatformIfAbsent(ctx, LegalParty{
			ID: uuid.NewV7(), Kind: KindPlatform, LegalType: platformDraftType, Title: PlatformDraftTitle,
			VATStatus: VATUnknown, Status: StatusDraft,
		})
		if err != nil || !created {
			return err
		}
		meta := map[string]any{"legal_type": string(p.LegalType), "status": string(p.Status), "seed": "bootstrap-commerce"}
		return sd.d.Audit.Log(ctx, partyEvent(authz.SystemActor(SeedJob), authz.PartiesLegalPartyCreate, nil, p, "", meta))
	})
	if err != nil {
		return LegalParty{}, false, err
	}
	sd.d.Log.InfoContext(ctx, "platform legal party ensured", "party_id", p.ID.String(), "created", created,
		"status", string(p.Status))
	return p.withoutSecrets(), created, nil
}
