package discovery_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/discovery"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/students"
	"koro/internal/talent"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// TestSearch_NoEntitlement_403 (03 k20, §7.7 "Hak yok"): a company without
// a running discovery_access right gets 403 entitlements.not_entitled for
// both operations — after the role check (a platform role is still 403
// forbidden, an outsider 404) and before any student is read or any
// search is audited.
func TestSearch_NoEntitlement_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ids := e.add(12, e.uniA, nil)
	anon, err := e.ps.AnonID(e.companyA, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	e.ents.entitled[e.companyA] = false
	for _, name := range []string{"company_member", "company_admin"} {
		if _, err := e.svc.Search(ctx, persona(t, name), e.companyA, discovery.Query{}); !errors.Is(err, entitlements.ErrNoEntitlement) {
			t.Fatalf("%s search without the right = %v, want entitlements.not_entitled", name, err)
		}
		if _, err := e.svc.Get(ctx, persona(t, name), e.companyA, anon); !errors.Is(err, entitlements.ErrNoEntitlement) {
			t.Fatalf("%s read without the right = %v, want entitlements.not_entitled", name, err)
		}
	}
	if _, err := e.svc.Search(ctx, persona(t, "platform_admin"), e.companyA, discovery.Query{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform_admin = %v, want 403 forbidden (role before right)", err)
	}
	if _, err := e.svc.Search(ctx, persona(t, "other_company_member"), e.companyA, discovery.Query{}); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("another company = %v, want 404", err)
	}
	if n := e.students.callCount("DiscoveryMatches") + e.students.callCount("DiscoveryCandidatesAfter"); n != 0 {
		t.Fatalf("students read %d times without the right", n)
	}
	if len(e.audit.events) != 0 {
		t.Fatalf("refused searches were audited: %v", e.audit.events)
	}

	// Through HTTP: the problem code of 03 §3.1.
	rec := get(t, newHTTP(t, e), "company_member", "/api/v1/orgs/"+e.companyA.String()+"/discovery/profiles")
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusForbidden || p.Code != "entitlements.not_entitled" {
		t.Fatalf("HTTP = %d %s", rec.Code, rec.Body)
	}

	// The right back (a grant): the search works again.
	e.ents.entitled[e.companyA] = true
	if _, err := e.svc.Search(ctx, persona(t, "company_member"), e.companyA, discovery.Query{}); err != nil {
		t.Fatal(err)
	}
}

