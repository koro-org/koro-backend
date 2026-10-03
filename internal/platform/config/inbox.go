package config

import (
	"errors"
	"time"
)

// Inbox configures the webhook inbox (internal/platform/inbox; 02 §1.2).
// Owner: BE-006. The periods apply until kvkk provides retention_policies
// 'webhook_payload' and 'webhook_row' (hooks.retention).
type Inbox struct {
	PayloadRetention time.Duration `env:"WEBHOOK_PAYLOAD_RETENTION" envDefault:"2160h"` // 90 days
	RowRetention     time.Duration `env:"WEBHOOK_ROW_RETENTION" envDefault:"17520h"`    // 2 years
}

func (i *Inbox) validate(Common) error {
	if i.PayloadRetention < 24*time.Hour || i.RowRetention < i.PayloadRetention {
		return errors.New("WEBHOOK_PAYLOAD_RETENTION must be at least 24h and WEBHOOK_ROW_RETENTION at least as long")
	}
	return nil
}
