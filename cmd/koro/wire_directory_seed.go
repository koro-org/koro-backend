package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"koro/db/reference"
	"koro/internal/audit"
	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
)

// runSeedReference runs `koro seed reference [-dir DIR]` (BE-020; 02 §23:
// reference data is a command, not a migration). It upserts the public
// reference data (universities with their orgs, e-mail domains, faculties,
// departments, skills) from the CSV files embedded in the binary
// (db/reference) or from DIR. Public data only, so it runs in every
// environment; it is idempotent (a second run changes nothing).
func runSeedReference(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	fset := flag.NewFlagSet("seed reference", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	dir := fset.String("dir", "", "read the CSV files from this directory instead of the embedded copy")
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("%w: seed reference takes only -dir", errUsage)
	}
	var src fs.FS = reference.FS
	if *dir != "" {
		src = os.DirFS(*dir)
	}
	data, err := directory.ParseReference(src)
	if err != nil {
		return err
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
	// orgs opens the university orgs (OrgAdmin.Create); the import emits
	// no event and takes no Idempotency-Key, but orgs requires both.
	orgMod, err := orgs.New(orgs.Deps{
		Store: orgs.NewGormStore(database.ORM), Tx: database.Tx, Audit: au, Clock: clk, Log: log.With("module", "orgs"),
		Outbox: outbox.New(outbox.Deps{Store: outbox.NewGormStore(database.ORM), Tx: database.Tx, Subs: outbox.NewRegistry(), Clock: clk,
			Log: log.With("component", "outbox")}),
		Idempotency: idempotency.New(idempotency.Deps{Tx: database.Tx, Store: idempotency.NewGormStore(database.ORM), Clock: clk,
			Log: log.With("component", "idempotency")}),
	})
	if err != nil {
		return err
	}
	imp, err := directory.NewImporter(directory.ImporterDeps{
		Store: directory.NewGormStore(database.ORM), Tx: database.Tx, Audit: au, Orgs: orgMod.Service, Clock: clk,
		Log: log.With("module", "directory"),
	})
	if err != nil {
		return err
	}
	res, err := imp.Import(ctx, data)
	if err != nil {
		return err
	}
	for _, r := range []struct {
		name string
		c    directory.Counts
	}{
		{"universities", res.Universities}, {"email domains", res.Domains}, {"faculties", res.Faculties},
		{"departments", res.Departments}, {"skills", res.Skills},
	} {
		_, _ = fmt.Fprintf(stdout, "%s: %d created, %d updated, %d unchanged\n", r.name, r.c.Created, r.c.Updated, r.c.Unchanged)
	}
	return nil
}
