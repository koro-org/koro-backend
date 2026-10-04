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
