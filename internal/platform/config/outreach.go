package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Outreach configures the outreach module (internal/outreach). Owner:
// BE-030; only tickets of the outreach module edit this file and the
// "# --- outreach ---" block of .env.example (BE-001 R7).
type Outreach struct {
	// CountDailyLimit is how many audience counts an org may run in 24
	// hours (countAudience, createSegment, updateSegment with a definition
	// and the targeted post's Compile together). With the per-minute
	// audience_count class (04 §1.9) and the rounded estimates it keeps an
	// org from narrowing a small group by differences of many slightly
	// different definitions.
	CountDailyLimit int `env:"OUTREACH_COUNT_DAILY_LIMIT" envDefault:"300"`
	// UnsubscribeURL is the https one-click unsubscribe endpoint of
	// commercial dispatch e-mail (BE-035; 6563 and RFC 8058: the footer link
	// and the List-Unsubscribe headers). Empty: commercial dispatches cannot
	// use e-mail (push only); informational e-mail is unaffected.
	UnsubscribeURL string `env:"OUTREACH_UNSUBSCRIBE_URL"`
}

func (o *Outreach) validate(Common) error {
	if o.CountDailyLimit < 1 || o.CountDailyLimit > 10_000 {
		return fmt.Errorf("OUTREACH_COUNT_DAILY_LIMIT must be between 1 and 10000 (got %d)", o.CountDailyLimit)
	}
	o.UnsubscribeURL = strings.TrimSpace(o.UnsubscribeURL)
	if o.UnsubscribeURL != "" {
		u, err := url.Parse(o.UnsubscribeURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Fragment != "" || len(o.UnsubscribeURL) > 500 {
			return fmt.Errorf("OUTREACH_UNSUBSCRIBE_URL must be an absolute https URL without a fragment (got %q)", o.UnsubscribeURL)
		}
	}
	return nil
}
