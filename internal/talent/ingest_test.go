package talent_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
	"koro/internal/talent"
)

// everySignal is one event of each signal topic for user (and its club).
func everySignal(user, club uuid.UUID) []domainevent.Payload {
	reg := uuid.NewV7()
	return []domainevent.Payload{
		domainevent.RegistrationConfirmed{RegistrationID: reg, EventID: uuid.NewV7(), OrgID: club, UserID: user},
		domainevent.RegistrationNoShow{RegistrationID: reg, EventID: uuid.NewV7(), UserID: user},
		domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: reg, EventID: uuid.NewV7(), UserID: user, StaffUserID: uuid.NewV7()},
		domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: club, OrgKind: "club", UserID: user, Role: "member"},
		domainevent.StudentProfileSectionCompleted{UserID: user, Section: "skills"},
		domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: club},
		domainevent.PostSaved{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: club},
		domainevent.UserActiveDay{UserID: user, Day: civil.Today(t0)},
		domainevent.RegistrationCancelled{RegistrationID: reg, EventID: uuid.NewV7(), UserID: user, By: "user"},
		domainevent.CheckinVoided{CheckinID: uuid.NewV7(), RegistrationID: reg, UserID: user},
		domainevent.MembershipEnded{MembershipID: uuid.NewV7(), OrgID: club, OrgKind: "club", UserID: user, Role: "member", Reason: "left"},
		domainevent.PostUnliked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: club},
	}
}

// TestIngest_FlagOff_NoWrite (T-32): with talent.default_enabled=false no
// consumer writes the ledger or a counter, each one is counted as
// flag_off, and every band is neutral. The neutralisations still apply:
// an objection recorded now holds when the flag turns on.
func TestIngest_FlagOff_NoWrite(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	user, club := e.student(), uuid.NewV7()
	for _, p := range everySignal(user, club) {
		e.send(t, p, t0)
	}
	if n := len(e.store.AllPoints()); n != 0 {
		t.Fatalf("ledger rows = %d with the flag off, want 0", n)
	}
	if n := e.store.Counters(); n != 0 {
		t.Fatalf("daily counters = %d with the flag off, want 0", n)
	}
	for _, signal := range []string{talent.SignalRegistrationConfirmed, talent.SignalEventNoShow, talent.SignalEventCheckin,
		talent.SignalClubMembership, talent.SignalProfileSection, talent.SignalFeedLike, talent.SignalFeedSave, talent.SignalActiveDay} {
		if got := e.count(signal, talent.OutcomeFlagOff); got == 0 {
			t.Errorf("%s: flag_off not counted", signal)
		}
		if got := e.count(signal, talent.OutcomeWritten); got != 0 {
			t.Errorf("%s: %d written with the flag off", signal, got)
		}
	}
	e.store.PutScore(user, 3, false)
	bands, err := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{user})
	if err != nil || len(bands) != 0 {
		t.Fatalf("Bands with the flag off = %d entries, %v; want none (neutral)", len(bands), err)
	}

	dsr := uuid.NewV7()
	e.send(t, domainevent.DSRProfilingObjection{DSRID: dsr, UserID: user}, t0)
	if a := e.active(user); len(a) != 1 || a[0].Reason != talent.NeutralizeObjection || *a[0].DSRID != dsr {
		t.Fatalf("objection with the flag off: active = %+v", a)
	}
}

