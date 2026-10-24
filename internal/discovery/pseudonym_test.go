package discovery_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/discovery"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/students"
	"koro/internal/talent"
)

// TestPseudonyms_FormAndRing (01 §4.16): a pseudonym is 16 characters of
// lower-case base32, deterministic for (company, student) and key; the
// session seed differs per session and per company; a ring needs its
// active key first and only known ids.
func TestPseudonyms_FormAndRing(t *testing.T) {
	t.Parallel()
	ps := testPseudonyms(t)
	c, u := uuid.NewV7(), uuid.NewV7()
	a, err := ps.AnonID(c, u)
	if err != nil || len(a) != discovery.AnonIDLen || !discovery.ValidAnonID(a) || strings.ToLower(a) != a {
		t.Fatalf("anon = %q, %v", a, err)
	}
	if b, _ := ps.AnonID(c, u); b != a {
		t.Fatal("not deterministic")
	}
	if b, _ := ps.AnonID(u, c); b == a {
		t.Fatal("company and student are interchangeable")
	}
	if ok, err := ps.Matches(a, c, u); err != nil || !ok {
		t.Fatalf("Matches = %v, %v", ok, err)
	}
	if ok, _ := ps.Matches(a, uuid.NewV7(), u); ok {
		t.Fatal("matched under another company")
	}
	s1, err := ps.Seed(uuid.NewV7(), c)
	if err != nil || len(s1) != 32 {
		t.Fatalf("seed = %x, %v", s1, err)
	}
	s2, _ := ps.Seed(uuid.NewV7(), c)
	if string(s1) == string(s2) {
		t.Fatal("two sessions share a seed")
	}
	for _, s := range []string{"", "abc", "ABCDEFGHIJKLMNOP", "abcdefghijklmno1", "abcdefghijklmnopq", "abcdefghijklmn8p"} {
		if discovery.ValidAnonID(s) {
			t.Fatalf("%q is valid", s)
		}
	}
	for name, keys := range map[string][]string{"empty": nil, "not active first": {"k1", "k2"}, "unknown": {"k2", "k9"}} {
		if _, err := discovery.NewPseudonyms(testDeriver(t, testKeys...), keys); err == nil {
			t.Fatalf("%s ring accepted", name)
		}
	}
	if _, err := discovery.NewPseudonyms(nil, []string{"k2"}); err == nil {
		t.Fatal("nil deriver accepted")
	}
}

// TestRanking_Multipliers pins the band multipliers of ADR-0015 (only
// upwards, at most +15 %, neutral ×1.00) and the relevance shares.
func TestRanking_Multipliers(t *testing.T) {
	t.Parallel()
	for b, want := range map[talent.Band]int64{0: 10_000, 1: 10_500, 2: 11_000, 3: 11_500, 7: 10_000} {
		if got := discovery.BoostBP(b); got != want {
			t.Errorf("band %d = %d, want %d", b, got, want)
		}
	}
	for _, tc := range []struct {
		m                 students.DiscoveryMatch
		skills, languages int
		want              int64
	}{
		{students.DiscoveryMatch{}, 0, 0, 10_000},
		{students.DiscoveryMatch{SkillHits: 1}, 2, 0, 15_000},
		{students.DiscoveryMatch{SkillHits: 2, LanguageHits: 1}, 2, 3, 23_333},
		{students.DiscoveryMatch{SkillHits: 5}, 2, 0, 20_000}, // never above the share 1
	} {
		if got := discovery.RelevanceBP(tc.m, tc.skills, tc.languages); got != tc.want {
			t.Errorf("relevance %+v of %d/%d = %d, want %d", tc.m, tc.skills, tc.languages, got, tc.want)
		}
	}
}

// TestPseudonyms_RenewalWindow (BE-102, T-329): rotating discovery's keyring
// renames the students for new answers at once; during the renewal window
// (the previous key second in the ring) the pseudonyms a company kept
// still read, under the pseudonym asked for (no bridge to the new one);
// once the previous key leaves the ring they are the same 404 as an
// unknown pseudonym. The session seed follows the active key, so a search
// restarts after a rotation.
func TestPseudonyms_RenewalWindow(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // ring k2 (active), k1 (previous): the renewal window
	ctx := t.Context()
	ids := e.add(10, e.uniA, nil)
	member := persona(t, "company_member")

	before, err := discovery.NewPseudonyms(testDeriver(t, testKeys[1]), []string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := discovery.NewPseudonyms(testDeriver(t, testKeys[0]), []string{"k2"})
	if err != nil {
		t.Fatal(err)
	}
	retired, err := discovery.New(discovery.Deps{
		Students: e.students, Ranking: e.ranking, Entitlements: e.ents, Orgs: e.orgs, Audit: e.audit,
		Tx: db.NopTx{}, Pseudonyms: after, Log: obs.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}

	kept, err := before.AnonID(e.companyA, ids[0]) // what the company saved before the rotation
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := e.ps.AnonID(e.companyA, ids[0])
	if err != nil || renamed == kept {
		t.Fatalf("the rotation did not rename: %q, %q, %v", kept, renamed, err)
	}
	if got, _ := after.AnonID(e.companyA, ids[0]); got != renamed {
		t.Fatal("the active key alone derives another pseudonym")
	}
	p, err := e.svc.Search(ctx, member, e.companyA, discovery.Query{Limit: 100})
	if err != nil || len(p.Items) != len(ids) {
		t.Fatalf("search = %d items, %v", len(p.Items), err)
	}
	if !slices.ContainsFunc(p.Items, func(it students.AnonProfileDTO) bool { return it.AnonID == renamed }) ||
		slices.ContainsFunc(p.Items, func(it students.AnonProfileDTO) bool { return it.AnonID == kept }) {
		t.Fatal("new answers do not use the active key only")
	}

	// Renewal window: the kept pseudonym still reads, under itself.
	d, err := e.svc.Get(ctx, member, e.companyA, kept)
	if err != nil || d.AnonID != kept {
		t.Fatalf("kept pseudonym in the window = %q, %v", d.AnonID, err)
	}
	if id, found, err := e.svc.Resolve(ctx, e.companyA, kept); err != nil || !found || id != ids[0] {
		t.Fatalf("Resolve(kept) in the window = %v, %v, %v", id, found, err)
	}

	// The previous key left the ring: the kept pseudonym is unknown.
	if _, err := retired.Service.Get(ctx, member, e.companyA, kept); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("kept pseudonym after the window = %v, want 404", err)
	}
	if _, found, err := retired.Service.Resolve(ctx, e.companyA, kept); err != nil || found {
		t.Fatalf("Resolve(kept) after the window = %v, %v", found, err)
	}
	if d, err := retired.Service.Get(ctx, member, e.companyA, renamed); err != nil || d.AnonID != renamed {
		t.Fatalf("the active pseudonym after the window = %q, %v", d.AnonID, err)
	}

	session := uuid.NewV7()
	s1, _ := before.Seed(session, e.companyA)
	s2, _ := e.ps.Seed(session, e.companyA)
	s3, _ := after.Seed(session, e.companyA)
	if string(s1) == string(s2) || string(s2) != string(s3) {
		t.Fatal("the session seed does not follow the active key")
	}
}
