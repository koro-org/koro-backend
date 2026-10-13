package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type outreachOnly struct {
	config.Common
	Outreach config.Outreach
}

func TestOutreach_CountDailyLimitDefaultAndBounds(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "dev"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err := config.Load[outreachOnly](env())
	if err != nil || cfg.Outreach.CountDailyLimit != 300 {
		t.Fatalf("default = %+v, %v", cfg.Outreach, err)
	}
	if cfg, err := config.Load[outreachOnly](env("OUTREACH_COUNT_DAILY_LIMIT", "50")); err != nil || cfg.Outreach.CountDailyLimit != 50 {
		t.Fatalf("50 = %+v, %v", cfg.Outreach, err)
	}
	for _, v := range []string{"0", "-1", "10001"} {
		if _, err := config.Load[outreachOnly](env("OUTREACH_COUNT_DAILY_LIMIT", v)); err == nil ||
			!strings.Contains(err.Error(), "OUTREACH_COUNT_DAILY_LIMIT") {
			t.Fatalf("%s accepted: %v", v, err)
		}
	}
}

func TestOutreach_UnsubscribeURL(t *testing.T) {
	t.Parallel()
	env := func(v string) map[string]string {
		return map[string]string{"APP_ENV": "dev", "OUTREACH_UNSUBSCRIBE_URL": v}
	}
	cfg, err := config.Load[outreachOnly](map[string]string{"APP_ENV": "dev"})
	if err != nil || cfg.Outreach.UnsubscribeURL != "" {
		t.Fatalf("default = %+v, %v (empty: no commercial e-mail)", cfg.Outreach, err)
	}
	if cfg, err := config.Load[outreachOnly](env(" https://koro.example.test/unsubscribe ")); err != nil ||
		cfg.Outreach.UnsubscribeURL != "https://koro.example.test/unsubscribe" {
		t.Fatalf("https = %+v, %v", cfg.Outreach, err)
	}
	for _, v := range []string{"http://koro.example.test/u", "/unsubscribe", "https://", "https://koro.example.test/u#x"} {
		if _, err := config.Load[outreachOnly](env(v)); err == nil || !strings.Contains(err.Error(), "OUTREACH_UNSUBSCRIBE_URL") {
			t.Fatalf("%q accepted: %v", v, err)
		}
	}
}
