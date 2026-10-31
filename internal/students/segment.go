package students

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"koro/internal/identity"
)

// AudienceSpec is a targeting (02 §9.1, §13.1): audience keys, OR inside a
// dimension and AND across dimensions. The dimensions are 'all', 'uni',
// 'dept', 'year', 'club', 'event' and 'attended' (BE-161: the
// registrants and the attendees of targetable events). Their number is
// derived from the keys
// (T-132), never taken from the caller: a wrong count would size another
// set than the one targeted.
type AudienceSpec struct {
	Keys []string // 'all', 'uni:<uuid>', 'dept:<uuid>', 'year:<1-8>', 'club:<uuid>', 'event:<uuid>', 'attended:<uuid>'
}

// Audience is the segment contract of outreach (01 §4.9): sizes with
// k ≥ 10 and the recipient walk of a dispatch. Talent values play no part.
type Audience interface {
	// Count returns the number of audience eligible students the spec
	// targets, through count_segment; below 10 it returns
	// ErrAudienceTooSmall and no number (04 §V.2).
	Count(ctx context.Context, spec AudienceSpec) (int, error)
	// Stream calls fn with every audience eligible adult student the spec
	// targets, in user_id order (worker; outreach dispatch). An audience
	// below 10 is ErrAudienceTooSmall and fn is never called; an error of
	// fn stops the walk and is returned.
	Stream(ctx context.Context, spec AudienceSpec, fn func(userID uuid.UUID) error) error
}

// Limits of a spec.
const (
	maxAudienceKeys = 500
	streamBatch     = batchSize
)

// segment is a compiled spec: the keys count_segment matches (sensitive
// clubs removed) and the number of dimensions the spec asked for.
type segment struct {
	keys []string
	dims int16
}

// dimensionOf validates one key and returns its dimension.
func dimensionOf(k string) (string, bool) {
	if k == KeyAll {
		return KeyAll, true
	}
	dim, val, ok := strings.Cut(k, ":")
	if !ok || val == "" {
		return "", false
	}
	switch dim + ":" {
	case keyUni, keyDept, keyClub, keyEvent, keyAttended:
		id, err := uuid.Parse(val)
		return dim, err == nil && id != uuid.Nil() && id.String() == val
	case keyYear:
		y, err := strconv.Atoi(val)
		return dim, err == nil && y >= 1 && y <= 8 && strconv.Itoa(y) == val
	}
	return "", false
}

// compile validates spec, derives its dimensions and removes the keys of
// sensitive clubs (03 Y2: special-category data never targets; the stored
// keys hold none either, this guards a stale key and a sloppy caller) and
// of events that are not targetable now (BE-161: members-only, sensitive,
// cancelled or too old; the same guard). A dimension left without a key
// matches nobody: the dimension count stays what the spec asked for, so
// removing a key never widens the audience.
func (s *service) compile(ctx context.Context, spec AudienceSpec) (segment, error) {
	if len(spec.Keys) == 0 || len(spec.Keys) > maxAudienceKeys {
		return segment{}, ErrInvalidAudience.Wrap(fmt.Errorf("students: %d audience keys", len(spec.Keys)))
	}
	keys := slices.Clone(spec.Keys)
	dims := map[string]bool{}
	var clubs []uuid.UUID
	for _, k := range keys {
		dim, ok := dimensionOf(k)
		if !ok {
			return segment{}, ErrInvalidAudience.Wrap(fmt.Errorf("students: malformed audience key %q", k))
		}
		dims[dim] = true
		if dim+":" == keyClub {
			clubs = append(clubs, uuid.MustParse(strings.TrimPrefix(k, keyClub)))
		}
	}
	if len(dims) > 1 && dims[KeyAll] { // every profile holds 'all': it narrows nothing
		delete(dims, KeyAll)
		keys = slices.DeleteFunc(keys, func(k string) bool { return k == KeyAll })
	}
	if len(clubs) > 0 {
		sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, clubs)
		if err != nil {
			return segment{}, fmt.Errorf("students: sensitive clubs: %w", err)
		}
		keys = slices.DeleteFunc(keys, func(k string) bool {
			id, found := strings.CutPrefix(k, keyClub)
			return found && sensitive[uuid.MustParse(id)]
		})
	}
	keys, err := s.dropUntargetable(ctx, keys)
	if err != nil {
		return segment{}, err
	}
	var n int16 // at most 6: uni, dept, year, club, event, attended ('all' alone is 1)
	for range dims {
		n++
	}
	return segment{keys: sortKeys(keys), dims: n}, nil
}

// size is count_segment (the threshold applies in the database and here).
func (s *service) size(ctx context.Context, seg segment) (int, error) {
	if len(seg.keys) == 0 {
		return 0, ErrAudienceTooSmall
	}
	n, ok, err := s.matches().CountSegment(ctx, seg.keys, seg.dims)
	switch {
	case err != nil:
		return 0, err
	case !ok || n < KThreshold:
		return 0, ErrAudienceTooSmall
	}
	return n, nil
}

// Count implements Audience.
func (s *service) Count(ctx context.Context, spec AudienceSpec) (int, error) {
	seg, err := s.compile(ctx, spec)
	if err != nil {
		return 0, err
	}
	return s.size(ctx, seg)
}

// Stream implements Audience. The walk reads only audience eligible
// profiles (count_segment's predicate); each batch is checked against
// identity again (an active adult account: a stale eligibility flag never
// reaches a minor, T-135).
func (s *service) Stream(ctx context.Context, spec AudienceSpec, fn func(userID uuid.UUID) error) error {
	seg, err := s.compile(ctx, spec)
	if err != nil {
		return err
	}
	if _, err := s.size(ctx, seg); err != nil {
		return err
	}
	after := uuid.Nil()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids, err := s.matches().AudienceAfter(ctx, seg.keys, seg.dims, after, streamBatch)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		after = ids[len(ids)-1]
		users, err := s.d.Users.GetMany(ctx, ids)
		if err != nil {
			return fmt.Errorf("students: users: %w", err)
		}
		for _, id := range ids {
			u, ok := users[id]
			if !ok || u.IsMinor || u.Status != identity.StatusActive {
				continue
			}
			if err := fn(id); err != nil {
				return err
			}
		}
	}
}
