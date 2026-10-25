package students_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

// jsonFields returns every JSON member path of t ("a", "a.b", "a[].c"),
// sorted: the field set a DTO type can ever put on the wire.
func jsonFields(t reflect.Type) []string {
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		depth := 0
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			if t.Kind() == reflect.Slice {
				depth++
			}
			t = t.Elem()
		}
		prefix += strings.Repeat("[]", depth)
		if t.Kind() != reflect.Struct || t == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if f.Anonymous {
				panic("embedded field in a DTO: " + t.Name()) // 04 §V.0-1: never embed
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				panic("DTO field without a JSON name: " + t.Name() + "." + f.Name)
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out = append(out, p)
			walk(f.Type, p)
		}
	}
	walk(t, "")
	sort.Strings(out)
	return out
}

// presentFields returns the member paths of a marshalled value (what the
// company actually receives in one answer).
func presentFields(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out = append(out, p)
				walk(c, p)
			}
		case []any:
			for _, c := range x {
				walk(c, prefix+"[]")
			}
		}
	}
	walk(m, "")
	sort.Strings(out)
	return slices.Compact(out)
}

func wantFields(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s fields:\n got  %v\n want %v", what, got, want)
	}
}

// forbiddenRe are member names no DTO may carry at any level (04 §V.0-2;
// 01 §0-7): talent values, scores, bands, points, ranks, sensitive data,
// identifiers a company must never see.
var forbiddenRe = regexp.MustCompile(`(^|[._\[\]])(talent\w*|band|score\w*|point\w*|rank\w*|sensitive|birth_date|is_minor|tckn|edu_email|answers_sensitive|password|token|audience_keys|audience_eligible)$`)

// TestDTO_SelfProfileHasNoTalentField (01 §0-7, 04 §V.1): the student's own
// profile carries the activity ranking CHOICE but no talent value (no
// talent, band, score, point or rank member anywhere in the tree), and its
// field set is exactly 04 §V.1's profile part.
func TestDTO_SelfProfileHasNoTalentField(t *testing.T) {
	t.Parallel()
	fields := jsonFields(reflect.TypeFor[students.StudentProfileSelfDTO]())
	for _, f := range fields {
		if f != "privacy.activity_ranking_choice" && forbiddenRe.MatchString(f) {
			t.Errorf("self profile member %q is forbidden", f)
		}
	}
	wantFields(t, "StudentProfileSelfDTO", fields, []string{
		"user_id", "university", "university.university_id", "university.name", "department", "department.department_id",
		"department.name", "degree_level", "class_year", "expected_graduation", "status", "headline", "about", "city",
		"avatar_url", "completed_sections", "skills", "skills[].skill_id", "skills[].name", "skills[].level", "experiences",
		"experiences[].experience_id", "experiences[].kind", "experiences[].title", "experiences[].organization_name",
		"experiences[].start_month", "experiences[].end_month", "experiences[].description", "languages",
		"languages[].language_code", "languages[].level", "privacy", "privacy.discoverable", "privacy.show_contact_on_grant",
		"privacy.show_clubs_on_grant", "privacy.activity_ranking_choice",
		"cvs", "cvs[].cv_id", "cvs[].file_name", "cvs[].is_current", "cvs[].created_at", // BE-025 (04 §V.1)
	})
	wantFields(t, "StudentPrivacyDTO", jsonFields(reflect.TypeFor[students.StudentPrivacyDTO]()),
		[]string{"discoverable", "show_contact_on_grant", "show_clubs_on_grant", "activity_ranking_choice"})
}

// TestDTO_NoLevelCarriesTalentField: no visibility type of any level
// carries a forbidden member (04 §V.0-2).
func TestDTO_NoLevelCarriesTalentField(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{
		reflect.TypeFor[students.AudienceCountDTO](), reflect.TypeFor[students.AnonProfileDTO](),
		reflect.TypeFor[students.StudentProfileL3DTO](), reflect.TypeFor[students.StudentL3CardDTO](),
		reflect.TypeFor[students.StudentSkillList](), reflect.TypeFor[students.StudentLanguageList](), reflect.TypeFor[students.StudentExperienceDTO](),
		reflect.TypeFor[students.StudentCVList](), reflect.TypeFor[students.StudentCVDownloadDTO](), reflect.TypeFor[students.MemberCardDTO](),
	} {
		for _, f := range jsonFields(typ) {
			if forbiddenRe.MatchString(f) {
				t.Errorf("%s member %q is forbidden", typ.Name(), f)
			}
		}
	}
}

