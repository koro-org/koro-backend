package discovery

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"uuid"

	"koro/internal/students"
	"koro/internal/talent"
)

// Ranking arithmetic in basis points (10 000 = ×1.00).
const (
	bpOne = 10_000
	// The band multipliers of ADR-0015 / R-003 §3.4: only upwards, at most
	// +15 %; neutral (no score, a low score, talent off) is ×1.00.
	boostBand1 = 10_500
	boostBand2 = 11_000
	boostBand3 = 11_500
)

// boostBP is the ranking multiplier of a talent band. The band itself
// goes no further than this function.
func boostBP(b talent.Band) int64 {
	switch b {
	case 1:
		return boostBand1
	case 2:
		return boostBand2
	case talent.BandMax:
		return boostBand3
	}
	return bpOne
}

// relevanceBP is the relevance of a match: 1 plus the share of the
// requested skills and the share of the requested languages the student
// holds (each 0 without that filter). The other filters are exact, so
// they do not order. Range ×1.00–×3.00.
func relevanceBP(m students.DiscoveryMatch, skills, languages int) int64 {
	r := int64(bpOne)
	if skills > 0 {
		r += bpOne * int64(min(m.SkillHits, skills)) / int64(skills)
	}
	if languages > 0 {
		r += bpOne * int64(min(m.LanguageHits, languages)) / int64(languages)
	}
	return r
}

// tiebreak is the seeded position of a student among equal scores: stable
// for a seed, unpredictable without it.
func tiebreak(seed []byte, userID uuid.UUID) uint64 {
	h := sha256.New()
	h.Write(seed)
	h.Write(userID[:])
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// rank orders the matches by relevance × band multiplier (descending),
// then by the seeded tiebreak, then by user ID (a total order). skills and
// languages are the number of distinct requested values.
func rank(matches []students.DiscoveryMatch, bands map[uuid.UUID]talent.Band, skills, languages int, seed []byte) []uuid.UUID {
	type row struct {
		id    uuid.UUID
		score int64
		tie   uint64
	}
	rows := make([]row, len(matches))
	for i, m := range matches {
		rows[i] = row{id: m.UserID, score: relevanceBP(m, skills, languages) * boostBP(bands[m.UserID]),
			tie: tiebreak(seed, m.UserID)}
	}
	slices.SortFunc(rows, func(a, b row) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		if c := cmp.Compare(a.tie, b.tie); c != 0 {
			return c
		}
		return bytes.Compare(a.id[:], b.id[:])
	})
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.id
	}
	return out
}
