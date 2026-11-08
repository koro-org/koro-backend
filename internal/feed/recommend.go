package feed

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/civil"
)

// MaxFollowedOrgs bounds FollowedOrgIDs.
const MaxFollowedOrgs = 500

// Follows is the follow read of the rule-based recommendations (BE-174;
// recommend, uyum denetimi #25): a system read of one user's own follows
// (org_follows, manual or automatic), without authorisation: the caller
// authorises its own read first (cell O: the student's own data for the
// student only). Org IDs only, in one query; listMyFollows stays the
// paged, authorised HTTP read with the org cards.
type Follows interface {
	// FollowedOrgIDs returns up to limit (at most MaxFollowedOrgs) orgs
	// userID follows, newest follow first.
	FollowedOrgIDs(ctx context.Context, userID uuid.UUID, limit int) ([]uuid.UUID, error)
}

var _ Follows = (*service)(nil)

// FollowedOrgIDs implements Follows.
func (s *service) FollowedOrgIDs(ctx context.Context, userID uuid.UUID, limit int) ([]uuid.UUID, error) {
	limit = min(limit, MaxFollowedOrgs)
	if limit <= 0 || userID == uuid.Nil() {
		return []uuid.UUID{}, nil
	}
	rows, err := s.interactions().ListFollows(ctx, userID, nil, limit)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, len(rows))
	for i, f := range rows {
		out[i] = f.OrgID
	}
	return out, nil
}

// PopularThreshold is the k-anonymity floor of an opportunity call's
// popularity in the recommendations (BE-191): popular from 10 likes or 10
// saves on (each a count of distinct readers); only the flag leaves the
// module, never a number (the save count is the author org's alone).
const PopularThreshold = 10

// MaxRecommendableOpportunities bounds each candidate read of
// RecommendableOpportunities.
const MaxRecommendableOpportunities = 50

// MaxExcludedOpportunities bounds OpportunityQuery.Exclude (BE-198): the
// calls a student was contacted about in the window are few (each request
// spends a company's paid unit), the excess is ignored.
const MaxExcludedOpportunities = 200

// Opportunities is the opportunity call read of the rule-based
// recommendations (BE-191; recommend, uyum denetimi #25, R-7). The caller
// (recommend) authorises its own read first (cell O: the student's own
// data for the student only); feed never imports it. A call is a candidate
// only when the reader would see it in the feed (03 §3.6: published, in
// the audience, a followers call for a follower or member, no commercial
// call for a minor, not of a muted org, not hidden) and it is still open.
// Special-category data is never a signal and never a recommendation (03
// Y2, KVKK md. 6): a call of a sensitive club or addressed to a sensitive
// club's members (a club: audience key) is left out, the sensitivity read
// from directory (fail closed: an error is returned; without the catalog
// every club counts as sensitive).
type Opportunities interface {
	// RecommendableOpportunities returns up to 2 × q.Limit (q.Limit at
	// most MaxRecommendableOpportunities) opportunity calls a may see in
	// the feed, published at or after q.Since, whose deadline, if any, is
	// today or later (Europe/Istanbul), that a has neither liked nor saved,
	// not among q.Exclude, of an active org: up to q.Limit authored by one of q.OrgIDs, open
	// to a's own class year (eligible_years lists the year of a's profile)
	// or of one of q.Types (BE-204: the student's interests), and up to
	// q.Limit popular (PopularThreshold) ones; newest first.
	// Candidates only: the caller ranks them and asks OpportunityCards for
	// the few it shows.
	RecommendableOpportunities(ctx context.Context, a authz.Actor, q OpportunityQuery) ([]OpportunityCandidate, error)
	// OpportunityCards returns the reader views of rows (candidates
	// RecommendableOpportunities returned to a) as getFeed builds them,
	// in the order of rows; a row it did not return is left out.
	OpportunityCards(ctx context.Context, a authz.Actor, rows []OpportunityCandidate) ([]PostView, error)
}

var _ Opportunities = (*service)(nil)