// company is a fully filled source: every toggle on, a sensitive and a
// plain club, a club_role experience, contact and consents.
func company(t *testing.T) (students.CompanySource, uuid.UUID, students.Grant) {
	t.Helper()
	user, uni, dept, skill := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	year, lvl := int16(3), int16(4)
	grad, head, about, city, org, desc := ym(2027, 6), "Yazılımcı", "Hakkımda", "İstanbul", "Acme", "Açıklama"
	end := ym(2025, 9)
	up := t0.Add(-time.Hour)
	src := students.CompanySource{
		View: students.ProfileView{
			Profile: students.Profile{UserID: user, UniversityID: uni, DepartmentID: &dept, DegreeLevel: students.DegreeBachelor,
				ClassYear: &year, ExpectedGraduation: &grad, Status: students.ProfileActive, Headline: &head, About: &about,
				City: &city, CompletedSections: []string{"basics", "skills"}, AudienceKeys: []string{"all"}, AudienceEligible: true},
			Stored: true, UniversityName: "Boğaziçi", DepartmentName: "Bilgisayar", AvatarURL: "https://files.test/a",
			Skills:    []students.Skill{{UserID: user, SkillID: skill, Level: &lvl}},
			Languages: []students.Language{{UserID: user, LanguageCode: "en", Level: "C1"}},
			Experiences: []students.Experience{
				{ID: uuid.NewV7(), UserID: user, Kind: students.KindInternship, Title: "Stajyer", OrganizationName: &org,
					StartMonth: ym(2025, 7), EndMonth: &end, Description: &desc},
				{ID: uuid.NewV7(), UserID: user, Kind: students.KindClubRole, Title: "Başkan", OrganizationName: ptr("Hassas Kulüp"),
					StartMonth: ym(2024, 1)},
			},
			Privacy: students.Privacy{UserID: user, Discoverable: true, ShowContactOnGrant: true, ShowClubsOnGrant: true,
				ActivityRankingChoice: students.RankingEnabled},
		},
		FirstName: "Ada", LastName: "Yılmaz", Verified: true, DiscoveryConsent: true, ContactConsent: true,
		Email: "ada@example.com", SkillNames: map[uuid.UUID]string{skill: "Go"}, CVUploadedAt: &up,
		Clubs: []students.ClubFact{
			{OrgID: uuid.NewV7(), Name: "Satranç", Role: "member"},
			{OrgID: uuid.NewV7(), Name: "Hassas Kulüp", Role: "club_admin", Sensitive: true},
		},
	}
	companyA := uuid.NewV7()
	return src, companyA, students.Grant{CompanyOrgID: companyA, Active: true, GrantedAt: t0, Source: "contact_request",
		ContactRequestID: ptr(uuid.NewV7()), ContactShared: true}
}

// TestDTO_Level0_CountsOnly (04 §V.2 level 0): a number or null with
// below_threshold, never a person; 9 is null, 10 is the first number.
func TestDTO_Level0_CountsOnly(t *testing.T) {
	t.Parallel()
	wantFields(t, "AudienceCountDTO", jsonFields(reflect.TypeFor[students.AudienceCountDTO]()), []string{"count", "below_threshold"})
	for n, want := range map[int]string{-1: `{"count":null,"below_threshold":true}`, 0: `{"count":null,"below_threshold":true}`,
		9: `{"count":null,"below_threshold":true}`, 10: `{"count":10,"below_threshold":false}`, 420: `{"count":420,"below_threshold":false}`} {
		b, _ := json.Marshal(students.NewAudienceCount(n))
		if string(b) != want {
			t.Errorf("NewAudienceCount(%d) = %s, want %s", n, b, want)
		}
	}
}

