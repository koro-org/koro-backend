package audit_test

import (
	"slices"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/platform/civil"
	"koro/internal/platform/privacy"
)

// TestSweeper_AuditEvents (audit_events, 2 years; BE-057): unapproved, the
// sweep counts the chained rows of the days past the period; approved, it
// deletes them day by day up to their roots and the chain stays
// verifiable from the newest purged root; unchained and young rows stay.
func TestSweeper_AuditEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	if got := e.m.Sweeper.PolicyCodes(); !slices.Equal(got, []string{audit.PolicyEvents, audit.PolicyRoots}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	e.logN(t, ctx, 3, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	e.clk.Set(t0.Add(24 * time.Hour))
	if err := audit.DailyRoots(ctx, e.m); err != nil {
		t.Fatal(err)
	}
	e.logN(t, ctx, 2, "identity.session.create", authztest.ID("student"), nil) // after the root
	e.chain(t)

	later := t0.AddDate(2, 0, 3) // the day0 root is past the 2-year period
	p := privacy.RetentionPolicy{Code: audit.PolicyEvents, Retention: later.Sub(later.AddDate(-2, 0, 0))}
	res, err := e.m.Sweeper.Sweep(ctx, p, later)
	if err != nil || res.Count != 3 || res.Table != "audit_events" {
		t.Fatalf("dry run = %+v, %v; want the 3 rows of day0", res, err)
	}
	if n := len(e.chained(t)); n != 5 {
		t.Fatalf("a dry run deleted: %d chained rows", n)
	}
	p.LegalApproved = true
	if res, err = e.m.Sweeper.Sweep(ctx, p, later); err != nil || res.Count != 3 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if rows := e.chained(t); len(rows) != 2 || *rows[0].ChainSeq != 4 {
		t.Fatalf("chained rows after the purge = %d", len(rows))
	}
	// The chain goes on from the root: a new row links and verifies.
	e.clk.Set(later)
	e.logN(t, ctx, 1, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	if res, err := audit.VerifyRange(ctx, e.m, day0.AddDays(1), civil.Today(later)); err != nil || !res.OK || res.CheckedRows != 3 {
		t.Fatalf("VerifyRange() after the purge = %+v, %v", res, err)
	}
	// Idempotent; one day earlier nothing would have gone.
	if res, err := e.m.Sweeper.Sweep(ctx, p, later); err != nil || res.Count != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
}
