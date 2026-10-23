package students

import (
	"context"
	"errors"
	"slices"
	"uuid"
)

// The bulk side of level 2 for discovery's ranking (01 §4.16; BE-051).
// Search (BE-046) answers keyset pages in user_id order; discovery ranks the
// whole result (relevance × talent band, ties by a session seed) and reads
// a student by pseudonym, so it needs every matching student with the
// relevance inputs first, then the level 2 projections of one page only.
// None of these methods calls authz.Can: the caller decides who searches
// (discovery.profile.search / discovery.profile.read). Everything they
// return carries user IDs and never marshals.
const (
	// MaxDiscoveryMatches bounds how many students one ranking reads; the
	// walk stops there (DiscoveryMatchSet.Truncated).
	MaxDiscoveryMatches = 5000
	// discoveryMatchBatch is how many candidates DiscoveryMatches checks
	// per store call (the read-time checks run per batch).
	discoveryMatchBatch = 500
	// discoveryIDBatch bounds a page of DiscoveryCandidatesAfter and of the
	// relevance walks (user IDs only).
	discoveryIDBatch = 1000
	// maxAnonProfiles bounds one AnonProfiles call (a result page).
	maxAnonProfiles = 100
)

// DiscoveryMatch is one discoverable student of a level 2 search with the
// relevance inputs of discovery's ranking: how many of the requested
// skills and languages the student holds (0 when the query has no such
// filter). It carries the user ID: never an answer.
type DiscoveryMatch struct {
	UserID       uuid.UUID
	SkillHits    int
	LanguageHits int
}

// DiscoveryMatchSet is the result of DiscoveryMatches, in user_id order.
// BelowThreshold: fewer than 10 students match the filters in total, the
// set is empty (T-68). Truncated: more students match than were read.
type DiscoveryMatchSet struct {
	Matches        []DiscoveryMatch
	BelowThreshold bool
	Truncated      bool
}

// MarshalJSON refuses: a match set carries user IDs.
func (DiscoveryMatchSet) MarshalJSON() ([]byte, error) { return nil, errAnonProfileJSON }

// DiscoveryIndex is the bulk level 2 contract of discovery (BE-051), next
// to Discoverable. cmd/koro binds it from the students service.
type DiscoveryIndex interface {
	// DiscoveryMatches returns every discoverable student matching q (the
	// read-time checks of Search: account, age, verification, the consent
	// in force; T-237) up to most (≤ MaxDiscoveryMatches), with the
	// relevance inputs. q.After and q.Limit are ignored. Fewer than 10 in
	// total: an empty set with BelowThreshold (T-68). Invalid filters are
	// 422 as in Search.
	DiscoveryMatches(ctx context.Context, q DiscoveryQuery, most int) (DiscoveryMatchSet, error)
	// AnonProfiles returns the level 2 projections of the users among ids
	// (at most 100) who are discoverable now, each checked as
	// IsDiscoverable; the others are absent.
	AnonProfiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AnonProfile, error)
	// DiscoveryCandidatesAfter returns up to limit (≤ 1000) user IDs >
	// after, in user_id order, of the active profiles with level 2 on,
	// without the read-time checks: the walk discovery resolves a
	// pseudonym on (the match is then checked with AnonProfiles or
	// IsDiscoverable).
	DiscoveryCandidatesAfter(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error)
}

var _ DiscoveryIndex = (*service)(nil)

