package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/platform/config"
)

// catalogSeed is what `koro seed bootstrap-commerce` needs to write the
// default commerce policy (BE-038; tested with the in-memory stores).
type catalogSeed struct {
	deps catalog.SeederDeps
	// admins lists the platform admins in effect (06 H-33): the policy is
	// recorded as created by the first (commerce_policies.created_by is a
	// user; the audit actor is the system job catalog.bootstrap_commerce).
	admins func(ctx context.Context) ([]uuid.UUID, error)
	env    string // APP_ENV
	// rateBP is -commission-rate-bp: < 0 when not given.
	rateBP int
}

// errNoAdmin: the default policy needs a platform admin (seed bootstrap-admin first).
var errNoAdmin = errors.New("no platform admin in effect: run `koro seed bootstrap-admin` first")

// seedDefaultCommercePolicy ensures the default commerce policy (02 §18.3
// MVP seed: model A, bank transfer, commission from the seller on the
// amount net of VAT, manual invoices). The rate (T-10) is
// -commission-rate-bp when given; otherwise the development default
// (catalog.DevCommissionRateBP, 10 %) outside production, and nothing at
// all in production: the live rate is the team's decision and never
// entered by default. A default policy in effect now or later is left as
// it is (idempotent).
func seedDefaultCommercePolicy(ctx context.Context, s catalogSeed, stdout io.Writer) error {
	rate := s.rateBP
	if rate < 0 {
		if s.env == config.EnvProd {
			_, _ = fmt.Fprintln(stdout, "default commerce policy: not written (production takes the commission rate from the team, "+
				"T-10): run again with -commission-rate-bp <basis points> once it is decided")
			return nil
		}
		rate = catalog.DevCommissionRateBP
	}
	sd, err := catalog.NewSeeder(s.deps)
	if err != nil {
		return err
	}
	admins, err := s.admins(ctx)
	if err != nil {
		return err
	}
	if len(admins) == 0 {
		return errNoAdmin
	}
	slices.SortFunc(admins, func(a, b uuid.UUID) int { return compareUUIDs(a, b) })
	p, created, err := sd.EnsureDefaultPolicy(ctx, rate, admins[0])
	if err != nil {
		return err
	}
	state := "already present, left unchanged"
	if created {
		state = "created"
	}
	_, _ = fmt.Fprintf(stdout, "default commerce policy %s (model %s, commission %d bp from the %s): %s\n", p.ID, p.ModelCode,
		p.CommissionRateBP, p.CommissionPayer, state)
	return nil
}

func compareUUIDs(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
