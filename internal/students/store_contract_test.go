package students_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/problem"
	"koro/internal/students"
)

// world creates the parent rows a store needs (users, universities,
// departments, skills, files: FKs in PostgreSQL; plain IDs in memory).
type world struct {
	user       func(t *testing.T) uuid.UUID
	university func(t *testing.T) uuid.UUID
	department func(t *testing.T, uni uuid.UUID) uuid.UUID
	skill      func(t *testing.T) uuid.UUID
	file       func(t *testing.T) uuid.UUID
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{user: id, university: id, department: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		skill: id, file: id}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

func isInvalid(err error) bool {
	var pe *problem.Error
	return errors.As(err, &pe) && pe.Status() == 422
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (students.Store, world)) {
	t.Helper()
	newProfile := func(t *testing.T, s students.Store, w world, uni uuid.UUID) students.Profile {
		t.Helper()
		p := students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}
		got, ok, err := s.InsertProfile(t.Context(), p)
		if err != nil || !ok {
			t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
		}
		return got
	}

	t.Run("profile: insert, conflict, update, audience, checks", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni := w.university(t)
		p := newProfile(t, s, w, uni)
		if p.CreatedAt.IsZero() || len(p.CompletedSections) != 0 || len(p.AudienceKeys) != 0 || p.AudienceEligible {
			t.Fatalf("inserted = %+v", p)
		}
		if _, ok, err := s.InsertProfile(ctx, p); err != nil || ok {
			t.Fatalf("second insert: ok=%v err=%v", ok, err)
		}
		dept := w.department(t, uni)
		year := int16(2)
		grad := ym(2027, 6)
		p.DepartmentID, p.ClassYear, p.ExpectedGraduation, p.Headline = &dept, &year, &grad, ptr("Başlık")
		p.CompletedSections, p.AudienceKeys, p.AudienceEligible = []string{"basics"}, []string{"all", "uni:x"}, true
		got, err := s.UpdateProfile(ctx, p)
		if err != nil || *got.DepartmentID != dept || *got.ClassYear != 2 || *got.ExpectedGraduation != grad ||
			!slices.Equal(got.AudienceKeys, []string{"all", "uni:x"}) || !got.AudienceEligible || *got.Headline != "Başlık" {
			t.Fatalf("updated = %+v, %v", got, err)
		}
		locked, err := s.LockProfile(ctx, p.UserID)
		if err != nil || locked.UserID != p.UserID {
			t.Fatalf("LockProfile = %v", err)
		}
		at := t0
		if err := s.SetAudience(ctx, p.UserID, []string{"all"}, false, at); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetProfile(ctx, p.UserID); !slices.Equal(got.AudienceKeys, []string{"all"}) || got.AudienceEligible ||
			got.AudienceRefreshedAt == nil {
			t.Fatalf("after SetAudience = %+v", got)
		}
		for name, bad := range map[string]func(students.Profile) students.Profile{
			"class year":  func(p students.Profile) students.Profile { y := int16(9); p.ClassYear = &y; return p },
			"graduated":   func(p students.Profile) students.Profile { p.Status = students.ProfileGraduated; return p },
			"section":     func(p students.Profile) students.Profile { p.CompletedSections = []string{"talent"}; return p },
			"not 1st day": func(p students.Profile) students.Profile { d := grad; d.Day = 2; p.ExpectedGraduation = &d; return p },
		} {
			if _, err := s.UpdateProfile(ctx, bad(got)); !isInvalid(err) {
				t.Errorf("%s: %v, want 422", name, err)
			}
		}
		if _, err := s.GetProfile(ctx, uuid.NewV7()); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if _, err := s.UpdateProfile(ctx, students.Profile{UserID: uuid.NewV7(), UniversityID: uni,
			DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("update missing: %v", err)
		}
	})

	t.Run("profile: walks, key removal, graduation, avatars, delete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni := w.university(t)
		var ids []uuid.UUID
		for i := range 5 {
			p := newProfile(t, s, w, uni)
			p.AudienceKeys = []string{"all", "club:c1"}
			if i == 4 {
				p.AudienceKeys = []string{"all"}
			}
			old := ym(2026, 6)
			if i < 2 {
				p.ExpectedGraduation = &old
			}
			if i == 1 {
				now := t0
				p.Status, p.GraduatedAt = students.ProfileGraduated, &now
			}
			if i == 3 {
				f := w.file(t)
				p.AvatarFileID = &f
			}
			if _, err := s.UpdateProfile(ctx, p); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, p.UserID)
		}
		var walked []uuid.UUID
		after := uuid.Nil()
		for {
			page, err := s.ProfilesAfter(ctx, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, p := range page {
				walked = append(walked, p.UserID)
			}
			after = page[len(page)-1].UserID
		}
		sorted := slices.Clone(ids)
		slices.SortFunc(sorted, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		if !slices.Equal(walked, sorted) {
			t.Fatalf("walk = %v, want %v", walked, sorted)
		}
		n, err := s.RemoveAudienceKey(ctx, "club:c1", t0, 3)
		if err != nil || n != 3 {
			t.Fatalf("RemoveAudienceKey batch = %d, %v", n, err)
		}
		if n, err = s.RemoveAudienceKey(ctx, "club:c1", t0, 3); err != nil || n != 1 {
			t.Fatalf("RemoveAudienceKey rest = %d, %v", n, err)
		}
		for _, id := range ids {
			if p, _ := s.GetProfile(ctx, id); slices.Contains(p.AudienceKeys, "club:c1") || !slices.Contains(p.AudienceKeys, "all") {
				t.Fatalf("keys of %v = %v", id, p.AudienceKeys)
			}
		}
		cand, err := s.GraduationCandidates(ctx, ym(2026, 10), uuid.Nil(), 10)
		if err != nil || len(cand) != 1 || cand[0].UserID != ids[0] {
			t.Fatalf("GraduationCandidates = %v, %v", cand, err)
		}
		if cand, _ := s.GraduationCandidates(ctx, ym(2026, 6), uuid.Nil(), 10); len(cand) != 0 {
			t.Fatalf("candidates before the month is over = %v", cand)
		}
		grads, err := s.GraduatedAmong(ctx, append(slices.Clone(ids), uuid.NewV7()))
		if err != nil || !slices.Equal(grads, []uuid.UUID{ids[1]}) {
			t.Fatalf("GraduatedAmong = %v, %v", grads, err)
		}
		av, err := s.AvatarIDs(ctx, ids)
		if err != nil || len(av) != 1 || av[ids[3]] == uuid.Nil() {
			t.Fatalf("AvatarIDs = %v, %v", av, err)
		}
		got, err := s.Profiles(ctx, []uuid.UUID{ids[0], ids[2], uuid.NewV7()})
		if err != nil || len(got) != 2 {
			t.Fatalf("Profiles = %v, %v", got, err)
		}
		if ok, err := s.DeleteProfile(ctx, ids[2]); err != nil || !ok {
			t.Fatalf("DeleteProfile = %v, %v", ok, err)
		}
		if ok, err := s.DeleteProfile(ctx, ids[2]); err != nil || ok {
			t.Fatalf("second DeleteProfile = %v, %v", ok, err)
		}
	})

	t.Run("privacy: lock creates defaults, update, flagged, delete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, v := w.user(t), w.user(t)
		if _, err := s.GetPrivacy(ctx, u); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("GetPrivacy before = %v", err)
		}
		p, err := s.LockPrivacy(ctx, u)
		if err != nil || p.Discoverable || p.ShowContactOnGrant || p.ShowClubsOnGrant || p.ActivityRankingChoice != students.RankingDefault {
			t.Fatalf("defaults = %+v, %v", p, err)
		}
		p.Discoverable, p.ActivityRankingChoice = true, students.RankingDisabled
		if got, err := s.UpdatePrivacy(ctx, p); err != nil || !got.Discoverable || got.ActivityRankingChoice != students.RankingDisabled {
			t.Fatalf("UpdatePrivacy = %+v, %v", got, err)
		}
		q, _ := s.LockPrivacy(ctx, v)
		q.ShowClubsOnGrant = true
		if _, err := s.UpdatePrivacy(ctx, q); err != nil {
			t.Fatal(err)
		}
		flagged, err := s.PrivacyFlagged(ctx, uuid.Nil(), 10)
		if err != nil || len(flagged) != 1 || flagged[0].UserID != u {
			t.Fatalf("PrivacyFlagged = %v, %v", flagged, err)
		}
		p.ActivityRankingChoice = "maybe"
		if _, err := s.UpdatePrivacy(ctx, p); !isInvalid(err) {
			t.Fatalf("bad ranking choice: %v", err)
		}
		if ok, err := s.DeletePrivacy(ctx, u); err != nil || !ok {
			t.Fatalf("DeletePrivacy = %v, %v", ok, err)
		}
	})

	t.Run("sections: replace keeps created_at, owner scoping, delete all", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, other := w.user(t), w.user(t)
		k1, k2, k3 := w.skill(t), w.skill(t), w.skill(t)
		lvl := int16(3)
		first, err := s.ReplaceSkills(ctx, u, []students.Skill{{SkillID: k1, Level: &lvl}, {SkillID: k2}})
		if err != nil || len(first) != 2 {
			t.Fatalf("ReplaceSkills = %v, %v", first, err)
		}
		lvl5 := int16(5)
		second, err := s.ReplaceSkills(ctx, u, []students.Skill{{SkillID: k2, Level: &lvl5}, {SkillID: k3}})
		if err != nil || len(second) != 2 {
			t.Fatalf("second ReplaceSkills = %v, %v", second, err)
		}
		i := slices.IndexFunc(second, func(x students.Skill) bool { return x.SkillID == k2 })
		j := slices.IndexFunc(first, func(x students.Skill) bool { return x.SkillID == k2 })
		if i < 0 || *second[i].Level != 5 || !second[i].CreatedAt.Equal(first[j].CreatedAt) {
			t.Fatalf("kept skill = %+v (was %+v)", second, first)
		}
		bad := int16(6)
		if _, err := s.ReplaceSkills(ctx, u, []students.Skill{{SkillID: k1, Level: &bad}}); !isInvalid(err) {
			t.Fatalf("level 6: %v", err)
		}
		if got, _ := s.Skills(ctx, u); len(got) != 2 { // a refused replacement changes nothing
			t.Fatalf("skills after a refused replacement = %v", got)
		}
		if got, _ := s.Skills(ctx, other); len(got) != 0 {
			t.Fatal("another user's skills")
		}
		langs, err := s.ReplaceLanguages(ctx, u, []students.Language{{LanguageCode: "en", Level: "C1"}, {LanguageCode: "tr", Level: "native"}})
		if err != nil || len(langs) != 2 {
			t.Fatalf("ReplaceLanguages = %v, %v", langs, err)
		}
		if langs, err = s.ReplaceLanguages(ctx, u, []students.Language{{LanguageCode: "en", Level: "B2"}}); err != nil || len(langs) != 1 ||
			langs[0].Level != "B2" {
			t.Fatalf("second ReplaceLanguages = %v, %v", langs, err)
		}
		if _, err := s.ReplaceLanguages(ctx, u, []students.Language{{LanguageCode: "EN", Level: "B2"}}); !isInvalid(err) {
			t.Fatalf("bad code: %v", err)
		}
		if got, _ := s.Languages(ctx, u); len(got) != 1 {
			t.Fatalf("languages after a refused replacement = %v", got)
		}
		end := ym(2025, 9)
		e1 := students.Experience{ID: uuid.NewV7(), UserID: u, Kind: students.KindJob, Title: "İş", StartMonth: ym(2025, 1), EndMonth: &end}
		e2 := students.Experience{ID: uuid.NewV7(), UserID: u, Kind: students.KindProject, Title: "Proje", StartMonth: ym(2025, 6)}
		for _, e := range []students.Experience{e1, e2} {
			if _, err := s.InsertExperience(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := s.Experiences(ctx, u); len(got) != 2 || got[0].ID != e2.ID {
			t.Fatalf("Experiences order = %+v", got)
		}
		if _, err := s.GetExperience(ctx, other, e1.ID); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("another user's experience: %v", err)
		}
		stolen := e1
		stolen.UserID, stolen.Title = other, "Çalındı"
		if _, err := s.UpdateExperience(ctx, stolen); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("update as another user: %v", err)
		}
		if ok, err := s.DeleteExperience(ctx, other, e1.ID); err != nil || ok {
			t.Fatalf("delete as another user: %v, %v", ok, err)
		}
		e1.EndMonth = ptr(ym(2024, 12))
		if _, err := s.UpdateExperience(ctx, e1); !isInvalid(err) {
			t.Fatalf("end before start: %v", err)
		}
		e1.EndMonth, e1.Title = nil, "İş 2"
		if got, err := s.UpdateExperience(ctx, e1); err != nil || got.Title != "İş 2" || got.EndMonth != nil {
			t.Fatalf("UpdateExperience = %+v, %v", got, err)
		}
		n, err := s.DeleteSections(ctx, u)
		if err != nil || n != 5 { // 2 skills + 1 language + 2 experiences
			t.Fatalf("DeleteSections = %d, %v", n, err)
		}
		if n, _ := s.DeleteSections(ctx, u); n != 0 {
			t.Fatalf("second DeleteSections = %d", n)
		}
	})
}
