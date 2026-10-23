package talent_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/talent"
)

// thresholds returns p with the three gaming thresholds (test values).
func thresholds(p talent.ScoreParams, likeDays, confirmed, distinct int) talent.ScoreParams {
	p.MinAccountAgeDaysForLikes, p.MinEventConfirmed, p.MinEventDistinctCheckins = &likeDays, &confirmed, &distinct
	return p
}

// sourced appends a ledger row of user with its source, as a consumer
// would have written it.
func (e *env) sourced(t *testing.T, user uuid.UUID, signal, srcType string, src uuid.UUID, points int, at time.Time) talent.PointEvent {
	t.Helper()
	p := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: signal, SourceType: srcType, SourceID: src, OccurredAt: at,
		RawPointsCP: points, PointsCP: points, ReasonCode: signal, ConfigVersionID: e.cfg.ID, SourceEventID: uuid.NewV7()}
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		p, _, err = e.store.InsertPoint(ctx, p)
		return err
	})
	return p
}

// reversal appends the consumer's reversal of orig.
func (e *env) reversal(t *testing.T, orig talent.PointEvent) {
	t.Helper()
	id := orig.ID
	r := talent.PointEvent{ID: uuid.NewV7(), UserID: orig.UserID, Signal: orig.Signal, SourceType: talent.SourceReversal,
		SourceID: orig.ID, OccurredAt: orig.OccurredAt, RawPointsCP: -orig.RawPointsCP, PointsCP: -orig.PointsCP,
		ReasonCode: "checkin_voided", ConfigVersionID: orig.ConfigVersionID, ReversalOf: &id, SourceEventID: uuid.NewV7()}
	e.inTalentTx(t, func(ctx context.Context) error {
		_, _, err := e.store.InsertPoint(ctx, r)
		return err
	})
}

// TestIngest_LikeAccountAge_BelowAndAbove (02 §15.1, R-003 §3.4; BE-085):
// with min_account_age_days_for_likes = 7 a like from an account younger
// than 7 days at the like writes nothing (outcome young_account), an
// unknown account neither; an account exactly 7 days old and an older one
// earn the like; the young account's later like, once it is old enough,
// earns too. Without the threshold the young account's like is written.
func TestIngest_LikeAccountAge_BelowAndAbove(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, thresholds(testParams(), 7, 0, 0))
	young, exact, old, unknown := e.student(), e.student(), e.student(), e.student()
	e.accounts.set(young, t0.Add(-7*24*time.Hour+time.Minute))
	e.accounts.set(exact, t0.Add(-7*24*time.Hour))
	e.accounts.set(old, t0.Add(-30*24*time.Hour))
	like := func(user uuid.UUID, at time.Time) {
		e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, at)
	}
	for _, u := range []uuid.UUID{young, exact, old, unknown} {
		like(u, t0)
	}
	for _, u := range []uuid.UUID{young, unknown} {
		if rows := e.points(u); len(rows) != 0 {
			t.Fatalf("a like below the account age threshold wrote %d rows", len(rows))
		}
	}
	for _, u := range []uuid.UUID{exact, old} {
		if got := e.total(u, talent.SignalFeedLike); got != 10 {
			t.Fatalf("a like at or above the threshold earned %d cp, want 10", got)
		}
	}
	if n := e.count(talent.SignalFeedLike, talent.OutcomeYoungAccount); n != 2 {
		t.Fatalf("young_account = %d, want 2", n)
	}
	like(young, t0.Add(time.Hour))
	if got := e.total(young, talent.SignalFeedLike); got != 10 {
		t.Fatalf("the account's like once 7 days old earned %d cp, want 10", got)
	}

	off := scoreEnv(t, testParams())
	u := off.student()
	off.accounts.set(u, t0.Add(-time.Hour))
	off.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	if got := off.total(u, talent.SignalFeedLike); got != 10 {
		t.Fatalf("without the threshold the like earned %d cp, want 10", got)
	}
}

