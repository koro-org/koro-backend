package students_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// index is the bulk level 2 contract of the students service (BE-051).
func (e *env) index(t *testing.T) students.DiscoveryIndex {
	t.Helper()
	idx, ok := e.svc.(students.DiscoveryIndex)
	if !ok {
		t.Fatal("the students service does not implement DiscoveryIndex")
	}
	return idx
}

func matchIDs(set students.DiscoveryMatchSet) []uuid.UUID {
	out := make([]uuid.UUID, len(set.Matches))
	for i, m := range set.Matches {
		out[i] = m.UserID
	}
	return out
}

func sortedIDs(as []authz.Actor) []uuid.UUID {
	out := make([]uuid.UUID, len(as))
	for i, a := range as {
		out[i] = a.UserID
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

// TestDiscoveryMatches_ThresholdChecksAndHits (BE-051; T-68, T-237): the
// match set holds exactly the students Search finds (every read-time
// check: a lapsed verification does not count towards 10), in user ID
// order; fewer than 10 in total is an empty set with BelowThreshold. Each
// match carries how many of the requested skills and languages the
// student holds (repeated values count once); a single requested value is
// held by every match. The set never marshals; invalid filters are 422.
func TestDiscoveryMatches_ThresholdChecksAndHits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	idx := e.index(t)
	var as []authz.Actor
	for range 9 {
		as = append(as, e.discoverable(t, e.uniA, e.deptA))
	}
	ghost := e.discoverable(t, e.uniA, e.deptA)
	e.users.lapse(ghost.UserID)
	e.discoverable(t, e.uniB, e.deptB)
	q := students.DiscoveryQuery{UniversityIDs: []uuid.UUID{e.uniA}}
	set, err := idx.DiscoveryMatches(ctx, q, 0)
	if err != nil || !set.BelowThreshold || len(set.Matches) != 0 || set.Truncated {
		t.Fatalf("9 matching = %+v, %v", set, err)
	}
	if _, err := json.Marshal(set); err == nil {
		t.Fatal("a match set marshalled")
	}

	both := e.discoverable(t, e.uniA, e.deptA)
	if _, err := e.svc.PutMySkills(ctx, both, []students.SkillInput{{SkillID: e.skill1}, {SkillID: e.skill2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMyLanguages(ctx, both, []students.LanguageInput{{LanguageCode: "en", Level: "B2"}, {LanguageCode: "de", Level: "A2"}}); err != nil {
		t.Fatal(err)
	}
	as = append(as, both)
	ids := sortedIDs(as)
	set, err = idx.DiscoveryMatches(ctx, q, 0)
	if err != nil || set.BelowThreshold || set.Truncated || !slices.Equal(matchIDs(set), ids) {
		t.Fatalf("10 matching = %v, below %v, %v", matchIDs(set), set.BelowThreshold, err)
	}
	for _, m := range set.Matches {
		if m.SkillHits != 0 || m.LanguageHits != 0 {
			t.Fatalf("hits without a skill or language filter: %+v", m)
		}
	}
	page, err := e.svc.Search(ctx, students.DiscoveryQuery{UniversityIDs: []uuid.UUID{e.uniA}, Limit: 50})
	if err != nil || !slices.Equal(searchIDs(t, page), ids) {
		t.Fatal("DiscoveryMatches and Search disagree")
	}

	// Only "both" holds a skill: the skill filter matches 1 → below 10.
	set, err = idx.DiscoveryMatches(ctx, students.DiscoveryQuery{SkillIDs: []uuid.UUID{e.skill1, e.skill2}}, 0)
	if err != nil || !set.BelowThreshold {
		t.Fatalf("one skilled student = %+v, %v", set, err)
	}
	for _, a := range as[:9] {
		if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PutMyLanguages(ctx, a, []students.LanguageInput{{LanguageCode: "de", Level: "B1"}}); err != nil {
			t.Fatal(err)
		}
	}
	set, err = idx.DiscoveryMatches(ctx, students.DiscoveryQuery{SkillIDs: []uuid.UUID{e.skill2, e.skill1, e.skill1},
		LanguageCodes: []string{"en", "de", "de"}}, 0)
	if err != nil || set.BelowThreshold || len(set.Matches) != 10 {
		t.Fatalf("skilled set = %d, below %v, %v", len(set.Matches), set.BelowThreshold, err)
	}
	for _, m := range set.Matches {
		wantS, wantL := 1, 1
		if m.UserID == both.UserID {
			wantS, wantL = 2, 2
		}
		if m.SkillHits != wantS || m.LanguageHits != wantL {
			t.Fatalf("hits = %d/%d, want %d/%d", m.SkillHits, m.LanguageHits, wantS, wantL)
		}
	}
	set, err = idx.DiscoveryMatches(ctx, students.DiscoveryQuery{SkillIDs: []uuid.UUID{e.skill1}, LanguageCodes: []string{"de"}}, 0)
	if err != nil || set.BelowThreshold || len(set.Matches) != 10 {
		t.Fatalf("single values = %d, %v", len(set.Matches), err)
	}
	for _, m := range set.Matches {
		if m.SkillHits != 1 || m.LanguageHits != 1 {
			t.Fatalf("a single requested value: %+v", m)
		}
	}
	if _, err := idx.DiscoveryMatches(ctx, students.DiscoveryQuery{ClassYears: []int16{9}}, 0); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("invalid filter = %v, want 422", err)
	}
}

// TestDiscoveryMatches_TruncatedAtMost: the set stops at most (never below
// 10) and says so; the walk covers the batches.
func TestDiscoveryMatches_TruncatedAtMost(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	var as []authz.Actor
	for range 13 {
		as = append(as, e.discoverable(t, e.uniA, e.deptA))
	}
	ids := sortedIDs(as)
	for _, tc := range []struct {
		most, want int
		truncated  bool
	}{{11, 11, true}, {3, 10, true}, {13, 13, false}, {0, 13, false}, {students.MaxDiscoveryMatches + 1, 13, false}} {
		set, err := e.index(t).DiscoveryMatches(ctx, students.DiscoveryQuery{}, tc.most)
		if err != nil || len(set.Matches) != tc.want || set.Truncated != tc.truncated || !slices.Equal(matchIDs(set), ids[:tc.want]) {
			t.Fatalf("most %d = %d matches, truncated %v, %v", tc.most, len(set.Matches), set.Truncated, err)
		}
	}
}

// TestAnonProfiles_OnlyDiscoverableNow (BE-051): the level 2 projections
// of a page exist only for students who pass every IsDiscoverable check at
// read time — a minor, a lapsed verification, a suspended account, the
// consent withdrawn elsewhere, the setting off, a graduated profile and an
// unknown user are absent — and they are the projections of Search.
// DiscoveryCandidatesAfter walks the level-2-on profiles without the
// read-time checks (the lapsed student is a candidate), in user ID order.
func TestAnonProfiles_OnlyDiscoverableNow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	idx := e.index(t)
	var as []authz.Actor
	for range 10 {
		as = append(as, e.discoverable(t, e.uniA, e.deptA))
	}
	if _, err := e.svc.PutMySkills(ctx, as[0], []students.SkillInput{{SkillID: e.skill1, Level: ptr[int16](4)}}); err != nil {
		t.Fatal(err)
	}
	ids := sortedIDs(as)
	got, err := idx.AnonProfiles(ctx, append(ids, ids[0], uuid.NewV7()))
	if err != nil || len(got) != 10 {
		t.Fatalf("10 discoverable = %d, %v", len(got), err)
	}
	page, err := e.svc.Search(ctx, students.DiscoveryQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Items {
		want, _ := p.WithAnonID("x")
		have, err := got[p.UserID].WithAnonID("x")
		if err != nil || !jsonEqual(t, want, have) {
			t.Fatalf("AnonProfiles and Search disagree for %v", p.UserID)
		}
	}

	e.users.setMinor(as[1].UserID, true)
	e.users.lapse(as[2].UserID)
	e.setStatus(as[3].UserID, identity.StatusSuspended)
	e.consent.mu.Lock()
	e.consent.removed[as[4].UserID] = true
	e.consent.mu.Unlock()
	if _, err := e.svc.UpdateMyPrivacy(ctx, as[5], students.PrivacyPatch{Discoverable: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	p6 := e.profile(t, as[6].UserID)
	now := t0
	p6.Status, p6.GraduatedAt = students.ProfileGraduated, &now
	if _, err := e.store.UpdateProfile(ctx, p6); err != nil {
		t.Fatal(err)
	}
	got, err = idx.AnonProfiles(ctx, ids)
	if err != nil || len(got) != 4 {
		t.Fatalf("after the changes = %d, %v; want 4", len(got), err)
	}
	for _, a := range as[1:7] {
		if _, ok := got[a.UserID]; ok {
			t.Fatalf("%v is still projected", a.UserID)
		}
	}
	if _, err := idx.AnonProfiles(ctx, make([]uuid.UUID, 101)); err != nil {
		t.Fatalf("101 equal IDs are one: %v", err)
	}
	many := make([]uuid.UUID, 101)
	for i := range many {
		many[i] = uuid.NewV7()
	}
	if _, err := idx.AnonProfiles(ctx, many); err == nil {
		t.Fatal("101 users in one call were accepted")
	}

	// Candidates: level 2 on and an active profile, no read-time check (the
	// minor, the lapsed, the suspended, the withdrawn are still there).
	var walked []uuid.UUID
	for after := uuid.Nil(); ; {
		batch, err := idx.DiscoveryCandidatesAfter(ctx, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, batch...)
		if len(batch) < 3 {
			break
		}
		after = batch[len(batch)-1]
	}
	want := slices.DeleteFunc(slices.Clone(ids), func(id uuid.UUID) bool { return id == as[5].UserID || id == as[6].UserID })
	if !slices.Equal(walked, want) {
		t.Fatalf("candidates = %d, want %d", len(walked), len(want))
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}
