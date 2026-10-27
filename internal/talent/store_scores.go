package talent

import (
	"context"
	"uuid"
)

// scoreStore is the read side of talent.scores (02 §15): the stored bands
// and scores the ranking, the report and the distribution read
// (report.go, admin.go).
type scoreStore interface {
	// Bands returns the stored bands > 0 of the users among userIDs whose
	// score is not neutralised and who have no active neutralisation.
	Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error)
	// StoredBand returns the user's stored band and whether a score row
	// exists (the report shows it as is).
	StoredBand(ctx context.Context, userID uuid.UUID) (band Band, found bool, err error)
	// Score returns the user's score row; found is false without one.
	Score(ctx context.Context, userID uuid.UUID) (s Score, found bool, err error)
	// BandCounts counts the scores by band (0..3) that are not neutralised
	// and whose user has no active neutralisation, of the cohorts whose key
	// is cohortPrefix or starts with cohortPrefix + ":" ("" = every cohort):
	// getTalentDistribution.
	BandCounts(ctx context.Context, cohortPrefix string) (map[int]int, error)
}
