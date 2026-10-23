package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type studentsOnly struct {
	config.Common
	Students config.Students
}

// TestStudents_GrantSuspensionWindowDefaultAndBounds (BE-088, T-331): the
// suspension window of a level 3 grant is 90 days by default and must lie
// between one day and one year.
func TestStudents_GrantSuspensionWindowDefaultAndBounds(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "dev"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err := config.Load[studentsOnly](env())
	if err != nil || cfg.Students.GrantSuspensionWindow != 90*24*time.Hour {
		t.Fatalf("default = %+v, %v", cfg.Students, err)
	}
	for v, want := range map[string]time.Duration{"24h": 24 * time.Hour, "720h": 30 * 24 * time.Hour, "8760h": 365 * 24 * time.Hour} {
		if cfg, err := config.Load[studentsOnly](env("STUDENTS_GRANT_SUSPENSION_WINDOW", v)); err != nil ||
			cfg.Students.GrantSuspensionWindow != want {
			t.Fatalf("%s = %+v, %v", v, cfg.Students, err)
		}
	}
	for _, v := range []string{"0s", "23h", "8761h", "-24h"} {
		if _, err := config.Load[studentsOnly](env("STUDENTS_GRANT_SUSPENSION_WINDOW", v)); err == nil ||
			!strings.Contains(err.Error(), "STUDENTS_GRANT_SUSPENSION_WINDOW") {
			t.Fatalf("%s accepted: %v", v, err)
		}
	}
}
