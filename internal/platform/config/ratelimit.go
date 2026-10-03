package config

import "errors"

// RateLimit configures rate limiting (internal/platform/ratelimit; 05 §10.5,
// 04 §1.9). API only. Owner: BE-006. The class limits are code constants
// (ratelimit.Rules) until measurements move them here.
type RateLimit struct {
	// Enabled may be switched off only in dev (load tests, local tooling).
	Enabled bool `env:"RATE_LIMIT_ENABLED" envDefault:"true"`
}

func (r *RateLimit) validate(c Common) error {
	if !r.Enabled && c.ProdLike() {
		return errors.New("RATE_LIMIT_ENABLED=false is allowed only with APP_ENV=dev")
	}
	return nil
}
