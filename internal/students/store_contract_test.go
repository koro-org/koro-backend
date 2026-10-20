package students_test

import (
	"errors"
	"slices"
	"strings"
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
	file       func(t *testing.T) uuid.UUID // an avatar
	cvFile     func(t *testing.T) uuid.UUID // a cv upload
	company    func(t *testing.T) uuid.UUID // a company org
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{user: id, university: id, department: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		skill: id, file: id, cvFile: id, company: id}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

func isConflict(err error) bool {
	var pe *problem.Error
	return errors.As(err, &pe) && pe.Status() == 409
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

	t.Run("cvs: insert, single current, owner scoping, by file, delete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, other := w.user(t), w.user(t)
		f1, f2, f3 := w.cvFile(t), w.cvFile(t), w.cvFile(t)
		c1, err := s.InsertCV(ctx, students.CV{ID: uuid.NewV7(), UserID: u, FileID: f1})
		if err != nil || c1.IsCurrent || c1.CreatedAt.IsZero() {
			t.Fatalf("InsertCV = %+v, %v", c1, err)
		}
		c2, err := s.InsertCV(ctx, students.CV{ID: uuid.NewV7(), UserID: u, FileID: f2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertCV(ctx, students.CV{ID: uuid.NewV7(), UserID: other, FileID: f1}); !isConflict(err) {
			t.Fatalf("a file used twice: %v, want 409", err)
		}
		for _, id := range []uuid.UUID{c1.ID, c2.ID, c2.ID, c1.ID} {
			got, err := s.SetCurrentCV(ctx, u, id)
			if err != nil || !got.IsCurrent || got.ID != id {
				t.Fatalf("SetCurrentCV(%v) = %+v, %v", id, got, err)
			}
			cvs, _ := s.CVs(ctx, u)
			n := 0
			for _, c := range cvs {
				if c.IsCurrent {
					n++
					if c.ID != id {
						t.Fatalf("current = %v, want %v", c.ID, id)
					}
				}
			}
			if n != 1 || len(cvs) != 2 || cvs[0].ID != c1.ID {
				t.Fatalf("CVs = %+v", cvs)
			}
		}
		if _, err := s.SetCurrentCV(ctx, other, c1.ID); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("another user's SetCurrentCV: %v", err)
		}
		if _, err := s.GetCV(ctx, other, c1.ID); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("another user's GetCV: %v", err)
		}
		if got, err := s.CVByFile(ctx, f2); err != nil || got.ID != c2.ID {
			t.Fatalf("CVByFile = %+v, %v", got, err)
		}
		if _, err := s.CVByFile(ctx, f3); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("CVByFile(unused) = %v", err)
		}
		if ok, err := s.DeleteCV(ctx, other, c1.ID); err != nil || ok {
			t.Fatalf("another user's DeleteCV = %v, %v", ok, err)
		}
		if ok, err := s.DeleteCV(ctx, u, c1.ID); err != nil || !ok {
			t.Fatalf("DeleteCV = %v, %v", ok, err)
		}
		if cvs, _ := s.CVs(ctx, u); len(cvs) != 1 || cvs[0].ID != c2.ID {
			t.Fatalf("CVs after delete = %+v", cvs)
		}
	})

	t.Run("segment: count_segment semantics and the eligible walk", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni := w.university(t)
		var eligible []uuid.UUID
		put := func(keys []string, ok bool) uuid.UUID {
			p := newProfile(t, s, w, uni)
			p.AudienceKeys, p.AudienceEligible = keys, ok
			if _, err := s.UpdateProfile(ctx, p); err != nil {
				t.Fatal(err)
			}
			return p.UserID
		}
		for i := range 12 {
			year := "year:2"
			if i >= 8 {
				year = "year:3"
			}
			eligible = append(eligible, put([]string{"all", "uni:a", year}, true))
		}
		for range 3 {
			put([]string{"all", "uni:a", "year:2"}, false) // ineligible: never counted
		}
		put([]string{"all", "uni:b", "year:2"}, true)
		for _, c := range []struct {
			keys []string
			dims int16
			want int // -1: below 10 (no number)
		}{
			{[]string{"uni:a"}, 1, 12},
			{[]string{"uni:a", "uni:b"}, 1, 13},
			{[]string{"uni:a", "year:2", "year:3"}, 2, 12},
			{[]string{"uni:a", "uni:b", "year:2"}, 2, -1}, // 8 + 1 = 9
			{[]string{"uni:a", "year:3"}, 2, -1},          // 4
			{[]string{"all"}, 1, 13},
			{[]string{"uni:x"}, 1, -1},
		} {
			n, ok, err := s.CountSegment(ctx, c.keys, c.dims)
			if err != nil || (c.want < 0) == ok || (ok && n != c.want) || (!ok && n != 0) {
				t.Errorf("CountSegment(%v, %d) = %d, %v, %v; want %d", c.keys, c.dims, n, ok, err, c.want)
			}
		}
		var walked []uuid.UUID
		after := uuid.Nil()
		for {
			page, err := s.AudienceAfter(ctx, []string{"uni:a", "year:2", "year:3"}, 2, after, 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			walked = append(walked, page...)
			after = page[len(page)-1]
		}
		slices.SortFunc(eligible, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		if !slices.Equal(walked, eligible) {
			t.Fatalf("walk = %v\n want %v", walked, eligible)
		}
	})

	t.Run("grants: insert once per source, owner scoping, revoke, pages, export, purge", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, v := w.user(t), w.user(t)
		c1, c2, c3 := w.company(t), w.company(t), w.company(t)
		stale := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) // a caller's stamp the stores ignore (BE-069)
		grant := func(student, company uuid.UUID, at time.Time) students.AccessGrant {
			t.Helper()
			g, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: student, CompanyOrgID: company,
				Source: students.SourceContactRequest, SourceID: uuid.NewV7(), GrantedAt: at, CreatedAt: stale, UpdatedAt: stale})
			if err != nil || !ok {
				t.Fatalf("InsertGrant: ok=%v err=%v", ok, err)
			}
			return g
		}
		if err := s.LockGrants(ctx, u); err != nil {
			t.Fatalf("LockGrants: %v", err)
		}
		g1 := grant(u, c1, t0)
		if !g1.Active() || !g1.GrantedAt.Equal(t0) || g1.CreatedAt.IsZero() || g1.CreatedAt.Equal(stale) ||
			!g1.UpdatedAt.Equal(g1.CreatedAt) || g1.Source != students.SourceContactRequest {
			t.Fatalf("inserted = %+v (one store stamp on insert, BE-069)", g1)
		}
		if _, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: v, CompanyOrgID: c2,
			Source: g1.Source, SourceID: g1.SourceID, GrantedAt: t0}); err != nil || ok {
			t.Fatalf("a second grant of the same request: ok=%v err=%v", ok, err)
		}
		if _, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: u, CompanyOrgID: c2,
			Source: "advert", SourceID: uuid.NewV7(), GrantedAt: t0}); !isInvalid(err) || ok {
			t.Fatalf("unknown source: ok=%v err=%v, want 422", ok, err)
		}
		g2 := grant(u, c2, t0.Add(time.Minute))
		g3 := grant(u, c3, t0.Add(2*time.Minute))
		grant(v, c1, t0.Add(3*time.Minute))
		if got, err := s.GrantBySource(ctx, g1.Source, g1.SourceID); err != nil || got.ID != g1.ID {
			t.Fatalf("GrantBySource = %+v, %v", got, err)
		}
		if _, err := s.GetGrant(ctx, v, g1.ID); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("another student's GetGrant: %v", err)
		}
		if got, err := s.ActiveGrant(ctx, u, c1); err != nil || got.ID != g1.ID {
			t.Fatalf("ActiveGrant = %+v, %v", got, err)
		}
		if _, err := s.ActiveGrant(ctx, v, c2); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("ActiveGrant without a grant: %v", err)
		}

		// Pages: newest first, keyset.
		page, err := s.StudentGrants(ctx, u, nil, 2)
		if err != nil || len(page) != 2 || page[0].ID != g3.ID || page[1].ID != g2.ID {
			t.Fatalf("StudentGrants page 1 = %+v, %v", page, err)
		}
		page, err = s.StudentGrants(ctx, u, &students.GrantKey{GrantedAt: page[1].GrantedAt, ID: page[1].ID}, 2)
		if err != nil || len(page) != 1 || page[0].ID != g1.ID {
			t.Fatalf("StudentGrants page 2 = %+v, %v", page, err)
		}
		if page, err = s.CompanyGrants(ctx, c1, nil, 10); err != nil || len(page) != 2 || page[0].StudentUserID != v {
			t.Fatalf("CompanyGrants = %+v, %v", page, err)
		}

		// Revoke: every active grant of the pair; a reason over 300 characters
		// is refused; a second call revokes nothing.
		long := strings.Repeat("x", 301)
		if _, err := s.RevokeGrants(ctx, u, c1, students.RevokedByStudent, &long, t0); !isInvalid(err) {
			t.Fatalf("a 301-character reason: %v, want 422", err)
		}
		why := "artık değil"
		revoked, err := s.RevokeGrants(ctx, u, c1, students.RevokedByStudent, &why, t0.Add(time.Hour))
		if err != nil || len(revoked) != 1 || revoked[0].ID != g1.ID || revoked[0].Active() ||
			*revoked[0].RevokedByKind != students.RevokedByStudent || *revoked[0].RevokeReason != why ||
			revoked[0].UpdatedAt.Before(revoked[0].CreatedAt) || !revoked[0].CreatedAt.Equal(g1.CreatedAt) {
			t.Fatalf("RevokeGrants = %+v, %v", revoked, err)
		}
		if again, err := s.RevokeGrants(ctx, u, c1, students.RevokedByCompany, nil, t0); err != nil || len(again) != 0 {
			t.Fatalf("revoking again = %+v, %v", again, err)
		}
		if _, err := s.ActiveGrant(ctx, u, c1); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("ActiveGrant after the revocation: %v", err)
		}
		if got, _ := s.GetGrant(ctx, u, g1.ID); got.Active() {
			t.Fatal("GetGrant returns the revoked grant as active")
		}
		if page, _ = s.CompanyGrants(ctx, c1, nil, 10); len(page) != 1 || page[0].StudentUserID != v {
			t.Fatalf("CompanyGrants after the revocation = %+v", page)
		}
		if all, err := s.AllGrants(ctx, u); err != nil || len(all) != 3 || all[0].ID != g1.ID {
			t.Fatalf("AllGrants = %+v, %v", all, err)
		}

		// Retention: revoked before the cutoff only, at most limit per call.
		if _, err := s.RevokeGrants(ctx, u, c2, students.RevokedByCompany, nil, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		cutoff := t0.Add(90 * time.Minute)
		if n, err := s.CountRevokedGrantsBefore(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("CountRevokedGrantsBefore = %d, %v", n, err)
		}
		if n, err := s.PurgeRevokedGrants(ctx, t0.Add(3*time.Hour), 1); err != nil || n != 1 {
			t.Fatalf("PurgeRevokedGrants(limit 1) = %d, %v", n, err)
		}
		if _, err := s.GetGrant(ctx, u, g1.ID); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("the oldest revocation is not the first purged: %v", err)
		}
		if n, err := s.PurgeRevokedGrants(ctx, t0.Add(3*time.Hour), 10); err != nil || n != 1 {
			t.Fatalf("PurgeRevokedGrants = %d, %v", n, err)
		}
		if n, err := s.DeleteGrants(ctx, u); err != nil || n != 1 {
			t.Fatalf("DeleteGrants = %d, %v (g3 is left)", n, err)
		}
		if all, _ := s.AllGrants(ctx, v); len(all) != 1 {
			t.Fatalf("another student's grants = %+v", all)
		}
	})

	t.Run("discovery: candidates are active discoverable profiles matching every filter", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uniA, uniB := w.university(t), w.university(t)
		dept, skill, other := w.department(t, uniA), w.skill(t), w.skill(t)
		mk := func(uni uuid.UUID, discoverable bool, edit func(*students.Profile)) uuid.UUID {
			t.Helper()
			p := newProfile(t, s, w, uni)
			if edit != nil {
				edit(&p)
				if _, err := s.UpdateProfile(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			pr, err := s.LockPrivacy(ctx, p.UserID)
			if err != nil {
				t.Fatal(err)
			}
			pr.Discoverable = discoverable
			if _, err := s.UpdatePrivacy(ctx, pr); err != nil {
				t.Fatal(err)
			}
			return p.UserID
		}
		year := int16(3)
		p1 := mk(uniA, true, func(p *students.Profile) { p.DepartmentID, p.ClassYear = &dept, &year })
		if _, err := s.ReplaceSkills(ctx, p1, []students.Skill{{UserID: p1, SkillID: skill}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReplaceLanguages(ctx, p1, []students.Language{{UserID: p1, LanguageCode: "en", Level: "B2"}}); err != nil {
			t.Fatal(err)
		}
		p2 := mk(uniA, true, nil)
		p3 := mk(uniB, true, func(p *students.Profile) { p.DegreeLevel = students.DegreeMaster })
		mk(uniA, false, nil) // level 2 off
		now := t0
		mk(uniA, true, func(p *students.Profile) { p.Status, p.GraduatedAt = students.ProfileGraduated, &now })
		sorted := func(ids ...uuid.UUID) []uuid.UUID {
			slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
			return ids
		}
		for _, c := range []struct {
			name string
			q    students.DiscoveryQuery
			want []uuid.UUID
		}{
			{"everyone", students.DiscoveryQuery{}, sorted(p1, p2, p3)},
			{"university", students.DiscoveryQuery{UniversityIDs: []uuid.UUID{uniA}}, sorted(p1, p2)},
			{"two universities (OR)", students.DiscoveryQuery{UniversityIDs: []uuid.UUID{uniA, uniB}}, sorted(p1, p2, p3)},
			{"department", students.DiscoveryQuery{DepartmentIDs: []uuid.UUID{dept}}, []uuid.UUID{p1}},
			{"class year", students.DiscoveryQuery{ClassYears: []int16{2, 3}}, []uuid.UUID{p1}},
			{"degree", students.DiscoveryQuery{DegreeLevel: students.DegreeMaster}, []uuid.UUID{p3}},
			{"skill (OR)", students.DiscoveryQuery{SkillIDs: []uuid.UUID{other, skill}}, []uuid.UUID{p1}},
			{"language", students.DiscoveryQuery{LanguageCodes: []string{"en"}}, []uuid.UUID{p1}},
			{"AND across filters", students.DiscoveryQuery{UniversityIDs: []uuid.UUID{uniB}, SkillIDs: []uuid.UUID{skill}}, nil},
		} {
			got, err := s.DiscoveryCandidates(ctx, c.q, uuid.Nil(), 10)
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("%s = %v, %v; want %v", c.name, got, err, c.want)
			}
		}
		all := sorted(p1, p2, p3)
		if got, err := s.DiscoveryCandidates(ctx, students.DiscoveryQuery{}, all[0], 1); err != nil || !slices.Equal(got, all[1:2]) {
			t.Fatalf("keyset page = %v, %v", got, err)
		}
	})

}
