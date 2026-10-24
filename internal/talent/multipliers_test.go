package talent_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/talent"
)

// multVersion inserts a config version of p effective from at.
func (e *env) multVersion(t *testing.T, at time.Time, p talent.ScoreParams) talent.ConfigVersion {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var v talent.ConfigVersion
	e.inTalentTx(t, func(ctx context.Context) error {
		v, err = e.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: at, Params: raw, CreatedBy: uuid.NewV7()})
		return err
	})
	return v
}

// withBands is testParams with the band rules of mults (min_pct_bp grows
// with the band).
func withBands(mults ...int) talent.ScoreParams {
	p := testParams()
	p.Bands = nil
	for i, m := range mults {
		p.Bands = append(p.Bands, talent.BandRule{Band: i + 1, MinPctBP: 5000 + 1000*i, MultBP: m})
	}
	return p
}

func multipliers(t *testing.T, r talent.Ranking) (talent.BandMultipliers, bool) {
	t.Helper()
	m, found, err := r.BandMultipliers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return m, found
}

// TestBandMultipliers_ConfigVersionInForce (00 T-328, BE-099): the
// multipliers are the bands[].mult_bp of the version in force now (the
// latest effective_from ≤ now): none without a version, a newer version
// replaces them at once, a future one only from its effective_from.
func TestBandMultipliers_ConfigVersionInForce(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, true, false)
	if m, found := multipliers(t, e.mod.Ranking); found || m != nil {
		t.Fatalf("no version: %v, %v", m, found)
	}
	e.multVersion(t, t0.Add(-time.Hour), testParams())
	if m, found := multipliers(t, e.mod.Ranking); !found || !slices.Equal(m, talent.BandMultipliers{10500, 11000, 11500}) {
		t.Fatalf("first version: %v, %v", m, found)
	}
	e.multVersion(t, t0.Add(-time.Minute), withBands(10200, 10800))
	e.multVersion(t, t0.Add(time.Hour), withBands(11100, 11200, 11300))
	if m, found := multipliers(t, e.mod.Ranking); !found || !slices.Equal(m, talent.BandMultipliers{10200, 10800}) {
		t.Fatalf("newer version: %v, %v", m, found)
	}
	e.clk.Advance(2 * time.Hour)
	if m, found := multipliers(t, e.mod.Ranking); !found || !slices.Equal(m, talent.BandMultipliers{11100, 11200, 11300}) {
		t.Fatalf("the future version once in force: %v, %v", m, found)
	}
}

// TestBandMultipliers_Of (BE-099): neutral is ×1.00, a band its rule's
// multiplier, a band above the version's highest the highest band's (the
// order of the bands holds until the nightly run); no multipliers is
// ×1.00 for every band.
func TestBandMultipliers_Of(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    talent.BandMultipliers
		want map[talent.Band]int
	}{
		{talent.BandMultipliers{10500, 11000, 11500}, map[talent.Band]int{0: 10000, 1: 10500, 2: 11000, 3: 11500}},
		{talent.BandMultipliers{10200, 10800}, map[talent.Band]int{0: 10000, 1: 10200, 2: 10800, 3: 10800}},
		{talent.BandMultipliers{11500}, map[talent.Band]int{0: 10000, 1: 11500, 2: 11500, 3: 11500}},
		{nil, map[talent.Band]int{0: 10000, 1: 10000, 2: 10000, 3: 10000}},
	} {
		for b, want := range tc.want {
			if got := tc.m.Of(b); got != want {
				t.Errorf("%v.Of(band %d) = %d, want %d", tc.m, int(b), got, want)
			}
		}
	}
}

// TestBandMultipliers_OffInvalidOrForeignTx (BE-099, T-32): with collection
// off nothing is read (every band is neutral); a version that does not
// validate is an error, not a guess; the read never joins another
// module's transaction.
func TestBandMultipliers_OffInvalidOrForeignTx(t *testing.T) {
	t.Parallel()
	off := buildEnv(t, false, false)
	off.multVersion(t, t0.Add(-time.Hour), testParams())
	if m, found := multipliers(t, off.mod.Ranking); found || m != nil {
		t.Fatalf("collection off: %v, %v", m, found)
	}

	e := buildEnv(t, true, true) // the env's version has no bands
	if _, _, err := e.mod.Ranking.BandMultipliers(t.Context()); err == nil {
		t.Fatal("a version without bands gave multipliers")
	}
	e.multVersion(t, t0.Add(-time.Hour), testParams())
	err := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		_, _, err := e.mod.Ranking.BandMultipliers(ctx)
		return err
	})
	if err == nil {
		t.Fatal("BandMultipliers ran inside another transaction")
	}
	if _, found := multipliers(t, e.mod.Ranking); !found {
		t.Fatal("the valid version is not in force")
	}
}