// DiscoveryMatches implements DiscoveryIndex: the walk of Search over every
// candidate (batch by batch through the read-time checks), then one ID walk
// per requested skill and language when a filter holds more than one value
// (with a single value every match holds it).
func (s *service) DiscoveryMatches(ctx context.Context, q DiscoveryQuery, most int) (DiscoveryMatchSet, error) {
	q, err := cleanDiscovery(q)
	if err != nil {
		return DiscoveryMatchSet{}, err
	}
	if most <= 0 || most > MaxDiscoveryMatches {
		most = MaxDiscoveryMatches
	}
	most = max(most, KThreshold)
	var ids []uuid.UUID
	truncated := false
walk:
	for after := uuid.Nil(); ; {
		batch, err := s.d.Store.DiscoveryCandidates(ctx, q, after, discoveryMatchBatch)
		if err != nil {
			return DiscoveryMatchSet{}, err
		}
		ok, err := s.discoverableAmong(ctx, batch)
		if err != nil {
			return DiscoveryMatchSet{}, err
		}
		for _, id := range batch {
			if !ok[id] {
				continue
			}
			if len(ids) == most {
				truncated = true
				break walk
			}
			ids = append(ids, id)
		}
		if len(batch) < discoveryMatchBatch {
			break
		}
		after = batch[len(batch)-1]
	}
	if len(ids) < KThreshold {
		return DiscoveryMatchSet{Matches: []DiscoveryMatch{}, BelowThreshold: true}, nil
	}
	out := DiscoveryMatchSet{Matches: make([]DiscoveryMatch, len(ids)), Truncated: truncated}
	at := make(map[uuid.UUID]int, len(ids))
	for i, id := range ids {
		out.Matches[i].UserID, at[id] = id, i
	}
	last := ids[len(ids)-1]
	switch len(q.SkillIDs) {
	case 0:
	case 1:
		for i := range out.Matches {
			out.Matches[i].SkillHits = 1
		}
	default:
		for _, skill := range q.SkillIDs {
			one := q
			one.SkillIDs = []uuid.UUID{skill}
			if err := s.hitWalk(ctx, one, last, func(i int) { out.Matches[i].SkillHits++ }, at); err != nil {
				return DiscoveryMatchSet{}, err
			}
		}
	}
	switch len(q.LanguageCodes) {
	case 0:
	case 1:
		for i := range out.Matches {
			out.Matches[i].LanguageHits = 1
		}
	default:
		for _, code := range q.LanguageCodes {
			one := q
			one.LanguageCodes = []string{code}
			if err := s.hitWalk(ctx, one, last, func(i int) { out.Matches[i].LanguageHits++ }, at); err != nil {
				return DiscoveryMatchSet{}, err
			}
		}
	}
	return out, nil
}

// hitWalk walks the candidates of q (a query narrowed to one skill or one
// language) up to last and calls hit for every one of them in at.
func (s *service) hitWalk(ctx context.Context, q DiscoveryQuery, last uuid.UUID, hit func(int), at map[uuid.UUID]int) error {
	for after := uuid.Nil(); ; {
		batch, err := s.d.Store.DiscoveryCandidates(ctx, q, after, discoveryIDBatch)
		if err != nil {
			return err
		}
		for _, id := range batch {
			if compareUUID(id, last) > 0 {
				return nil
			}
			if i, ok := at[id]; ok {
				hit(i)
			}
		}
		if len(batch) < discoveryIDBatch {
			return nil
		}
		after = batch[len(batch)-1]
	}
}

// errTooManyProfiles is a programming error of the caller (a page is at
// most 100 students).
var errTooManyProfiles = errors.New("students: AnonProfiles takes at most 100 users")

// AnonProfiles implements DiscoveryIndex.
func (s *service) AnonProfiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AnonProfile, error) {
	uniq := slices.Clone(ids)
	slices.SortFunc(uniq, compareUUID)
	uniq = slices.Compact(uniq)
	if len(uniq) > maxAnonProfiles {
		return nil, errTooManyProfiles
	}
	ok, err := s.discoverableAmong(ctx, uniq)
	if err != nil {
		return nil, err
	}
	var srcs []CompanySource
	for _, id := range uniq {
		if !ok[id] {
			continue
		}
		on, err := s.levelTwoOn(ctx, id)
		if err != nil {
			return nil, err
		}
		if !on {
			continue
		}
		src, found, err := s.anonSource(ctx, id)
		if err != nil {
			return nil, err
		}
		if found {
			srcs = append(srcs, src)
		}
	}
	if err := s.nameSkills(ctx, srcs); err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]AnonProfile, len(srcs))
	for _, src := range srcs {
		id := src.View.Profile.UserID
		out[id] = AnonProfile{UserID: id, dto: anonDTO(src)}
	}
	return out, nil
}

// levelTwoOn reports the module's own part of IsDiscoverable: an active
// profile whose settings have level 2 on.
func (s *service) levelTwoOn(ctx context.Context, userID uuid.UUID) (bool, error) {
	p, err := s.d.Store.GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case p.Status != ProfileActive:
		return false, nil
	}
	pr, err := s.d.Store.GetPrivacy(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return pr.Discoverable, nil
}

// DiscoveryCandidatesAfter implements DiscoveryIndex.
func (s *service) DiscoveryCandidatesAfter(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	if limit <= 0 || limit > discoveryIDBatch {
		limit = discoveryIDBatch
	}
	return s.d.Store.DiscoveryCandidates(ctx, DiscoveryQuery{}, after, limit)
}
