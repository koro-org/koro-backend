package students_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
)

// participationsFake is events.Targeting as students sees it: the
// targetable participations per user and the targetable events (events
// applies the targetable rule; the fake answers what it was told).
type participationsFake struct {
	mu    sync.Mutex
	parts map[uuid.UUID][]students.EventParticipation
	off   map[uuid.UUID]bool // events no longer targetable
	asked [][]uuid.UUID      // the user IDs of each TargetableParticipations call
}

func newParticipations() *participationsFake {
	return &participationsFake{parts: map[uuid.UUID][]students.EventParticipation{}, off: map[uuid.UUID]bool{}}
}

func (f *participationsFake) TargetableParticipations(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]students.EventParticipation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(ids))
	out := map[uuid.UUID][]students.EventParticipation{}
	for _, id := range ids {
		for _, p := range f.parts[id] {
			if !f.off[p.EventID] {
				out[id] = append(out[id], p)
			}
		}
	}
	return out, nil
}

func (f *participationsFake) TargetableEvents(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		out[id] = !f.off[id]
	}
	return out, nil
}

// join records userID's participation in eventID.
func (f *participationsFake) join(userID, eventID uuid.UUID, attended bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parts[userID] = slices.DeleteFunc(f.parts[userID], func(p students.EventParticipation) bool { return p.EventID == eventID })
	f.parts[userID] = append(f.parts[userID], students.EventParticipation{EventID: eventID, Attended: attended})
}

// leave drops userID's participation in eventID.
func (f *participationsFake) leave(userID, eventID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parts[userID] = slices.DeleteFunc(f.parts[userID], func(p students.EventParticipation) bool { return p.EventID == eventID })
}

func (f *participationsFake) untarget(eventID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.off[eventID] = true
}

// withParticipations binds f as the events reader of the event keys.
func withParticipations(f *participationsFake) func(*students.Deps) {
	return func(d *students.Deps) {
		d.Participations = func() students.Participations { return f }
	}
}

// grantMarketing records a commercial consent of purpose for userID (the
// consent ledger the fake checker reads).
func (e *env) grantMarketing(t *testing.T, userID uuid.UUID, purpose consent.Purpose) {
	t.Helper()
	nv := uuid.NewV7()
	if _, err := e.consent.Record(t.Context(), consent.RecordInput{UserID: userID, Purpose: purpose, Action: consent.Granted,
		NoticeVersionID: &nv, TextSHA256: make([]byte, 32), Channel: consent.ChannelWeb}); err != nil {
		t.Fatal(err)
	}
}

// withdrawMarketing records the withdrawal of purpose and delivers
// consent.recorded to students.apply_marketing_consent.
func (e *env) withdrawMarketing(t *testing.T, userID uuid.UUID, purpose consent.Purpose) {
	t.Helper()
	id, err := e.consent.Record(t.Context(), consent.RecordInput{UserID: userID, Purpose: purpose, Action: consent.Withdrawn,
		Channel: consent.ChannelWeb})
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, students.ApplyMarketingConsentTaskName, domainevent.ConsentRecorded{ConsentID: id, UserID: userID,
		Purpose: string(purpose), Action: string(consent.Withdrawn)}, userID)
}

