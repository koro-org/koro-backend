package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// ErrTrack configures error tracking (internal/platform/errtrack, OPS-014;
// teknik rapor §3.1, §7.7: GlitchTip, Sentry protocol). Loaded by koro api
// and koro worker.
//
// Error tracking is optional: with SENTRY_DSN empty it is off in every
// environment (the GlitchTip server is OPS-009). ERRORS_PROVIDER follows the
// adapter rule of provider.go, except that noop is allowed in staging and
// prod: an absent tracker loses no data (the log keeps every error), it is
// reported at start instead.
type ErrTrack struct {
	// Provider selects the errtrack.Reporter adapter (ERRORS_PROVIDER):
	// "sentry" (GlitchTip or Sentry) or "noop". Empty means sentry when
	// SENTRY_DSN is set, noop otherwise.
	Provider string `env:"ERRORS_PROVIDER"`
	// DSN is the project's DSN, https://<public key>@<host>/<project id>.
	// It carries the ingest key: a secret (SENTRY_DSN_FILE in prod).
	DSN Secret `env:"SENTRY_DSN"`
	// SampleRate is the share of error events sent, in (0, 1].
	SampleRate float64 `env:"SENTRY_SAMPLE_RATE" envDefault:"1"`
}

// Error tracking adapters (ERRORS_PROVIDER).
const (
	ErrorsProviderSentry = "sentry"
	ErrorsProviderNoop   = ProviderNoop
)

// ErrorsProviders lists the accepted ERRORS_PROVIDER values; cmd/koro has
// one constructor per name (wire_errtrack.go).
func ErrorsProviders() []string { return []string{ErrorsProviderSentry, ErrorsProviderNoop} }

// Enabled reports whether errors are sent to a tracker.
func (e ErrTrack) Enabled() bool { return e.Provider == ErrorsProviderSentry }

func (e *ErrTrack) validate(c Common) error {
	e.Provider = strings.ToLower(strings.TrimSpace(e.Provider))
	if e.Provider == "" {
		e.Provider = ErrorsProviderNoop
		if !e.DSN.IsZero() {
			e.Provider = ErrorsProviderSentry
		}
	}
	if !slices.Contains(ErrorsProviders(), e.Provider) {
		return fmt.Errorf("ERRORS_PROVIDER must be %s (got %q)", orList(ErrorsProviders()), e.Provider)
	}
	var errs []error
	if e.SampleRate <= 0 || e.SampleRate > 1 {
		errs = append(errs, fmt.Errorf("SENTRY_SAMPLE_RATE must be in (0, 1] (got %v)", e.SampleRate))
	}
	switch e.Provider {
	case ErrorsProviderNoop:
		if !e.DSN.IsZero() {
			errs = append(errs, errors.New("SENTRY_DSN is set but ERRORS_PROVIDER=noop; unset one of them"))
		}
	case ErrorsProviderSentry:
		if err := checkDSN(c, e.DSN.Reveal()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkDSN checks the shape of a Sentry DSN without echoing it (it holds
// the ingest key): http(s), a public key, a host and a project ID; https
// and no loopback host in staging and prod.
func checkDSN(c Common, dsn string) error {
	if dsn == "" {
		return errors.New("ERRORS_PROVIDER=sentry needs SENTRY_DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User == nil ||
		u.User.Username() == "" || strings.Trim(u.Path, "/") == "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("SENTRY_DSN must be http(s)://<public key>@<host>/<project id>")
	}
	if _, hasSecret := u.User.Password(); hasSecret {
		return errors.New("SENTRY_DSN must not contain the legacy secret key (use the public DSN)")
	}
	switch {
	case c.ProdLike() && u.Scheme != "https":
		return fmt.Errorf("SENTRY_DSN must use https in %s", c.Env)
	case c.ProdLike() && isLoopbackHost(u.Hostname()):
		return fmt.Errorf("SENTRY_DSN must not be a loopback address in %s", c.Env)
	}
	return nil
}
