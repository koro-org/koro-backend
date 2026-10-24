package config

import (
	"errors"
	"fmt"
	"net/url"
)

// Webcache configures the publication webhook of the worker
// (internal/platform/webcache, BE-106): when public content changes, the
// worker signs the cache tags and POSTs them to koro-web's
// /api/revalidate. Owner: BE-106 (BE-001 R7: only the owning ticket edits
// this file and the "# --- webcache ---" block of .env.example).
//
// It is a section of `koro worker` only: the API never sends the webhook,
// so it never holds the secret (least privilege).
//
// Both values are required in staging and prod. In dev they may be empty:
// the webhook is then skipped with a warning (no web to revalidate).
type Webcache struct {
	// WebInternalURL is koro-web's address on the internal network
	// (e.g. http://koro-web:3000); /api/revalidate is appended.
	WebInternalURL string `env:"KORO_WEB_INTERNAL_URL"`
	// RevalidateSecret is the HMAC-SHA256 key shared with koro-web (the
	// same KORO_REVALIDATE_SECRET), at least 32 characters. Prod gives it
	// as KORO_REVALIDATE_SECRET_FILE. Generate: openssl rand -hex 32.
	RevalidateSecret Secret `env:"KORO_REVALIDATE_SECRET"`
}

// minRevalidateSecret is koro-web's lower bound (src/lib/env.ts).
const minRevalidateSecret = 32

// Enabled reports whether the webhook is configured.
func (w Webcache) Enabled() bool { return w.WebInternalURL != "" && !w.RevalidateSecret.IsZero() }

func (w *Webcache) validate(c Common) error {
	var errs []error
	switch {
	case w.WebInternalURL == "" && c.ProdLike():
		errs = append(errs, fmt.Errorf("KORO_WEB_INTERNAL_URL is required in %s", c.Env))
	case w.WebInternalURL != "":
		if err := checkWebURL(w.WebInternalURL); err != nil {
			errs = append(errs, fmt.Errorf("KORO_WEB_INTERNAL_URL: %w", err))
		}
	}
	secret := w.RevalidateSecret.Reveal()
	switch {
	case secret == "" && c.ProdLike():
		errs = append(errs, fmt.Errorf("KORO_REVALIDATE_SECRET is required in %s", c.Env))
	case secret != "" && len(secret) < minRevalidateSecret:
		errs = append(errs, fmt.Errorf("KORO_REVALIDATE_SECRET must be at least %d characters", minRevalidateSecret))
	case secret != "" && c.ProdLike() && weakSecret([]byte(secret)):
		errs = append(errs, errors.New("KORO_REVALIDATE_SECRET looks like a placeholder in staging/prod"))
	}
	return errors.Join(errs...)
}

// checkWebURL accepts scheme://host[:port] with an optional trailing
// slash: plain http is fine on the internal network.
func checkWebURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must use http or https")
	case u.Host == "" || u.User != nil:
		return errors.New("must be scheme://host[:port]")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "":
		return errors.New("must be scheme://host[:port] only (no path, query or fragment)")
	}
	return nil
}
