package talent_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/talent"
)

// restart is the process after a restart with collection on or off: a
// second module over the env's store and ports (env.module, BE-087), with
// its own subscriptions.
type restart struct {
	mod  talent.Module
	subs *outbox.Registry
}

func newRestart(mod talent.Module) restart {
	r := restart{mod: mod, subs: outbox.NewRegistry()}
	mod.Subscribe(r.subs)
	return r
}

func (e *env) restart(t *testing.T, collection bool) restart {
	t.Helper()
	return newRestart(e.module(t, envOptions{collection: collection}))
}

// send delivers p at at to the restarted module's talent subscription.
func (r restart) send(t *testing.T, p domainevent.Payload, at time.Time) {
	t.Helper()
	ev := envelope(t, p, at)
	subs := r.subs.For(ev.Topic)
	if len(subs) != 1 {
		t.Fatalf("%s: %d talent subscriptions, want 1", ev.Topic, len(subs))
	}
	if err := subs[0].Handle(t.Context(), ev); err != nil {
		t.Fatalf("%s: %v", ev.Topic, err)
	}
}

// state reads talent.collection_state.
func (e *env) state(t *testing.T) talent.CollectionState {
	t.Helper()
	var st talent.CollectionState
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		st, err = e.store.CollectionState(ctx)
		return err
	})
	return st
}

// closedPeriod is the scenario of 00 T-321 over one env: with collection
// on a student registers, checks in, likes a post and joins a club (one
// of each is undone later) plus one more of each that stays; collection
// goes off and the four undoings arrive (dropped, counted); the sources'
// modules record the undoings. It returns the student and the four
// originals that must be reversed.
func closedPeriod(t *testing.T, e *env) (uuid.UUID, []talent.PointEvent) {
	t.Helper()
	u := e.student()
	org := uuid.NewV7()
	ev := e.events.event(org, 10, 5)
	type src struct{ reg, checkin, post, membership uuid.UUID }
	var gone, kept src
	for _, s := range []*src{&gone, &kept} {
		*s = src{reg: uuid.NewV7(), checkin: uuid.NewV7(), post: uuid.NewV7(), membership: uuid.NewV7()}
		e.send(t, domainevent.RegistrationConfirmed{RegistrationID: s.reg, EventID: ev, OrgID: org, UserID: u}, t0)
		e.send(t, domainevent.CheckinRecorded{CheckinID: s.checkin, RegistrationID: s.reg, EventID: ev, OrgID: org,
			UserID: u, StaffUserID: uuid.NewV7()}, t0)
		e.send(t, domainevent.PostLiked{PostID: s.post, UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
		e.send(t, domainevent.MembershipStarted{MembershipID: s.membership, OrgID: uuid.NewV7(), OrgKind: string(authz.OrgClub),
			UserID: u, Role: string(authz.MemberMember)}, t0)
	}
	if n := len(e.points(u)); n != 8 {
		t.Fatalf("credited rows = %d, want 8", n)
	}
	var want []talent.PointEvent
	for _, p := range e.points(u) {
		if p.SourceID == gone.reg || p.SourceID == gone.checkin || p.SourceID == gone.post || p.SourceID == gone.membership {
			want = append(want, p)
		}
	}

	off := e.restart(t, false)
	e.clk.Advance(time.Hour)
	off.send(t, domainevent.RegistrationCancelled{RegistrationID: gone.reg, EventID: ev, UserID: u, By: "user"}, e.clk.Now())
	off.send(t, domainevent.CheckinVoided{CheckinID: gone.checkin, RegistrationID: gone.reg, UserID: u}, e.clk.Now())
	off.send(t, domainevent.PostUnliked{PostID: gone.post, UserID: u, AuthorOrgID: uuid.NewV7()}, e.clk.Now())
	off.send(t, domainevent.MembershipEnded{MembershipID: gone.membership, OrgID: uuid.NewV7(), OrgKind: string(authz.OrgClub),
		UserID: u, Role: string(authz.MemberMember), Reason: "left"}, e.clk.Now())
	e.events.undo(gone.reg)
	e.events.undo(gone.checkin)
	e.likes.unlike(u, gone.post)
	e.members.end(gone.membership)
	return u, want
}

// TestReconcile_ClosedPeriodCancellations_ReversedOnFlagOn (00 T-321, user
// decision of 2026-10-23; BE-090): while collection is off a cancelled
// registration, a voided check-in, a withdrawn like and an ended club
// membership write no ledger row; the four drops are counted in
// talent.collection_state and nothing else (no source read). The nightly
// run with collection off reconciles nothing. The first nightly run with
// collection on asks the source modules, writes the consumer's reversal of
// each undone source (reason, negated points, the original's occurred_at;
// source_event_id the run's) and keeps the rest; the score counts only the
// rows that stand. A second run changes nothing and reads nothing.
func TestReconcile_ClosedPeriodCancellations_ReversedOnFlagOn(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	u, undone := closedPeriod(t, e)
	if n := len(e.points(u)); n != 8 {
		t.Fatalf("collection off wrote ledger rows: %d, want 8", n)
	}
	if st := e.state(t); st.ReversalsDropped != 4 || !st.Pending() || st.LastDroppedAt == nil || !st.LastDroppedAt.Equal(e.clk.Now()) {
		t.Fatalf("state after the closed period = %+v, want 4 drops pending", st)
	}
	off := e.restart(t, false)
	if err := off.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.points(u)); n != 8 || e.likes.reads != 0 {
		t.Fatalf("the run with collection off reconciled: %d rows, %d like reads", n, e.likes.reads)
	}

	on := e.restart(t, true)
	e.clk.Advance(time.Hour)
	if err := on.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := e.points(u)
	if len(rows) != 12 {
		t.Fatalf("ledger rows after the reconciliation = %d, want 12 (8 + 4 reversals)", len(rows))
	}
	reasons := map[string]string{talent.SignalRegistrationConfirmed: "registration_cancelled", talent.SignalEventCheckin: "checkin_voided",
		talent.SignalFeedLike: "post_unliked", talent.SignalClubMembership: "membership_ended"}
	var run uuid.UUID
	for _, orig := range undone {
		var rev *talent.PointEvent
		for i, r := range rows {
			if r.ReversalOf != nil && *r.ReversalOf == orig.ID {
				rev = &rows[i]
			}
		}
		switch {
		case rev == nil:
			t.Fatalf("%s %s was not reversed", orig.Signal, orig.SourceID)
		case rev.PointsCP != -orig.PointsCP || rev.RawPointsCP != -orig.RawPointsCP || !rev.OccurredAt.Equal(orig.OccurredAt) ||
			rev.ReasonCode != reasons[orig.Signal] || rev.SourceType != talent.SourceReversal || rev.ConfigVersionID != orig.ConfigVersionID:
			t.Fatalf("reversal of %s = %+v", orig.Signal, *rev)
		case run == uuid.Nil():
			run = rev.SourceEventID
		case rev.SourceEventID != run:
			t.Fatal("the reversals of one reconciliation carry different source_event_ids")
		}
	}
	for sig := range reasons {
		if n := countOf(on.mod, sig, talent.OutcomeReconciled); n != 1 {
			t.Fatalf("reconciled{%s} = %d, want 1", sig, n)
		}
	}
	st := e.state(t)
	if st.Pending() || st.ReconciledDrops != 4 || st.ReconciledAt == nil || !st.ReconciledAt.Equal(e.clk.Now()) {
		t.Fatalf("state after the reconciliation = %+v", st)
	}
	// The score counts the standing rows only: one registration (100), one
	// check-in (500), one like (10), one membership (200).
	s, ok := e.store.AllScores()[u]
	if !ok || s.RawCP != 810 || s.NSignals != 4 {
		t.Fatalf("score = %+v (found %v), want raw 810 cp from 4 signals", s, ok)
	}

	reads := e.likes.reads
	e.clk.Advance(24 * time.Hour)
	if err := on.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.points(u)); n != 12 || e.likes.reads != reads {
		t.Fatalf("the second run changed the ledger (%d rows) or read the sources (%d reads)", n, e.likes.reads-reads)
	}
}