func eventKeysIn(keys []string) []string {
	var out []string
	for _, k := range keys {
		if strings.HasPrefix(k, "event:") || strings.HasPrefix(k, "attended:") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(keys ...string) []string {
	slices.Sort(keys)
	return keys
}

// TestEventKeys_OnlyConsentingStudents (BE-161, Q-39): the stored audience
// holds event:<id> for each targetable participation and attended:<id>
// for each checked-in one — only for a student whose commercial message
// consent (any channel) is in force. A student without it holds none and
// events is not even asked about them; withdrawing the last commercial
// consent removes every event key at once (consent.recorded), and the
// nightly refresh agrees with the event path.
func TestEventKeys_OnlyConsentingStudents(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	talk, fair := uuid.NewV7(), uuid.NewV7()
	ada, can, deniz := e.student(t, e.uniA, false), e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	e.grantMarketing(t, ada.UserID, consent.PurposeCommercialEmail)
	e.grantMarketing(t, deniz.UserID, consent.PurposeCommercialPush)
	for _, a := range []authz.Actor{ada, can, deniz} {
		parts.join(a.UserID, talk, false)
		parts.join(a.UserID, fair, true)
	}
	all := sorted(students.EventKey(talk), students.EventKey(fair), students.AttendedKey(fair))
	for _, a := range []authz.Actor{ada, can, deniz} {
		e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	}
	if got := eventKeysIn(e.profile(t, ada.UserID).AudienceKeys); !slices.Equal(got, all) {
		t.Fatalf("event keys of a consenting student = %v, want %v", got, all)
	}
	if got := eventKeysIn(e.profile(t, deniz.UserID).AudienceKeys); !slices.Equal(got, all) {
		t.Fatalf("event keys of a push-consenting student = %v, want %v", got, all)
	}
	if got := eventKeysIn(e.profile(t, can.UserID).AudienceKeys); len(got) != 0 {
		t.Fatalf("event keys without a commercial consent = %v", got)
	}
	for _, ids := range parts.asked {
		if slices.Contains(ids, can.UserID) {
			t.Fatal("events was asked about a student without a commercial consent")
		}
	}

	e.withdrawMarketing(t, ada.UserID, consent.PurposeCommercialEmail)
	if got := eventKeysIn(e.profile(t, ada.UserID).AudienceKeys); len(got) != 0 {
		t.Fatalf("event keys after the withdrawal = %v", got)
	}
	// Another purpose's record changes nothing; the nightly refresh agrees.
	e.deliver(t, students.ApplyMarketingConsentTaskName, domainevent.ConsentRecorded{ConsentID: uuid.NewV7(),
		UserID: deniz.UserID, Purpose: string(consent.PurposeDiscoveryProfile), Action: string(consent.Withdrawn)}, deniz.UserID)
	e.job(t, students.AudienceRefreshJobName)
	for u, want := range map[uuid.UUID][]string{ada.UserID: nil, can.UserID: nil, deniz.UserID: all} {
		if got := eventKeysIn(e.profile(t, u).AudienceKeys); !slices.Equal(got, want) {
			t.Fatalf("after the nightly refresh %s holds %v, want %v", u, got, want)
		}
	}
}

// TestEventKeys_FollowRegistrationsAndCheckins (BE-161): registration.
// confirmed and checkin.recorded add the keys, registration.cancelled and
// checkin.voided remove them; a sensitive event's message changes nothing
// (no key can come from it). Without the events reader nobody holds an
// event key.
func TestEventKeys_FollowRegistrationsAndCheckins(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	a := e.student(t, e.uniA, false)
	e.grantMarketing(t, a.UserID, consent.PurposeCommercialEmail)
	e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	ev, reg := uuid.NewV7(), uuid.NewV7()
	keys := func() []string { return eventKeysIn(e.profile(t, a.UserID).AudienceKeys) }

	parts.join(a.UserID, ev, false)
	e.deliver(t, students.AddEventKeyTaskName, domainevent.RegistrationConfirmed{RegistrationID: reg, EventID: ev,
		UserID: a.UserID}, reg)
	if got := keys(); !slices.Equal(got, []string{students.EventKey(ev)}) {
		t.Fatalf("after registration.confirmed: %v", got)
	}
	parts.join(a.UserID, ev, true)
	e.deliver(t, students.AddAttendedKeyTaskName, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), RegistrationID: reg,
		EventID: ev, UserID: a.UserID}, reg)
	if got, want := keys(), sorted(students.EventKey(ev), students.AttendedKey(ev)); !slices.Equal(got, want) {
		t.Fatalf("after checkin.recorded: %v, want %v", got, want)
	}
	parts.join(a.UserID, ev, false)
	e.deliver(t, students.RemoveAttendedKeyTaskName, domainevent.CheckinVoided{CheckinID: uuid.NewV7(), RegistrationID: reg,
		UserID: a.UserID}, reg)
	if got := keys(); !slices.Equal(got, []string{students.EventKey(ev)}) {
		t.Fatalf("after checkin.voided: %v", got)
	}
	parts.leave(a.UserID, ev)
	e.deliver(t, students.RemoveEventKeyTaskName, domainevent.RegistrationCancelled{RegistrationID: reg, EventID: ev,
		UserID: a.UserID, By: "user"}, reg)
	if got := keys(); len(got) != 0 {
		t.Fatalf("after registration.cancelled: %v", got)
	}
	// A sensitive event's confirmation is not even a refresh.
	parts.join(a.UserID, ev, false)
	e.deliver(t, students.AddEventKeyTaskName, domainevent.RegistrationConfirmed{RegistrationID: reg, EventID: ev,
		UserID: a.UserID, Sensitive: true}, reg)
	if got := keys(); len(got) != 0 {
		t.Fatalf("a sensitive event's confirmation added %v", got)
	}

	// Without the events reader: no event key, whatever events would say.
	bare := newEnv(t)
	b := bare.student(t, bare.uniA, false)
	bare.grantMarketing(t, b.UserID, consent.PurposeCommercialEmail)
	if p := bare.basics(t, b, bare.deptA, 2, t0.AddDate(2, 0, 0)); len(eventKeysIn(p.AudienceKeys)) != 0 {
		t.Fatalf("event keys without the events reader: %v", p.AudienceKeys)
	}
}

