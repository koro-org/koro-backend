package discovery_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/discovery"
	"koro/internal/talent"
)

// multWorld is the ranking fixture of the band multiplier tests (BE-099):
// ten requested skills; x holds 7 (relevance ×1.7) at band 1, y holds 6
// (×1.6) at band 3, w holds 6 and is neutral, ten fillers hold 1 (×1.1).
// Under ADR-0015 (×1.05 / ×1.15) y leads x; the config version's
// multipliers decide between them, w and the fillers follow.
type multWorld struct {
	e       *env
	q       discovery.Query
	x, y, w uuid.UUID
	all     []uuid.UUID
}

func newMultWorld(t *testing.T) multWorld {
	t.Helper()
	e := newEnv(t)
	skills := make([]uuid.UUID, 10)
	for i := range skills {
		skills[i] = uuid.NewV7()
	}
	m := multWorld{e: e, q: discovery.Query{SkillIDs: skills, Limit: 100}}
	m.x = e.add(1, e.uniA, skills[:7])[0]
	m.y = e.add(1, e.uniA, skills[:6])[0]
	m.w = e.add(1, e.uniA, skills[4:])[0]
	fillers := e.add(10, e.uniA, skills[9:])
	m.all = append([]uuid.UUID{m.x, m.y, m.w}, fillers...)
	e.ranking.bands[m.x] = 1
	e.ranking.bands[m.y] = talent.BandMax
	return m
}

// search returns the students of one search in ranking order.
func (m multWorld) search(t *testing.T) []uuid.UUID {
	t.Helper()
	p, err := m.e.svc.Search(t.Context(), persona(t, "company_member"), m.e.companyA, m.q)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(discovery.DiscoveryProfileList{Items: p.Items, BelowThreshold: p.BelowThreshold})
	if err != nil {
		t.Fatal(err)
	}
	if forbidden.Match(body) || strings.Contains(string(body), "mult") {
		t.Fatalf("talent field in the answer: %s", body)
	}
	users := m.e.usersOf(t, m.e.companyA, p)
	if !sameSet(users, m.all) {
		t.Fatal("the multipliers changed who is found")
	}
	return users
}

// TestRanking_ConfigVersionMultipliersChangeTheOrder (00 T-328, BE-099):
// the multipliers of the talent config version in force (bands[].mult_bp)
// rank, ADR-0015's without a version; a new version changes the order at
// the next search; a band above the version's highest gets the highest
// band's multiplier. The multipliers never reach the answer and are read
// outside a transaction.
func TestRanking_ConfigVersionMultipliersChangeTheOrder(t *testing.T) {
	t.Parallel()
	m := newMultWorld(t)
	for _, tc := range []struct {
		name        string
		mults       talent.BandMultipliers
		first, next uuid.UUID
	}{
		{"no version: ADR-0015 ×1.05/×1.10/×1.15", nil, m.y, m.x},
		{"version ×1.10/×1.12/×1.15", talent.BandMultipliers{11000, 11200, 11500}, m.x, m.y},
		{"version of two bands: band 3 at ×1.10", talent.BandMultipliers{10500, 11000}, m.x, m.y},
		{"version ×1.01/×1.02/×1.15", talent.BandMultipliers{10100, 10200, 11500}, m.y, m.x},
	} {
		m.e.ranking.setMults(tc.mults)
		got := m.search(t)
		if got[0] != tc.first || got[1] != tc.next || got[2] != m.w {
			t.Errorf("%s: order starts %v, want %v %v %v", tc.name, got[:3], tc.first, tc.next, m.w)
		}
	}
	if m.e.ranking.inTx {
		t.Fatal("the talent read ran inside a transaction")
	}
}

// TestRanking_MultipliersHeldToADR0015Range (ADR-0015: only upwards, at
// most ×1.15): a multiplier outside 10001..11500 (talent validates every
// version, so only a broken read) neither lifts past ×1.15 nor lowers a
// banded student below ×1.00.
func TestRanking_MultipliersHeldToADR0015Range(t *testing.T) {
	t.Parallel()
	m := newMultWorld(t)
	// Unbounded y (×4.0) would lead x (×2.0); held at ×1.15 both, x leads.
	m.e.ranking.setMults(talent.BandMultipliers{20000, 30000, 40000})
	if got := m.search(t); got[0] != m.x || got[1] != m.y {
		t.Fatalf("lift above ×1.15: order starts %v", got[:2])
	}
	// Unbounded w (neutral ×1.6) would lead x (×1.7 × 0.5) and y; held at
	// ×1.00, x leads and y ties w.
	m.e.ranking.setMults(talent.BandMultipliers{5000, 6000, 7000})
	if got := m.search(t); got[0] != m.x || !sameSet(got[1:3], []uuid.UUID{m.y, m.w}) {
		t.Fatalf("penalty below ×1.00: order starts %v", got[:3])
	}
}

// TestRanking_MultipliersReadOnlyWithABand (BE-099): with every match
// neutral (talent off, no score) the multipliers are not read; with a band
// they are read once per search, and a failed read fails the search before
// anything is shown or audited.
func TestRanking_MultipliersReadOnlyWithABand(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.add(12, e.uniA, nil)
	e.ranking.setMults(talent.BandMultipliers{11000, 11200, 11500})
	a := persona(t, "company_member")
	if _, err := e.svc.Search(t.Context(), a, e.companyA, discovery.Query{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if e.ranking.multCalls != 0 {
		t.Fatalf("multipliers read %d times without a band", e.ranking.multCalls)
	}
	e.ranking.bands[e.students.sorted()[3]] = 2
	if _, err := e.svc.Search(t.Context(), a, e.companyA, discovery.Query{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if e.ranking.multCalls != 1 {
		t.Fatalf("multipliers read %d times with a band, want 1", e.ranking.multCalls)
	}
	boom := errors.New("talent down")
	e.ranking.multErr = boom
	if _, err := e.svc.Search(t.Context(), a, e.companyA, discovery.Query{Limit: 5}); !errors.Is(err, boom) {
		t.Fatalf("search with a failed read = %v", err)
	}
	if n := len(e.audit.of(authz.DiscoveryProfileSearch)); n != 2 {
		t.Fatalf("%d search records, want 2 (the failed search has none)", n)
	}
	if e.ranking.inTx {
		t.Fatal("the talent read ran inside a transaction")
	}
}
