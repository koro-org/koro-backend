package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Ingest configures the ingest module (internal/ingest; BE-210): the
// collection of clubs and events from official pages. Only tickets of the
// ingest module edit this file and the "# --- ingest ---" block of
// .env.example (BE-001 R7).
type Ingest struct {
	// ExtractorProvider selects the ingest.Extractor adapter
	// (INGEST_EXTRACTOR_PROVIDER, BE-113): "fake" (date patterns, no model;
	// the default) or "claude" (Claude Messages API with structured
	// output). claude without INGEST_ANTHROPIC_API_KEY falls back to fake
	// (logged at start): collection works, extraction is crude.
	ExtractorProvider string `env:"INGEST_EXTRACTOR_PROVIDER" envDefault:"fake"`
	// AnthropicAPIKey is the API key of the claude extractor (a Docker
	// secret: INGEST_ANTHROPIC_API_KEY_FILE).
	AnthropicAPIKey Secret `env:"INGEST_ANTHROPIC_API_KEY"`
	// ExtractorModel is the model of the claude extractor.
	ExtractorModel string `env:"INGEST_EXTRACTOR_MODEL" envDefault:"claude-haiku-5-5"`
	// ExtractorMaxTokens bounds one answer.
	ExtractorMaxTokens int `env:"INGEST_EXTRACTOR_MAX_TOKENS" envDefault:"8000"`
	// ExtractorTimeout bounds one extraction call.
	ExtractorTimeout time.Duration `env:"INGEST_EXTRACTOR_TIMEOUT" envDefault:"90s"`

	// FetchEnabled is the global switch of ingest.fetch_due (off: nothing
	// is fetched, the sources keep their state).
	FetchEnabled bool `env:"INGEST_FETCH_ENABLED" envDefault:"true"`
	// SourceDefault is the state of a new source: "auto" (enabled in dev
	// and staging, disabled in prod until the lawyer's review), "enabled"
	// or "disabled".
	SourceDefault string `env:"INGEST_SOURCE_DEFAULT" envDefault:"auto"`
	// BotInfoURL is the page that explains the crawler (and how to opt
	// out), named in the User-Agent "KoroBot/1.0 (+<url>)". https only.
	BotInfoURL string `env:"INGEST_BOT_INFO_URL"`
	// ListInterval and EventsInterval are the default periods of a new
	// club list and events source (1h–720h).
	ListInterval   time.Duration `env:"INGEST_LIST_INTERVAL" envDefault:"24h"`
	EventsInterval time.Duration `env:"INGEST_EVENTS_INTERVAL" envDefault:"6h"`
	// HostGap is the least time between two fetches of a host (≥ 10s; the
	// host's Crawl-delay wins when longer).
	HostGap time.Duration `env:"INGEST_HOST_GAP" envDefault:"10s"`
	// FetchTimeout bounds one fetch; MaxBytes one body.
	FetchTimeout time.Duration `env:"INGEST_FETCH_TIMEOUT" envDefault:"20s"`
	MaxBytes     int64         `env:"INGEST_MAX_BYTES" envDefault:"2097152"`
	// FailLimit failures in a row pause a source (paused_reason failures).
	FailLimit int `env:"INGEST_FAIL_LIMIT" envDefault:"5"`
	// ZeroYieldAlarm empty successful runs in a row raise the alarm.
	ZeroYieldAlarm int `env:"INGEST_ZERO_YIELD_ALARM" envDefault:"3"`
	// MaxTextChars bounds the masked text sent per extraction.
	MaxTextChars int `env:"INGEST_MAX_TEXT_CHARS" envDefault:"30000"`
	// MinConfidence drops the candidates the extractor is less sure of.
	MinConfidence float64 `env:"INGEST_MIN_CONFIDENCE" envDefault:"0.6"`
	// EventRetention is the default of the imported_event policy (the
	// policy row wins, hooks.retention).
	EventRetention time.Duration `env:"INGEST_EVENT_RETENTION" envDefault:"168h"`

	// QuickImportImages decides whether the quick import (BE-211) sends a
	// club's posters to the extractor: "auto" (dev and staging on, prod off
	// until the lawyer's review: a poster cannot be masked, so its faces
	// and names would leave the country), "enabled" or "disabled". The
	// post text is always read, masked.
	QuickImportImages string `env:"INGEST_QUICK_IMPORT_IMAGES" envDefault:"auto"`
	// QuickImportOrgHourly and QuickImportOrgDaily bound one club's quick
	// imports; QuickImportDailyBudget the whole platform's (the language
	// model's cost; each call is one unit).
	QuickImportOrgHourly   int `env:"INGEST_QUICK_IMPORT_ORG_HOURLY" envDefault:"20"`
	QuickImportOrgDaily    int `env:"INGEST_QUICK_IMPORT_ORG_DAILY" envDefault:"60"`
	QuickImportDailyBudget int `env:"INGEST_QUICK_IMPORT_DAILY_BUDGET" envDefault:"1000"`

	// defaultOn is SourceDefault resolved for APP_ENV by validate.
	defaultOn bool
	// imagesOn is QuickImportImages resolved for APP_ENV by validate.
	imagesOn bool
}

// Extractor names (INGEST_EXTRACTOR_PROVIDER).
const (
	ExtractorFake   = "fake"
	ExtractorClaude = "claude"
)

// ExtractorProviders lists the accepted INGEST_EXTRACTOR_PROVIDER values;
// cmd/koro has one constructor per name (wire_ingest.go).
func ExtractorProviders() []string { return []string{ExtractorFake, ExtractorClaude} }

