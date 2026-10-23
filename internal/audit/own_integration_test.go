//go:build integration

package audit_test

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// BE-078 on PostgreSQL: the store's sign-in attempt read and the summary
// over it.
func TestListMySecurityEvents_SignInAttemptsWithoutNetworkFacts_Postgres(t *testing.T) {
	t.Parallel()
	runSignInAttemptsVisible(t, newPGEnv(t, nil).env)
}

func TestListMySecurityEvents_AttemptBurstsSummarisedAcrossPages_Postgres(t *testing.T) {
	t.Parallel()
	runAttemptBursts(t, newPGEnv(t, nil).env)
}

// BE-091 on PostgreSQL: the break-glass justification read back from
// audit_events.reason through the break-glass subject read of ListOwn.
func TestListMySecurityEvents_BreakGlassShowsJustification_Postgres(t *testing.T) {
	t.Parallel()
	runBreakGlassJustification(t, newPGEnv(t, nil).env)
}

// TestListOwn_EveryKindReadsAnIndex_Postgres: with sequential scans
// disabled the plan of ListOwn has none — each kind of security event is
// read through an index (own events: audit_events_actor_idx, break-glass
// accesses: audit_events_break_glass_subject_idx of 000132, sign-in
// attempts: audit_events_resource_idx; on the empty test table the planner
// may pick audit_events_action_idx for the last). Before BE-078 one OR
// over the kinds made every listMySecurityEvents a scan of the whole log.
func TestListOwn_EveryKindReadsAnIndex_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	for _, after := range []*audit.After{nil, {At: t0, ID: uuid.NewV7()}} {
		q, args, err := audit.ListOwnQuery(uuid.NewV7(), after, 26)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		err = d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
			conn := db.Conn(ctx, d.ORM)
			if err := conn.Exec(`SET LOCAL enable_seqscan = off`).Error; err != nil {
				return err
			}
			rows, err := conn.Raw(`EXPLAIN `+q, args...).Rows()
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				plan = append(plan, line)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		out := strings.Join(plan, "\n")
		if strings.Contains(out, "Seq Scan") {
			t.Fatalf("cursor %v: ListOwn scans the log:\n%s", after != nil, out)
		}
		for _, idx := range []string{"audit_events_actor_idx", "audit_events_break_glass_subject_idx"} {
			if !strings.Contains(out, idx) {
				t.Fatalf("cursor %v: the plan does not read %s:\n%s", after != nil, idx, out)
			}
		}
	}
}
