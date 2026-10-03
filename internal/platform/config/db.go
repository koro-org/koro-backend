package config

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// DB configures the application's database pool (internal/platform/db;
// 05 §6.1, §12.1). Owner: BE-002. The api and the worker connect as app_rw;
// the owner role (app_owner) is only for `koro migrate` (Migratekit).
type DB struct {
	URL              Secret        `env:"DATABASE_URL,required,notEmpty"`
	MaxConns         int32         `env:"DB_MAX_CONNS" envDefault:"20"` // api 20, worker 10 (05 §6.1)
	MinConns         int32         `env:"DB_MIN_CONNS" envDefault:"2"`  // api 2, worker 1
	StatementTimeout time.Duration `env:"DB_STATEMENT_TIMEOUT" envDefault:"15s"`
	IdleInTxTimeout  time.Duration `env:"DB_IDLE_IN_TX_TIMEOUT" envDefault:"30s"`
}

func (d *DB) validate(Common) error {
	var errs []error
	user, err := postgresURLUser(d.URL.Reveal())
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("DATABASE_URL %w", err))
	case user == ownerRole:
		errs = append(errs, errors.New("DATABASE_URL must use the application role (app_rw), not the owner role app_owner"))
	}
	if d.MaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be at least 1"))
	}
	if d.MinConns < 0 || d.MinConns > d.MaxConns {
		errs = append(errs, errors.New("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS"))
	}
	if d.StatementTimeout <= 0 {
		errs = append(errs, errors.New("DB_STATEMENT_TIMEOUT must be positive"))
	}
	if d.IdleInTxTimeout <= 0 {
		errs = append(errs, errors.New("DB_IDLE_IN_TX_TIMEOUT must be positive"))
	}
	return errors.Join(errs...)
}

// ownerRole owns every table (deploy/db/bootstrap.sql, 06 H-3).
const ownerRole = "app_owner"

// postgresURLUser checks that raw is a postgres:// URL with a host and a
// database and returns its user. Errors never contain raw (it holds a
// password).
func postgresURLUser(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("must be a postgres://user:password@host:port/database URL")
	}
	if len(u.Path) < 2 {
		return "", errors.New("must name a database (postgres://…/<database>)")
	}
	return u.User.Username(), nil
}
