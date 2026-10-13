package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"koro/db/seed"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/obs"
	"koro/internal/platform/passwd"
)

// demoMinPassword is the password length of the demo accounts: the
// shortest password an account without MFA may have (identity, 05 §14.1).
const demoMinPassword = 15

// errDemoEnv refuses `koro seed demo` outside dev and staging (05 §13.7).
var errDemoEnv = errors.New("seed demo: synthetic demo data is for APP_ENV=dev or staging only")

// runSeedDemo runs `koro seed demo [-password P] [-seed N]` (BE-033): the
// synthetic Turkish demo data of db/seed over the reference data of `koro
// seed reference` (run that first). dev and staging only: APP_ENV is
// checked before anything else, so prod refuses without touching the
// database. Idempotent: a second run inserts nothing. -password gives
// the student, club and company demo accounts that password (≥ 15
// characters); without it the accounts have none (a password reset sets
// it). Platform role accounts never get it (06 H-66: password reset +
// TOTP). The accounts and what
// was inserted are printed; the password never is.
func runSeedDemo(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	if env := strings.TrimSpace(environ["APP_ENV"]); env != config.EnvDev && env != config.EnvStaging {
		return fmt.Errorf("%w (APP_ENV=%q)", errDemoEnv, env)
	}
	fset := flag.NewFlagSet("seed demo", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	password := fset.String("password", "", "password of the student, club and company demo accounts (at least 15 characters; default: none; never the platform role accounts)")
	rnd := fset.Uint64("seed", seed.DefaultSeed, "random seed of the demo data")
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("%w: seed demo takes only -password and -seed", errUsage)
	}
	if *password != "" && utf8.RuneCountInString(*password) < demoMinPassword {
		return fmt.Errorf("%w: -password must be at least %d characters", errUsage, demoMinPassword)
	}
	cfg, err := config.LoadSeed(environ)
	if err != nil {
		return err
	}
	if cfg.Env != config.EnvDev && cfg.Env != config.EnvStaging {
		return fmt.Errorf("%w (APP_ENV=%q)", errDemoEnv, cfg.Env)
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-seed", Env: cfg.Env, Version: version})
	keys, err := crypto.ParseKeyring(cfg.Platform.Crypto.TokenKeys.Reveal())
	if err != nil {
		return fmt.Errorf("CRYPTO_TOKEN_KEYS: %w", err)
	}
	tickets, err := crypto.NewDeriver(keys)
	if err != nil {
		return fmt.Errorf("CRYPTO_TOKEN_KEYS: %w", err)
	}
	var hash string
	if *password != "" {
		h, err := passwd.New([]byte(cfg.Passwd.Pepper.Reveal()), cfg.Passwd.Concurrency)
		if err != nil {
			return err
		}
		// One hash for every demo account: they are synthetic and share
		// the password anyway (argon2id per account would cost seconds).
		if hash, err = h.Hash(ctx, *password); err != nil {
			return err
		}
	}
	clk := clock.Real()
	database, err := openDB(ctx, cfg.Platform.DB, "koro-seed", clk, log)
	if err != nil {
		return err
	}
	defer database.Close()
	ref, err := loadSeedReference(ctx, database.Pool)
	if err != nil {
		return err
	}
	plan, err := seed.Build(ref, seed.Options{Now: clk.Now(), Seed: *rnd, PasswordHash: hash, Tickets: tickets})
	if err != nil {
		return err
	}
	res, err := seed.Apply(ctx, database.Pool, plan)
	if err != nil {
		return err
	}
	printDemo(stdout, plan, res, hash != "")
	return nil
}

// loadSeedReference reads what the demo builds on: the platform org and
// the active universities (by YÖK code) with their active departments.
func loadSeedReference(ctx context.Context, pool *pgxpool.Pool) (seed.Reference, error) {
	var ref seed.Reference
	err := pool.QueryRow(ctx, `SELECT id FROM organizations WHERE kind = 'platform'`).Scan(&ref.PlatformOrgID)
	if err != nil {
		return seed.Reference{}, fmt.Errorf("%w (platform org: %w)", seed.ErrNoReference, err)
	}
	rows, err := pool.Query(ctx, `SELECT id, org_id, yok_code, name, coalesce(short_name, ''), city
		FROM universities WHERE is_active ORDER BY yok_code`)
	if err != nil {
		return seed.Reference{}, fmt.Errorf("seed demo: universities: %w", err)
	}
	for rows.Next() {
		var u seed.University
		if err := rows.Scan(&u.ID, &u.OrgID, &u.YokCode, &u.Name, &u.ShortName, &u.City); err != nil {
			rows.Close()
			return seed.Reference{}, fmt.Errorf("seed demo: universities: %w", err)
		}
		ref.Universities = append(ref.Universities, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return seed.Reference{}, fmt.Errorf("seed demo: universities: %w", err)
	}
	for i := range ref.Universities {
		u := &ref.Universities[i]
		drows, err := pool.Query(ctx, `SELECT id, name, degree_level, coalesce(duration_years, 0)
			FROM departments WHERE university_id = $1 AND is_active ORDER BY name, id`, u.ID)
		if err != nil {
			return seed.Reference{}, fmt.Errorf("seed demo: departments: %w", err)
		}
		for drows.Next() {
			var d seed.Department
			var years int16
			if err := drows.Scan(&d.ID, &d.Name, &d.DegreeLevel, &years); err != nil {
				drows.Close()
				return seed.Reference{}, fmt.Errorf("seed demo: departments: %w", err)
			}
			d.DurationYears = int(years)
			u.Departments = append(u.Departments, d)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return seed.Reference{}, fmt.Errorf("seed demo: departments: %w", err)
		}
	}
	return ref, nil
}

// printDemo prints what the run did and the demo accounts (never the
// password).
func printDemo(stdout io.Writer, plan *seed.Plan, res seed.Result, withPassword bool) {
	inserted, existing := res.Total()
	_, _ = fmt.Fprintf(stdout, "demo seed: %d rows inserted, %d already there\n", inserted, existing)
	counts := plan.Counts()
	for _, table := range slices.Sorted(maps.Keys(counts)) {
		_, _ = fmt.Fprintf(stdout, "  %-28s %5d planned, %5d inserted\n", table, counts[table], res.Inserted[table])
	}
	pw := "no password: use the password reset"
	if withPassword {
		pw = "the -password value"
	}
	_, _ = fmt.Fprintf(stdout, "demo accounts (%s):\n", pw)
	for _, a := range plan.Accounts {
		note := ""
		if a.NoPassword {
			note = " (no password: password reset + TOTP)"
		}
		_, _ = fmt.Fprintf(stdout, "  %-34s %s%s\n", a.Email, a.Role, note)
	}
	_, _ = fmt.Fprintf(stdout, "  ... and %d synthetic students (@example.test)\n", seed.StudentCount)
}