// OpportunityQuery narrows RecommendableOpportunities.
type OpportunityQuery struct {
	OrgIDs []uuid.UUID // calls of these orgs (the student's signals)
	Since  time.Time   // published at or after
	Limit  int
	// Exclude are calls left out whatever matches them (BE-198: those a
	// company already contacted the student about); the first
	// MaxExcludedOpportunities count.
	Exclude []uuid.UUID
	// Types are opportunity types (OpportunityTypes) a call of matches
	// (BE-204: the types the student's interests map to); an unknown one
	// matches nothing.
	Types []string
}

// OpportunityCandidate is a recommendable opportunity call: what the rules
// read, without a count of people.
type OpportunityCandidate struct {
	PostID      uuid.UUID
	OrgID       uuid.UUID
	PublishedAt time.Time
	Opportunity Opportunity
	// ForYourYear: the call's eligible_years lists the reader's class year.
	ForYourYear bool
	// Popular: at least PopularThreshold likes or PopularThreshold saves.
	Popular bool

	post Post // the row OpportunityCards builds the card of
}

// OpportunityFilter narrows recommendStore.RecommendableOpportunities. The
// four matchers are ORed; none set matches nothing.
type OpportunityFilter struct {
	// Reader is the signed-in reader: the visibility of readerConds, and
	// whose mutes, hides, likes and saves leave calls out.
	Reader Reader
	Since  time.Time  // published_at at or after
	Today  civil.Date // a deadline before it is over
	// OrgIDs: authored by one of them.
	OrgIDs []uuid.UUID
	// ClassYear: eligible_years lists it (0: not this way).
	ClassYear int
	// Types: opportunity.type is one of them (BE-204).
	Types []string
	// MinInterest: like_count or save_count at least this (0: not this
	// way).
	MinInterest int
	// ExcludeIDs are left out whatever matches them (BE-198).
	ExcludeIDs []uuid.UUID
}

// recommendStore is the opportunity read of the recommendations (BE-191).
type recommendStore interface {
	// RecommendableOpportunities returns up to limit published opportunity
	// calls f.Reader may see (readerConds), published at or after f.Since,
	// without a deadline or with one on or after f.Today, of no org the
	// reader muted, neither hidden, liked nor saved by the reader, not in
	// f.ExcludeIDs, that match one of f's matchers, newest first by
	// (published_at, id). An anonymous reader gets none.
	RecommendableOpportunities(ctx context.Context, f OpportunityFilter, limit int) ([]Post, error)
}

// recommendations is the recommendation view of the store (BE-191).
func (s *service) recommendations() recommendStore { return s.d.Store }

// RecommendableOpportunities implements Opportunities: two store reads
// (the matches of the student's signals, the popular calls), the author
// orgs and one sensitivity read.
func (s *service) RecommendableOpportunities(ctx context.Context, a authz.Actor, q OpportunityQuery) ([]OpportunityCandidate, error) {
	if err := authz.Can(ctx, a, authz.FeedFeedRead, feedResource); err != nil {
		return nil, err
	}
	out := []OpportunityCandidate{}
	limit := min(max(q.Limit, 0), MaxRecommendableOpportunities)
	if limit == 0 || !a.IsUser() {
		return out, nil
	}
	r, err := s.readerOf(ctx, a)
	if err != nil {
		return nil, err
	}
	year := classYearOf(r.Keys)
	f := OpportunityFilter{Reader: r, Since: q.Since, Today: civil.Today(s.d.Clock.Now()),
		ExcludeIDs: q.Exclude[:min(len(q.Exclude), MaxExcludedOpportunities)]}
	var rows []Post
	types := knownTypes(q.Types)
	if len(q.OrgIDs) > 0 || year > 0 || len(types) > 0 {
		f.OrgIDs, f.ClassYear, f.Types = q.OrgIDs, year, types
		if rows, err = s.recommendations().RecommendableOpportunities(ctx, f, limit); err != nil {
			return nil, err
		}
	}
	f.OrgIDs, f.ClassYear, f.Types, f.MinInterest = nil, 0, nil, PopularThreshold
	popular, err := s.recommendations().RecommendableOpportunities(ctx, f, limit)
	if err != nil {
		return nil, err
	}
	for _, p := range popular {
		if !slices.ContainsFunc(rows, func(x Post) bool { return x.ID == p.ID }) {
			rows = append(rows, p)
		}
	}
	if rows, err = s.recommendable(ctx, rows); err != nil || len(rows) == 0 {
		return out, err
	}
	slices.SortFunc(rows, func(x, y Post) int {
		return cmp.Or(y.PublishedAt.Compare(*x.PublishedAt), y.ID.Compare(x.ID))
	})
	for _, p := range rows {
		o := decodeOpportunity(p.Opportunity)
		if o == nil {
			continue // posts_opportunity_chk: an opportunity_call always has one
		}
		out = append(out, OpportunityCandidate{
			PostID: p.ID, OrgID: p.OrgID, PublishedAt: *p.PublishedAt, Opportunity: *o,
			ForYourYear: year > 0 && slices.Contains(o.EligibleYears, year),
			Popular:     p.LikeCount >= PopularThreshold || p.SaveCount >= PopularThreshold,
			post:        p,
		})
	}
	return out, nil
}

