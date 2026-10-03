package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// HTTP configures the API server (internal/platform/httpx; 05 §10, §12.1).
// Owner: BE-001.
type HTTP struct {
	Addr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	PublicBaseURL   string        `env:"PUBLIC_BASE_URL,required,notEmpty"`
	TrustedOrigins  []string      `env:"TRUSTED_ORIGINS,required,notEmpty" envSeparator:","`
	TrustedProxies  []string      `env:"TRUSTED_PROXIES" envSeparator:","`
	RequestTimeout  time.Duration `env:"REQUEST_TIMEOUT" envDefault:"15s"`
	ShutdownDelay   time.Duration `env:"SHUTDOWN_DELAY" envDefault:"5s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`
	ExposeOpenAPI   bool          `env:"EXPOSE_OPENAPI"`
	ProblemTypeBase string        `env:"PROBLEM_TYPE_BASE" envDefault:"urn:koro:problem:"`
}

func (h *HTTP) validate(c Common) error {
	var errs []error
	if _, _, err := net.SplitHostPort(h.Addr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_ADDR must be host:port: %w", err))
	}
	if err := checkOrigin(h.PublicBaseURL, c.ProdLike(), true); err != nil {
		errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL: %w", err))
	}
	h.TrustedOrigins = compact(h.TrustedOrigins)
	if len(h.TrustedOrigins) == 0 {
		errs = append(errs, errors.New("TRUSTED_ORIGINS must list at least one origin"))
	}
	for _, o := range h.TrustedOrigins {
		if err := checkOrigin(o, c.ProdLike(), false); err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_ORIGINS: %w", err))
		}
	}
	h.TrustedProxies = compact(h.TrustedProxies)
	for _, p := range h.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				errs = append(errs, fmt.Errorf("TRUSTED_PROXIES: %q is neither an IP nor a CIDR", p))
			}
		}
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{{"REQUEST_TIMEOUT", h.RequestTimeout}, {"SHUTDOWN_DELAY", h.ShutdownDelay}, {"SHUTDOWN_TIMEOUT", h.ShutdownTimeout}} {
		if d.v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", d.name))
		}
	}
	if h.ExposeOpenAPI && c.Env == EnvProd {
		errs = append(errs, errors.New("EXPOSE_OPENAPI must be false in prod"))
	}
	if strings.TrimSpace(h.ProblemTypeBase) == "" {
		errs = append(errs, errors.New("PROBLEM_TYPE_BASE must not be empty"))
	}
	return errors.Join(errs...)
}

// checkOrigin validates an absolute http(s) URL; origins have no path.
// staging/prod require https.
func checkOrigin(raw string, httpsOnly, allowPath bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%q is not an absolute http(s) URL", raw)
	}
	if httpsOnly && u.Scheme != "https" {
		return fmt.Errorf("%q must use https in staging/prod", raw)
	}
	if !allowPath && (u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "") {
		return fmt.Errorf("%q must be scheme://host[:port] only", raw)
	}
	return nil
}

func compact(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
