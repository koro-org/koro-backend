package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type ordersOnly struct {
	config.Common
	Orders config.Orders
}

// TestOrders_PaymentWindowDefaultAndBounds: ORDERS_PAYMENT_WINDOW is 7 days
// by default (T-13) and must lie between one hour and 30 days.
func TestOrders_PaymentWindowDefaultAndBounds(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "dev"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err := config.Load[ordersOnly](env())
	if err != nil || cfg.Orders.PaymentWindow != 7*24*time.Hour {
		t.Fatalf("default = %+v, %v", cfg.Orders, err)
	}
	for v, want := range map[string]time.Duration{"1h": time.Hour, "72h": 72 * time.Hour, "720h": 720 * time.Hour} {
		if cfg, err := config.Load[ordersOnly](env("ORDERS_PAYMENT_WINDOW", v)); err != nil || cfg.Orders.PaymentWindow != want {
			t.Fatalf("%s = %+v, %v", v, cfg.Orders, err)
		}
	}
	for _, v := range []string{"0s", "59m", "721h", "-1h"} {
		if _, err := config.Load[ordersOnly](env("ORDERS_PAYMENT_WINDOW", v)); err == nil ||
			!strings.Contains(err.Error(), "ORDERS_PAYMENT_WINDOW") {
			t.Fatalf("%s accepted: %v", v, err)
		}
	}
}
