package config

import (
	"errors"
	"time"
)

// Idempotency configures Idempotency-Key handling
// (internal/platform/idempotency; 05 §10.4). Owner: BE-006.
type Idempotency struct {
	// TTL is how long a key replays its response (02 §1.3: 24 hours).
	TTL time.Duration `env:"IDEMPOTENCY_TTL" envDefault:"24h"`
}

func (i *Idempotency) validate(Common) error {
	if i.TTL < time.Hour || i.TTL > 7*24*time.Hour {
		return errors.New("IDEMPOTENCY_TTL must be between 1h and 168h")
	}
	return nil
}
