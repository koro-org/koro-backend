package outreach

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"koro/internal/platform/problem"
	"koro/internal/students"
)

// Audience estimates (BE-203, Q-53; 06 H-61): how the exact size of a
// definition becomes the number an org sees.
//
//   - The value floor: in a definition that needs k ≥ 10, every value of a
//     dimension with several values must reach KThreshold students together
//     with the other dimensions, or the count is refused (422
//     outreach.audience_value_too_small at that value, no number). A
//     dimension with a single value is the whole audience, which k already
//     covers.
//   - The offset: before the rounding, a fixed offset of at most half a
//     bucket step is added (Noise). It is drawn under a secret key from the
//     canonical definition and its exact size, so the same audience always
//     shows the same number — counting it again shows nothing new — and the
//     offset changes only when the audience does. The org is not part of
//     the seed: every org sees the same number for the same definition. A
//     size the offset takes below KThreshold is shown as below the
//     threshold (a compile that needs k then fails with
//     outreach.audience_too_small).
//
// The exact size and the offset stay inside outreach: neither is returned,
// stored or logged.

// noiseSeedVersion versions the seed layout: a new version draws new
// offsets for every estimate.
const noiseSeedVersion = "v1"

// noiseHalf is the offset range of an exact size: half its bucket step (5
// below 100, 25 below 1 000, 50 below 10 000, then 500), so an estimate is
// at most one bucket away from the plain bucket of the size.
func noiseHalf(n int) int { return bucketStep(n) / 2 }

// noiseSeed is the seed of a normalized definition whose exact size is n:
// the seed version, the sorted audience keys (the canonical form: the
// order and the duplicates of a request do not matter) and n.
func noiseSeed(def Definition, n int) []byte {
	return []byte(noiseSeedVersion + "\n" + strings.Join(def.Keys(), "\n") + "\n#" + strconv.Itoa(n))
}

// publish is the published estimate of a normalized definition whose exact
// size is n (≥ KThreshold): the offset, then PublicEstimate.
func (s *service) publish(def Definition, n int) (Estimate, error) {
	off, err := s.d.Noise.Offset(noiseSeed(def, n), noiseHalf(n))
	if err != nil {
		return Estimate{}, err
	}
	return PublicEstimate(n + off), nil
}

// measure sizes a normalized definition: students' count_segment, then —
// with floor (a definition that needs k) — the value floor, then publish.
// A refused value comes back as a *smallValue with a zero Estimate. Below
// KThreshold there is no number and the floor is not consulted.
func (s *service) measure(ctx context.Context, def Definition, floor bool) (Estimate, *smallValue, error) {
	n, err := s.d.Audience.Count(ctx, students.AudienceSpec{Keys: def.Keys()})
	switch {
	case errors.Is(err, students.ErrAudienceTooSmall):
		return Estimate{BelowThreshold: true}, nil, nil
	case err != nil:
		return Estimate{}, nil, err
	}
	if floor {
		small, err := s.belowFloor(ctx, def)
		if err != nil || small != nil {
			return Estimate{}, small, err
		}
	}
	est, err := s.publish(def, n)
	return est, nil, err
}

// smallValue is a value the floor refuses: its dimension and its value as
// text (Definition.values).
type smallValue struct{ dim, value string }

// belowFloor returns the first value — in dimensionNames order, then in
// the definition's (sorted) order — of a dimension with several values
// that reaches fewer than KThreshold students together with the other
// dimensions, or nil when every such value reaches it.
func (s *service) belowFloor(ctx context.Context, def Definition) (*smallValue, error) {
	for _, name := range dimensionNames {
		vals := def.values(name)
		if len(vals) < 2 {
			continue
		}
		for i := range vals {
			_, err := s.d.Audience.Count(ctx, students.AudienceSpec{Keys: def.only(name, i).Keys()})
			switch {
			case errors.Is(err, students.ErrAudienceTooSmall):
				return &smallValue{dim: name, value: vals[i]}, nil
			case err != nil:
				return nil, err
			}
		}
	}
	return nil, nil
}

// at is the refusal of v as the caller sees it: 422
// outreach.audience_value_too_small at the value's place in raw, the
// definition as the caller wrote it at loc. It carries no number.
func (v smallValue) at(loc string, raw Definition) error {
	where := v.dim
	if i := slices.Index(raw.values(v.dim), v.value); i >= 0 {
		where += "[" + strconv.Itoa(i) + "]"
	}
	if loc != "" {
		where = loc + "." + where
	}
	return ErrAudienceValueTooSmall.WithFields(problem.FieldError{Location: where,
		Message: "with the other dimensions, this value reaches too few students to be counted"})
}