// TestDTO_Level2_AnonProfileFields (04 §V.2 level 2): exactly the
// allow-listed members; no user ID, name, photo, free text, organisation,
// club, contact or CV; club_role experiences never; graduation as a year.
// Not discoverable (off, consent withdrawn, unverified, minor, graduated,
// no profile) → not found.
func TestDTO_Level2_AnonProfileFields(t *testing.T) {
	t.Parallel()
	src, _, _ := company(t)
	wantFields(t, "AnonProfileDTO type", jsonFields(reflect.TypeFor[students.AnonProfileDTO]()), []string{
		"anon_id", "university", "university.university_id", "university.name", "department", "department.department_id",
		"department.name", "degree_level", "class_year", "expected_graduation_year", "skills", "skills[].skill_id",
		"skills[].name", "skills[].level", "languages", "languages[].language_code", "languages[].level", "experiences",
		"experiences[].kind", "experiences[].title", "experiences[].start_year", "experiences[].end_year", "profile_completeness",
	})
	d, err := students.ToAnonProfile(src, "anon_7f3a")
	if err != nil {
		t.Fatal(err)
	}
	wantFields(t, "AnonProfileDTO answer", presentFields(t, d), jsonFields(reflect.TypeFor[students.AnonProfileDTO]()))
	if len(d.Experiences) != 1 || d.Experiences[0].Kind != "internship" || *d.ExpectedGraduationYear != 2027 ||
		d.ProfileCompleteness != 2 || d.Skills[0].Name != "Go" {
		t.Fatalf("level 2 = %+v", d)
	}
	b, _ := json.Marshal(d)
	for _, leak := range []string{src.View.Profile.UserID.String(), "Ada", "Yılmaz", "ada@example.com", "Acme", "Hakkımda",
		"Yazılımcı", "İstanbul", "Hassas", "Satranç", "files.test", "Başkan"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("level 2 leaks %q: %s", leak, b)
		}
	}
	for name, mutate := range map[string]func(*students.CompanySource){
		"level 2 off":       func(c *students.CompanySource) { c.View.Privacy.Discoverable = false },
		"consent withdrawn": func(c *students.CompanySource) { c.DiscoveryConsent = false },
		"unverified":        func(c *students.CompanySource) { c.Verified = false },
		"minor":             func(c *students.CompanySource) { c.IsMinor = true },
		"graduated":         func(c *students.CompanySource) { c.View.Profile.Status = students.ProfileGraduated },
		"no profile":        func(c *students.CompanySource) { c.View.Stored = false },
	} {
		c, _, _ := company(t)
		mutate(&c)
		if _, err := students.ToAnonProfile(c, "anon_7f3a"); !errors.Is(err, students.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
}

// TestDTO_Level3_ProfileFields (04 §V.3): with every toggle on the answer
// has exactly the allow-listed members, the non-sensitive club only and
// the e-mail; with the toggles off clubs, club_role entries and contact
// are gone; contact needs both the setting and the consent.
func TestDTO_Level3_ProfileFields(t *testing.T) {
	t.Parallel()
	all := []string{
		"user_id", "first_name", "last_name", "avatar_url", "university", "university.university_id", "university.name",
		"department", "department.department_id", "department.name", "degree_level", "class_year", "expected_graduation",
		"headline", "about", "city", "skills", "skills[].skill_id", "skills[].name", "skills[].level", "languages",
		"languages[].language_code", "languages[].level", "experiences", "experiences[].kind", "experiences[].title",
		"experiences[].organization_name", "experiences[].start_month", "experiences[].end_month", "experiences[].description",
		"clubs", "clubs[].org_id", "clubs[].name", "clubs[].role", "cv", "cv.available", "cv.uploaded_at", "contact",
		"contact.email", "grant", "grant.granted_at", "grant.source", "grant.contact_request_id",
	}
	wantFields(t, "StudentProfileL3DTO type", jsonFields(reflect.TypeFor[students.StudentProfileL3DTO]()), all)

	src, org, g := company(t)
	d, err := students.ToStudentProfileL3(src, org, g)
	if err != nil {
		t.Fatal(err)
	}
	wantFields(t, "level 3 (all on)", presentFields(t, d), all)
	if len(d.Clubs) != 1 || d.Clubs[0].Name != "Satranç" || d.Contact.Email != "ada@example.com" || len(d.Experiences) != 2 ||
		!d.CV.Available {
		t.Fatalf("level 3 = %+v", d)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "Hassas Kulüp\",\"role\"") || strings.Count(string(b), "club_admin") != 0 {
		t.Fatalf("a sensitive club reached level 3: %s", b)
	}

	// Toggles off: no clubs, no club_role entry, no contact.
	// The e-mail follows the grant's share (BE-089), not the setting.
	src.View.Privacy.ShowClubsOnGrant, g.ContactShared = false, false
	d, err = students.ToStudentProfileL3(src, org, g)
	if err != nil {
		t.Fatal(err)
	}
	off := slices.DeleteFunc(slices.Clone(all), func(f string) bool {
		return strings.HasPrefix(f, "clubs") || strings.HasPrefix(f, "contact")
	})
	wantFields(t, "level 3 (toggles off)", presentFields(t, d), off)
	if len(d.Experiences) != 1 || d.Experiences[0].Kind == "club_role" {
		t.Fatalf("club_role entry without show_clubs_on_grant: %+v", d.Experiences)
	}
	// The share without the consent (withdrawn in the ledger): no e-mail.
	g.ContactShared, src.ContactConsent = true, false
	if d, _ = students.ToStudentProfileL3(src, org, g); d.Contact != nil {
		t.Fatal("e-mail shared without the consent in force")
	}
	// Only sensitive clubs: the member is absent, not an empty list.
	src.View.Privacy.ShowClubsOnGrant = true
	src.Clubs = src.Clubs[1:]
	if d, _ = students.ToStudentProfileL3(src, org, g); d.Clubs != nil {
		t.Fatalf("clubs = %+v", d.Clubs)
	}
}

// TestDTO_Level3_OtherCompanyGrant_404 (03 Y5, IDOR): a grant is the
// granting company's only. Another company (with company A's grant in
// hand, or none), a revoked grant, a minor or a missing profile: not
// found, for the profile and the list card alike.
func TestDTO_Level3_OtherCompanyGrant_404(t *testing.T) {
	t.Parallel()
	src, companyA, g := company(t)
	companyB := uuid.NewV7()
	cases := map[string]func() (students.CompanySource, uuid.UUID, students.Grant){
		"company B with A's grant": func() (students.CompanySource, uuid.UUID, students.Grant) { return src, companyB, g },
		"company B's own grant revoked": func() (students.CompanySource, uuid.UUID, students.Grant) {
			gb := g
			gb.CompanyOrgID, gb.Active = companyB, false
			return src, companyB, gb
		},
		"nil viewer": func() (students.CompanySource, uuid.UUID, students.Grant) { return src, uuid.Nil(), g },
		"revoked": func() (students.CompanySource, uuid.UUID, students.Grant) {
			gr := g
			gr.Active = false
			return src, companyA, gr
		},
		"minor": func() (students.CompanySource, uuid.UUID, students.Grant) {
			m := src
			m.IsMinor = true
			return m, companyA, g
		},
		"no profile": func() (students.CompanySource, uuid.UUID, students.Grant) {
			m := src
			m.View.Stored = false
			return m, companyA, g
		},
	}
	for name, c := range cases {
		s, org, gr := c()
		if _, err := students.ToStudentProfileL3(s, org, gr); !errors.Is(err, students.ErrNotFound) {
			t.Errorf("%s: profile %v, want not found", name, err)
		}
		if _, err := students.ToStudentL3Card(s, org, gr); !errors.Is(err, students.ErrNotFound) {
			t.Errorf("%s: card %v, want not found", name, err)
		}
	}
	if _, err := students.ToStudentProfileL3(src, companyA, g); err != nil {
		t.Fatalf("company A's own grant: %v", err)
	}
}

// TestDTO_Level3_CardFields (04 §V.3 StudentL3CardDTO): names, avatar,
// university and department names, class year, grant time; nothing else.
func TestDTO_Level3_CardFields(t *testing.T) {
	t.Parallel()
	want := []string{"user_id", "first_name", "last_name", "avatar_url", "university", "university.name", "department",
		"department.name", "class_year", "granted_at"}
	wantFields(t, "StudentL3CardDTO type", jsonFields(reflect.TypeFor[students.StudentL3CardDTO]()), want)
	src, org, g := company(t)
	d, err := students.ToStudentL3Card(src, org, g)
	if err != nil {
		t.Fatal(err)
	}
	wantFields(t, "StudentL3CardDTO answer", presentFields(t, d), want)
}
