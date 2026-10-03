package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"koro/db/migrations"
	"koro/internal/platform/config"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
)

const migrateUsage = `usage: koro migrate <command>

commands (MIGRATE_DATABASE_URL, owner role app_owner, direct connection):
  up          apply every pending migration
  down N      revert exactly N migrations (N is required)
  version     print the applied version ("dirty" if a migration failed half-way)
  force V     set the version without running anything (fix a dirty state; -1 = none)
  freeze V    print the .frozen manifest for versions <= V (no database; BE-001 R2)
`

// runMigrate runs `koro migrate up | down N | version | force V | freeze V`
// (05 §7; R-002 §3d). Migrations never run at api/worker start: in
// deployment this is a separate job that finishes before the new release
// starts (deploy/README.md).
func runMigrate(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing command\n\n%s", errUsage, migrateUsage)
	}
	cmd, rest := args[0], args[1:]
	wantArgs := map[string]int{"up": 0, "version": 0, "down": 1, "force": 1, "freeze": 1}
	n, known := wantArgs[cmd]
	switch {
	case !known:
		return fmt.Errorf("%w: unknown migrate command %q\n\n%s", errUsage, cmd, migrateUsage)
	case len(rest) != n:
		return fmt.Errorf("%w: migrate %s takes %d argument(s)\n\n%s", errUsage, cmd, n, migrateUsage)
	}

	// Arguments are checked before anything is loaded or opened.
	var num int
	if n == 1 {
		v, err := strconv.Atoi(rest[0])
		switch {
		case err != nil:
			return fmt.Errorf("%w: %s needs a number", errUsage, cmd)
		case cmd == "down" && v <= 0:
			return fmt.Errorf("%w: down needs a positive step count", errUsage)
		case cmd == "freeze" && v <= 0:
			return fmt.Errorf("%w: freeze needs a positive version", errUsage)
		}
		num = v
	}

	if cmd == "freeze" {
		manifest, err := migratekit.Freeze(migrations.FS, uint(num))
		if err != nil {
			return err
		}
		_, err = stdout.Write(manifest)
		return err
	}

	cfg, err := config.LoadMigrate(environ)
	if err != nil {
		return err
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{
		Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-migrate", Env: cfg.Env, Version: version,
	})
	m, err := migratekit.New(migrations.FS, cfg.Migratekit.URL.Reveal(), log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := m.Close(); cerr != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "migrate: close", slog.Any("err", cerr))
		}
	}()

	switch cmd {
	case "up":
		err = m.Up(ctx)
	case "down":
		err = m.Down(ctx, num)
	case "force":
		err = m.Force(num)
	}
	if err != nil {
		return err
	}
	v, dirty, err := m.Version()
	if err != nil {
		return err
	}
	if cmd == "version" {
		state := ""
		if dirty {
			state = " (dirty)"
		}
		_, err = fmt.Fprintf(stdout, "%d%s\n", v, state)
		return err
	}
	log.LogAttrs(ctx, slog.LevelInfo, "migrate: done", slog.String("command", cmd), slog.Uint64("version", uint64(v)), slog.Bool("dirty", dirty))
	if dirty {
		return errors.New("migrate: database is dirty")
	}
	return nil
}
