package students_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/students"
)

// cohort creates n verified students at uni with the class year year
// (their profiles and audience keys written by the service, as in
// production) and returns their actors.
func (e *env) cohort(t *testing.T, n int, uni uuid.UUID, year int16, minor bool) []authz.Actor {
	t.Helper()
	out := make([]authz.Actor, n)
	for i := range out {
		a := e.student(t, uni, minor)
		y := year
		if _, err := e.svc.UpdateMyProfile(t.Context(), a, students.ProfilePatch{ClassYear: &y}); err != nil {
			t.Fatal(err)
		}
		out[i] = a
	}
	return out
}

func ids(as []authz.Actor) []uuid.UUID {
	out := make([]uuid.UUID, len(as))
	for i, a := range as {
		out[i] = a.UserID
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

func uniKey(id uuid.UUID) string { return "uni:" + id.String() }
func yearKey(y int) string       { return "year:" + strconv.Itoa(y) }

// TestAudienceCount_Below10_ReturnsTooSmallWithoutNumber (04 §V.2,
// count_segment): 9 matching students answer ErrAudienceTooSmall and the
// number 0, never 9; the 10th makes the real number visible. Stream
// refuses the same small audience before calling fn once.
func TestAudienceCount_Below10_ReturnsTooSmallWithoutNumber(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.cohort(t, 9, e.uniA, 2, false)
	spec := students.AudienceSpec{Keys: []string{uniKey(e.uniA)}}
	n, err := e.svc.Count(ctx, spec)
	if !errors.Is(err, students.ErrAudienceTooSmall) || n != 0 {
		t.Fatalf("Count(9) = %d, %v; want 0, ErrAudienceTooSmall", n, err)
	}
	called := 0
	err = e.svc.Stream(ctx, spec, func(uuid.UUID) error { called++; return nil })
	if !errors.Is(err, students.ErrAudienceTooSmall) || called != 0 {
		t.Fatalf("Stream(9) = %v after %d calls", err, called)
	}
	e.cohort(t, 1, e.uniA, 3, false)
	if n, err := e.svc.Count(ctx, spec); err != nil || n != 10 {
		t.Fatalf("Count(10) = %d, %v", n, err)
	}
	// The DTO of level 0 follows the same rule.
	if d := students.NewAudienceCount(9); d.Count != nil || !d.BelowThreshold {
		t.Fatalf("level 0 DTO of 9 = %+v", d)
	}
}

// TestAudienceCount_ExcludesMinorsAndIneligible (02 §9.1, 03 Y4, T-135):
// minors, students whose verification lapsed and suspended accounts are
// never counted, whatever their keys; ten adults are.
func TestAudienceCount_ExcludesMinorsAndIneligible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	adults := e.cohort(t, 10, e.uniA, 2, false)
	e.cohort(t, 5, e.uniA, 2, true) // minors: same keys, never eligible
	lapsed := e.cohort(t, 2, e.uniA, 2, false)
	for _, a := range lapsed {
		e.users.lapse(a.UserID)
	}
	suspended := e.cohort(t, 1, e.uniA, 2, false)[0]
	e.users.mu.Lock()
	s := e.users.sum[suspended.UserID]
	s.Status = identity.StatusSuspended
	e.users.sum[suspended.UserID] = s
	e.users.mu.Unlock()
	e.job(t, students.AudienceRefreshJobName)
	for _, spec := range [][]string{{uniKey(e.uniA)}, {"all"}, {uniKey(e.uniA), yearKey(2)}} {
		if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: spec}); err != nil || n != len(adults) {
			t.Fatalf("Count(%v) = %d, %v; want %d adults", spec, n, err, len(adults))
		}
	}
	// One adult fewer and the audience is below the threshold again.
	e.users.lapse(adults[0].UserID)
	e.job(t, students.AudienceRefreshJobName)
	if _, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{uniKey(e.uniA)}}); !errors.Is(err, students.ErrAudienceTooSmall) {
		t.Fatalf("Count(9 adults) = %v", err)
	}
}

