package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type errTrackOnly struct {
	config.Common
	ErrTrack config.ErrTrack
}

const exampleDSN = "https://examplepublickey@glitchtip.koro.example/3"

func errTrackEnv(env string, kv ...string) map[string]string {
	m := map[string]string{"APP_ENV": env}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestConfig_ErrTrackOffWithoutDSN: without SENTRY_DSN error tracking is
// off in every environment (OPS-014; the server is OPS-009), noop included
// in prod; with a DSN the sentry adapter is chosen.
func TestConfig_ErrTrackOffWithoutDSN(t *testing.T) {
	t.Parallel()
	for _, env := range []string{config.EnvDev, config.EnvStaging, config.EnvProd} {
		cfg, err := config.Load[errTrackOnly](errTrackEnv(env))
		if err != nil || cfg.ErrTrack.Provider != config.ErrorsProviderNoop || cfg.ErrTrack.Enabled() || cfg.ErrTrack.SampleRate != 1 {
			t.Fatalf("%s default: %+v, %v", env, cfg.ErrTrack, err)
		}
	}
	cfg, err := config.Load[errTrackOnly](errTrackEnv(config.EnvProd, "SENTRY_DSN", exampleDSN, "SENTRY_SAMPLE_RATE", "0.5"))
	if err != nil || cfg.ErrTrack.Provider != config.ErrorsProviderSentry || !cfg.ErrTrack.Enabled() ||
		cfg.ErrTrack.DSN.Reveal() != exampleDSN || cfg.ErrTrack.SampleRate != 0.5 {
		t.Fatalf("DSN set: %+v, %v", cfg.ErrTrack, err)
	}
	cfg, err = config.Load[errTrackOnly](errTrackEnv(config.EnvDev, "ERRORS_PROVIDER", " Sentry ", "SENTRY_DSN", "http://k@127.0.0.1:8000/1"))
	if err != nil || cfg.ErrTrack.Provider != config.ErrorsProviderSentry {
		t.Fatalf("dev loopback: %+v, %v", cfg.ErrTrack, err)
	}
}

// TestConfig_ErrTrackRejects: errors name the variable, never the DSN.
func TestConfig_ErrTrackRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  string
		kv   []string
		want string
	}{
		{config.EnvDev, []string{"ERRORS_PROVIDER", "rollbar"}, "ERRORS_PROVIDER must be sentry or noop"},
		{config.EnvDev, []string{"ERRORS_PROVIDER", "sentry"}, "needs SENTRY_DSN"},
		{config.EnvDev, []string{"ERRORS_PROVIDER", "noop", "SENTRY_DSN", exampleDSN}, "ERRORS_PROVIDER=noop"},
		{config.EnvDev, []string{"SENTRY_DSN", "glitchtip.koro.example/3"}, "SENTRY_DSN must be"},
		{config.EnvDev, []string{"SENTRY_DSN", "https://glitchtip.koro.example/3"}, "SENTRY_DSN must be"},
		{config.EnvDev, []string{"SENTRY_DSN", "https://key@glitchtip.koro.example/"}, "SENTRY_DSN must be"},
		{config.EnvDev, []string{"SENTRY_DSN", "https://key:legacysecret@glitchtip.koro.example/3"}, "legacy secret"},
		{config.EnvProd, []string{"SENTRY_DSN", "http://key@glitchtip.koro.example/3"}, "https"},
		{config.EnvStaging, []string{"SENTRY_DSN", "https://key@localhost/3"}, "loopback"},
		{config.EnvDev, []string{"SENTRY_SAMPLE_RATE", "0"}, "SENTRY_SAMPLE_RATE"},
		{config.EnvDev, []string{"SENTRY_SAMPLE_RATE", "1.5"}, "SENTRY_SAMPLE_RATE"},
	} {
		_, err := config.Load[errTrackOnly](errTrackEnv(tc.env, tc.kv...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v: error %v, want %q", tc.env, tc.kv, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "legacysecret") || strings.Contains(err.Error(), "examplepublickey") {
			t.Errorf("%v: error echoes the DSN: %v", tc.kv, err)
		}
	}
}
