package config

import (
	"fmt"
)

// Outreach configures the outreach module (internal/outreach). Owner:
// BE-030; only tickets of the outreach module edit this file and the
// "# --- outreach ---" block of .env.example (BE-001 R7). The unsubscribe
// link of commercial e-mail is consent's (CONSENT_UNSUBSCRIBE_BASE_URL,
// BE-062; it replaced OUTREACH_UNSUBSCRIBE_URL).
type Outreach struct {
	// CountDailyLimit is how many audience counts an org may run in 24
	// hours (countAudience, createSegment, updateSegment with a definition
	// and the targeted post's Compile together). With the per-minute
	// audience_count class (04 §1.9) and the rounded estimates it keeps an
	// org from narrowing a small group by differences of many slightly
	// different definitions.
	CountDailyLimit int `env:"OUTREACH_COUNT_DAILY_LIMIT" envDefault:"300"`
}

func (o *Outreach) validate(Common) error {
	if o.CountDailyLimit < 1 || o.CountDailyLimit > 10_000 {
		return fmt.Errorf("OUTREACH_COUNT_DAILY_LIMIT must be between 1 and 10000 (got %d)", o.CountDailyLimit)
	}
	return nil
}
