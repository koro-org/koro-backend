package ledger_test

import (
	"context"
	"testing"
	"time"

	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// cutoffSpy records the cut-off the sweeper hands the store.
type cutoffSpy struct {
	ledger.Store
	cutoffs []time.Time
	dry     []bool
}

func (s *cutoffSpy) PurgeFinancialRecords(_ context.Context, cutoff time.Time, _ int, dryRun bool) (int, error) {
	s.cutoffs, s.dry = append(s.cutoffs, cutoff), append(s.dry, dryRun)
	return 0, nil
}

// TestFinancialRecords_RoundedToYearEnd (TTK md. 82, VUK md. 253; 06 H-57):
// the 10 years run from the end of the record's calendar year, so the
// cut-off is the start (Europe/Istanbul) of the year of now − 10 years: a
// record of 31 December 2026 goes on 1 January 2037, one of 1 January 2027
// not before 1 January 2038.
func TestFinancialRecords_RoundedToYearEnd(t *testing.T) {
	t.Parallel()
	ist := clock.Istanbul()
	ten := func(now time.Time) time.Duration { return now.Sub(now.AddDate(-10, 0, 0)) }
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2037, 1, 1, 0, 30, 0, 0, ist), time.Date(2027, 1, 1, 0, 0, 0, 0, ist)},
		{time.Date(2036, 12, 31, 23, 59, 0, 0, ist), time.Date(2026, 1, 1, 0, 0, 0, 0, ist)},
		{time.Date(2037, 6, 15, 12, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, ist)},
		// 31 December 22:30 UTC is already 1 January in Istanbul.
		{time.Date(2036, 12, 31, 22, 30, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, ist)},
	} {
		got := ledger.FinancialCutoff(c.now, ten(c.now))
		if !got.Equal(c.want) {
			t.Errorf("FinancialCutoff(%s) = %s, want %s", c.now, got, c.want)
		}
	}
	record2026 := time.Date(2026, 12, 31, 23, 0, 0, 0, ist)
	record2027 := time.Date(2027, 1, 1, 0, 0, 0, 0, ist)
	on := time.Date(2037, 1, 1, 0, 0, 1, 0, ist)
	cut := ledger.FinancialCutoff(on, ten(on))
	if !record2026.Before(cut) || record2027.Before(cut) {
		t.Fatalf("on %s: a record of 2026 must be due (%v), one of 2027 not (%v)", on, record2026.Before(cut), !record2027.Before(cut))
	}

	// The sweeper hands the rounded cut-off to the store, dry run first.
	spy := &cutoffSpy{Store: ledger.NewMemStore(func() time.Time { return on })}
	e := newEnvWith(t, clock.NewFake(on), spy, db.NopTx{})
	p := privacy.RetentionPolicy{Code: ledger.PolicyFinancialRecords, Retention: ten(on)}
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, on); err != nil || res.Count != 0 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	p.LegalApproved = true
	if _, err := e.mod.Sweeper.Sweep(t.Context(), p, on); err != nil {
		t.Fatal(err)
	}
	if len(spy.cutoffs) != 2 || !spy.dry[0] || spy.dry[1] || !spy.cutoffs[0].Equal(cut) || !spy.cutoffs[1].Equal(cut) {
		t.Fatalf("store calls = %v (dry %v), want the year start %s", spy.cutoffs, spy.dry, cut)
	}
}
