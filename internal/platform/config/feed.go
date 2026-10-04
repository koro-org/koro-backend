package config

import "errors"

// Feed configures the feed module (internal/feed). Owner: BE-027; only
// tickets of the feed module edit this file and the "# --- feed ---" block
// of .env.example (BE-001 R7).
type Feed struct {
	// ReviewThreshold is the number of moderator-approved posts after which
	// a new org publishes without review (02 §12.1, T-15: the first 3 posts
	// of a new org are reviewed). 1–50.
	ReviewThreshold int `env:"FEED_REVIEW_THRESHOLD" envDefault:"3"`
}

func (s *Feed) validate(Common) error {
	if s.ReviewThreshold < 1 || s.ReviewThreshold > 50 {
		return errors.New("FEED_REVIEW_THRESHOLD must be between 1 and 50")
	}
	return nil
}
