package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// SeedJob is the audit system actor of the default policy of `koro seed
// bootstrap-commerce`.
const SeedJob = "catalog.bootstrap_commerce"

// DevCommissionRateBP is the commission of the development default policy
// (02 §18.3, T-10: "geliştirme seed'i %10"). The live rate is the team's
// decision (E-01/E-02); the seed never writes it in production on its own.
const DevCommissionRateBP = 1000

// DefaultPolicy is the MVP default commerce policy (02 §18.3 "MVP seed",
// ADR-0014, T-10): model A (marketplace: bank transfer to Koro, payout to
// the payee), commission from the seller on the amount net of VAT, manual
// invoices, the commission VAT 20 %, a 7-day dispute window, no payout
// hold. rateBP is the commission (no default here: the caller decides).
func DefaultPolicy(rateBP int) PolicyInput {
	return PolicyInput{
		Scope: ScopeDefault, ModelCode: ModelA, CollectionMethod: CollectBankTransfer, CommissionRateBP: &rateBP,
		CommissionBasis: BasisNetExclVAT, CommissionPayer: PayerSeller, InvoiceAutomation: InvoiceManual,
		Notes: "bootstrap-commerce default (02 §18.3 MVP seed)",
	}
}

// SeederDeps are the dependencies of the seeder (all required).
type SeederDeps struct {
	Store Store
	Tx    db.TxManager
	Audit AuditLogger
	Clock clock.Clock
	Log   *slog.Logger
}

// Seeder writes the default commerce policy (`koro seed
// bootstrap-commerce`, CLI only, never over HTTP).
type Seeder struct{ d SeederDeps }

// NewSeeder returns the seeder; a missing dependency is an error.
func NewSeeder(d SeederDeps) (*Seeder, error) {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil}} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("catalog.SeederDeps.%s is missing", c.name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Seeder{d: d}, nil
}

// EnsureDefaultPolicy writes DefaultPolicy(rateBP) from now on, recorded as
// created by createdBy (an admin in effect: commerce_policies.created_by is
// a user), unless a default policy is already in effect now or starts
// later: then that one is returned with created=false and nothing changes
// (running the seed again never overrides an admin's policy). Audited as
// the system job SeedJob.
func (sd *Seeder) EnsureDefaultPolicy(ctx context.Context, rateBP int, createdBy uuid.UUID) (p CommercePolicy, created bool, err error) {
	now := sd.d.Clock.Now().UTC().Truncate(time.Microsecond)
	row, err := policyOf(DefaultPolicy(rateBP), now)
	if err != nil {
		return CommercePolicy{}, false, err
	}
	row.ID, row.CreatedBy = uuid.NewV7(), createdBy
	err = sd.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		all, err := sd.d.Store.ListPolicies(ctx, PolicyFilter{Scope: ScopeDefault})
		if err != nil {
			return err
		}
		for _, cur := range all {
			if cur.ValidTo == nil || cur.ValidTo.After(now) {
				p = cur // in effect now or later: the admins' decision stands
				return nil
			}
		}
		if p, err = sd.d.Store.InsertPolicy(ctx, row); err != nil {
			return err
		}
		created = true
		return sd.d.Audit.Log(ctx, policyEvent(authz.SystemActor(SeedJob), p, "created", ""))
	})
	if err != nil {
		return CommercePolicy{}, false, err
	}
	sd.d.Log.InfoContext(ctx, "default commerce policy ensured", "policy_id", p.ID.String(), "created", created,
		"commission_rate_bp", p.CommissionRateBP)
	return p, created, nil
}
