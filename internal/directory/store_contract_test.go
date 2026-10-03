package directory_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"uuid"

	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// world creates the parent rows a store needs: orgs (organizations in
// PostgreSQL, an orgs memory store in memory) and users (FKs).
type world struct {
	org  func(t *testing.T, kind orgs.Kind, name string) uuid.UUID
	user func(t *testing.T) uuid.UUID
}

var seq atomic.Int64

func next() string { return strconv.FormatInt(seq.Add(1), 10) }

// orgWorld opens orgs in an orgs store (memory or GORM).
func orgWorld(st orgs.Store, user func(t *testing.T) uuid.UUID) world {
	return world{
		org: func(t *testing.T, kind orgs.Kind, name string) uuid.UUID {
			t.Helper()
			o := orgs.Org{ID: uuid.NewV7(), Kind: kind, Name: name, Slug: "org-" + next(), Status: orgs.StatusActive}
			if _, ok, err := st.InsertOrg(t.Context(), o); err != nil || !ok {
				t.Fatalf("InsertOrg: %v %v", ok, err)
			}
			return o.ID
		},
		user: user,
	}
}

// newMemPair is the in-memory store with its orgs.
func newMemPair(t *testing.T) (directory.Store, world) {
	t.Helper()
	om := orgs.NewMemStore(clock.NewFake(t0).Now)
	st := directory.NewMemStore(clock.NewFake(t0).Now, func(ctx context.Context, id uuid.UUID) (directory.OrgRef, bool) {
		o, err := om.GetOrg(ctx, id)
		if err != nil {
			return directory.OrgRef{}, false
		}
		return directory.OrgRef{Name: o.Name, Slug: o.Slug, Status: string(o.Status)}, true
	})
	return st, orgWorld(om, func(*testing.T) uuid.UUID { return uuid.NewV7() })
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, newMemPair)
}

