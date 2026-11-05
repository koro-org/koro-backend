package main

import (
	"testing"
	"time"

	"koro/internal/platform/config"
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
