package config

import (
	"fmt"
	"time"
)

// Payments configures the payments module (internal/payments). Only tickets
// of the payments module edit this file and the "# --- payments ---" block
// of .env.example (BE-001 R7).
type Payments struct {
	// RefundRequestWindow is how long after an order line's delivery its
	// buyer may ask for a refund (BE-132, Q-26: 30 days by default, a user
	// question); an undelivered line has no deadline.
	RefundRequestWindow time.Duration `env:"PAYMENTS_REFUND_REQUEST_WINDOW" envDefault:"720h"`
}

func (p *Payments) validate(Common) error {
	if p.RefundRequestWindow < 24*time.Hour || p.RefundRequestWindow > 365*24*time.Hour {
		return fmt.Errorf("PAYMENTS_REFUND_REQUEST_WINDOW must be between 24h and 8760h (got %s)", p.RefundRequestWindow)
	}
	return nil
}
