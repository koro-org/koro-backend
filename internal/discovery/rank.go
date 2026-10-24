package discovery

import (
	"bytes"
	"cmp"
	"context"
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
	// The band multipliers of ADR-0015 / R-003 §3.4, applied while no
	// talent config version is in force (otherwise the version's
	// bands[].mult_bp, BE-099): only upwards, at most +15 %; neutral (no
	// score, a low score, talent off) is ×1.00.
	boostBand1 = 10_500
	boostBand2 = 11_000
	boostBand3 = 11_500
	// boostMax is ADR-0015's ceiling: no multiplier, whatever its source,
	// lifts a result by more than ×1.15.
	boostMax = boostBand3
)

// boostBP is the ADR-0015 ranking multiplier of a talent band (no config
// version in force). The band itself goes no further than this package.
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

// configBoostBP is the ranking multiplier of a talent band under the
// multipliers of the config version in force, held to ADR-0015's range
// ×1.00–×1.15: talent validated them with their version, and a stray value
// still neither lowers a result nor lifts it past the ceiling.
func configBoostBP(m talent.BandMultipliers, b talent.Band) int64 {
	if b == talent.BandNeutral {
		return bpOne
	}
	return min(max(int64(m.Of(b)), bpOne), boostMax)
}

// boosts returns the ranking multiplier of the users among ids whose band
// is above neutral (a missing user is ×1.00): the band (talent.Ranking)
// under the multipliers of the config version in force, ADR-0015's without
// one. The multipliers are read only when some band is above neutral.
func (s *service) boosts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int64, error) {
	bands, err := s.d.Ranking.Bands(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int64, len(bands))
	if len(bands) == 0 {
		return out, nil
	}
	m, found, err := s.d.Ranking.BandMultipliers(ctx)
	if err != nil {
		return nil, err
	}
	for id, b := range bands {
		if found {
			out[id] = configBoostBP(m, b)
		} else {
			out[id] = boostBP(b)
		}
	}
	return out, nil
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

// rank orders the matches by relevance × band multiplier (descending; a
// user missing from boosts is ×1.00), then by the seeded tiebreak, then by
// user ID (a total order). skills and languages are the number of distinct
// requested values.
func rank(matches []students.DiscoveryMatch, boosts map[uuid.UUID]int64, skills, languages int, seed []byte) []uuid.UUID {
	type row struct {
		id    uuid.UUID
		score int64
		tie   uint64
	}
	rows := make([]row, len(matches))
	for i, m := range matches {
		boost, ok := boosts[m.UserID]
		if !ok {
			boost = bpOne
		}
		rows[i] = row{id: m.UserID, score: relevanceBP(m, skills, languages) * boost, tie: tiebreak(seed, m.UserID)}
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