// eventCohort creates n consenting students of deptA in year who
// registered for (attended) ev, their keys written by the service.
func (e *env) eventCohort(t *testing.T, parts *participationsFake, n int, ev uuid.UUID, year int16, attended bool) []uuid.UUID {
	t.Helper()
	out := make([]uuid.UUID, n)
	for i := range out {
		a := e.student(t, e.uniA, false)
		e.grantMarketing(t, a.UserID, consent.PurposeCommercialEmail)
		parts.join(a.UserID, ev, attended)
		e.basics(t, a, e.deptA, year, t0.AddDate(2, 0, 0))
		out[i] = a.UserID
	}
	return out
}

// TestAudienceCount_EventDimension_KAnonymity (BE-161, 04 §V.2): the event
// is a dimension of count_segment like any other. A single event narrowed
// by department and class year to 9 students answers ErrAudienceTooSmall
// without a number, and the recipient walk never starts; the event alone
// (21 students) and its attendees (12) are counted. A non-consenting
// participant is never counted.
func TestAudienceCount_EventDimension_KAnonymity(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	ctx := t.Context()
	ev := uuid.NewV7()
	e.eventCohort(t, parts, 9, ev, 3, false)
	e.eventCohort(t, parts, 12, ev, 2, true)
	silent := e.student(t, e.uniA, false) // registered, no commercial consent
	parts.join(silent.UserID, ev, true)
	e.basics(t, silent, e.deptA, 3, t0.AddDate(2, 0, 0))

	for _, c := range []struct {
		keys []string
		want int // -1: below 10, no number
	}{
		{[]string{students.EventKey(ev)}, 21},
		{[]string{students.AttendedKey(ev)}, 12},
		{[]string{students.EventKey(ev), "dept:" + e.deptA.String(), yearKey(3)}, -1}, // 9 (+1 without consent)
		{[]string{students.AttendedKey(ev), yearKey(3)}, -1},                          // 0 (+1 without consent)
		{[]string{students.EventKey(ev), "dept:" + e.deptA.String(), yearKey(2)}, 12},
	} {
		spec := students.AudienceSpec{Keys: c.keys}
		n, err := e.svc.Count(ctx, spec)
		switch {
		case c.want < 0 && (!errors.Is(err, students.ErrAudienceTooSmall) || n != 0):
			t.Errorf("Count(%v) = %d, %v; want 0, ErrAudienceTooSmall", c.keys, n, err)
		case c.want >= 0 && (err != nil || n != c.want):
			t.Errorf("Count(%v) = %d, %v; want %d", c.keys, n, err, c.want)
		}
		if c.want < 0 {
			called := 0
			if err := e.svc.Stream(ctx, spec, func(uuid.UUID) error { called++; return nil }); !errors.Is(err,
				students.ErrAudienceTooSmall) || called != 0 {
				t.Errorf("Stream(%v) = %v after %d calls", c.keys, err, called)
			}
		}
	}
}

// TestAudienceCount_UntargetableEventMatchesNobody (BE-161): a spec naming
// an event that is no longer targetable (members-only, sensitive,
// cancelled, too old) matches nobody through it, whatever stale key the
// profiles still hold; without the events reader no event matches.
func TestAudienceCount_UntargetableEventMatchesNobody(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	ctx := t.Context()
	ev, other := uuid.NewV7(), uuid.NewV7()
	e.eventCohort(t, parts, 12, ev, 2, true)
	e.eventCohort(t, parts, 12, other, 2, false)
	if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.EventKey(ev)}}); err != nil || n != 12 {
		t.Fatalf("setup: Count = %d, %v", n, err)
	}
	parts.untarget(ev)
	for _, keys := range [][]string{
		{students.EventKey(ev)}, {students.AttendedKey(ev)}, {students.EventKey(ev), uniKey(e.uniA)},
	} {
		if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrAudienceTooSmall) || n != 0 {
			t.Errorf("Count(%v) of an untargetable event = %d, %v", keys, n, err)
		}
		if err := e.svc.Stream(ctx, students.AudienceSpec{Keys: keys}, func(uuid.UUID) error {
			t.Fatal("Stream reached a student through an untargetable event")
			return nil
		}); !errors.Is(err, students.ErrAudienceTooSmall) {
			t.Errorf("Stream(%v) = %v", keys, err)
		}
	}
	// The other event of the same spec still counts (OR inside the dimension).
	if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.EventKey(ev), students.EventKey(other)}}); err != nil || n != 12 {
		t.Fatalf("Count(untargetable OR targetable) = %d, %v; want 12", n, err)
	}

	// Profiles holding an event key, no events reader: the key matches
	// nobody (fail closed).
	bare := newEnv(t)
	for range 12 {
		a := bare.student(t, bare.uniA, false)
		p := bare.basics(t, a, bare.deptA, 2, t0.AddDate(2, 0, 0))
		p.AudienceKeys = append(p.AudienceKeys, students.EventKey(other))
		if _, err := bare.store.UpdateProfile(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := bare.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.EventKey(other)}}); !errors.Is(err,
		students.ErrAudienceTooSmall) || n != 0 {
		t.Fatalf("Count without the events reader = %d, %v; want ErrAudienceTooSmall", n, err)
	}
}

