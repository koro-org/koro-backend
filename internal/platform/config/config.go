// Package config is the only place that reads the process environment
// (05 §12). Configuration is split by subcommand (koro api, worker, migrate,
// seed, healthcheck) so that each process asks only for what it uses.
//
// Layout (BE-001 R7): this file holds the root structs, the loader and the
// validation walk. Every platform package and every feature module owns one
// section file (config/<name>.go) with one struct; tickets edit only their own
// section file and their own block in .env.example. A section validates
// itself by implementing
//
//	func (s *Section) validate(c Common) error
//
// which the loader calls automatically after parsing.
//
// Secrets: a variable X can also be given as X_FILE=/run/secrets/x (Docker
// secret); the file content becomes X. Setting both is an error. A missing
// required secret fails the process at start; generating a random fallback
// or silently using an in-memory substitute is forbidden (05 §12.2).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Deployment environments (APP_ENV).
const (
	EnvDev     = "dev"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// Common is shared by every long-running subcommand.
type Common struct {
	Env       string     `env:"APP_ENV,required,notEmpty"`
	LogLevel  slog.Level `env:"LOG_LEVEL" envDefault:"INFO"`
	LogFormat string     `env:"LOG_FORMAT" envDefault:"json"` // json | text (text only in dev)
}

// ProdLike reports whether the environment is staging or prod.
func (c Common) ProdLike() bool { return c.Env == EnvStaging || c.Env == EnvProd }

func (c Common) common() Common { return c }

func (c *Common) validate(Common) error {
	switch c.Env {
	case EnvDev, EnvStaging, EnvProd:
	default:
		return fmt.Errorf("APP_ENV must be one of dev, staging, prod (got %q)", c.Env)
	}
	switch c.LogFormat {
	case "json":
	case "text":
		if c.Env != EnvDev {
			return errors.New("LOG_FORMAT=text is allowed only with APP_ENV=dev")
		}
	default:
		return fmt.Errorf("LOG_FORMAT must be json or text (got %q)", c.LogFormat)
	}
	return nil
}

// Platform groups the platform sections used by both the API and the worker.
type Platform struct {
	DB          DB
	Queue       Queue
	Outbox      Outbox
	Idempotency Idempotency
	Inbox       Inbox
	Storage     Storage
	Crypto      Crypto
	Realtime    Realtime
}

// Modules groups the sections of the 22 feature modules (01 §2), in layer
// order (01 §5.1). Modules run in both the API and the worker.
type Modules struct {
	Audit        Audit
	Files        Files
	Consent      Consent
	Entitlements Entitlements
	Notify       Notify
	Identity     Identity
	Orgs         Orgs
	Directory    Directory
	Students     Students
	Parties      Parties
	Events       Events
	Talent       Talent
	Outreach     Outreach
	Feed         Feed
	Catalog      Catalog
	Discovery    Discovery
	Messaging    Messaging
	Orders       Orders
	Ledger       Ledger
	Billing      Billing
	Payments     Payments
	Kvkk         Kvkk
	Stats        Stats
}

// API is the configuration of `koro api`.
type API struct {
	Common
	HTTP      HTTP
	Obs       Obs
	RateLimit RateLimit
	Passwd    Passwd
	Platform  Platform
	Modules   Modules
}

// Worker is the configuration of `koro worker`. AuditRoot (the daily root
// signing keys and archive) is loaded only here: the API never signs (BE-107).
// Webcache (BE-106) is the worker's alone too: only the worker sends the
// publication webhook.
type Worker struct {
	Common
	Obs       Obs
	AuditRoot AuditRoot
	Platform  Platform
	Modules   Modules
	Webcache  Webcache
}

// Migrate is the configuration of `koro migrate` (owner role only).
type Migrate struct {
	Common
	Migratekit Migratekit
}

// Seed is the configuration of `koro seed` (synthetic data; dev/staging only).
type Seed struct {
	Common
	Obs      Obs
	Passwd   Passwd
	Platform Platform
	Modules  Modules
}

// Healthcheck is the configuration of `koro healthcheck` (Docker HEALTHCHECK).
// It deliberately requires nothing.
type Healthcheck struct {
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
}

// Environ returns a copy of the process environment.
func Environ() map[string]string { return env.ToMap(os.Environ()) }

// LoadAPI loads the configuration of `koro api`.
func LoadAPI(environ map[string]string) (API, error) { return Load[API](environ) }

// LoadWorker loads the configuration of `koro worker`.
func LoadWorker(environ map[string]string) (Worker, error) { return Load[Worker](environ) }

// LoadMigrate loads the configuration of `koro migrate`.
func LoadMigrate(environ map[string]string) (Migrate, error) { return Load[Migrate](environ) }

// LoadSeed loads the configuration of `koro seed`.
func LoadSeed(environ map[string]string) (Seed, error) { return Load[Seed](environ) }

// LoadHealthcheck loads the configuration of `koro healthcheck`.
func LoadHealthcheck(environ map[string]string) (Healthcheck, error) {
	return Load[Healthcheck](environ)
}

// Load parses T from environ, resolves *_FILE secrets and runs the section
// validators. environ is not modified (tests pass a literal map; t.Setenv is
// not needed). Error messages name variables, never values.
func Load[T any](environ map[string]string) (T, error) {
	var zero T
	resolved, err := resolveFiles[T](environ)
	if err != nil {
		return zero, err
	}
	cfg, err := env.ParseAsWithOptions[T](env.Options{Environment: resolved})
	if err != nil {
		return zero, fmt.Errorf("config: %w", err)
	}
	if err := validateAll(&cfg); err != nil {
		return zero, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// resolveFiles copies environ and, for every variable declared by T, moves
// the content of <VAR>_FILE into <VAR> (os.Setenv is never used).
func resolveFiles[T any](environ map[string]string) (map[string]string, error) {
	var probe T
	params, err := env.GetFieldParamsWithOptions(&probe, env.Options{Environment: map[string]string{}})
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	out := maps.Clone(environ)
	if out == nil {
		out = map[string]string{}
	}
	for _, p := range params {
		fileKey := p.Key + "_FILE"
		path := out[fileKey]
		if path == "" {
			continue
		}
		if _, set := out[p.Key]; set {
			return nil, fmt.Errorf("config: %s and %s are both set", p.Key, fileKey)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", fileKey, err)
		}
		out[p.Key] = strings.TrimRight(string(b), "\r\n")
	}
	return out, nil
}

// validator is implemented by sections that check themselves.
type validator interface {
	validate(c Common) error
}

var pkgPath = reflect.TypeFor[Common]().PkgPath()

// validateAll calls validate on Common first and then on every section of
// cfg (depth first, declaration order) and joins the errors.
func validateAll(cfg any) error {
	var c Common
	if h, ok := cfg.(interface{ common() Common }); ok {
		c = h.common()
	}
	var errs []error
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		for i := range v.NumField() {
			f := v.Field(i)
			if f.Kind() != reflect.Struct || f.Type().PkgPath() != pkgPath || f.Type() == reflect.TypeFor[Secret]() {
				continue
			}
			// The root is not checked itself: it would only see the
			// validate method promoted from the embedded Common.
			if s, ok := f.Addr().Interface().(validator); ok {
				if err := s.validate(c); err != nil {
					errs = append(errs, err)
				}
			}
			walk(f)
		}
	}
	walk(reflect.ValueOf(cfg).Elem())
	return errors.Join(errs...)
}
