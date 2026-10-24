package talent

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §15, §24.2; BE-079, the BE-057 pattern; 00 T-323):
// kvkk.retention_sweep (02:30) calls the sweeper with the four talent
// policies, whatever talent.default_enabled says (the flag stops new
// rows, not the periods of the rows already held). Every policy is a hard
// delete:
//
//	talent_ledger          talent.point_events created 24 months ago, through
//	                       talent.point_events_purge (append-only, 000121);
//	                       then the config versions no row refers to any
//	                       more (talent.config_versions_purge: a version
//	                       explains ledger rows, so it follows talent_ledger)
//	talent_counters        talent.daily_counters of the days before the
//	                       cut-off's day (Europe/Istanbul; the counter's day
//	                       is its creation)
//	talent_runs            talent.score_runs started a year ago (a run is
//	                       created when it starts)
//	talent_neutralization  talent.neutralizations lifted 3 years ago (the
//	                       policy's ended_at is lifted_at); an active one,
//	                       an objection always, stays
//
// Unapproved (the seed state, T-35), a policy is a dry run: the sweeper
// counts inside the transaction kvkk opened and always rolls back, on
// app_talent, and gives the transaction back on the session role. Approved,
// it deletes in batches of RetentionBatch rows, each in a talent
// transaction of its own, oldest first, until nothing is left: an
// interrupted run continues the next night and a second run changes
// nothing. The scores and cohorts have no sweeper: talent.recompute
// rewrites them every night with collection on, deletes them with
// collection off (talent_collection_off, a job policy; T-324, BE-087:
// collection_off.go), deletes those of the users it no longer scores with
// collection on (talent_score_dropped, a job policy; T-342, BE-101:
// score_dropped.go) and the KVKK provider erases them with the account
// (02 §15.5, §15.6).

// Retention policy codes of talent (retention_policies.code).
const (
	PolicyLedger         = "talent_ledger"
	PolicyCounters       = "talent_counters"
	PolicyRuns           = "talent_runs"
	PolicyNeutralization = "talent_neutralization"

	// RetentionBatch is the most rows one purge transaction deletes (01 §7;
	// the batch limit of the 000121 purge functions).
	RetentionBatch = 1000
)

// sweeper is talent's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	return []string{PolicyCounters, PolicyLedger, PolicyNeutralization, PolicyRuns}
}

// purge is one purge of a policy: a batch of at most limit rows (dry:
// the count of every eligible row).
type purge struct {
	run func(ctx context.Context, limit int, dry bool) (int, error)
	// phased: a short batch is not the last one (the ledger deletes old
	// reversals before their originals); repeat until a batch is empty.
	phased bool
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	st := w.s.d.Store
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Method: privacy.MethodHardDelete}
	var purges []purge
	switch p.Code {
	case PolicyLedger:
		res.Table = "talent.point_events, talent.config_versions"
		purges = []purge{
			{run: func(ctx context.Context, limit int, dry bool) (int, error) {
				return st.PurgeLedger(ctx, cutoff, limit, dry)
			}, phased: true},
			{run: func(ctx context.Context, limit int, dry bool) (int, error) {
				return st.PurgeConfigVersions(ctx, cutoff, limit, dry)
			}},
		}
	case PolicyCounters:
		res.Table = "talent.daily_counters"
		before := civil.Today(cutoff)
		purges = []purge{{run: func(ctx context.Context, limit int, dry bool) (int, error) {
			return st.PurgeCounters(ctx, before, limit, dry)
		}}}
	case PolicyRuns:
		res.Table = "talent.score_runs"
		purges = []purge{{run: func(ctx context.Context, limit int, dry bool) (int, error) {
			return st.PurgeRuns(ctx, cutoff, limit, dry)
		}}}
	case PolicyNeutralization:
		res.Table = "talent.neutralizations"
		purges = []purge{{run: func(ctx context.Context, limit int, dry bool) (int, error) {
			return st.PurgeNeutralizations(ctx, cutoff, limit, dry)
		}}}
	default:
		return privacy.SweepResult{}, fmt.Errorf("talent: no retention sweep for policy %q", p.Code)
	}
	if !p.LegalApproved {
		err := w.s.dryTx(ctx, func(ctx context.Context) error {
			for _, pg := range purges {
				n, err := pg.run(ctx, RetentionBatch, true)
				if err != nil {
					return err
				}
				res.Count += n
			}
			return nil
		})
		return res, err
	}
	for _, pg := range purges {
		for {
			var n int
			err := w.s.inTx(ctx, func(ctx context.Context) error {
				var err error
				n, err = pg.run(ctx, RetentionBatch, false)
				return err
			})
			if err != nil {
				return res, fmt.Errorf("talent: retention %s: %w", p.Code, err)
			}
			res.Count += n
			if n == 0 || (!pg.phased && n < RetentionBatch) {
				break
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// dryTx runs the counting fn of a dry run on app_talent. kvkk runs a dry
// sweep inside a transaction it always rolls back (so a faulty sweeper
// destroys nothing): fn then runs in that transaction, which is switched to
// app_talent for fn and back to the session role afterwards (Store.Leave),
// so the role never outlives the count. Outside a transaction (a direct
// call) fn gets a talent transaction of its own.
func (s *service) dryTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if !db.InTx(ctx) {
		return s.inTx(ctx, fn)
	}
	tctx, err := s.d.Store.Enter(ctx)
	if err != nil {
		return err
	}
	if err := fn(tctx); err != nil {
		return err // the statement aborted the caller's transaction; its rollback ends the role
	}
	_, err = s.d.Store.Leave(tctx)
	return err
}