// TestReconcile_DropAfterTheRun_PendingAgain (00 T-321; BE-090): a drop
// after a reconciliation (an old worker with collection off during a
// rolling deploy) leaves the state pending again; the next run reverses
// that source only. Without a drop a run with collection on never reads a
// source; an unknown source (gone from its module) is never reversed.
func TestReconcile_DropAfterTheRun_PendingAgain(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	u := e.student()
	if err := e.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.likes.reads != 0 {
		t.Fatal("a run without a drop read the sources")
	}
	posts := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	for _, p := range posts {
		e.send(t, domainevent.PostLiked{PostID: p, UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	}
	off := e.restart(t, false)
	off.send(t, domainevent.PostUnliked{PostID: posts[0], UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	e.likes.unlike(u, posts[0])
	if err := e.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	off.send(t, domainevent.PostUnliked{PostID: posts[1], UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	e.likes.unlike(u, posts[1])
	// posts[2]'s unlike was dropped too, but feed no longer knows the post.
	off.send(t, domainevent.PostUnliked{PostID: posts[2], UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	if st := e.state(t); !st.Pending() || st.ReversalsDropped != 3 || st.ReconciledDrops != 1 {
		t.Fatalf("state after a later drop = %+v, want pending (3 drops, 1 reconciled)", st)
	}
	if err := e.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.total(u, talent.SignalFeedLike); got != 10 {
		t.Fatalf("likes after two reconciliations = %d cp, want 10 (only the like feed cannot judge stays)", got)
	}
	if st := e.state(t); st.Pending() || st.ReconciledDrops != 3 {
		t.Fatalf("state = %+v", st)
	}
}

// countOf returns koro_talent_ingest_total{signal, outcome} of mod.
func countOf(mod talent.Module, signal, outcome string) uint64 {
	for _, c := range mod.Counters() {
		if c.Signal == signal && c.Outcome == outcome {
			return c.Value
		}
	}
	return 0
}