// TestIngest_Organiser_ZeroAtOwnEvent (ADR-0015, R-003 §3.4; BE-085): a
// user holding an organiser role (club_admin) in the event's org earns 0
// from its registration, check-in (OrgID of the payload, or of Events when
// a payload predates the field) and no-show (org from Events): the rows
// are recorded with 0 points as per_source. A plain member of the org and
// the club_admin of another org earn the full points; a payload's OrgID
// holds even when Events does not know the event.
func TestIngest_Organiser_ZeroAtOwnEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	org, otherOrg := uuid.NewV7(), uuid.NewV7()
	admin, member, foreign := e.student(), e.student(), e.student()
	e.members.add(admin, org, authz.OrgClub, authz.MemberClubAdmin)
	e.members.add(member, org, authz.OrgClub, authz.MemberMember)
	e.members.add(foreign, otherOrg, authz.OrgClub, authz.MemberClubAdmin)
	ev := e.events.event(org, 10, 5)
	staff := uuid.NewV7()

	attend := func(user, orgInPayload, event uuid.UUID) {
		e.send(t, domainevent.RegistrationConfirmed{RegistrationID: uuid.NewV7(), EventID: event, OrgID: orgInPayload, UserID: user}, t0)
		e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: event,
			OrgID: orgInPayload, UserID: user, StaffUserID: staff}, t0)
	}
	attend(admin, org, ev)
	// A check-in published before BE-085 (no OrgID) and a no-show (no
	// OrgID in its payload): the organiser comes from Events.
	e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: ev, UserID: admin,
		StaffUserID: staff}, t0)
	e.send(t, domainevent.RegistrationNoShow{RegistrationID: uuid.NewV7(), EventID: ev, UserID: admin}, t0)
	// The payload's org decides even for an event Events does not know.
	e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(),
		OrgID: org, UserID: admin, StaffUserID: staff}, t0)

	rows := e.points(admin)
	if len(rows) != 5 {
		t.Fatalf("organiser rows = %d, want 5", len(rows))
	}
	for _, r := range rows {
		if r.PointsCP != 0 || !r.Capped || r.CapReason != talent.CapPerSource {
			t.Errorf("organiser %s row = %d cp, capped %v %q; want 0, per_source", r.Signal, r.PointsCP, r.Capped, r.CapReason)
		}
	}

	attend(member, org, ev)
	attend(foreign, org, ev)
	for _, u := range []uuid.UUID{member, foreign} {
		if got := e.total(u, ""); got != 600 {
			t.Fatalf("a non-organiser earned %d cp, want 600 (registration 100 + check-in 500)", got)
		}
	}
}