// TestIngest_DuplicateEvent_DedupeNoDoubleCount (01 §6.1): the same event
// delivered twice, and the same source re-emitted under another envelope,
// write one row and advance the daily counter once.
func TestIngest_DuplicateEvent_DedupeNoDoubleCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	p := domainevent.RegistrationConfirmed{RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), OrgID: uuid.NewV7(), UserID: user}
	ev := envelope(t, p, t0)
	e.deliver(t, ev)
	e.deliver(t, ev) // at-least-once redelivery
	p.Promoted = true
	e.send(t, p, t0.Add(time.Minute)) // same registration, new envelope
	rows := e.points(user)
	if len(rows) != 1 || rows[0].PointsCP != 100 || rows[0].SourceEventID != ev.ID || rows[0].ConfigVersionID != e.cfg.ID {
		t.Fatalf("rows = %+v, want one row of 100 cp from the first envelope", rows)
	}
	if got := e.count(talent.SignalRegistrationConfirmed, talent.OutcomeDuplicate); got != 2 {
		t.Fatalf("duplicates counted %d, want 2", got)
	}
	// The counter advanced once: two more confirmations fill the 300 cp
	// daily cap exactly, a fourth is capped.
	for range 3 {
		e.send(t, domainevent.RegistrationConfirmed{RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), OrgID: uuid.NewV7(), UserID: user}, t0)
	}
	if got := e.total(user, talent.SignalRegistrationConfirmed); got != 300 {
		t.Fatalf("total = %d, want the 300 cp daily cap", got)
	}

	// A reversal is idempotent as well, and the original stays the only
	// row of its source: a re-confirmation after the cancellation is a
	// duplicate, not new points.
	cancel := envelope(t, domainevent.RegistrationCancelled{RegistrationID: p.RegistrationID, EventID: p.EventID, UserID: user, By: "user"}, t0)
	e.deliver(t, cancel)
	e.deliver(t, cancel)
	e.send(t, p, t0.Add(time.Hour))
	var reversals []talent.PointEvent
	for _, r := range e.points(user) {
		if r.ReversalOf != nil {
			reversals = append(reversals, r)
		}
	}
	if len(reversals) != 1 || reversals[0].PointsCP != -100 || *reversals[0].ReversalOf != rows[0].ID ||
		!reversals[0].OccurredAt.Equal(rows[0].OccurredAt) || reversals[0].ReasonCode != "registration_cancelled" ||
		reversals[0].SourceType != talent.SourceReversal || reversals[0].SourceID != rows[0].ID {
		t.Fatalf("reversals = %+v", reversals)
	}
	if got := e.total(user, talent.SignalRegistrationConfirmed); got != 200 {
		t.Fatalf("total after the cancellation = %d, want 200", got)
	}
	// A reversal without an original (never written) is a no-op.
	e.send(t, domainevent.CheckinVoided{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), UserID: user}, t0)
	if got := e.count(talent.SignalEventCheckin, talent.OutcomeNoOriginal); got != 1 {
		t.Fatalf("no_original counted %d, want 1", got)
	}
}

// TestIngest_SensitiveClubMembership_Ignored (02 §15.3, 03 Y2, T-34): a
// sensitive club's membership, a sensitive event and a sensitive club's
// post produce no signal; a plain club does; a company membership is no
// club signal at all.
func TestIngest_SensitiveClubMembership_Ignored(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	sensitive, plain := uuid.NewV7(), uuid.NewV7()
	e.dir.sensitive[sensitive] = true

	e.send(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: sensitive, OrgKind: "club", UserID: user, Role: "member"}, t0)
	e.send(t, domainevent.RegistrationConfirmed{RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), OrgID: plain, UserID: user, Sensitive: true}, t0)
	e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), UserID: user,
		StaffUserID: uuid.NewV7(), Sensitive: true}, t0)
	e.send(t, domainevent.RegistrationNoShow{RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), UserID: user, Sensitive: true}, t0)
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: sensitive}, t0)
	e.send(t, domainevent.PostSaved{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: sensitive}, t0)
	e.send(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: uuid.NewV7(), OrgKind: "company", UserID: user, Role: "member"}, t0)
	if rows := e.points(user); len(rows) != 0 {
		t.Fatalf("sensitive sources wrote %+v", rows)
	}
	if got := e.count(talent.SignalClubMembership, talent.OutcomeSensitive); got != 1 {
		t.Fatalf("sensitive membership counted %d, want 1", got)
	}

	ms := domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: plain, OrgKind: "club", UserID: user, Role: "member"}
	e.send(t, ms, t0)
	if got := e.total(user, talent.SignalClubMembership); got != 200 {
		t.Fatalf("plain club membership = %d cp, want 200", got)
	}
	e.send(t, domainevent.MembershipEnded{MembershipID: ms.MembershipID, OrgID: plain, OrgKind: "club", UserID: user, Role: "member", Reason: "left"}, t0)
	if got := e.total(user, talent.SignalClubMembership); got != 0 {
		t.Fatalf("ended membership = %d cp, want 0 (reversed)", got)
	}
}

