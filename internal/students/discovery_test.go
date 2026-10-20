package students_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// discoverable is an adult verified student with a stored profile at uni
// (department dept, class year 3) who turned level 2 on with the consent.
func (e *env) discoverable(t *testing.T, uni, dept uuid.UUID) authz.Actor {
	t.Helper()
	ctx := t.Context()
	a := e.student(t, uni, false)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &dept, ClassYear: ptr[int16](3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true), DiscoveryConsent: proof()}); err != nil {
		t.Fatal(err)
	}
	return a
}

func searchIDs(t *testing.T, p students.DiscoveryPage) []uuid.UUID {
	t.Helper()
	out := make([]uuid.UUID, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.UserID
	}
	return out
}

// TestDiscoverable_MinorFalse (01 §4.9, 03 Y4, 04 §V.0-3): a minor is never
// discoverable, even with the setting and the consent on (a birth date
// corrected after the fact), and never appears in a search. The other
// conditions hold the same way: no verification, the consent withdrawn in
// the consent module (T-237), the setting off, an inactive account, a
// graduated profile or no profile at all.
func TestDiscoverable_MinorFalse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.discoverable(t, e.uniA, e.deptA)
	if ok, err := e.svc.IsDiscoverable(ctx, a.UserID); err != nil || !ok {
		t.Fatalf("adult = %v, %v", ok, err)
	}
	e.users.setMinor(a.UserID, true)
	if ok, err := e.svc.IsDiscoverable(ctx, a.UserID); err != nil || ok {
		t.Fatalf("minor = %v, %v; want false", ok, err)
	}
	for range 11 {
		e.discoverable(t, e.uniA, e.deptA)
	}
	p, err := e.svc.Search(ctx, students.DiscoveryQuery{Limit: 50})
	if err != nil || p.BelowThreshold || len(p.Items) != 11 || slices.Contains(searchIDs(t, p), a.UserID) {
		t.Fatalf("search with a minor = %d items, below %v, %v", len(p.Items), p.BelowThreshold, err)
	}
	e.users.setMinor(a.UserID, false)

	for name, c := range map[string]struct {
		spoil, restore func(id uuid.UUID)
	}{
		"unverified": {func(id uuid.UUID) { e.users.lapse(id) }, func(id uuid.UUID) { e.users.verify(id, e.uniA, t0) }},
		"consent withdrawn elsewhere": {
			func(id uuid.UUID) { e.consent.mu.Lock(); e.consent.removed[id] = true; e.consent.mu.Unlock() },
			func(id uuid.UUID) { e.consent.mu.Lock(); delete(e.consent.removed, id); e.consent.mu.Unlock() },
		},
		"suspended": {
			func(id uuid.UUID) { e.setStatus(id, identity.StatusSuspended) },
			func(id uuid.UUID) { e.setStatus(id, identity.StatusActive) },
		},
	} {
		c.spoil(a.UserID)
		if ok, err := e.svc.IsDiscoverable(ctx, a.UserID); err != nil || ok {
			t.Errorf("%s = %v, %v; want false", name, ok, err)
		}
		c.restore(a.UserID)
		if ok, _ := e.svc.IsDiscoverable(ctx, a.UserID); !ok {
			t.Fatalf("%s: not restored", name)
		}
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.IsDiscoverable(ctx, a.UserID); ok {
		t.Fatal("setting off: still discoverable")
	}
	b := e.discoverable(t, e.uniA, e.deptA)
	p0 := e.profile(t, b.UserID)
	now := t0
	p0.Status, p0.GraduatedAt = students.ProfileGraduated, &now
	if _, err := e.store.UpdateProfile(ctx, p0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.IsDiscoverable(ctx, b.UserID); ok {
		t.Fatal("graduated profile: still discoverable")
	}
	if ok, err := e.svc.IsDiscoverable(ctx, uuid.NewV7()); err != nil || ok {
		t.Fatalf("no profile = %v, %v", ok, err)
	}
}

func (e *env) setStatus(id uuid.UUID, st identity.Status) {
	e.users.mu.Lock()
	defer e.users.mu.Unlock()
	s := e.users.sum[id]
	s.Status = st
	e.users.sum[id] = s
}

// TestSearch_Below10Total_EmptyWithBelowThreshold (T-68, 04 §2.10, §V.2):
// a filter that matches fewer than 10 discoverable students in total
// answers an empty page with below_threshold — whatever the page size, and
// counting only students who pass every read-time check (a stale flag of
// an unverified student does not lift a group over 10); at 10 the students
// appear; pages walk the result in user ID order; invalid filters are 422.
func TestSearch_Below10Total_EmptyWithBelowThreshold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	var ids []uuid.UUID
	for range 9 {
		ids = append(ids, e.discoverable(t, e.uniA, e.deptA).UserID)
	}
	ghost := e.discoverable(t, e.uniA, e.deptA)
	e.users.lapse(ghost.UserID) // the flag stays on, the verification is gone
	e.discoverable(t, e.uniB, e.deptB)
	for _, q := range []students.DiscoveryQuery{
		{UniversityIDs: []uuid.UUID{e.uniA}},
		{UniversityIDs: []uuid.UUID{e.uniA}, Limit: 1},
		{DepartmentIDs: []uuid.UUID{e.deptA}, ClassYears: []int16{3}, DegreeLevel: students.DegreeBachelor},
	} {
		p, err := e.svc.Search(ctx, q)
		if err != nil || !p.BelowThreshold || len(p.Items) != 0 || p.Next != nil {
			t.Fatalf("9 matching (%+v) = %d items, below %v, next %v, %v", q, len(p.Items), p.BelowThreshold, p.Next, err)
		}
	}
	ids = append(ids, e.discoverable(t, e.uniA, e.deptA).UserID)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	p, err := e.svc.Search(ctx, students.DiscoveryQuery{UniversityIDs: []uuid.UUID{e.uniA}})
	if err != nil || p.BelowThreshold || !slices.Equal(searchIDs(t, p), ids) || p.Next != nil {
		t.Fatalf("10 matching = %v, below %v, %v", searchIDs(t, p), p.BelowThreshold, err)
	}
	// Pages of 4: 4 + 4 + 2, in user ID order.
	var walked []uuid.UUID
	q := students.DiscoveryQuery{UniversityIDs: []uuid.UUID{e.uniA}, Limit: 4}
	for range 4 {
		p, err := e.svc.Search(ctx, q)
		if err != nil || p.BelowThreshold {
			t.Fatalf("page: %v, below %v", err, p.BelowThreshold)
		}
		walked = append(walked, searchIDs(t, p)...)
		if p.Next == nil {
			break
		}
		q.After = *p.Next
	}
	if !slices.Equal(walked, ids) {
		t.Fatalf("walk = %v\n want %v", walked, ids)
	}
	// The whole platform (no filter) is 11.
	if p, _ := e.svc.Search(ctx, students.DiscoveryQuery{Limit: 50}); len(p.Items) != 11 {
		t.Fatalf("no filter = %d items", len(p.Items))
	}
	for name, bad := range map[string]students.DiscoveryQuery{
		"class year": {ClassYears: []int16{9}}, "degree": {DegreeLevel: "phd"}, "language": {LanguageCodes: []string{"EN"}},
	} {
		if _, err := e.svc.Search(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Errorf("%s: %v, want 422", name, err)
		}
	}
}

