//go:build integration

package talent_test

import (
	"context"
	"testing"
	"time"

	"koro/internal/platform/db"
	"koro/internal/talent"
)

// pgWorld is the retention world over PostgreSQL (collection off): the
// GORM store on the app_rw pool, the 000121 purge functions, the seeded
// retention policies (unapproved until approve). The clock is the
// database's: its purge functions clamp the cut-off to their own now().
func pgWorld(t *testing.T) retentionWorld {
	t.Helper()
	p := newPGEnv(t, false)
	var now time.Time
	if err := p.ownr.QueryRow(t.Context(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return retentionWorld{mod: p.mod, store: talent.NewGormStore(p.d.ORM), tx: p.d.Tx, now: now.UTC(), cfg: p.cfg, user: p.user,
		point: func(t *testing.T, e talent.PointEvent, created time.Time) {
			t.Helper()
			var of any
			if e.ReversalOf != nil {
				of = *e.ReversalOf
			}
			// app_talent cannot choose created_at; the owner backdates the row.
			if _, err := p.ownr.Exec(t.Context(), `INSERT INTO talent.point_events (id, user_id, signal_code, source_type, source_id,
				occurred_at, raw_points_cp, points_cp, reason_code, config_version_id, reversal_of, source_event_id, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, e.ID, e.UserID, e.Signal, e.SourceType, e.SourceID,
				e.OccurredAt, e.RawPointsCP, e.PointsCP, e.ReasonCode, e.ConfigVersionID, of, e.SourceEventID, created); err != nil {
				t.Fatal(err)
			}
		},
		rows: func(t *testing.T, table string) int {
			t.Helper()
			return p.owner(t, `SELECT count(*) FROM talent.`+table)
		},
		role: func(t *testing.T, ctx context.Context) string {
			t.Helper()
			var who string
			if err := db.Conn(ctx, p.d.ORM).Raw(`SELECT current_user`).Scan(&who).Error; err != nil {
				t.Fatal(err)
			}
			return who
		},
		approve: func(t *testing.T) {
			t.Helper()
			if _, err := p.d.Pool.Exec(t.Context(), `UPDATE retention_policies SET legal_approved = true,
				approved_ref = 'Hukuk görüşü 2026/21', enabled = true WHERE owner_module = 'talent'`); err != nil {
				t.Fatal(err)
			}
		}}
}

// TestSweeper_TalentRetention_Gorm (BE-079; acceptance): over PostgreSQL,
// collection off, the unapproved run counts inside kvkk's rolled-back
// transaction (on app_talent, handed back on app_rw) and destroys
// nothing; once the lawyer approved the policies the sweep deletes the
// ledger rows of 24 months (old reversals before their originals, through
// talent.point_events_purge), the config version nothing uses any more,
// the counters of 14 days, the runs of a year and the neutralisations
// lifted 3 years ago; the rows inside their periods stay and a second
// sweep changes nothing.
func TestSweeper_TalentRetention_Gorm(t *testing.T) {
	t.Parallel()
	checkRetention(t, pgWorld(t), "app_rw")
}

// TestSweeper_GormLedgerRefusedUntilApproved (T-35, 000121): a real purge
// of the ledger before the policy row is legally approved is refused by
// the database (55000) whatever the caller claims; nothing goes.
func TestSweeper_GormLedgerRefusedUntilApproved(t *testing.T) {
	t.Parallel()
	w := pgWorld(t)
	f := seedRetention(t, w)
	_, err := w.mod.Sweeper.Sweep(t.Context(), retentionPolicy(talent.PolicyLedger, w.now, true), w.now)
	if db.SQLState(err) != "55000" {
		t.Fatalf("ledger purge of an unapproved policy: %v; want SQLSTATE 55000", err)
	}
	if n := w.rows(t, "point_events"); n != f.before["point_events"] {
		t.Fatalf("ledger rows = %d, want %d", n, f.before["point_events"])
	}
	for range 3 { // the pool is app_rw again
		if who := w.role(t, t.Context()); who != "app_rw" {
			t.Fatalf("pool role after the refused purge = %s", who)
		}
	}
}
