package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type ingestOnly struct {
	config.Common
	Ingest config.Ingest
}

func ingestEnv(kv ...string) map[string]string {
	env := map[string]string{"APP_ENV": "dev"}
	for i := 0; i+1 < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	return env
}

// TestIngest_DefaultsSourceDefaultAndValidation (BE-210): the fake extractor
// and the haiku model by default; new sources start enabled in dev and
// staging, disabled in prod (auto) unless set; the bot page is https; the
// bounds of the periods and limits hold; the key is a secret.
func TestIngest_DefaultsSourceDefaultAndValidation(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[ingestOnly](ingestEnv())
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Ingest
	if g.ExtractorProvider != config.ExtractorFake || g.ExtractorModel != "claude-haiku-5-5" || !g.FetchEnabled ||
		g.ListInterval != 24*time.Hour || g.EventsInterval != 6*time.Hour || g.HostGap != 10*time.Second ||
		g.EventRetention != 7*24*time.Hour || !g.SourceDefaultEnabled() || g.UserAgent() != "KoroBot/1.0" {
		t.Fatalf("ingest defaults = %+v", g)
	}
	for env, want := range map[string]bool{"dev": true, "staging": true, "prod": false} {
		c, err := config.Load[ingestOnly](ingestEnv("APP_ENV", env, "LOG_FORMAT", "json"))
		if err != nil {
			t.Fatal(err)
		}
		if c.Ingest.SourceDefaultEnabled() != want {
			t.Errorf("%s: auto source default = %v, want %v", env, c.Ingest.SourceDefaultEnabled(), want)
		}
	}
	if c, err := config.Load[ingestOnly](ingestEnv("APP_ENV", "prod", "INGEST_SOURCE_DEFAULT", "enabled")); err != nil ||
		!c.Ingest.SourceDefaultEnabled() {
		t.Fatalf("prod enabled = %v %v", c.Ingest.SourceDefaultEnabled(), err)
	}
	c, err := config.Load[ingestOnly](ingestEnv("INGEST_EXTRACTOR_PROVIDER", " Claude ", "INGEST_ANTHROPIC_API_KEY", "test-key-not-a-real-one",
		"INGEST_BOT_INFO_URL", "https://koro.example.org/bot"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ingest.ExtractorProvider != config.ExtractorClaude || c.Ingest.AnthropicAPIKey.Reveal() != "test-key-not-a-real-one" ||
		strings.Contains(c.Ingest.AnthropicAPIKey.String(), "test-key") || c.Ingest.UserAgent() != "KoroBot/1.0 (+https://koro.example.org/bot)" {
		t.Fatalf("claude config = %+v", c.Ingest)
	}
	for _, tc := range []struct{ name, key, value, want string }{
		{"unknown extractor", "INGEST_EXTRACTOR_PROVIDER", "gpt", "INGEST_EXTRACTOR_PROVIDER"},
		{"unknown source default", "INGEST_SOURCE_DEFAULT", "sometimes", "INGEST_SOURCE_DEFAULT"},
		{"http bot page", "INGEST_BOT_INFO_URL", "http://koro.example.org/bot", "INGEST_BOT_INFO_URL"},
		{"a five-second host gap", "INGEST_HOST_GAP", "5s", "INGEST_HOST_GAP"},
		{"a ten-minute list interval", "INGEST_LIST_INTERVAL", "10m", "INGEST_LIST_INTERVAL"},
		{"a 100 MB body", "INGEST_MAX_BYTES", "104857600", "INGEST_MAX_BYTES"},
		{"a confidence over 1", "INGEST_MIN_CONFIDENCE", "1.5", "INGEST_MIN_CONFIDENCE"},
		{"an empty model", "INGEST_EXTRACTOR_MODEL", " ", "INGEST_EXTRACTOR_MODEL"},
	} {
		_, err := config.Load[ingestOnly](ingestEnv(tc.key, tc.value))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to name %s", tc.name, err, tc.want)
		}
	}
}
