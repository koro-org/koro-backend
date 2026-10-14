package config

import (
	"fmt"
	"time"
)

// Orders configures the orders module (internal/orders). Owner: BE-040;
// only tickets of the orders module edit this file and the
// "# --- orders ---" block of .env.example (BE-001 R7).
type Orders struct {
	// PaymentWindow is how long a placed order waits for its bank transfer
	// before orders.expire_unpaid ends it and releases its units (T-13: 7
	// days). The dispute window and the payout hold come from the commerce
	// policy (02 §18.3), not from here.
	PaymentWindow time.Duration `env:"ORDERS_PAYMENT_WINDOW" envDefault:"168h"`
}

func (o *Orders) validate(Common) error {
	if o.PaymentWindow < time.Hour || o.PaymentWindow > 30*24*time.Hour {
		return fmt.Errorf("ORDERS_PAYMENT_WINDOW must be between 1h and 720h (got %s)", o.PaymentWindow)
	}
	return nil
}
