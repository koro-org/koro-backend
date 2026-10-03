// Package db is the database layer of Koro (05 §6, 01 §1.3): one pgx pool
// shared by GORM (through database/sql) and raw pgx, the transaction model
// (WithTx joins an outer transaction, AfterCommit), the org scope (ScopeOrg)
// and the translation of PostgreSQL errors into problem sentinels.
//
// The pool always uses pgx.QueryExecModeExec (ADR-0011, R-002 §5.1): no
// server-side prepared statements, so a schema change never produces
// "cached plan must not change result type" (0A000) on old replicas, and
// PgBouncer in transaction mode stays possible. GORM's PrepareStmt is off.
//
// The application connects as app_rw (DATABASE_URL). Schema changes are
// made only by `koro migrate` as app_owner (internal/platform/migratekit).
//
// Parameter binding (TestExecModeTypes): uuid.UUID, []uuid.UUID for "IN ?",
// json.RawMessage (jsonb), []byte (bytea), time.Time, pointers for NULL,
// bool and integers bind as they are. Two GORM rules for raw SQL and models:
//
//   - text[] / uuid[] columns and single array parameters ("= ANY(?)",
//     "tags @> ?") use TextArray / UUIDArray; GORM turns a plain slice into a
//     row expression "($1,$2)".
//   - GORM expands a parameter written directly after "(" element by element
//     (meant for "IN (?)"). uuid.UUID is a [16]byte array, so "VALUES (?",
//     "(@id" or "(?," binds sixteen bytes and fails. Write "( ?" / "( @id",
//     and "IN ?" for lists.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"koro/internal/platform/clock"
)

// Pool defaults (05 §6.1).
const (
	maxConnLifetime       = 30 * time.Minute
	maxConnLifetimeJitter = 3 * time.Minute
	maxConnIdleTime       = 5 * time.Minute
	healthCheckPeriod     = time.Minute
	connectTimeout        = 5 * time.Second
	slowQueryThreshold    = 200 * time.Millisecond
)

// Config configures Open. cmd/koro maps config.DB onto it.
type Config struct {
	URL              string        // postgres:// URL of the application role (app_rw)
	AppName          string        // application_name: koro-api | koro-worker
	MaxConns         int32         // api 20, worker 10
	MinConns         int32         // api 2, worker 1
	StatementTimeout time.Duration // server-side statement_timeout (15 s)
	IdleInTxTimeout  time.Duration // idle_in_transaction_session_timeout (30 s)
	Clock            clock.Clock   // GORM NowFunc; nil means clock.Real()
}

// DB bundles the pool and its two views. Close it once, after the HTTP
// server and the workers have drained.
type DB struct {
	Pool *pgxpool.Pool // raw pgx: COPY, advisory locks, LISTEN
	SQL  *sql.DB       // database/sql view of Pool (GORM uses it)
	ORM  *gorm.DB      // stores: always through Conn(ctx, d.ORM)
	Tx   TxManager     // services draw transaction boundaries with it
}

// Open creates the pool, pings the server and opens GORM on top of it.
// Errors never contain the connection string.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*DB, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgconn redacts passwords only on a best-effort basis: report a
		// fixed message instead of the parse error.
		return nil, errors.New("db: invalid database URL")
	}
	pc.MaxConns, pc.MinConns = cfg.MaxConns, cfg.MinConns
	pc.MaxConnLifetime, pc.MaxConnLifetimeJitter = maxConnLifetime, maxConnLifetimeJitter
	pc.MaxConnIdleTime, pc.HealthCheckPeriod = maxConnIdleTime, healthCheckPeriod
	pc.ConnConfig.ConnectTimeout = connectTimeout
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec // ADR-0011: mandatory
	rp := pc.ConnConfig.RuntimeParams
	rp["application_name"] = cfg.AppName
	rp["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	rp["idle_in_transaction_session_timeout"] = strconv.FormatInt(cfg.IdleInTxTimeout.Milliseconds(), 10)
	rp["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool) // max idle 0: idle connections live in pgxpool
	sqlDB.SetMaxOpenConns(int(cfg.MaxConns))

	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real()
	}
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		SkipDefaultTransaction: true, // no implicit BEGIN/COMMIT: services own transactions
		TranslateError:         true, // keeps the *pgconn.PgError wrapped (errors.As works)
		PrepareStmt:            false,
		NowFunc:                func() time.Time { return clk.Now().UTC() },
		Logger: logger.NewSlogLogger(log, logger.Config{
			SlowThreshold:             slowQueryThreshold,
			LogLevel:                  logger.Warn,
			ParameterizedQueries:      true, // never log parameter values (PII)
			IgnoreRecordNotFoundError: true,
		}),
	})
	if err != nil {
		_ = sqlDB.Close()
		pool.Close()
		return nil, fmt.Errorf("db: gorm: %w", err)
	}
	return &DB{Pool: pool, SQL: sqlDB, ORM: orm, Tx: NewTxManager(orm)}, nil
}

// Close closes the database/sql view, then the pool.
func (d *DB) Close() {
	_ = d.SQL.Close()
	d.Pool.Close()
}

// Ping checks that a connection can be acquired and the server answers
// (/readyz).
func (d *DB) Ping(ctx context.Context) error {
	if err := d.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// SQLState returns the SQLSTATE of the PostgreSQL error wrapped in err, or
// "" if err carries none.
func SQLState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ConstraintName returns the constraint named by the PostgreSQL error
// wrapped in err ("" if none). Constraint names are explicit in every
// migration (02 §23); never depend on generated names.
func ConstraintName(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}
