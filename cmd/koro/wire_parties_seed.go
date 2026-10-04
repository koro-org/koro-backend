package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"koro/internal/audit"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/obs"
)

// runSeedBootstrapCommerce runs `koro seed bootstrap-commerce` (BE-036;
// 00 T-31): it writes Koro's own legal party (kind platform) as a draft
// when there is none. Koro's legal entity is not founded yet, so finance
// completes and verifies the party later (adminUpdateLegalParty,
// adminVerifyLegalParty); until then no invoice names it. Idempotent: an
// existing platform party (any status) is left as it is. No personal data,
// so it runs in every environment. CLI only, audited as the system job
// parties.bootstrap_commerce.
func runSeedBootstrapCommerce(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	fset := flag.NewFlagSet("seed bootstrap-commerce", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("%w: seed bootstrap-commerce takes no arguments", errUsage)
	}
	cfg, err := config.LoadSeed(environ)
	if err != nil {
		return err
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-seed", Env: cfg.Env, Version: version})
	clk := clock.Real()
	database, err := openDB(ctx, cfg.Platform.DB, "koro-seed", clk, log)
	if err != nil {
		return err
	}
	defer database.Close()
	au := audit.New(audit.Deps{Store: audit.NewGormStore(database.ORM), Tx: database.Tx, Clock: clk, Log: log.With("module", "audit")}).Service
	return seedBootstrapCommerce(ctx, parties.SeederDeps{
		Store: parties.NewGormStore(database.ORM), Tx: database.Tx, Audit: au, Clock: clk, Log: log.With("module", "parties"),
	}, stdout)
}

// seedBootstrapCommerce ensures the draft platform party and prints what it
// did (tested with the in-memory stores).
func seedBootstrapCommerce(ctx context.Context, d parties.SeederDeps, stdout io.Writer) error {
	sd, err := parties.NewSeeder(d)
	if err != nil {
		return err
	}
	p, created, err := sd.EnsurePlatform(ctx)
	if err != nil {
		return err
	}
	state := "already present, left unchanged"
	if created {
		state = "created as draft: complete and verify it (adminUpdateLegalParty, adminVerifyLegalParty) before invoicing"
	}
	_, _ = fmt.Fprintf(stdout, "platform legal party %s (status %s): %s\n", p.ID, p.Status, state)
	return nil
}