// TestSearch_NoSensitiveClubFilter (03 §5.10, Y2; 04 §V.2): the query has
// no club (or org) filter of any kind and a result never names a club: a
// discoverable student who runs a sensitive club, with a club_role
// experience naming it, is found like anyone else and the level 2 answer
// carries neither the club nor the club_role entry; skills and languages
// filter (OR inside a filter).
func TestSearch_NoSensitiveClubFilter(t *testing.T) {
	t.Parallel()
	q := reflect.TypeFor[students.DiscoveryQuery]()
	clubish := regexp.MustCompile(`(?i)club|org|sensitive|member`)
	for i := range q.NumField() {
		if clubish.MatchString(q.Field(i).Name) {
			t.Fatalf("DiscoveryQuery.%s filters by club", q.Field(i).Name)
		}
	}
	for _, f := range jsonFields(reflect.TypeFor[students.AnonProfileDTO]()) {
		if clubish.MatchString(f) {
			t.Fatalf("AnonProfileDTO member %q names a club", f)
		}
	}

	e := newEnv(t)
	ctx := t.Context()
	var all []authz.Actor
	for range 10 {
		all = append(all, e.discoverable(t, e.uniA, e.deptA))
	}
	s := all[0]
	e.orgs.join(s.UserID, e.clubSens, authz.MemberClubAdmin)
	e.orgs.join(s.UserID, e.clubPlain, authz.MemberMember)
	if _, err := e.svc.CreateMyExperience(ctx, s, students.ExperienceInput{Kind: students.KindClubRole, Title: "Başkan",
		OrganizationName: "İnanç Topluluğu", StartMonth: ym(2024, 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMySkills(ctx, s, []students.SkillInput{{SkillID: e.skill1, Level: ptr[int16](3)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMyLanguages(ctx, all[1], []students.LanguageInput{{LanguageCode: "de", Level: "B2"}}); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.Search(ctx, students.DiscoveryQuery{Limit: 50})
	if err != nil || len(p.Items) != 10 {
		t.Fatalf("search = %d items, %v", len(p.Items), err)
	}
	i := slices.IndexFunc(p.Items, func(x students.AnonProfile) bool { return x.UserID == s.UserID })
	if i < 0 {
		t.Fatal("the sensitive club's admin is not found")
	}
	d, err := p.Items[i].WithAnonID("anon_test0000000")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	for _, leak := range []string{"İnanç", "Satranç", "club", e.clubSens.String(), e.clubPlain.String(), s.UserID.String(), "Başkan"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("level 2 answer leaks %q: %s", leak, b)
		}
	}
	if d.AnonID != "anon_test0000000" || len(d.Skills) != 1 || d.Skills[0].Name != "Go" || d.University.Name != "Boğaziçi Üniversitesi" {
		t.Fatalf("level 2 = %+v", d)
	}
	if _, err := p.Items[i].WithAnonID(""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("no pseudonym: %v", err)
	}
	// Filters: one skill or one language matches a single student: below 10.
	for _, q := range []students.DiscoveryQuery{{SkillIDs: []uuid.UUID{e.skill1}}, {LanguageCodes: []string{"de", "fr"}}} {
		if p, err := e.svc.Search(ctx, q); err != nil || !p.BelowThreshold {
			t.Fatalf("narrow filter %+v = below %v, %v", q, p.BelowThreshold, err)
		}
	}
}
