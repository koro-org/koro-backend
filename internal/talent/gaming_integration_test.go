//go:build integration

package talent_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
)

// TestGaming_GormPipeline (02 §15.1, R-003 §3.4, ADR-0015; BE-085): over
// PostgreSQL, every talent statement as app_talent, with a config version
// holding the three thresholds (7 days, 5 confirmed, 3 distinct check-ins):
// a like from a 2-day-old account writes no ledger row, an old account's
// does; an organiser's check-in at their own club's event is a 0-point
// per_source row; the nightly score counts the check-in of an event at the
// thresholds and drops the one of an event below them (its user has no
// score when nothing else is left); the ledger keeps every row.
func TestGaming_GormPipeline(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true})
	p.pgVersion(t, t0.Add(-time.Hour), thresholds(bigCaps(testParams()), 7, 5, 3))

	young, old := p.pgStudent(t), p.pgStudent(t)
	p.accounts.set(young, t0.Add(-48*time.Hour))
	p.accounts.set(old, t0.Add(-30*24*time.Hour))
	for _, u := range []uuid.UUID{young, old} {
		p.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1`, young); n != 0 {
		t.Fatalf("a young account's like wrote %d rows", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1 AND points_cp = 10`, old); n != 1 {
		t.Fatal("an old account's like is not in the ledger")
	}

	club := uuid.NewV7()
	big := p.events.event(club, 5, 3)
	small := p.events.event(club, 2, 1)
	admin := p.pgStudent(t)
	p.members.add(admin, club, authz.OrgClub, authz.MemberClubAdmin)
	checkin := func(user, event uuid.UUID) uuid.UUID {
		src := p.events.source(event)
		p.send(t, domainevent.CheckinRecorded{CheckinID: src, RegistrationID: uuid.NewV7(), EventID: event, OrgID: club,
			UserID: user, StaffUserID: uuid.NewV7()}, t0)
		return src
	}
	adminSrc := checkin(admin, big)
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1 AND source_id = $2 AND points_cp = 0
		AND capped AND cap_reason = 'per_source'`, admin, adminSrc); n != 1 {
		t.Fatal("the organiser's check-in is not a 0-point per_source row")
	}

	attendee, dropped := p.pgStudent(t), p.pgStudent(t)
	checkin(attendee, big)
	checkin(attendee, small)
	checkin(dropped, small)
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND raw_cp = 500 AND n_signals = 1`, attendee); n != 1 {
		t.Fatal("the attendee's score does not count the big event only")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1`, dropped); n != 0 {
		t.Fatal("a user with only a dropped event has a score")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1`, dropped); n != 1 {
		t.Fatal("the ledger lost the dropped row")
	}
}