// TestSearch_Below10_EmptyBelowThreshold (T-68, 04 §2.10, §V.2): when the
// filters match fewer than 10 discoverable students in total the answer is
// an empty list with below_threshold — no band is read, no profile is
// built, there is no cursor, and the audit record holds no total. At 10
// the students appear.
func TestSearch_Below10_EmptyBelowThreshold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := persona(t, "company_member")
	e.add(9, e.uniA, nil)
	e.add(30, e.uniB, nil)
	for _, limit := range []int{0, 1, 100} {
		p, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{UniversityIDs: []uuid.UUID{e.uniA}, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if !p.BelowThreshold || len(p.Items) != 0 || p.NextCursor != "" || p.Items == nil {
			t.Fatalf("9 matches (limit %d) = %+v; want an empty list with below_threshold", limit, p)
		}
	}
	if e.ranking.calls != 0 || e.students.callCount("AnonProfiles") != 0 {
		t.Fatalf("below the threshold: %d band reads, %d profile reads", e.ranking.calls, e.students.callCount("AnonProfiles"))
	}
	ev := e.audit.of(authz.DiscoveryProfileSearch)
	if len(ev) != 3 || ev[0].Metadata["below_threshold"] != true || ev[0].Metadata["shown"] != 0 {
		t.Fatalf("audit = %+v", ev)
	}
	for k := range ev[0].Metadata {
		if strings.Contains(k, "total") || strings.Contains(k, "count") || strings.Contains(k, "match") {
			t.Fatalf("audit metadata has a total: %s", k)
		}
	}

	// Through HTTP: below_threshold true and items [] (never null).
	rec := get(t, newHTTP(t, e), "company_member", "/api/v1/orgs/"+e.companyA.String()+"/discovery/profiles?university_id="+e.uniA.String())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"below_threshold":true`) {
		t.Fatalf("HTTP = %d %s", rec.Code, rec.Body)
	}

	e.add(1, e.uniA, nil)
	p, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{UniversityIDs: []uuid.UUID{e.uniA}})
	if err != nil || p.BelowThreshold || len(p.Items) != 10 {
		t.Fatalf("10 matches = %d items, below %v, %v", len(p.Items), p.BelowThreshold, err)
	}
}

// TestAnonID_StablePerCompany_DifferentAcrossCompanies (01 §4.16): a
// student's pseudonym is the same in every search and read of one company,
// differs between companies and between students, is 16 base32
// characters, and is never the user ID. A pseudonym of an older key of the
// ring still resolves (rotation).
func TestAnonID_StablePerCompany_DifferentAcrossCompanies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ids := e.add(12, e.uniA, nil)
	search := func(name string, org uuid.UUID) map[uuid.UUID]string {
		t.Helper()
		p, err := e.svc.Search(ctx, persona(t, name), org, discovery.Query{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		users := e.usersOf(t, org, p)
		out := map[uuid.UUID]string{}
		for i, it := range p.Items {
			if !discovery.ValidAnonID(it.AnonID) || strings.Contains(it.AnonID, users[i].String()) {
				t.Fatalf("anon_id %q", it.AnonID)
			}
			out[users[i]] = it.AnonID
		}
		if len(out) != len(ids) {
			t.Fatalf("%d of %d students", len(out), len(ids))
		}
		return out
	}
	a1, a2 := search("company_member", e.companyA), search("company_admin", e.companyA)
	b := search("other_company_member", e.companyB)
	seen := map[string]bool{}
	for _, id := range ids {
		if a1[id] != a2[id] {
			t.Fatalf("company_a's pseudonym changed between searches: %s vs %s", a1[id], a2[id])
		}
		if a1[id] == b[id] {
			t.Fatalf("company_a and company_b share pseudonym %s", a1[id])
		}
		if seen[a1[id]] || seen[b[id]] {
			t.Fatal("two students share a pseudonym")
		}
		seen[a1[id]], seen[b[id]] = true, true
		d, err := e.svc.Get(ctx, persona(t, "company_member"), e.companyA, a1[id])
		if err != nil || d.AnonID != a1[id] {
			t.Fatalf("read under the search pseudonym = %q, %v", d.AnonID, err)
		}
		if got, err := e.svc.AnonID(e.companyA, id); err != nil || got != a1[id] {
			t.Fatalf("AnonID = %q, %v; search said %q", got, err, a1[id])
		}
	}

	// A pseudonym derived under the older key of the ring (before the
	// rotation to k2) still resolves; a ring whose first id is not the
	// active key is refused.
	if _, err := discovery.NewPseudonyms(testDeriver(t, testKeys[0], testKeys[1]), []string{"k1", "k2"}); err == nil {
		t.Fatal("a ring whose first id is not the active key was accepted")
	}
	before, err := discovery.NewPseudonyms(testDeriver(t, testKeys[1]), []string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := before.AnonID(e.companyA, ids[0])
	if err != nil || old == a1[ids[0]] {
		t.Fatalf("older-key pseudonym %q, %v", old, err)
	}
	if id, found, err := e.svc.Resolve(ctx, e.companyA, old); err != nil || !found || id != ids[0] {
		t.Fatalf("older-key pseudonym = %v, %v, %v", id, found, err)
	}
}

// TestGetProfile_OtherCompanyAnonID_404 (03 k20, §7.2-5): company_b's
// pseudonym of a student does not resolve under company_a — the same 404
// as an unknown or malformed pseudonym — and resolves under company_b. The
// messaging hook answers the same way.
func TestGetProfile_OtherCompanyAnonID_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.add(1, e.uniA, nil)[0]
	ofB, err := e.ps.AnonID(e.companyB, id)
	if err != nil {
		t.Fatal(err)
	}
	ofA, err := e.ps.AnonID(e.companyA, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, anon := range []string{ofB, "aaaaaaaaaaaaaaaa", "not-a-pseudonym", "", strings.ToUpper(ofA), ofA + "a"} {
		if _, err := e.svc.Get(ctx, persona(t, "company_member"), e.companyA, anon); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("company_a reads %q = %v, want 404", anon, err)
		}
		if _, found, err := e.svc.Resolve(ctx, e.companyA, anon); err != nil || found {
			t.Fatalf("hook resolves %q under company_a: %v, %v", anon, found, err)
		}
	}
	d, err := e.svc.Get(ctx, persona(t, "other_company_member"), e.companyB, ofB)
	if err != nil || d.AnonID != ofB {
		t.Fatalf("company_b reads its own pseudonym = %+v, %v", d, err)
	}
	if got, found, err := e.svc.Resolve(ctx, e.companyB, ofB); err != nil || !found || got != id {
		t.Fatalf("hook under company_b = %v, %v, %v", got, found, err)
	}
	// Through HTTP: 404 not_found, the body names nobody.
	rec := get(t, newHTTP(t, e), "company_member", "/api/v1/orgs/"+e.companyA.String()+"/discovery/profiles/"+ofB)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), id.String()) {
		t.Fatalf("HTTP = %d %s", rec.Code, rec.Body)
	}
}

// TestGetProfile_NoLongerDiscoverable_404 (04 §2.10): a pseudonym the
// company already holds stops resolving once the student leaves level 2
// (setting off, consent withdrawn, verification lapsed, ...: students'
// read-time checks) — read and messaging hook alike — and the student
// leaves the search.
func TestGetProfile_NoLongerDiscoverable_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ids := e.add(11, e.uniA, nil)
	anon, err := e.ps.AnonID(e.companyA, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, persona(t, "company_admin"), e.companyA, anon); err != nil {
		t.Fatal(err)
	}
	e.setDiscoverable(ids[0], false)
	if _, err := e.svc.Get(ctx, persona(t, "company_admin"), e.companyA, anon); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("read after leaving level 2 = %v, want 404", err)
	}
	if _, found, err := e.svc.Resolve(ctx, e.companyA, anon); err != nil || found {
		t.Fatalf("hook after leaving level 2 = %v, %v", found, err)
	}
	p, err := e.svc.Search(ctx, persona(t, "company_admin"), e.companyA, discovery.Query{Limit: 100})
	if err != nil || len(p.Items) != 10 || slices.ContainsFunc(p.Items, func(d students.AnonProfileDTO) bool { return d.AnonID == anon }) {
		t.Fatalf("search after leaving = %d items, %v", len(p.Items), err)
	}
	// A student who is still a candidate but fails a read-time check
	// between the candidate walk and the profile read is not found either.
	e.students.mu.Lock()
	e.students.students[ids[1]].discoverable = false
	e.students.mu.Unlock()
	anon1, _ := e.ps.AnonID(e.companyA, ids[1])
	if _, err := e.svc.Get(ctx, persona(t, "company_admin"), e.companyA, anon1); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("read = %v, want 404", err)
	}
	if n := len(e.audit.of(authz.DiscoveryProfileRead)); n != 1 {
		t.Fatalf("%d read audit records, want 1 (refused reads are not)", n)
	}
}

// TestRanking_BandAffectsOrderOnly_NotInResponse (01 §4.16, ADR-0015): the
// talent band lifts a student among equally relevant students and changes
// nothing else — the same students on the same pages' union, the same
// profiles, no band in the answer; a band never beats a clearly more
// relevant student (×1.15 at most). With talent off (every band neutral)
// only relevance and the session's order rank.
func TestRanking_BandAffectsOrderOnly_NotInResponse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := persona(t, "company_member")
	go1, sql := uuid.NewV7(), uuid.NewV7()
	both := e.add(4, e.uniA, []uuid.UUID{go1, sql}) // relevance 2.0
	onlyGo := e.add(12, e.uniA, []uuid.UUID{go1})   // relevance 1.5
	q := discovery.Query{SkillIDs: []uuid.UUID{go1, sql}, Limit: 100}

	off, err := e.svc.Search(ctx, a, e.companyA, q)
	if err != nil {
		t.Fatal(err)
	}
	offUsers := e.usersOf(t, e.companyA, off)
	if !sameSet(offUsers[:4], both) {
		t.Fatal("talent off: the students holding both skills do not lead")
	}

	lifted := offUsers[len(offUsers)-1] // the last of the equally relevant students
	pos := slices.Index(offUsers, lifted)
	if !slices.Contains(onlyGo, lifted) {
		t.Fatal("the least relevant student holds both skills")
	}
	e.ranking.bands[lifted] = talent.BandMax
	e.ranking.bands[both[0]] = 1
	on, err := e.svc.Search(ctx, a, e.companyA, q)
	if err != nil {
		t.Fatal(err)
	}
	onUsers := e.usersOf(t, e.companyA, on)
	if !sameSet(onUsers, offUsers) || !sameSet(onUsers[:4], both) {
		t.Fatal("the band changed who is found, or beat a more relevant student")
	}
	if onUsers[4] != lifted || pos == 4 {
		t.Fatalf("band 3 did not lift its student among the equally relevant (was %d)", pos)
	}
	if onUsers[0] != both[0] {
		t.Fatal("band 1 did not lift its student among the most relevant")
	}
	if e.ranking.inTx {
		t.Fatal("Bands was called inside a transaction")
	}
	// The profiles are the same answers, whatever the band; the JSON
	// carries no band, score or rank.
	byAnon := func(p discovery.Page) map[string]students.AnonProfileDTO {
		m := map[string]students.AnonProfileDTO{}
		for _, d := range p.Items {
			m[d.AnonID] = d
		}
		return m
	}
	if !reflect.DeepEqual(byAnon(off), byAnon(on)) {
		t.Fatal("the band changed a profile")
	}
	body, err := json.Marshal(discovery.DiscoveryProfileList{Items: on.Items, BelowThreshold: on.BelowThreshold})
	if err != nil {
		t.Fatal(err)
	}
	if forbidden.Match(body) {
		t.Fatalf("talent field in the answer: %s", body)
	}
}

func sameSet(a, b []uuid.UUID) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	cmp := func(p, q uuid.UUID) int { return strings.Compare(p.String(), q.String()) }
	slices.SortFunc(x, cmp)
	slices.SortFunc(y, cmp)
	return slices.Equal(x, y)
}

// forbidden are the member names no level 2 answer may carry (01 §0-7,
// 04 §V.0-2, §V.2): talent values, rank or position, totals, the user ID.
var forbidden = regexp.MustCompile(`"(user_id|talent[a-z_]*|band[a-z_]*|score[a-z_]*|point[a-z_]*|rank[a-z_]*|position|order|relevance|total[a-z_]*|count|first_name|last_name|email)"`)

// TestResponse_NoScoreBandRankFields (04 §V.2, 01 §4.16): the answers of
// both operations — their types, recursively, and the bytes sent — have no
// score, band, rank, position, relevance, total or user ID member, and the
// student's user ID appears nowhere in a body or a cursor.
func TestResponse_NoScoreBandRankFields(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{reflect.TypeFor[discovery.DiscoveryProfileList](), reflect.TypeFor[students.AnonProfileDTO]()} {
		for _, f := range jsonFields(typ, "") {
			if forbidden.MatchString(`"` + f[strings.LastIndex(f, ".")+1:] + `"`) {
				t.Errorf("%s has member %s", typ, f)
			}
		}
	}
	e := newEnv(t)
	ids := e.add(30, e.uniA, nil)
	e.ranking.bands[ids[3]] = talent.BandMax
	app := newHTTP(t, e)
	base := "/api/v1/orgs/" + e.companyA.String() + "/discovery/profiles"
	rec := get(t, app, "company_member", base+"?limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d %s", rec.Code, rec.Body)
	}
	var list discovery.DiscoveryProfileList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 10 || list.NextCursor == "" {
		t.Fatalf("search body = %s (%v)", rec.Body, err)
	}
	rec2 := get(t, app, "company_member", base+"/"+list.Items[0].AnonID)
	for _, r := range []string{rec.Body.String(), rec2.Body.String(), list.NextCursor} {
		if forbidden.MatchString(r) {
			t.Fatalf("forbidden member in %s", r)
		}
		for _, id := range ids {
			if strings.Contains(r, id.String()) {
				t.Fatalf("a user ID leaked: %s", r)
			}
		}
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("read = %d %s", rec2.Code, rec2.Body)
	}
}

// jsonFields lists the JSON member paths of t (structs, slices, pointers).
func jsonFields(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, prefix+name)
		out = append(out, jsonFields(f.Type, prefix+name+".")...)
	}
	return out
}

// TestSearch_Audited (03 §6.5 addition, BE-051): every search — below the
// threshold and every later page included — is one discovery.profile.search
// record of the company (org and actor org), in a transaction, with the
// filters and the number of profiles shown, never a user ID or a total;
// every read one discovery.profile.read record whose student is named only
// in the metadata (not the org-visible resource ID). An audit that cannot
// be written answers nothing.
func TestSearch_Audited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := persona(t, "company_member")
	skill := uuid.NewV7()
	ids := e.add(12, e.uniA, []uuid.UUID{skill}, "en")
	q := discovery.Query{UniversityIDs: []uuid.UUID{e.uniA, e.uniA}, SkillIDs: []uuid.UUID{skill}, LanguageCodes: []string{"en"},
		ClassYears: []int16{3, 2}, DegreeLevel: students.DegreeBachelor, Limit: 5}
	p1, err := e.svc.Search(ctx, a, e.companyA, q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = p1.NextCursor
	if _, err := e.svc.Search(ctx, a, e.companyA, q); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{UniversityIDs: []uuid.UUID{e.uniB}}); err != nil {
		t.Fatal(err)
	}
	ev := e.audit.of(authz.DiscoveryProfileSearch)
	if len(ev) != 3 || e.audit.outTx != 0 {
		t.Fatalf("%d search records (%d outside a transaction)", len(ev), e.audit.outTx)
	}
	for i, x := range ev {
		if x.Outcome != audit.Success || x.OrgID == nil || *x.OrgID != e.companyA || x.ActorOrgID == nil ||
			*x.ActorOrgID != e.companyA || x.ActorUserID == nil || *x.ActorUserID != a.UserID ||
			x.ResourceType != authz.TypeAnonProfile || x.ResourceID != nil {
			t.Fatalf("record %d = %+v", i, x)
		}
		raw, _ := json.Marshal(x.Metadata)
		for _, id := range ids {
			if strings.Contains(string(raw), id.String()) {
				t.Fatalf("record %d names a student: %s", i, raw)
			}
		}
	}
	m := ev[0].Metadata
	if !reflect.DeepEqual(m["university_ids"], []string{e.uniA.String()}) || !reflect.DeepEqual(m["class_years"], []string{"2", "3"}) ||
		m["degree_level"] != "bachelor" || !reflect.DeepEqual(m["language_codes"], []string{"en"}) || m["shown"] != 5 ||
		m["first_page"] != true || m["below_threshold"] != false {
		t.Fatalf("first page metadata = %v", m)
	}
	if ev[1].Metadata["first_page"] != false || ev[2].Metadata["below_threshold"] != true {
		t.Fatalf("later records = %v / %v", ev[1].Metadata, ev[2].Metadata)
	}

	anon := p1.Items[0].AnonID
	if _, err := e.svc.Get(ctx, a, e.companyA, anon); err != nil {
		t.Fatal(err)
	}
	rd := e.audit.of(authz.DiscoveryProfileRead)
	who := e.usersOf(t, e.companyA, discovery.Page{Items: p1.Items[:1]})[0]
	if len(rd) != 1 || rd[0].ResourceID != nil || rd[0].Metadata[audit.MetaSubjectUserID] != who.String() ||
		rd[0].Metadata["anon_id"] != anon || *rd[0].OrgID != e.companyA {
		t.Fatalf("read record = %+v", rd)
	}

	e.audit.fail = errors.New("audit down")
	if p, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{}); err == nil || len(p.Items) != 0 {
		t.Fatalf("search without its audit record = %d items, %v", len(p.Items), err)
	}
	if d, err := e.svc.Get(ctx, a, e.companyA, anon); err == nil || d.AnonID != "" {
		t.Fatalf("read without its audit record = %+v, %v", d, err)
	}
}

// TestSearch_CursorPagesTheRankingForTheSession (04 §1.6): pages walk the
// whole ranking once (no student twice, none missing); the cursor holds
// only a position and is bound to the session's seed and the filters: in
// another session or with other filters it is 422 page.invalid_cursor, as
// is a forged one. A session keeps its order (the same first page on a
// repeated query); another session orders equally relevant students
// differently.
func TestSearch_CursorPagesTheRankingForTheSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := persona(t, "company_member")
	ids := e.add(23, e.uniA, nil)
	var all []uuid.UUID
	q := discovery.Query{Limit: 10}
	for range 5 {
		p, err := e.svc.Search(ctx, a, e.companyA, q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, e.usersOf(t, e.companyA, p)...)
		if p.NextCursor == "" {
			break
		}
		c, err := page.Decode(p.NextCursor, "discovery_rank", page.Fingerprint("x"), 1)
		if err == nil || c != nil {
			t.Fatal("a cursor decoded under another fingerprint")
		}
		q.Cursor = p.NextCursor
	}
	if len(all) != 23 || !sameSet(all, ids) {
		t.Fatalf("pages returned %d students", len(all))
	}

	first, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{Limit: 10})
	if err != nil || !reflect.DeepEqual(first.Items, again.Items) {
		t.Fatal("the same session got another order")
	}
	other := a
	other.SessionID = uuid.NewV7()
	o, err := e.svc.Search(ctx, other, e.companyA, discovery.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(e.usersOf(t, e.companyA, o), e.usersOf(t, e.companyA, first)) {
		t.Fatal("another session got the same order of 23 equally relevant students")
	}
	for name, tc := range map[string]struct {
		a discovery.Query
		b authz.Actor
	}{
		"another session": {discovery.Query{Cursor: first.NextCursor}, other},
		"other filters":   {discovery.Query{Cursor: first.NextCursor, UniversityIDs: []uuid.UUID{e.uniA}}, a},
		"forged":          {discovery.Query{Cursor: page.Next("discovery_rank", "0000000000000000", "10")}, a},
		"garbage":         {discovery.Query{Cursor: "%%%"}, a},
	} {
		if _, err := e.svc.Search(ctx, tc.b, e.companyA, tc.a); !errors.Is(err, page.ErrInvalidCursor) {
			t.Fatalf("%s: %v, want page.invalid_cursor", name, err)
		}
	}
	// Repeating a filter value does not change the cursor's filters.
	q2 := discovery.Query{UniversityIDs: []uuid.UUID{e.uniA, e.uniA}, Cursor: "", Limit: 5}
	p, err := e.svc.Search(ctx, a, e.companyA, q2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{UniversityIDs: []uuid.UUID{e.uniA}, Cursor: p.NextCursor, Limit: 5}); err != nil {
		t.Fatalf("canonical filters: %v", err)
	}
}

// TestSearch_RelevanceIsTheShareOfRequestedSkillsAndLanguages: students
// holding more of the requested skills and languages rank first; the
// exact filters (university) do not order.
func TestSearch_RelevanceIsTheShareOfRequestedSkillsAndLanguages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	s1, s2, s3 := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	top := e.add(2, e.uniA, []uuid.UUID{s1, s2, s3}, "en", "de")
	mid := e.add(3, e.uniA, []uuid.UUID{s1, s2}, "en")
	low := e.add(6, e.uniA, []uuid.UUID{s3}, "de")
	p, err := e.svc.Search(ctx, persona(t, "company_admin"), e.companyA,
		discovery.Query{SkillIDs: []uuid.UUID{s1, s2, s3}, LanguageCodes: []string{"en", "de"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := e.usersOf(t, e.companyA, p)
	if len(got) != 11 || !sameSet(got[:2], top) || !sameSet(got[2:5], mid) || !sameSet(got[5:], low) {
		t.Fatal("relevance order broken")
	}
}

// TestSearch_CompanyStatusAndRole (03 k20, §7.6): only an active company
// searches: suspended 403 orgs.org_suspended, unverified 403
// orgs.org_not_verified, closed or missing 404; club members see their
// surface and get 403; malformed filters are 422 only after the role
// check (an outsider still gets 404).
func TestSearch_CompanyStatusAndRole(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.add(10, e.uniA, nil)
	a := persona(t, "company_member")
	for st, want := range map[orgs.Status]error{
		orgs.StatusSuspended: orgs.ErrOrgSuspended, orgs.StatusPendingReview: orgs.ErrOrgNotVerified,
		orgs.StatusClosed: problem.ErrNotFound,
	} {
		o := e.orgs.orgs[e.companyA]
		o.Status = st
		e.orgs.orgs[e.companyA] = o
		if _, err := e.svc.Search(ctx, a, e.companyA, discovery.Query{}); !errors.Is(err, want) {
			t.Fatalf("%s company = %v, want %v", st, err, want)
		}
	}
	o := e.orgs.orgs[e.companyA]
	o.Status = orgs.StatusActive
	e.orgs.orgs[e.companyA] = o
	if _, err := e.svc.Search(ctx, persona(t, "club_admin"), e.club, discovery.Query{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("club_admin on its club = %v, want 403", err)
	}
	app := newHTTP(t, e)
	path := "/api/v1/orgs/" + e.companyA.String() + "/discovery/profiles?skill_id=nope"
	if rec := get(t, app, "student", path); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider with a malformed filter = %d", rec.Code)
	}
	if rec := get(t, app, "company_member", path); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("member with a malformed filter = %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, app, "company_member", "/api/v1/orgs/"+e.companyA.String()+"/discovery/profiles?class_year=9"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("class_year 9 = %d", rec.Code)
	}
	if rec := get(t, app, "company_member", "/api/v1/orgs/not-a-uuid/discovery/profiles"); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed org = %d", rec.Code)
	}
	ok := "/api/v1/orgs/" + e.companyA.String() + "/discovery/profiles?class_year=2&class_year=3&language_code=en&degree_level=bachelor"
	if rec := get(t, app, "company_member", ok); rec.Code != http.StatusOK {
		t.Fatalf("repeated filters = %d %s", rec.Code, rec.Body)
	}
}