// TestRecompute_GamingThresholds_DropRowsBelow (02 §15.1, R-003 §3.4;
// BE-085): with min_event_confirmed 5, min_event_distinct_checkins 3 and
// min_account_age_days_for_likes 7 the nightly score counts an event at
// exactly the thresholds and drops the registration, check-in and no-show
// of an event below either one or of an unknown source, the reversal of a
// dropped row with it, and a like from an account younger than 7 days at
// the like; a user whose every row is dropped has no score. The figures of
// an event are read once per run.
func TestRecompute_GamingThresholds_DropRowsBelow(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, thresholds(bigCaps(testParams()), 7, 5, 3))
	org := uuid.NewV7()
	big := e.events.event(org, 5, 3)
	fewConfirmed := e.events.event(org, 4, 3)
	fewCheckins := e.events.event(org, 5, 2)
	checkin := func(user, event uuid.UUID, points int) talent.PointEvent {
		return e.sourced(t, user, talent.SignalEventCheckin, talent.SourceCheckin, e.events.source(event), points, t0)
	}
	registration := func(user, event uuid.UUID, signal string, points int) talent.PointEvent {
		return e.sourced(t, user, signal, talent.SourceRegistration, e.events.source(event), points, t0)
	}

	kept := e.student()
	checkin(kept, big, 500)
	registration(kept, big, talent.SignalRegistrationConfirmed, 100)

	onlyDropped := e.student()
	checkin(onlyDropped, fewConfirmed, 500)
	registration(onlyDropped, fewCheckins, talent.SignalRegistrationConfirmed, 100)

	unknownSource := e.student()
	e.sourced(t, unknownSource, talent.SignalEventCheckin, talent.SourceCheckin, uuid.NewV7(), 500, t0)
	e.sourced(t, unknownSource, talent.SignalProfileSection, talent.SourceProfileSection, uuid.NewV7(), 200, t0)

	// The reversal of a dropped check-in goes with it: kept alone it would
	// cancel the valid check-in.
	reversed := e.student()
	checkin(reversed, big, 500)
	e.reversal(t, checkin(reversed, fewCheckins, 500))

	// A no-show of a dropped event does not pull the category down.
	noShow := e.student()
	checkin(noShow, big, 500)
	registration(noShow, fewConfirmed, talent.SignalEventNoShow, -200)

	oldLiker, youngLiker := e.student(), e.student()
	e.accounts.set(oldLiker, t0.Add(-30*24*time.Hour))
	e.accounts.set(youngLiker, t0.Add(-3*24*time.Hour))
	for _, u := range []uuid.UUID{oldLiker, youngLiker} {
		e.sourced(t, u, talent.SignalFeedLike, talent.SourcePost, uuid.NewV7(), 10, t0.Add(-24*time.Hour))
		e.sourced(t, u, talent.SignalProfileSection, talent.SourceProfileSection, uuid.NewV7(), 200, t0)
	}

	e.recompute(t)
	for _, c := range []struct {
		name   string
		user   uuid.UUID
		raw, n int
	}{
		{"an event at the thresholds", kept, 600, 2},
		{"an unknown source", unknownSource, 200, 1},
		{"a dropped check-in with its reversal", reversed, 500, 1},
		{"a no-show of a dropped event", noShow, 500, 1},
		{"a like of an old account", oldLiker, 210, 2}, // 10 × 0.5^(1/30) ≈ 9.77 → 10
		{"a like of a young account", youngLiker, 200, 1},
	} {
		s := e.score(t, c.user)
		if s.RawCP != c.raw || s.NSignals != c.n {
			t.Errorf("%s: raw %d, n %d; want %d, %d", c.name, s.RawCP, s.NSignals, c.raw, c.n)
		}
	}
	if _, ok := e.store.AllScores()[onlyDropped]; ok {
		t.Error("a user whose every row was dropped has a score")
	}
	if e.events.factReads != 1 {
		t.Errorf("event figures read %d times, want once per run", e.events.factReads)
	}
	// The ledger keeps every row: the thresholds decide what the score
	// counts, not what is recorded.
	if n := len(e.points(onlyDropped)); n != 2 {
		t.Errorf("the ledger of a dropped user has %d rows, want 2", n)
	}
}

// TestRecompute_GamingThresholdsOff_CountEverything (BE-085): without the
// thresholds (absent or 0) the nightly score counts every row and reads
// neither identity nor events.
func TestRecompute_GamingThresholdsOff_CountEverything(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]talent.ScoreParams{
		"absent": bigCaps(testParams()),
		"zero":   thresholds(bigCaps(testParams()), 0, 0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := scoreEnv(t, p)
			small := e.events.event(uuid.NewV7(), 1, 0)
			u := e.student()
			e.accounts.set(u, t0)
			e.sourced(t, u, talent.SignalEventCheckin, talent.SourceCheckin, e.events.source(small), 500, t0)
			e.sourced(t, u, talent.SignalEventCheckin, talent.SourceCheckin, uuid.NewV7(), 500, t0)
			e.sourced(t, u, talent.SignalFeedLike, talent.SourcePost, uuid.NewV7(), 10, t0)
			e.recompute(t)
			if s := e.score(t, u); s.RawCP != 1010 || s.NSignals != 3 {
				t.Fatalf("raw %d, n %d; want 1010, 3", s.RawCP, s.NSignals)
			}
			if e.events.factReads != 0 {
				t.Fatalf("event figures read %d times without a threshold", e.events.factReads)
			}
		})
	}
}
