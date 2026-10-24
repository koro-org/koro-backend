//go:build integration

package talent_test

import (
	"slices"
	"testing"
	"time"

	"koro/internal/talent"
)

// TestBandMultipliers_Gorm (00 T-328, BE-099): over PostgreSQL, as
// app_talent: the version in force without bands is an error; a version
// by the auditor's rules gives its bands[].mult_bp, a newer one replaces
// them, a future one does not yet count.
func TestBandMultipliers_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t, true) // an empty version is in force since the day before t0
	if _, _, err := p.mod.Ranking.BandMultipliers(t.Context()); err == nil {
		t.Fatal("a version without bands gave multipliers")
	}
	p.pgVersion(t, t0.Add(-time.Hour), testParams())
	if m, found := multipliers(t, p.mod.Ranking); !found || !slices.Equal(m, talent.BandMultipliers{10500, 11000, 11500}) {
		t.Fatalf("version in force: %v, %v", m, found)
	}
	p.pgVersion(t, t0.Add(-time.Minute), withBands(10300, 10900))
	p.pgVersion(t, t0.Add(time.Hour), withBands(11100, 11200, 11300))
	if m, found := multipliers(t, p.mod.Ranking); !found || !slices.Equal(m, talent.BandMultipliers{10300, 10900}) {
		t.Fatalf("newer version: %v, %v", m, found)
	}
}
