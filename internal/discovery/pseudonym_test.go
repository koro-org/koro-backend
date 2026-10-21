package discovery_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/discovery"
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
