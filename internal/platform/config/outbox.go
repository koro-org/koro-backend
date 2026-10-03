package config

import (
	"errors"
	"time"
)

// Outbox configures the outbox relay and its cleanup job
// (internal/platform/outbox; 05 §8.3, 01 §7). Owner: BE-006.
type Outbox struct {
	RelayBatch      int           `env:"OUTBOX_RELAY_BATCH" envDefault:"100"`       // rows per relay transaction
	RelayIdle       time.Duration `env:"OUTBOX_RELAY_IDLE" envDefault:"1s"`         // pause when nothing is due (01 §7)
	RelayBackoffMax time.Duration `env:"OUTBOX_RELAY_BACKOFF_MAX" envDefault:"10m"` // cap of the per-row retry back-off
	// Retention keeps published rows (02 §1.1: 7 days). It applies until
	// kvkk provides retention_policies 'outbox_published' (hooks.retention).
	Retention time.Duration `env:"OUTBOX_RETENTION" envDefault:"168h"`
}

func (o *Outbox) validate(Common) error {
	var errs []error
	if o.RelayBatch < 1 || o.RelayBatch > 1000 {
		errs = append(errs, errors.New("OUTBOX_RELAY_BATCH must be between 1 and 1000"))
	}
	if o.RelayIdle <= 0 || o.RelayIdle > time.Minute {
		errs = append(errs, errors.New("OUTBOX_RELAY_IDLE must be between 0 and 1m"))
	}
	if o.RelayBackoffMax < time.Second {
		errs = append(errs, errors.New("OUTBOX_RELAY_BACKOFF_MAX must be at least 1s"))
	}
	if o.Retention < time.Hour {
		errs = append(errs, errors.New("OUTBOX_RETENTION must be at least 1h"))
	}
	return errors.Join(errs...)
}
