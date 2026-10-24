//go:build integration

package talent_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/testutil"
	"koro/internal/talent"
)

func TestStore_ReconcileContract_Gorm(t *testing.T) {
	t.Parallel()
	reconcileContract(t, func(t *testing.T) contractEnv {
		d := testutil.NewDB(t)
		return contractEnv{store: talent.NewGormStore(d.ORM), tx: d.Tx, user: pgUser(d)}
	})
}

// pgRestart is the process after a restart with collection on or off, over
// the env's database and ports.
func (p pgEnv) pgRestart(t *testing.T, collection bool) restart {
	t.Helper()
	return newRestart(p.module(t, envOptions{collection: collection}))
}

// TestReconcile_Gorm (00 T-321; BE-090): over PostgreSQL, every talent
// statement as app_talent. With collection on a student's registration,
// check-in, like and club membership are credited twice (two sources
// each); after a restart with collection off the undoing of one source of
// each writes no ledger row and counts four drops in
// talent.collection_state; after a restart with collection on the nightly
// run writes the four reversals (the consumers' reasons, one
// source_event_id), records the drops it covered and scores the standing
// rows; a second run changes nothing.
func TestReconcile_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true})
	p.pgVersion(t, t0.Add(-time.Hour), bigCaps(testParams()))
	u := p.pgStudent(t)
	org := uuid.NewV7()
	ev := p.events.event(org, 10, 5)
	type src struct{ reg, checkin, post, membership uuid.UUID }
	var gone, kept src
	for _, s := range []*src{&gone, &kept} {
		*s = src{reg: uuid.NewV7(), checkin: uuid.NewV7(), post: uuid.NewV7(), membership: uuid.NewV7()}
		p.send(t, domainevent.RegistrationConfirmed{RegistrationID: s.reg, EventID: ev, OrgID: org, UserID: u}, t0)
		p.send(t, domainevent.CheckinRecorded{CheckinID: s.checkin, RegistrationID: s.reg, EventID: ev, OrgID: org,
			UserID: u, StaffUserID: uuid.NewV7()}, t0)
		p.send(t, domainevent.PostLiked{PostID: s.post, UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
		p.send(t, domainevent.MembershipStarted{MembershipID: s.membership, OrgID: uuid.NewV7(), OrgKind: string(authz.OrgClub),
			UserID: u, Role: string(authz.MemberMember)}, t0)
	}
	ledger := func() int {
		t.Helper()
		return p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1`, u)
	}
	if n := ledger(); n != 8 {
		t.Fatalf("credited rows = %d, want 8", n)
	}

	off := p.pgRestart(t, false)
	p.clk.Advance(time.Hour)
	at := p.clk.Now()
	off.send(t, domainevent.RegistrationCancelled{RegistrationID: gone.reg, EventID: ev, UserID: u, By: "user"}, at)
	off.send(t, domainevent.CheckinVoided{CheckinID: gone.checkin, RegistrationID: gone.reg, UserID: u}, at)
	off.send(t, domainevent.PostUnliked{PostID: gone.post, UserID: u, AuthorOrgID: uuid.NewV7()}, at)
	off.send(t, domainevent.MembershipEnded{MembershipID: gone.membership, OrgID: uuid.NewV7(), OrgKind: string(authz.OrgClub),
		UserID: u, Role: string(authz.MemberMember), Reason: "left"}, at)
	p.events.undo(gone.reg)
	p.events.undo(gone.checkin)
	p.likes.unlike(u, gone.post)
	p.members.end(gone.membership)
	if n := ledger(); n != 8 {
		t.Fatalf("collection off wrote ledger rows: %d, want 8", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.collection_state WHERE reversals_dropped = 4 AND reconciled_drops = 0
		AND last_dropped_at = $1 AND reconciled_at IS NULL`, at); n != 1 {
		t.Fatal("talent.collection_state does not hold the four drops")
	}
	if err := off.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := ledger(); n != 8 {
		t.Fatalf("the run with collection off reconciled: %d rows", n)
	}

	on := p.pgRestart(t, true)
	p.clk.Advance(time.Hour)
	if err := on.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := ledger(); n != 12 {
		t.Fatalf("ledger rows after the reconciliation = %d, want 12", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events r JOIN talent.point_events o ON o.id = r.reversal_of
		WHERE r.user_id = $1 AND r.points_cp = -o.points_cp AND r.occurred_at = o.occurred_at AND o.source_id::text = ANY($2)
		  AND r.reason_code = CASE o.signal_code WHEN 'event_registration_confirmed' THEN 'registration_cancelled'
		    WHEN 'event_checkin' THEN 'checkin_voided' WHEN 'feed_like' THEN 'post_unliked' ELSE 'membership_ended' END`,
		u, []string{gone.reg.String(), gone.checkin.String(), gone.post.String(), gone.membership.String()}); n != 4 {
		t.Fatalf("reversals of the undone sources = %d, want 4", n)
	}
	if n := p.owner(t, `SELECT count(DISTINCT source_event_id) FROM talent.point_events WHERE user_id = $1 AND reversal_of IS NOT NULL`, u); n != 1 {
		t.Fatalf("the reconciliation's reversals carry %d source_event_ids, want 1", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.collection_state WHERE reconciled_drops = 4 AND reconciled_at = $1`, p.clk.Now()); n != 1 {
		t.Fatal("talent.collection_state does not record the reconciliation")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND raw_cp = 810 AND n_signals = 4`, u); n != 1 {
		t.Fatal("the score does not count the standing rows only")
	}

	p.clk.Advance(24 * time.Hour)
	if err := on.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := ledger(); n != 12 {
		t.Fatalf("the second run changed the ledger: %d rows", n)
	}
}
