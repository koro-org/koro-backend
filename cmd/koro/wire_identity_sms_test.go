package main

import (
	"testing"
	"time"

	"koro/internal/identity"
	"koro/internal/platform/config"
	"koro/internal/platform/obs"
	smsmailpit "koro/internal/platform/sms/mailpit"
)

// TestWire_SMSMailpitBox (BE-178): SMS_PROVIDER=mailpit wires the
// development SMS box at SMS_MAILPIT_ADDR without connecting (Mailpit may
// start later); an unusable address stops the build. Staging and prod
// never get here: the configuration refuses the adapter
// (config TestSMS_MailpitDevOnly).
func TestWire_SMSMailpitBox(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	cfg := config.SMS{Provider: config.SMSProviderMailpit, MailpitAddr: "127.0.0.1:1", Timeout: 10 * time.Second}
	s, err := smsSenders(c, cfg).pick("SMS_PROVIDER", config.SMSProviderMailpit)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*smsmailpit.Sender); !ok {
		t.Fatalf("sender = %T, want *mailpit.Sender", s)
	}
	cfg.MailpitAddr = "localhost"
	if _, err := smsSenders(c, cfg).pick("SMS_PROVIDER", config.SMSProviderMailpit); err == nil {
		t.Fatal("an address without a port was wired")
	}
}

// TestWire_SMSBudgetDefaults (BE-181): the configuration's budget and the
// value identity falls back to are the same.
func TestWire_SMSBudgetDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[struct {
		config.Common
		SMS config.SMS
	}](map[string]string{"APP_ENV": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got := (identity.SMSBudget{PerHour: cfg.SMS.BudgetPerHour, PerDay: cfg.SMS.BudgetPerDay}); got != identity.DefaultSMSBudget {
		t.Fatalf("configuration %+v, identity %+v", got, identity.DefaultSMSBudget)
	}
}

// TestWire_SMSMetrics (BE-181): the API and the worker each register
// koro_sms_codes_total on their own registry.
func TestWire_SMSMetrics(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if err := c.identity.metrics(obs.NewMetrics()); err != nil {
		t.Fatal(err)
	}
	if err := c.identity.metrics(nil); err != nil {
		t.Fatalf("metrics off: %v", err)
	}
}
