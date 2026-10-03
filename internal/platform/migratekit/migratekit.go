// Package migratekit runs the SQL migrations of db/migrations with
// golang-migrate (05 §7; R-002 §3d) and checks them:
//
//   - Migrator: `koro migrate up | down N | version | force V`, as the owner
//     role (MIGRATE_DATABASE_URL), directly against PostgreSQL (never through
//     PgBouncer: golang-migrate holds a session advisory lock so that two
//     migrate jobs never run at once). The application never migrates at
//     start.
//   - CheckVersion: the /readyz check "schema version >= newest embedded
//     migration".
//   - Lint / Freeze: the file rules run by `make test` (lint.go).
package migratekit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// MigrationsTable is the golang-migrate version table (public schema).
const MigrationsTable = "schema_migrations"

// Migrator applies the migrations of one file system to one database.
type Migrator struct {
	m   *migrate.Migrate
	log *slog.Logger
}

// New opens a migrator for the migrations in fsys (the root holds the
// *.sql files) and the database at dsn (postgres:// URL of app_owner).
// Errors never contain the password of dsn.
func New(fsys fs.FS, dsn string, log *slog.Logger) (*Migrator, error) {
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migratekit: source: %w", err)
	}
	target, err := pgx5URL(dsn)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return nil, fmt.Errorf("migratekit: open database: %s", redact(err.Error(), dsn))
	}
	m.Log = logAdapter{log: log}
	return &Migrator{m: m, log: log}, nil
}

// pgx5URL turns a postgres:// URL into the pgx5:// URL of golang-migrate,
// pinning the version table to public.schema_migrations.
func pgx5URL(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("migratekit: database URL must be a postgres:// URL")
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", `"public"."`+MigrationsTable+`"`)
	q.Set("x-migrations-table-quoted", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redact removes the password of dsn from s.
func redact(s, dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparsable database URL)"
	}
	if pw, ok := u.User.Password(); ok && pw != "" {
		s = strings.ReplaceAll(s, pw, "xxxxx")
		s = strings.ReplaceAll(s, url.QueryEscape(pw), "xxxxx")
	}
	return s
}

// Close releases the source and the database connection.
func (m *Migrator) Close() error {
	srcErr, dbErr := m.m.Close()
	return errors.Join(srcErr, dbErr)
}

// Up applies every pending migration. Nothing to do is not an error. ctx
// cancellation stops at the next migration boundary (GracefulStop).
func (m *Migrator) Up(ctx context.Context) error {
	err := m.withStop(ctx, m.m.Up)
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("migratekit: up: %w", err)
	}
	return m.ensureClean()
}

// Down reverts exactly n applied migrations (n > 0). Reverting more than
// are applied is an error after the available ones were reverted.
func (m *Migrator) Down(ctx context.Context, n int) error {
	if n <= 0 {
		return errors.New("migratekit: down needs a positive step count")
	}
	err := m.withStop(ctx, func() error { return m.m.Steps(-n) })
	if err != nil {
		return fmt.Errorf("migratekit: down %d: %w", n, err)
	}
	return m.ensureClean()
}

// Version returns the applied version (0 when nothing is applied) and
// whether the last migration failed half-way (dirty).
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	v, d, err := m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migratekit: version: %w", err)
	}
	return v, d, nil
}

// Force sets the version without running anything and clears the dirty
// flag: the manual fix after a failed migration (-1 means "nothing applied").
func (m *Migrator) Force(version int) error {
	if version < -1 {
		return errors.New("migratekit: force needs a version >= -1")
	}
	if err := m.m.Force(version); err != nil {
		return fmt.Errorf("migratekit: force %d: %w", version, err)
	}
	return nil
}

func (m *Migrator) ensureClean() error {
	v, dirty, err := m.Version()
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("migratekit: database is dirty at version %d: fix it, then `koro migrate force %d` (or the previous version)", v, v)
	}
	return nil
}

// withStop runs f and asks golang-migrate to stop at a safe point when ctx
// is cancelled (Up/Steps take no context).
func (m *Migrator) withStop(ctx context.Context, f func() error) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			select {
			case m.m.GracefulStop <- true:
			default:
			}
		case <-done:
		}
	}()
	return f()
}

// logAdapter routes golang-migrate's log lines to slog.
type logAdapter struct{ log *slog.Logger }

func (l logAdapter) Printf(format string, v ...any) {
	l.log.Info("migrate: " + strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (logAdapter) Verbose() bool { return false }

// Latest returns the highest migration number in fsys (0 if there is none).
func Latest(fsys fs.FS) (uint, error) {
	files, errs := scan(fsys)
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	var latest uint
	for _, f := range files {
		latest = max(latest, f.version)
	}
	return latest, nil
}

// Querier is the part of pgxpool.Pool / pgx.Conn used by CheckVersion.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckVersion is the /readyz migration check: the schema must be clean and
// at least at version want (the newest migration embedded in the binary).
// A newer schema is accepted: during a rolling deploy the migrate job runs
// first and the previous release keeps serving (expand → migrate →
// contract, 05 §7.3).
func CheckVersion(ctx context.Context, q Querier, want uint) error {
	var (
		version int64
		dirty   bool
	)
	err := q.QueryRow(ctx, `SELECT version, dirty FROM public.schema_migrations LIMIT 1`).Scan(&version, &dirty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		version = 0
	case err != nil:
		return fmt.Errorf("migratekit: read schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("migratekit: schema is dirty at version %d", version)
	}
	if version < 0 || uint(version) < want {
		return fmt.Errorf("migratekit: schema version %d is older than %d", version, want)
	}
	return nil
}