// TestIngest_Minor_NoSignal (T-22, T-23): a minor, a user without a
// student profile and a student who turned ranking off produce nothing.
func TestIngest_Minor_NoSignal(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	minor, nobody, optedOut := e.minor(), uuid.NewV7(), e.student()
	e.students.setChoice(optedOut, students.RankingDisabled)
	for _, user := range []uuid.UUID{minor, nobody, optedOut} {
		for _, p := range everySignal(user, uuid.NewV7())[:8] {
			e.send(t, p, t0)
		}
		if rows := e.points(user); len(rows) != 0 {
			t.Fatalf("user %s got %d rows", user, len(rows))
		}
	}
	for outcome, want := range map[string]uint64{talent.OutcomeMinor: 1, talent.OutcomeNotStudent: 1, talent.OutcomeOptedOut: 1} {
		if got := e.count(talent.SignalFeedLike, outcome); got != want {
			t.Errorf("feed_like %s = %d, want %d", outcome, got, want)
		}
	}
	// T-23: turning 18 starts from zero; nothing is back-filled.
	e.students.mu.Lock()
	c := e.students.cohorts[minor]
	c.IsMinor = false
	e.students.cohorts[minor] = c
	e.students.mu.Unlock()
	e.send(t, domainevent.StudentProfileSectionCompleted{UserID: minor, Section: "basics"}, t0)
	if got := e.total(minor, ""); got != 200 {
		t.Fatalf("after turning 18: %d cp, want only the new 200", got)
	}
}

// TestIngest_DailyCap (02 §15.4, seed 000081): caps are magnitudes of the
// signal definition per Istanbul day and per 7 days; a capped event is
// recorded with what was left (0 when nothing) and its cap reason.
func TestIngest_DailyCap(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	// feed_like: 10 cp, 50 cp a day.
	for range 6 {
		e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	}
	if got := e.total(user, talent.SignalFeedLike); got != 50 {
		t.Fatalf("likes in a day = %d cp, want the 50 cp cap", got)
	}
	var capped []talent.PointEvent
	for _, p := range e.points(user) {
		if p.Capped {
			capped = append(capped, p)
		}
	}
	if len(capped) != 1 || capped[0].PointsCP != 0 || capped[0].RawPointsCP != 10 || capped[0].CapReason != talent.CapDaily {
		t.Fatalf("capped rows = %+v", capped)
	}
	if got := e.count(talent.SignalFeedLike, talent.OutcomeCapped); got != 1 {
		t.Fatalf("capped counted %d", got)
	}
	// The next Istanbul day starts again (22:30 UTC is already the next day
	// in Istanbul).
	next := time.Date(2026, 10, 20, 22, 30, 0, 0, time.UTC)
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, next)
	if got := e.total(user, talent.SignalFeedLike); got != 60 {
		t.Fatalf("next day = %d cp, want 60", got)
	}

	// login_active_day: 20 cp a day, 100 cp in 7 days → the sixth day is capped.
	for d := range 6 {
		day := civil.Today(t0).AddDays(d)
		e.send(t, domainevent.UserActiveDay{UserID: user, Day: day}, day.In(clock.Istanbul()).Add(10*time.Hour))
	}
	if got := e.total(user, talent.SignalActiveDay); got != 100 {
		t.Fatalf("active days = %d cp, want the 100 cp weekly cap", got)
	}
	if got := e.count(talent.SignalActiveDay, talent.OutcomeCapped); got != 1 {
		t.Fatalf("weekly capped counted %d", got)
	}
	// event_no_show: −200 cp, 400 cp a week in magnitude.
	for range 3 {
		e.send(t, domainevent.RegistrationNoShow{RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), UserID: user}, t0)
	}
	if got := e.total(user, talent.SignalEventNoShow); got != -400 {
		t.Fatalf("no-shows = %d cp, want −400 (weekly magnitude cap)", got)
	}
	// event_checkin: 500 cp, 1000 cp a day; a self check-in earns 0.
	other := e.student()
	e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), UserID: other, StaffUserID: other}, t0)
	for range 3 {
		e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: uuid.NewV7(), EventID: uuid.NewV7(), UserID: other, StaffUserID: uuid.NewV7()}, t0)
	}
	if got := e.total(other, talent.SignalEventCheckin); got != 1000 {
		t.Fatalf("check-ins = %d cp, want 1000 (self check-in 0, then the daily cap)", got)
	}
	var perSource int
	for _, p := range e.points(other) {
		if p.CapReason == talent.CapPerSource {
			perSource++
		}
	}
	if perSource != 1 {
		t.Fatalf("self check-in rows with per_source = %d, want 1", perSource)
	}
}