// SourceDefaultEnabled is INGEST_SOURCE_DEFAULT resolved for APP_ENV: a
// new source starts enabled (auto: dev and staging) or disabled (auto: prod,
// until the lawyer's review). The zero value (no configuration) is false.
func (g Ingest) SourceDefaultEnabled() bool { return g.defaultOn }

// QuickImportImagesEnabled is INGEST_QUICK_IMPORT_IMAGES resolved for
// APP_ENV: the quick import reads posters (auto: dev and staging) or only
// the post text (auto: prod, until the lawyer's review). The zero value
// (no configuration) is false.
func (g Ingest) QuickImportImagesEnabled() bool { return g.imagesOn }

// UserAgent is the crawler's User-Agent.
func (g Ingest) UserAgent() string {
	if g.BotInfoURL == "" {
		return "KoroBot/1.0"
	}
	return "KoroBot/1.0 (+" + g.BotInfoURL + ")"
}

func (g *Ingest) validate(c Common) error {
	var errs []error
	if err := checkProvider(c, "INGEST_EXTRACTOR_PROVIDER", &g.ExtractorProvider, ExtractorProviders()); err != nil {
		errs = append(errs, err)
	}
	var err error
	if g.defaultOn, err = autoSwitch(c, "INGEST_SOURCE_DEFAULT", &g.SourceDefault); err != nil {
		errs = append(errs, err)
	}
	if g.imagesOn, err = autoSwitch(c, "INGEST_QUICK_IMPORT_IMAGES", &g.QuickImportImages); err != nil {
		errs = append(errs, err)
	}
	if g.BotInfoURL != "" {
		if u, err := url.Parse(g.BotInfoURL); err != nil || u.Scheme != "https" || u.Host == "" ||
			strings.ContainsAny(g.BotInfoURL, " ()") {
			errs = append(errs, errors.New("INGEST_BOT_INFO_URL must be an https:// URL"))
		}
	}
	if strings.TrimSpace(g.ExtractorModel) == "" {
		errs = append(errs, errors.New("INGEST_EXTRACTOR_MODEL is required"))
	}
	for _, d := range []struct {
		key      string
		v        time.Duration
		min, max time.Duration
	}{
		{"INGEST_LIST_INTERVAL", g.ListInterval, time.Hour, 720 * time.Hour},
		{"INGEST_EVENTS_INTERVAL", g.EventsInterval, time.Hour, 720 * time.Hour},
		{"INGEST_HOST_GAP", g.HostGap, 10 * time.Second, time.Hour},
		{"INGEST_FETCH_TIMEOUT", g.FetchTimeout, time.Second, 2 * time.Minute},
		{"INGEST_EXTRACTOR_TIMEOUT", g.ExtractorTimeout, 5 * time.Second, 10 * time.Minute},
		{"INGEST_EVENT_RETENTION", g.EventRetention, time.Hour, 90 * 24 * time.Hour},
	} {
		if d.v < d.min || d.v > d.max {
			errs = append(errs, fmt.Errorf("%s must be between %s and %s", d.key, d.min, d.max))
		}
	}
	switch {
	case g.MaxBytes < 64<<10 || g.MaxBytes > 20<<20:
		errs = append(errs, errors.New("INGEST_MAX_BYTES must be between 64 KiB and 20 MiB"))
	case g.FailLimit < 1 || g.FailLimit > 100:
		errs = append(errs, errors.New("INGEST_FAIL_LIMIT must be between 1 and 100"))
	case g.ZeroYieldAlarm < 1 || g.ZeroYieldAlarm > 100:
		errs = append(errs, errors.New("INGEST_ZERO_YIELD_ALARM must be between 1 and 100"))
	case g.MaxTextChars < 1000 || g.MaxTextChars > 400000:
		errs = append(errs, errors.New("INGEST_MAX_TEXT_CHARS must be between 1000 and 400000"))
	case g.MinConfidence < 0 || g.MinConfidence > 1:
		errs = append(errs, errors.New("INGEST_MIN_CONFIDENCE must be between 0 and 1"))
	case g.ExtractorMaxTokens < 256 || g.ExtractorMaxTokens > 64000:
		errs = append(errs, errors.New("INGEST_EXTRACTOR_MAX_TOKENS must be between 256 and 64000"))
	case g.QuickImportOrgHourly < 1 || g.QuickImportOrgHourly > 1000:
		errs = append(errs, errors.New("INGEST_QUICK_IMPORT_ORG_HOURLY must be between 1 and 1000"))
	case g.QuickImportOrgDaily < g.QuickImportOrgHourly || g.QuickImportOrgDaily > 10000:
		errs = append(errs, errors.New("INGEST_QUICK_IMPORT_ORG_DAILY must be between INGEST_QUICK_IMPORT_ORG_HOURLY and 10000"))
	case g.QuickImportDailyBudget < 1 || g.QuickImportDailyBudget > 1000000:
		errs = append(errs, errors.New("INGEST_QUICK_IMPORT_DAILY_BUDGET must be between 1 and 1000000"))
	}
	return errors.Join(errs...)
}

// autoSwitch resolves an auto|enabled|disabled setting for APP_ENV (auto:
// on in dev and staging, off in prod, until the lawyer's review), lower
// casing *v.
func autoSwitch(c Common, key string, v *string) (bool, error) {
	*v = strings.ToLower(strings.TrimSpace(*v))
	switch *v {
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	case "auto":
		return c.Env != EnvProd, nil
	}
	return false, fmt.Errorf("%s must be auto, enabled or disabled (got %q)", key, *v)
}
