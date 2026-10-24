package talent_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/talent"
)

// TestIngest_EventStaff_ZeroAtTheirEvent (00 T-341, user decision of
// 2026-10-23; BE-090): a user on the door staff (event_staff) of an event
// without an organiser role in its org (an ordinary member of the club, or
// no membership at all) earns 0 from its registration, check-in and
// no-show there: per_source rows, like the organiser rule of BE-085. At
// another event of the same org, where they are not staff, they earn the
// full points; staff of another org's event and a plain attendee earn in
// full.
func TestIngest_EventStaff_ZeroAtTheirEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	org := uuid.NewV7()
	staffed := e.events.event(org, 10, 5)
	other := e.events.event(org, 10, 5)
	foreignOrg := uuid.NewV7()
	foreign := e.events.event(foreignOrg, 10, 5)
	outsider, member, elsewhere := e.student(), e.student(), e.student()
	e.members.add(member, org, authz.OrgClub, authz.MemberMember)
	for _, u := range []uuid.UUID{outsider, member} {
		e.events.addStaff(org, staffed, u)
	}
	e.events.addStaff(foreignOrg, foreign, elsewhere)

	attend := func(user, event uuid.UUID, orgInPayload uuid.UUID) {
		t.Helper()
		e.send(t, domainevent.RegistrationConfirmed{RegistrationID: uuid.NewV7(), EventID: event, OrgID: orgInPayload, UserID: user}, t0)
		e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: event,
			OrgID: orgInPayload, UserID: user, StaffUserID: uuid.NewV7()}, t0)
	}
	for _, u := range []uuid.UUID{outsider, member} {
		attend(u, staffed, org)
		// The no-show payload carries no org: it comes from Events.
		e.send(t, domainevent.RegistrationNoShow{RegistrationID: uuid.NewV7(), EventID: staffed, UserID: u}, t0)
		rows := e.points(u)
		if len(rows) != 3 {
			t.Fatalf("staff rows = %d, want 3", len(rows))
		}
		for _, r := range rows {
			if r.PointsCP != 0 || !r.Capped || r.CapReason != talent.CapPerSource {
				t.Errorf("staff %s row = %d cp, capped %v %q; want 0, per_source", r.Signal, r.PointsCP, r.Capped, r.CapReason)
			}
		}
	}

	// Not on the staff of the other event of the org: full points.
	attend(member, other, org)
	if got := e.total(member, ""); got != 600 {
		t.Fatalf("the staff member earned %d cp at an event they do not staff, want 600", got)
	}
	// Staff of another org's event is not staff of this one; a plain
	// attendee earns in full too.
	attendee := e.student()
	for _, u := range []uuid.UUID{elsewhere, attendee} {
		attend(u, staffed, org)
		if got := e.total(u, ""); got != 600 {
			t.Fatalf("a non-staff attendee earned %d cp, want 600", got)
		}
	}
}

// TestIngest_ProfileRefresh_OncePerYear (00 T-337, user decision of
// 2026-10-23; BE-090): a renewed student verification (user.edu_verified
// with renewal) writes profile_refresh with the catalogue's points (200 cp,
// half-life 365 days); a second renewal in the same calendar year
// (Europe/Istanbul) is a duplicate, the first renewal of the next year
// earns again. A first verification earns nothing; with collection off
// nothing is written; a minor and a non-student never earn.
func TestIngest_ProfileRefresh_OncePerYear(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	u := e.student()
	verified := func(user uuid.UUID, renewal bool, at time.Time) {
		t.Helper()
		e.send(t, domainevent.UserEduVerified{UserID: user, UniversityID: uuid.NewV7(), ExpiresAt: at.AddDate(1, 0, 0), Renewal: renewal}, at)
	}
	verified(u, false, t0)
	if rows := e.points(u); len(rows) != 0 {
		t.Fatalf("a first verification wrote %d rows", len(rows))
	}
	verified(u, true, t0)
	rows := e.points(u)
	if len(rows) != 1 || rows[0].Signal != talent.SignalProfileRefresh || rows[0].SourceType != talent.SourceProfileRefresh ||
		rows[0].PointsCP != 200 || !rows[0].OccurredAt.Equal(t0) {
		t.Fatalf("renewal rows = %+v, want one profile_refresh row of 200 cp", rows)
	}
	// Later the same year, and on 31 December 23:30 in Istanbul (20:30
	// UTC): still 2026, a duplicate.
	verified(u, true, t0.AddDate(0, 1, 0))
	verified(u, true, time.Date(2026, 12, 31, 20, 30, 0, 0, time.UTC))
	if got := e.total(u, talent.SignalProfileRefresh); got != 200 {
		t.Fatalf("renewals in one year earned %d cp, want 200", got)
	}
	if n := e.count(talent.SignalProfileRefresh, talent.OutcomeDuplicate); n != 2 {
		t.Fatalf("duplicate = %d, want 2", n)
	}
	// 1 January 00:30 in Istanbul (31 December 21:30 UTC) is 2027.
	verified(u, true, time.Date(2026, 12, 31, 21, 30, 0, 0, time.UTC))
	if got := e.total(u, talent.SignalProfileRefresh); got != 400 {
		t.Fatalf("the renewal of the next year earned %d cp in total, want 400", got)
	}

	minor, stranger := e.minor(), uuid.NewV7()
	verified(minor, true, t0)
	verified(stranger, true, t0)
	if len(e.points(minor))+len(e.points(stranger)) != 0 {
		t.Fatal("a minor or a non-student earned profile_refresh")
	}

	off := newEnv(t, false)
	v := off.student()
	off.send(t, domainevent.UserEduVerified{UserID: v, UniversityID: uuid.NewV7(), ExpiresAt: t0.AddDate(1, 0, 0), Renewal: true}, t0)
	if rows := off.points(v); len(rows) != 0 || off.count(talent.SignalProfileRefresh, talent.OutcomeFlagOff) != 1 {
		t.Fatalf("collection off: %d rows, flag_off %d", len(rows), off.count(talent.SignalProfileRefresh, talent.OutcomeFlagOff))
	}
}