// knownTypes returns the opportunity types among types, each once.
func knownTypes(types []string) []string {
	var out []string
	for _, t := range types {
		if slices.Contains(opportunityTypes, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// recommendable keeps the rows of an active org that is not a sensitive
// club and that address no sensitive club's members (03 Y2: the student's
// sensitive membership is never what makes a call a candidate). One
// sensitivity read over the author clubs and the club keys; without the
// catalog every club counts as sensitive (fail closed).
func (s *service) recommendable(ctx context.Context, rows []Post) ([]Post, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	var authors, clubs []uuid.UUID
	add := func(ids *[]uuid.UUID, id uuid.UUID) {
		if !slices.Contains(*ids, id) {
			*ids = append(*ids, id)
		}
	}
	for _, p := range rows {
		add(&authors, p.OrgID)
		if p.AuthorOrgKind == AuthorClub {
			add(&clubs, p.OrgID)
		}
		for _, id := range clubKeyIDs(p.AudienceKeys) {
			add(&clubs, id)
		}
	}
	sensitive := map[uuid.UUID]bool{}
	if len(clubs) > 0 {
		if s.d.Clubs == nil {
			for _, id := range clubs {
				sensitive[id] = true
			}
		} else {
			set, err := s.d.Clubs.SensitiveOrgIDs(ctx, clubs)
			if err != nil {
				return nil, err
			}
			sensitive = set
		}
	}
	os, err := s.d.Orgs.GetMany(ctx, authors)
	if err != nil {
		return nil, err
	}
	out := make([]Post, 0, len(rows))
	for _, p := range rows {
		o, ok := os[p.OrgID]
		if !ok || o.Status != orgs.StatusActive || sensitive[p.OrgID] ||
			slices.ContainsFunc(clubKeyIDs(p.AudienceKeys), func(id uuid.UUID) bool { return sensitive[id] }) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// OpportunityCards implements Opportunities: getFeed's page builder over
// the rows (one org read, one media read, the reader's state).
func (s *service) OpportunityCards(ctx context.Context, a authz.Actor, rows []OpportunityCandidate) ([]PostView, error) {
	posts := make([]Post, 0, len(rows))
	for _, c := range rows {
		if c.PostID != uuid.Nil() && c.post.ID == c.PostID && c.post.Kind == KindOpportunityCall {
			posts = append(posts, c.post)
		}
	}
	if len(posts) == 0 {
		return []PostView{}, nil
	}
	return s.views(ctx, a, posts, false)
}

// PostDTOOf is the reader DTO of a view (getFeed's item; BE-191: the card
// of a recommended opportunity call).
func PostDTOOf(v PostView) PostDTO { return toPostDTO(v) }

// classYearOf is the class year of a reader's profile keys (year:<n>);
// 0 without one.
func classYearOf(keys []string) int {
	for _, k := range keys {
		if v, ok := strings.CutPrefix(k, "year:"); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// clubKeyIDs are the clubs of a post's club: audience keys.
func clubKeyIDs(keys []string) []uuid.UUID {
	var out []uuid.UUID
	for _, k := range keys {
		if v, ok := strings.CutPrefix(k, "club:"); ok {
			if id, err := uuid.Parse(v); err == nil {
				out = append(out, id)
			}
		}
	}
	return out
}
