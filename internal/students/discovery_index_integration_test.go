//go:build integration

package students_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/students"
)

// TestDiscoveryIndex_Postgres (BE-051): the bulk level 2 reads of
// discovery over PostgreSQL — the match set with the per-skill and
// per-language walks of the GORM candidate query, the level 2 projections
// of a page and the candidate walk of the pseudonym resolution.
func TestDiscoveryIndex_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	idx := e.index(t)
	w := pgWorld(e.d)
	uni := w.university(t)
	s1, s2 := w.skill(t), w.skill(t)
	e.catalog.mu.Lock()
	e.catalog.skills[s1], e.catalog.skills[s2] = true, true
	e.catalog.mu.Unlock()
	var as []authz.Actor
	for i := range 11 {
		a := e.pgStudentAt(t, uni, false)
		if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{ClassYear: ptr[int16](2)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true), Channel: consent.ChannelWeb,
			DiscoveryConsent: &students.ConsentProof{NoticeVersionID: e.notice.ID, TextSHA256: e.notice.BodySHA256}}); err != nil {
			t.Fatal(err)
		}
		skills := []students.SkillInput{{SkillID: s1}}
		langs := []students.LanguageInput{{LanguageCode: "de", Level: "B1"}}
		if i < 3 {
			skills = append(skills, students.SkillInput{SkillID: s2})
			langs = append(langs, students.LanguageInput{LanguageCode: "en", Level: "C1"})
		}
		if _, err := e.svc.PutMySkills(ctx, a, skills); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PutMyLanguages(ctx, a, langs); err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	set, err := idx.DiscoveryMatches(ctx, students.DiscoveryQuery{UniversityIDs: []uuid.UUID{uni},
		SkillIDs: []uuid.UUID{s1, s2}, LanguageCodes: []string{"en", "de"}}, 0)
	if err != nil || set.BelowThreshold || !slices.Equal(matchIDs(set), sortedIDs(as)) {
		t.Fatalf("matches = %d, below %v, %v", len(set.Matches), set.BelowThreshold, err)
	}
	rich := map[uuid.UUID]bool{as[0].UserID: true, as[1].UserID: true, as[2].UserID: true}
	for _, m := range set.Matches {
		want := 1
		if rich[m.UserID] {
			want = 2
		}
		if m.SkillHits != want || m.LanguageHits != want {
			t.Fatalf("hits %d/%d, want %d", m.SkillHits, m.LanguageHits, want)
		}
	}
	if set, err := idx.DiscoveryMatches(ctx, students.DiscoveryQuery{UniversityIDs: []uuid.UUID{uni}, SkillIDs: []uuid.UUID{s2}}, 0); err != nil ||
		!set.BelowThreshold {
		t.Fatalf("3 holders of s2 = %+v, %v", set, err)
	}

	got, err := idx.AnonProfiles(ctx, []uuid.UUID{as[0].UserID, as[5].UserID})
	if err != nil || len(got) != 2 {
		t.Fatalf("profiles = %d, %v", len(got), err)
	}
	d, err := got[as[0].UserID].WithAnonID("anon")
	if err != nil || len(d.Skills) != 2 || len(d.Languages) != 2 || d.University.UniversityID != uni.String() {
		t.Fatalf("level 2 = %+v, %v", d, err)
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, as[5], students.PrivacyPatch{Discoverable: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if got, err := idx.AnonProfiles(ctx, []uuid.UUID{as[5].UserID}); err != nil || len(got) != 0 {
		t.Fatalf("level 2 off = %d, %v", len(got), err)
	}
	var walked []uuid.UUID
	for after := uuid.Nil(); ; {
		batch, err := idx.DiscoveryCandidatesAfter(ctx, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, batch...)
		if len(batch) < 4 {
			break
		}
		after = batch[len(batch)-1]
	}
	for _, a := range as {
		if slices.Contains(walked, a.UserID) == (a.UserID == as[5].UserID) {
			t.Fatalf("candidate walk: %v (level 2 off: %v)", a.UserID, a.UserID == as[5].UserID)
		}
	}
}