func isStatus(err error, status int) bool {
	p, ok := errors.AsType[*problem.Error](err)
	return ok && p.Status() == status
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (directory.Store, world)) {
	t.Helper()
	uni := func(t *testing.T, s directory.Store, w world, name, city string) directory.University {
		t.Helper()
		u, err := s.InsertUniversity(t.Context(), directory.University{
			ID: uuid.NewV7(), OrgID: w.org(t, orgs.KindUniversity, name), YokCode: "Y-" + next(), Name: name, City: city,
			Type: directory.UniversityState, IsActive: true,
		})
		if err != nil {
			t.Fatalf("InsertUniversity: %v", err)
		}
		return u
	}
	club := func(t *testing.T, s directory.Store, w world, u directory.University, name string) directory.Club {
		t.Helper()
		c, err := s.InsertClub(t.Context(), directory.Club{
			OrgID: w.org(t, orgs.KindClub, name), UniversityID: u.ID, Category: directory.CategoryCulture,
			JoinPolicy: directory.JoinApproval, ClaimStatus: directory.ClaimUnclaimed,
		})
		if err != nil {
			t.Fatalf("InsertClub(%s): %v", name, err)
		}
		return c
	}

	t.Run("universities: insert, read, unique code, update, list", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u := uni(t, s, w, "Ege Üniversitesi", "İzmir")
		if u.CreatedAt.IsZero() || u.OrgKind != "university" || !u.IsActive {
			t.Fatalf("stored = %+v", u)
		}
		if g, err := s.GetUniversity(ctx, u.ID); err != nil || g.Name != u.Name {
			t.Fatalf("GetUniversity = %+v, %v", g, err)
		}
		if g, err := s.UniversityByYokCode(ctx, u.YokCode); err != nil || g.ID != u.ID {
			t.Fatalf("UniversityByYokCode = %+v, %v", g, err)
		}
		if g, err := s.UniversityByName(ctx, "Ege Üniversitesi"); err != nil || g.ID != u.ID {
			t.Fatalf("UniversityByName = %+v, %v", g, err)
		}
		if _, err := s.GetUniversity(ctx, uuid.NewV7()); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("missing university: %v", err)
		}
		dup := directory.University{ID: uuid.NewV7(), OrgID: w.org(t, orgs.KindUniversity, "Kopya"), YokCode: u.YokCode,
			Name: "Kopya", City: "Bursa", Type: directory.UniversityState, IsActive: true}
		if _, err := s.InsertUniversity(ctx, dup); !errors.Is(err, directory.ErrYokCodeTaken) {
			t.Fatalf("duplicate yok code: %v", err)
		}
		short := "EÜ"
		u.ShortName, u.IsActive, u.YokCode = &short, false, "Y-NEW-"+next()
		g, err := s.UpdateUniversity(ctx, u)
		if err != nil || *g.ShortName != "EÜ" || g.IsActive || g.YokCode != u.YokCode || !g.UpdatedAt.After(u.CreatedAt) && !g.UpdatedAt.Equal(u.CreatedAt) {
			t.Fatalf("UpdateUniversity = %+v, %v", g, err)
		}
		// Inactive universities are not listed; filters and Turkish search.
		uni(t, s, w, "İstanbul Kent Üniversitesi", "İstanbul")
		uni(t, s, w, "Işıklı Üniversitesi", "Ankara")
		list := func(f directory.UniversityFilter) []string {
			rows, err := s.ListUniversities(ctx, f, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, r := range rows {
				out = append(out, r.Name)
			}
			slices.Sort(out)
			return out
		}
		if got := list(directory.UniversityFilter{}); len(got) != 2 {
			t.Fatalf("active universities = %v", got)
		}
		if got := list(directory.UniversityFilter{Q: "ISTANBUL"}); !slices.Equal(got, []string{"İstanbul Kent Üniversitesi"}) {
			t.Fatalf("q=ISTANBUL: %v", got)
		}
		if got := list(directory.UniversityFilter{Q: "isikli"}); !slices.Equal(got, []string{"Işıklı Üniversitesi"}) {
			t.Fatalf("q=isikli: %v", got)
		}
		if got := list(directory.UniversityFilter{City: "ANKARA"}); !slices.Equal(got, []string{"Işıklı Üniversitesi"}) {
			t.Fatalf("city=ANKARA: %v", got)
		}
		// Keyset by name: no row lost or repeated.
		first, err := s.ListUniversities(ctx, directory.UniversityFilter{Sort: directory.SortName}, nil, 1)
		if err != nil || len(first) != 1 {
			t.Fatal(first, err)
		}
		rest, err := s.ListUniversities(ctx, directory.UniversityFilter{Sort: directory.SortName},
			&directory.After{Name: first[0].Name, ID: first[0].ID}, 10)
		if err != nil || len(rest) != 1 || rest[0].ID == first[0].ID {
			t.Fatalf("second page = %+v, %v", rest, err)
		}
	})

	t.Run("domains: insert, conflict, update, longest active match", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		a, b := uni(t, s, w, "A Üniversitesi", "İzmir"), uni(t, s, w, "B Üniversitesi", "Bursa")
		by := w.user(t)
		d, err := s.InsertDomain(ctx, directory.EmailDomain{Domain: "a.edu.tr", UniversityID: a.ID, DomainKind: directory.DomainMixed,
			IsActive: true, VerifiedBy: &by})
		if err != nil || d.CreatedAt.IsZero() || *d.VerifiedBy != by {
			t.Fatalf("InsertDomain = %+v, %v", d, err)
		}
		if _, err := s.InsertDomain(ctx, directory.EmailDomain{Domain: "a.edu.tr", UniversityID: b.ID, DomainKind: directory.DomainMixed,
			IsActive: true}); !errors.Is(err, directory.ErrDomainTaken) {
			t.Fatalf("duplicate domain: %v", err)
		}
		if _, err := s.InsertDomain(ctx, directory.EmailDomain{Domain: "a.example.com", UniversityID: a.ID,
			DomainKind: directory.DomainMixed, IsActive: true}); !isStatus(err, 422) {
			t.Fatalf("non .edu.tr domain: %v", err)
		}
		if _, err := s.InsertDomain(ctx, directory.EmailDomain{Domain: "ogr.a.edu.tr", UniversityID: b.ID,
			DomainKind: directory.DomainStudent, IsActive: true}); err != nil {
			t.Fatal(err)
		}
		m, err := s.MatchDomain(ctx, []string{"x.ogr.a.edu.tr", "ogr.a.edu.tr", "a.edu.tr", "edu.tr", "tr"})
		if err != nil || m.Domain != "ogr.a.edu.tr" || m.UniversityID != b.ID {
			t.Fatalf("MatchDomain = %+v, %v", m, err)
		}
		cur, err := s.GetDomain(ctx, "ogr.a.edu.tr")
		if err != nil {
			t.Fatal(err)
		}
		cur.IsActive = false
		if g, err := s.UpdateDomain(ctx, cur); err != nil || g.IsActive {
			t.Fatalf("UpdateDomain = %+v, %v", g, err)
		}
		if m, err := s.MatchDomain(ctx, []string{"ogr.a.edu.tr", "a.edu.tr"}); err != nil || m.Domain != "a.edu.tr" {
			t.Fatalf("MatchDomain after deactivation = %+v, %v", m, err)
		}
		// An inactive university's domain does not match.
		a.IsActive = false
		if _, err := s.UpdateUniversity(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MatchDomain(ctx, []string{"a.edu.tr"}); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("inactive university's domain: %v", err)
		}
	})

	t.Run("faculties and departments", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u := uni(t, s, w, "C Üniversitesi", "İzmir")
		f, err := s.InsertFaculty(ctx, directory.Faculty{ID: uuid.NewV7(), UniversityID: u.ID, Name: "Mühendislik Fakültesi", IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertFaculty(ctx, directory.Faculty{ID: uuid.NewV7(), UniversityID: u.ID, Name: "Mühendislik Fakültesi", IsActive: true}); !isStatus(err, 409) {
			t.Fatalf("duplicate faculty: %v", err)
		}
		f2, _ := s.InsertFaculty(ctx, directory.Faculty{ID: uuid.NewV7(), UniversityID: u.ID, Name: "Eğitim Fakültesi", IsActive: true})
		facs, err := s.FacultiesOf(ctx, u.ID)
		if err != nil || len(facs) != 2 || facs[0].ID != f2.ID {
			t.Fatalf("FacultiesOf (Turkish name order) = %+v, %v", facs, err)
		}
		if g, err := s.FacultyByName(ctx, u.ID, "Mühendislik Fakültesi"); err != nil || g.ID != f.ID {
			t.Fatalf("FacultyByName = %+v, %v", g, err)
		}
		code, four := "P-1", int16(4)
		d, err := s.InsertDepartment(ctx, directory.Department{ID: uuid.NewV7(), UniversityID: u.ID, FacultyID: &f.ID,
			Name: "Bilgisayar Mühendisliği", YokProgramCode: &code, DegreeLevel: directory.DegreeBachelor, DurationYears: &four, IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertDepartment(ctx, directory.Department{ID: uuid.NewV7(), UniversityID: u.ID, Name: "Başka",
			YokProgramCode: &code, DegreeLevel: directory.DegreeBachelor, IsActive: true}); !errors.Is(err, directory.ErrDepartmentExists) {
			t.Fatalf("duplicate program code: %v", err)
		}
		eight := int16(8)
		if _, err := s.InsertDepartment(ctx, directory.Department{ID: uuid.NewV7(), UniversityID: u.ID, Name: "Uzun",
			DegreeLevel: directory.DegreeBachelor, DurationYears: &eight, IsActive: true}); !isStatus(err, 422) {
			t.Fatalf("duration 8: %v", err)
		}
		plain, err := s.InsertDepartment(ctx, directory.Department{ID: uuid.NewV7(), UniversityID: u.ID, FacultyID: &f.ID,
			Name: "Endüstri Mühendisliği", DegreeLevel: directory.DegreeBachelor, IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		if g, err := s.FindDepartment(ctx, directory.DepartmentKey{UniversityID: u.ID, YokProgramCode: code}); err != nil || g.ID != d.ID {
			t.Fatalf("FindDepartment by code = %+v, %v", g, err)
		}
		if g, err := s.FindDepartment(ctx, directory.DepartmentKey{UniversityID: u.ID, FacultyID: &f.ID, Name: "Endüstri Mühendisliği",
			DegreeLevel: directory.DegreeBachelor}); err != nil || g.ID != plain.ID {
			t.Fatalf("FindDepartment by name = %+v, %v", g, err)
		}
		if _, err := s.FindDepartment(ctx, directory.DepartmentKey{UniversityID: u.ID, Name: "Endüstri Mühendisliği",
			DegreeLevel: directory.DegreeBachelor}); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("FindDepartment without the faculty: %v", err)
		}
		// Updates write zero values (duration cleared).
		d.DurationYears = nil
		if g, err := s.UpdateDepartment(ctx, d); err != nil || g.DurationYears != nil {
			t.Fatalf("UpdateDepartment = %+v, %v", g, err)
		}
		rows, err := s.ListDepartments(ctx, directory.DepartmentFilter{UniversityID: u.ID, Q: "BILGISAYAR"}, nil, 10)
		if err != nil || len(rows) != 1 || rows[0].ID != d.ID {
			t.Fatalf("ListDepartments q = %+v, %v", rows, err)
		}
		rows, err = s.ListDepartments(ctx, directory.DepartmentFilter{UniversityID: u.ID, FacultyID: &f.ID}, nil, 1)
		if err != nil || len(rows) != 1 || rows[0].ID != plain.ID { // newest first
			t.Fatalf("ListDepartments page = %+v, %v", rows, err)
		}
		if g, err := s.GetDepartment(ctx, plain.ID); err != nil || g.Name != plain.Name {
			t.Fatalf("GetDepartment = %+v, %v", g, err)
		}
	})

	t.Run("clubs: sensitive category requires the flag", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u := uni(t, s, w, "D Üniversitesi", "İzmir")
		for _, cat := range []directory.Category{directory.CategoryReligious, directory.CategoryPolitical, directory.CategoryIdentity} {
			_, err := s.InsertClub(ctx, directory.Club{OrgID: w.org(t, orgs.KindClub, "Hassas "+string(cat)), UniversityID: u.ID,
				Category: cat, JoinPolicy: directory.JoinApproval, ClaimStatus: directory.ClaimUnclaimed})
			if !errors.Is(err, directory.ErrSensitivityRequired) {
				t.Fatalf("%s without the flag: %v", cat, err)
			}
		}
		// The reason and the stamp go with the flag.
		reason := directory.SensitiveReason("religion")
		if _, err := s.InsertClub(ctx, directory.Club{OrgID: w.org(t, orgs.KindClub, "Yarım"), UniversityID: u.ID,
			Category: directory.CategoryReligious, IsSensitive: true, SensitiveReason: &reason, JoinPolicy: directory.JoinApproval,
			ClaimStatus: directory.ClaimUnclaimed}); !isStatus(err, 422) {
			t.Fatalf("flag without set_at: %v", err)
		}
		c := club(t, s, w, u, "Normal Kulüp")
		c.Category = directory.CategoryPolitical
		if _, err := s.UpdateClub(ctx, c); !errors.Is(err, directory.ErrSensitivityRequired) {
			t.Fatalf("update to political without the flag: %v", err)
		}
		by, now := w.user(t), t0
		c.IsSensitive, c.SensitiveReason, c.SensitivitySetBy, c.SensitivitySetAt = true, &reason, &by, &now
		got, err := s.UpdateClub(ctx, c)
		if err != nil || !got.IsSensitive || got.Category != directory.CategoryPolitical {
			t.Fatalf("flagged political club = %+v, %v", got, err)
		}
		ids, err := s.SensitiveAmong(ctx, []uuid.UUID{c.OrgID, uuid.NewV7()})
		if err != nil || !slices.Equal(ids, []uuid.UUID{c.OrgID}) {
			t.Fatalf("SensitiveAmong = %v, %v", ids, err)
		}
	})

	t.Run("clubs: insert, read, update zero values, org scope", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u := uni(t, s, w, "E Üniversitesi", "İzmir")
		c := club(t, s, w, u, "Satranç Kulübü")
		if c.OrgKind != "club" || c.SocialLinks == nil || len(c.SocialLinks) != 0 || c.CreatedAt.IsZero() {
			t.Fatalf("stored club = %+v", c)
		}
		if _, err := s.GetClub(ctx, uuid.NewV7()); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("missing club: %v", err)
		}
		if _, err := s.InsertClub(ctx, directory.Club{OrgID: w.org(t, orgs.KindClub, "Yetim"), UniversityID: uuid.NewV7(),
			Category: directory.CategoryArts, JoinPolicy: directory.JoinOpen, ClaimStatus: directory.ClaimUnclaimed}); err == nil {
			t.Fatal("club of an unknown university was stored")
		}
		// A club row needs an org of kind club (composite FK in PostgreSQL;
		// the memory store does not see orgs: the service guarantees it).
		year := int16(2010)
		c.FoundedYear, c.SocialLinks, c.JoinPolicy = &year, directory.SocialLinks{"instagram": "https://instagram.com/satranc"}, directory.JoinOpen
		g, err := s.UpdateClub(ctx, c)
		if err != nil || *g.FoundedYear != 2010 || g.SocialLinks["instagram"] != "https://instagram.com/satranc" || !g.UpdatedAt.After(c.UpdatedAt) {
			t.Fatalf("UpdateClub = %+v, %v", g, err)
		}
		g.FoundedYear, g.SocialLinks = nil, directory.SocialLinks{}
		g2, err := s.UpdateClub(ctx, g)
		if err != nil || g2.FoundedYear != nil || len(g2.SocialLinks) != 0 {
			t.Fatalf("zero values = %+v, %v", g2, err)
		}
		if r, err := s.GetClub(ctx, c.OrgID); err != nil || r.JoinPolicy != directory.JoinOpen || len(r.SocialLinks) != 0 {
			t.Fatalf("GetClub = %+v, %v", r, err)
		}
		if _, err := s.LockClub(ctx, uuid.Nil()); err == nil {
			t.Fatal("LockClub without an org scope succeeded")
		}
		ids, err := s.ClubOrgsOf(ctx, u.ID)
		if err != nil || !slices.Equal(ids, []uuid.UUID{c.OrgID}) {
			t.Fatalf("ClubOrgsOf = %v, %v", ids, err)
		}
	})

	t.Run("clubs: Turkish search, filters, keyset", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, other := uni(t, s, w, "F Üniversitesi", "İzmir"), uni(t, s, w, "G Üniversitesi", "Bursa")
		for _, n := range []string{"İstanbul Gezginleri", "ISTANBUL Tiyatro Topluluğu", "Işık Fotoğrafçılık", "Ankara Kültür Kulübü"} {
			club(t, s, w, u, n)
		}
		claimed := club(t, s, w, other, "İstanbullu Satranççılar")
		claimed.ClaimStatus = directory.ClaimClaimed
		if _, err := s.UpdateClub(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		find := func(f directory.ClubFilter) []string {
			rows, err := s.ListClubs(ctx, f, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, r := range rows {
				out = append(out, r.Name)
			}
			slices.Sort(out)
			return out
		}
		want := []string{"ISTANBUL Tiyatro Topluluğu", "İstanbul Gezginleri", "İstanbullu Satranççılar"}
		for _, q := range []string{"istanbul", "İSTANBUL", "ISTANBUL", "Istanbul", "ıstanbul", "İstanbul"} {
			if got := find(directory.ClubFilter{Q: q}); !slices.Equal(got, want) {
				t.Errorf("q=%q: %v, want %v", q, got, want)
			}
		}
		if got := find(directory.ClubFilter{Q: "ışık"}); !slices.Equal(got, []string{"Işık Fotoğrafçılık"}) {
			t.Errorf("q=ışık: %v", got)
		}
		if got := find(directory.ClubFilter{Q: "KULTUR"}); !slices.Equal(got, []string{"Ankara Kültür Kulübü"}) {
			t.Errorf("q=KULTUR: %v", got)
		}
		uid := other.ID
		yes := true
		if got := find(directory.ClubFilter{UniversityID: &uid, Claimed: &yes}); !slices.Equal(got, []string{"İstanbullu Satranççılar"}) {
			t.Errorf("university+claimed: %v", got)
		}
		if got := find(directory.ClubFilter{Category: directory.CategoryArts}); len(got) != 0 {
			t.Errorf("category arts: %v", got)
		}
		// Keyset by name: pages of two, no loss, no repeat.
		var seen []string
		var after *directory.After
		for range 5 {
			rows, err := s.ListClubs(ctx, directory.ClubFilter{Sort: directory.SortName}, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				seen = append(seen, r.Name)
			}
			if len(rows) < 2 {
				break
			}
			last := rows[len(rows)-1]
			after = &directory.After{Name: last.Name, ID: last.OrgID}
		}
		if len(seen) != 5 || seen[0] != "Ankara Kültür Kulübü" || seen[4] != "İstanbullu Satranççılar" {
			t.Fatalf("pages by name = %v", seen)
		}
	})

	t.Run("skills: unique slug, update, count active", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		en := "Machine learning"
		k, err := s.InsertSkill(ctx, directory.Skill{ID: uuid.NewV7(), Slug: "ml-" + next(), NameTR: "Makine öğrenmesi", NameEN: &en,
			Category: "technical", IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertSkill(ctx, directory.Skill{ID: uuid.NewV7(), Slug: k.Slug, NameTR: "Kopya", Category: "technical",
			IsActive: true}); !errors.Is(err, directory.ErrSkillSlugTaken) {
			t.Fatalf("duplicate slug: %v", err)
		}
		if _, err := s.InsertSkill(ctx, directory.Skill{ID: uuid.NewV7(), Slug: "Kötü Slug", NameTR: "x", Category: "technical",
			IsActive: true}); !isStatus(err, 422) {
			t.Fatalf("bad slug: %v", err)
		}
		other, _ := s.InsertSkill(ctx, directory.Skill{ID: uuid.NewV7(), Slug: "go-" + next(), NameTR: "Go", Category: "technical", IsActive: true})
		if n, err := s.CountActiveSkills(ctx, []uuid.UUID{k.ID, other.ID, uuid.NewV7()}); err != nil || n != 2 {
			t.Fatalf("CountActiveSkills = %d, %v", n, err)
		}
		k.IsActive, k.NameEN = false, nil
		if g, err := s.UpdateSkill(ctx, k); err != nil || g.IsActive || g.NameEN != nil {
			t.Fatalf("UpdateSkill = %+v, %v", g, err)
		}
		if n, _ := s.CountActiveSkills(ctx, []uuid.UUID{k.ID}); n != 0 {
			t.Fatalf("inactive skill counted")
		}
		rows, err := s.ListSkills(ctx, directory.SkillFilter{Q: "GO"}, nil, 10)
		if err != nil || len(rows) != 1 || rows[0].ID != other.ID {
			t.Fatalf("ListSkills = %+v, %v", rows, err)
		}
		if g, err := s.SkillBySlug(ctx, other.Slug); err != nil || g.ID != other.ID {
			t.Fatalf("SkillBySlug = %+v, %v", g, err)
		}
	})

	t.Run("club claims: pending unique, review checks, lists, rep scope, erase", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u := uni(t, s, w, "Talep Üniversitesi", "Bursa")
		other := uni(t, s, w, "Başka Üniversite", "Bursa")
		c1, c2 := club(t, s, w, u, "Talep Kulübü"), club(t, s, w, other, "Öteki Kulüp")
		sens := club(t, s, w, u, "Hassas Talep Kulübü")
		reason, by, now := directory.SensitiveReason("religion"), w.user(t), t0
		sens.IsSensitive, sens.SensitiveReason, sens.SensitivitySetBy, sens.SensitivitySetAt = true, &reason, &by, &now
		if _, err := s.UpdateClub(ctx, sens); err != nil {
			t.Fatal(err)
		}
		alice, bob, reviewer := w.user(t), w.user(t), w.user(t)
		text := "Genel kurul tutanağı"
		ins := func(club, user uuid.UUID) (directory.ClubClaim, error) {
			return s.InsertClaim(ctx, directory.ClubClaim{ID: uuid.NewV7(), ClubOrgID: club, ClaimantUserID: user,
				Status: directory.ClaimPending, RoleInClub: "Başkan", EvidenceText: &text})
		}
		a1, err := ins(c1.OrgID, alice)
		if err != nil || a1.CreatedAt.IsZero() || a1.Status != directory.ClaimPending || *a1.EvidenceText != text {
			t.Fatalf("InsertClaim = %+v, %v", a1, err)
		}
		if _, err := ins(c1.OrgID, alice); !errors.Is(err, directory.ErrClaimPending) {
			t.Fatalf("second pending claim: %v", err)
		}
		if _, err := ins(uuid.NewV7(), alice); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("claim of an unknown club: %v", err) // the club is the parent row (06 H-38)
		}
		if _, err := s.InsertClaim(ctx, directory.ClubClaim{ID: uuid.NewV7(), ClubOrgID: c2.OrgID, ClaimantUserID: alice,
			Status: directory.ClaimPending, RoleInClub: "B"}); !isStatus(err, 422) {
			t.Fatalf("role_in_club of 1 character: %v", err)
		}
		b1, err := ins(c1.OrgID, bob)
		if err != nil {
			t.Fatal(err)
		}
		b2, err := ins(c2.OrgID, bob)
		if err != nil {
			t.Fatal(err)
		}
		bs, err := ins(sens.OrgID, bob)
		if err != nil {
			t.Fatal(err)
		}
		// A half review and the claimant as reviewer are refused.
		half := a1
		half.Status = directory.ClaimApproved
		half.ReviewedBy = &reviewer
		if _, err := s.UpdateClaim(ctx, half); !isStatus(err, 422) {
			t.Fatalf("approved without reviewed_at: %v", err)
		}
		self := half
		self.ReviewedBy, self.ReviewedAt = &alice, &now
		if _, err := s.UpdateClaim(ctx, self); !isStatus(err, 403) {
			t.Fatalf("claimant as reviewer: %v", err)
		}
		note := "Yeterli"
		ok := half
		ok.ReviewedAt, ok.DecisionNote = &now, &note
		got, err := s.UpdateClaim(ctx, ok)
		if err != nil || got.Status != directory.ClaimApproved || *got.ReviewedBy != reviewer || *got.DecisionNote != note {
			t.Fatalf("UpdateClaim = %+v, %v", got, err)
		}
		if l, err := s.LockClaim(ctx, a1.ID); err != nil || l.Status != directory.ClaimApproved {
			t.Fatalf("LockClaim = %+v, %v", l, err)
		}
		// A decided claim frees the pair for a new pending claim.
		a2, err := ins(c1.OrgID, alice)
		if err != nil {
			t.Fatalf("new claim after a decision: %v", err)
		}
		ids := func(cs []directory.ClubClaim) []uuid.UUID {
			out := make([]uuid.UUID, len(cs))
			for i, c := range cs {
				out[i] = c.ID
			}
			return out
		}
		// ClaimsOf: newest first, keyset.
		mine, err := s.ClaimsOf(ctx, alice, nil, 10)
		if err != nil || !slices.Equal(ids(mine), []uuid.UUID{a2.ID, a1.ID}) {
			t.Fatalf("ClaimsOf = %v, %v", ids(mine), err)
		}
		if mine, _ = s.ClaimsOf(ctx, alice, &a2.ID, 10); !slices.Equal(ids(mine), []uuid.UUID{a1.ID}) {
			t.Fatalf("ClaimsOf after = %v", ids(mine))
		}
		// ListClaims: the pending queue oldest first, keyset; decided newest first.
		q, err := s.ListClaims(ctx, directory.ClaimFilter{Status: directory.ClaimPending}, nil, 10)
		if err != nil || !slices.Equal(ids(q), []uuid.UUID{b1.ID, b2.ID, bs.ID, a2.ID}) {
			t.Fatalf("pending queue = %v, %v", ids(q), err)
		}
		if q, _ = s.ListClaims(ctx, directory.ClaimFilter{Status: directory.ClaimPending}, &b2.ID, 2); !slices.Equal(ids(q), []uuid.UUID{bs.ID, a2.ID}) {
			t.Fatalf("pending queue after = %v", ids(q))
		}
		if q, _ = s.ListClaims(ctx, directory.ClaimFilter{Status: directory.ClaimApproved}, nil, 10); !slices.Equal(ids(q), []uuid.UUID{a1.ID}) {
			t.Fatalf("approved = %v", ids(q))
		}
		// A rep's scope: their universities' non-sensitive clubs only.
		rep := directory.ClaimFilter{Status: directory.ClaimPending, RepUniversityOrgs: []uuid.UUID{u.OrgID}}
		if q, _ = s.ListClaims(ctx, rep, nil, 10); !slices.Equal(ids(q), []uuid.UUID{b1.ID, a2.ID}) {
			t.Fatalf("rep queue = %v", ids(q))
		}
		rep.RepUniversityOrgs = []uuid.UUID{}
		if q, _ = s.ListClaims(ctx, rep, nil, 10); len(q) != 0 {
			t.Fatalf("empty rep scope = %v", ids(q))
		}
		// Erase: hard delete.
		if err := s.DeleteClaim(ctx, a1.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetClaim(ctx, a1.ID); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("deleted claim: %v", err)
		}
		if _, err := s.ClaimOfFile(ctx, uuid.NewV7()); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("ClaimOfFile of an unknown file: %v", err)
		}
	})
}