// TestIngest_AtMostTwoClubs: a third concurrent club is recorded with 0
// points (per_source); a profile section counts once.
func TestIngest_AtMostTwoClubs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	for range 3 {
		e.send(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: uuid.NewV7(), OrgKind: "club", UserID: user, Role: "member"}, t0)
	}
	if got := e.total(user, talent.SignalClubMembership); got != 400 {
		t.Fatalf("three clubs = %d cp, want 400", got)
	}
	for range 2 {
		e.send(t, domainevent.StudentProfileSectionCompleted{UserID: user, Section: "skills"}, t0)
	}
	if got := e.total(user, talent.SignalProfileSection); got != 200 {
		t.Fatalf("a section completed twice = %d cp, want 200", got)
	}
}

// TestIngest_NoConfigOrDisabledSignal: without a config version in force
// nothing is written; a disabled signal neither.
func TestIngest_NoConfigOrDisabledSignal(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	e.config(t, t0.Add(48*time.Hour)) // a future version is not in force
	e.store.SetSignalEnabled(talent.SignalFeedSave, false)
	e.send(t, domainevent.PostSaved{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	if got := e.count(talent.SignalFeedSave, talent.OutcomeSignalDisabled); got != 1 {
		t.Fatalf("disabled signal counted %d", got)
	}

	bare := buildEnv(t, true, false)
	u := bare.student()
	bare.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	if rows := bare.points(u); len(rows) != 0 || bare.count(talent.SignalFeedLike, talent.OutcomeNoConfig) != 1 {
		t.Fatalf("without a config version: rows %+v", rows)
	}
}

// TestIngest_MalformedEventsArePermanent: an undecodable payload or a
// missing day is not retried.
func TestIngest_MalformedEventsArePermanent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	ev := envelope(t, domainevent.UserActiveDay{UserID: e.student(), Day: civil.Today(t0)}, t0)
	ev.Payload = []byte(`{"user_id":"x"}`)
	if err := e.deliverErr(t, ev); err == nil || !isPermanent(err) {
		t.Fatalf("bad payload: %v", err)
	}
	ev = envelope(t, domainevent.StudentProfileSectionCompleted{UserID: e.student()}, t0)
	if err := e.deliverErr(t, ev); err == nil || !isPermanent(err) {
		t.Fatalf("section without a name: %v", err)
	}
}

// TestService_RefusesForeignTransaction: talent never joins another
// module's transaction (the role would leak into it).
func TestService_RefusesForeignTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	err := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := e.mod.Ranking.Bands(ctx, []uuid.UUID{uuid.NewV7()})
		return err
	})
	if err == nil {
		t.Fatal("Bands ran inside another transaction")
	}
	// The store refuses to work outside a talent transaction.
	if _, err := e.store.SignalDef(t.Context(), talent.SignalFeedLike); err == nil {
		t.Fatal("store read outside a talent transaction")
	}
	noEnter := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := e.store.SignalDef(ctx, talent.SignalFeedLike) // a transaction, but no Enter
		return err
	})
	if noEnter == nil {
		t.Fatal("store read without SET LOCAL ROLE app_talent")
	}
	e.inTalentTx(t, func(ctx context.Context) error {
		return e.store.AsApp(ctx, func(ctx context.Context) error {
			if _, err := e.store.SignalDef(ctx, talent.SignalFeedLike); err == nil {
				return errors.New("talent table read on the session role")
			}
			return nil
		})
	})
}