// TestEventUpdated_DropsKeysOfUntargetableEvent (BE-161): when an edit of
// the visibility, the sensitive category or the start leaves an event
// untargetable, its event and attended keys leave every profile at once
// (not at the nightly refresh); an edit of another field, or one that
// keeps the event targetable, changes nothing. Idempotent.
func TestEventUpdated_DropsKeysOfUntargetableEvent(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	ev, kept := uuid.NewV7(), uuid.NewV7()
	holders := e.eventCohort(t, parts, 3, ev, 2, true)
	for _, u := range holders {
		parts.join(u, kept, false)
	}
	e.job(t, students.AudienceRefreshJobName)
	update := func(fields ...string) {
		t.Helper()
		e.deliver(t, students.DropEventKeysTaskName, domainevent.EventUpdated{EventID: ev, OrgID: uuid.NewV7(), Fields: fields}, ev)
	}
	update("visibility") // still targetable: nothing changes
	for _, u := range holders {
		if got, want := eventKeysIn(e.profile(t, u).AudienceKeys), sorted(students.EventKey(ev), students.AttendedKey(ev),
			students.EventKey(kept)); !slices.Equal(got, want) {
			t.Fatalf("keys of %s = %v, want %v", u, got, want)
		}
	}
	parts.untarget(ev)
	update("title") // not a targeting field: the nightly refresh will do
	if !slices.Contains(e.profile(t, holders[0]).AudienceKeys, students.EventKey(ev)) {
		t.Fatal("a title edit removed the keys")
	}
	for range 2 {
		update("visibility", "title")
	}
	for _, u := range holders {
		if got := eventKeysIn(e.profile(t, u).AudienceKeys); !slices.Equal(got, []string{students.EventKey(kept)}) {
			t.Fatalf("keys of %s after the event left the targeting = %v", u, got)
		}
	}
}

// TestViewerKeys_IncludeStoredEventKeys (BE-161): the feed's viewer keys
// carry the stored event keys, so a post targeting an event's attendees
// reaches the students its count counted (and no one else).
func TestViewerKeys_IncludeStoredEventKeys(t *testing.T) {
	t.Parallel()
	parts := newParticipations()
	e := newEnv(t, withParticipations(parts))
	ev := uuid.NewV7()
	u := e.eventCohort(t, parts, 1, ev, 2, true)[0]
	vk, err := e.svc.ViewerKeys(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := eventKeysIn(vk.Keys), sorted(students.EventKey(ev), students.AttendedKey(ev)); !slices.Equal(got, want) {
		t.Fatalf("viewer event keys = %v, want %v", got, want)
	}
	silent := e.student(t, e.uniA, false)
	parts.join(silent.UserID, ev, true)
	e.basics(t, silent, e.deptA, 2, t0.AddDate(2, 0, 0))
	if vk, err := e.svc.ViewerKeys(t.Context(), silent.UserID); err != nil || len(eventKeysIn(vk.Keys)) != 0 {
		t.Fatalf("viewer keys without a commercial consent = %v, %v", vk.Keys, err)
	}
}

// TestAudienceSpec_EventKeys (BE-161): event and attended keys are
// dimensions of their own and must name a UUID.
func TestAudienceSpec_EventKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, keys := range [][]string{{"event:"}, {"event:x"}, {"attended:" + uuid.Nil().String()}} {
		if _, err := e.svc.Count(t.Context(), students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrInvalidAudience) {
			t.Errorf("Count(%q) = %v, want ErrInvalidAudience", keys, err)
		}
	}
	for _, keys := range [][]string{{students.EventKey(uuid.NewV7())}, {students.AttendedKey(uuid.NewV7()), uniKey(e.uniA)}} {
		if _, err := e.svc.Count(t.Context(), students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrAudienceTooSmall) {
			t.Errorf("Count(%q) = %v, want ErrAudienceTooSmall", keys, err)
		}
	}
}
