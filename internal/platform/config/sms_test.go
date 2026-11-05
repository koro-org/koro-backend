package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type smsOnly struct {
	config.Common
	SMS config.SMS
}

// smsNetgsm is a complete set of Netgsm SMS settings (test values, no real
// account).
func smsNetgsm(env string, kv ...string) map[string]string {
	m := map[string]string{
		"APP_ENV": env, "SMS_PROVIDER": "netgsm", "SMS_NETGSM_USERNAME": "8500000000",
		"SMS_NETGSM_PASSWORD": "test-api-user-password", "SMS_NETGSM_HEADER": "KORO",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestSMS_Defaults: no adapter unless chosen (every SMS feature answers
// 503), Netgsm's API by default, a 10s timeout.
func TestSMS_Defaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[smsOnly](map[string]string{"APP_ENV": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.SMS
	if s.Provider != config.SMSProviderNone || s.Enabled() || s.Timeout != 10*time.Second ||
		s.Netgsm.BaseURL != "https://api.netgsm.com.tr" || s.MailpitAddr != "localhost:1025" {
		t.Fatalf("defaults = %+v", s)
	}
	if got := config.SMSProviders(); strings.Join(got, ",") != "none,mem,netgsm,mailpit" {
		t.Fatalf("SMSProviders = %v", got)
	}
}

// TestSMS_Validation pins SMS_PROVIDER and the Netgsm settings: mem only in
// dev, Netgsm's account and sender name together, https in staging and
// prod, no Netgsm setting without the adapter; errors never repeat a value.
func TestSMS_Validation(t *testing.T) {
	t.Parallel()
	ok := []map[string]string{
		{"APP_ENV": "dev", "SMS_PROVIDER": "mem"},
		{"APP_ENV": "dev", "SMS_PROVIDER": " MEM "},
		smsNetgsm("prod"),
		smsNetgsm("staging", "SMS_NETGSM_BASE_URL", "https://api.netgsm.com.tr/"),
		smsNetgsm("dev", "SMS_NETGSM_BASE_URL", "http://127.0.0.1:9000", "SMS_NETGSM_HEADER", "Koro Dev"),
	}
	for _, env := range ok {
		if cfg, err := config.Load[smsOnly](env); err != nil {
			t.Errorf("%v: %v", env, err)
		} else if cfg.SMS.Netgsm.BaseURL != "" && strings.HasSuffix(cfg.SMS.Netgsm.BaseURL, "/") {
			t.Errorf("%v: base URL keeps its slash", env)
		}
	}
	bad := map[string]map[string]string{
		"mem in prod":              {"APP_ENV": "prod", "SMS_PROVIDER": "mem"},
		"mem in staging":           {"APP_ENV": "staging", "SMS_PROVIDER": "mem"},
		"unknown adapter":          {"APP_ENV": "dev", "SMS_PROVIDER": "twilio"},
		"no password":              smsNetgsm("prod", "SMS_NETGSM_PASSWORD", ""),
		"no header":                smsNetgsm("prod", "SMS_NETGSM_HEADER", ""),
		"bad header":               smsNetgsm("prod", "SMS_NETGSM_HEADER", "KORO-PLATFORMU"),
		"http in prod":             smsNetgsm("prod", "SMS_NETGSM_BASE_URL", "http://api.netgsm.com.tr"),
		"credentials in url":       smsNetgsm("dev", "SMS_NETGSM_BASE_URL", "https://u:p@api.netgsm.com.tr"),
		"settings without adapter": {"APP_ENV": "dev", "SMS_PROVIDER": "none", "SMS_NETGSM_USERNAME": "8500000000"},
		"timeout":                  {"APP_ENV": "dev", "SMS_TIMEOUT": "2m"},
	}
	for name, env := range bad {
		_, err := config.Load[smsOnly](env)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "test-api-user-password") {
			t.Errorf("%s: the error repeats the password", name)
		}
	}
}

// TestSMS_MailpitDevOnly (BE-178): the development SMS box runs in dev
// only — staging and prod refuse it at start — and needs a host:port.
func TestSMS_MailpitDevOnly(t *testing.T) {
	t.Parallel()
	for _, env := range []map[string]string{
		{"APP_ENV": "dev", "SMS_PROVIDER": "mailpit"},
		{"APP_ENV": "dev", "SMS_PROVIDER": " Mailpit ", "SMS_MAILPIT_ADDR": " mailpit:1025 "},
		{"APP_ENV": "prod", "SMS_MAILPIT_ADDR": "not an address"}, // ignored without the adapter
	} {
		if _, err := config.Load[smsOnly](env); err != nil {
			t.Errorf("%v: %v", env, err)
		}
	}
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"APP_ENV": "prod", "SMS_PROVIDER": "mailpit"}, "SMS_PROVIDER=mailpit is a development adapter, which is not allowed in prod"},
		{map[string]string{"APP_ENV": "staging", "SMS_PROVIDER": "mailpit"}, "SMS_PROVIDER=mailpit is a development adapter, which is not allowed in staging"},
		{map[string]string{"APP_ENV": "dev", "SMS_PROVIDER": "mailpit", "SMS_MAILPIT_ADDR": "localhost"}, "SMS_MAILPIT_ADDR must be host:port"},
		{map[string]string{"APP_ENV": "dev", "SMS_PROVIDER": "mailpit", "SMS_MAILPIT_ADDR": "localhost:0"}, "SMS_MAILPIT_ADDR must be host:port"},
	} {
		_, err := config.Load[smsOnly](c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.env, err, c.want)
		}
	}
}