// TestAudienceStream_OnlyEligible (01 §4.9): Stream walks exactly the
// eligible adults Count counts, in user_id order, never a minor or an
// ineligible profile; a stale eligibility flag of a minor (birth date
// corrected, refresh not run yet) is filtered by the account check; an
// error of fn stops the walk.
func TestAudienceStream_OnlyEligible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	adults := e.cohort(t, 11, e.uniA, 2, false)
	e.cohort(t, 3, e.uniA, 2, true)
	other := e.cohort(t, 4, e.uniB, 2, false) // another university: not targeted
	lapsed := adults[10]
	e.users.lapse(lapsed.UserID)
	e.job(t, students.AudienceRefreshJobName)
	stale := adults[9] // eligible in the store, a minor in identity
	e.users.setMinor(stale.UserID, true)

	spec := students.AudienceSpec{Keys: []string{uniKey(e.uniA), yearKey(2)}}
	var got []uuid.UUID
	if err := e.svc.Stream(ctx, spec, func(id uuid.UUID) error { got = append(got, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := ids(adults[:9]); !slices.Equal(got, want) {
		t.Fatalf("streamed %d users %v\n want %d %v", len(got), got, len(want), want)
	}
	for _, a := range other {
		if slices.Contains(got, a.UserID) {
			t.Fatal("a student of another university was streamed")
		}
	}
	boom := errors.New("boom")
	calls := 0
	if err := e.svc.Stream(ctx, spec, func(uuid.UUID) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("fn error: %v after %d calls", err, calls)
	}
}

// TestAudienceCount_DimensionsFromKeys (02 §9.1, T-132): OR inside a
// dimension, AND across dimensions, the dimension count derived from the
// keys ('all' narrows nothing next to another dimension).
func TestAudienceCount_DimensionsFromKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.cohort(t, 6, e.uniA, 2, false)
	e.cohort(t, 6, e.uniB, 2, false)
	e.cohort(t, 5, e.uniA, 3, false)
	for _, c := range []struct {
		keys []string
		want int // 0: too small
	}{
		{[]string{uniKey(e.uniA), uniKey(e.uniB)}, 17},             // OR inside uni
		{[]string{uniKey(e.uniA), uniKey(e.uniB), yearKey(2)}, 12}, // (A ∨ B) ∧ year 2
		{[]string{uniKey(e.uniA), yearKey(2), yearKey(3)}, 11},     // A ∧ (2 ∨ 3)
		{[]string{uniKey(e.uniA), yearKey(2)}, 0},                  // 6: below 10
		{[]string{"all", uniKey(e.uniA)}, 11},                      // 'all' dropped, not a fourth dimension
		{[]string{"all"}, 17},
	} {
		n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: c.keys})
		switch {
		case c.want == 0 && !errors.Is(err, students.ErrAudienceTooSmall):
			t.Errorf("Count(%v) = %d, %v; want too small", c.keys, n, err)
		case c.want > 0 && (err != nil || n != c.want):
			t.Errorf("Count(%v) = %d, %v; want %d", c.keys, n, err, c.want)
		}
	}
}

// TestAudienceCount_SensitiveClubNeverTargets (03 Y2, T-132): a sensitive
// club key is removed from the spec before counting, and a club dimension
// left with no key matches nobody (removing a key never widens the
// audience); a plain club targets its members.
func TestAudienceCount_SensitiveClubNeverTargets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	for _, a := range e.cohort(t, 12, e.uniA, 2, false) {
		e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
		e.orgs.join(a.UserID, e.clubSens, authz.MemberMember)
	}
	e.job(t, students.AudienceRefreshJobName)
	if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.ClubKey(e.clubPlain)}}); err != nil || n != 12 {
		t.Fatalf("plain club = %d, %v", n, err)
	}
	// A stale stored key of the sensitive club is not enough either.
	for _, a := range e.cohort(t, 12, e.uniB, 2, false) {
		p := e.profile(t, a.UserID)
		p.AudienceKeys = append(p.AudienceKeys, students.ClubKey(e.clubSens))
		if _, err := e.store.UpdateProfile(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, keys := range [][]string{
		{students.ClubKey(e.clubSens)},
		{uniKey(e.uniB), students.ClubKey(e.clubSens)},
	} {
		if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrAudienceTooSmall) {
			t.Fatalf("Count(%v) = %d, %v; want too small", keys, n, err)
		}
	}
	if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.ClubKey(e.clubPlain), students.ClubKey(e.clubSens)}}); err != nil || n != 12 {
		t.Fatalf("plain ∨ sensitive = %d, %v; want the plain club's 12", n, err)
	}
}

// TestAudienceSpec_Invalid: no key, a malformed key, an unknown dimension
// or a year out of range is 422 validation_failed (never a count).
func TestAudienceSpec_Invalid(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, keys := range [][]string{
		nil, {}, {"uni:"}, {"uni:not-a-uuid"}, {"year:9"}, {"year:02"}, {"talent:high"}, {"club:" + uuid.Nil().String()},
		{"uni:" + "ABCDEF00-0000-7000-8000-000000000000"},
	} {
		_, err := e.svc.Count(t.Context(), students.AudienceSpec{Keys: keys})
		if !errors.Is(err, students.ErrInvalidAudience) {
			t.Errorf("Count(%q) = %v, want ErrInvalidAudience", keys, err)
		}
	}
}
