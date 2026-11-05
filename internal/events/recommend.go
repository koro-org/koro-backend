package events

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
)

// PopularThreshold is the k-anonymity floor of an event's popularity in the
// recommendations (BE-174): an event is popular from 10 held places
// (confirmed_count) on, and only the flag leaves, never the number.
const PopularThreshold = 10

// RecommendSignalWindow bounds the participations whose category and
// organiser are recommendation signals (BE-174): a year back, like
// TargetingWindow; a registration for an event that has yet to start is a
// signal too.
const RecommendSignalWindow = 365 * 24 * time.Hour

// MaxRecommendable bounds RecommendableEvents.
const MaxRecommendable = 50

// maxSignalRows bounds the (category, organiser) rows of one signal read.
const maxSignalRows = 200

// Recommendations is the event read of the rule-based recommendations
// (BE-174; recommend, uyum denetimi #25). The caller (recommend) authorises
// its own read first (cell O: the student's own data for the student only);
// events never imports it. Special-category data is never a signal and
// never a recommendation (03 Y2, KVKK md. 6): an event of a sensitive
// category or organised by a sensitive club is left out of both reads, the
// organiser's sensitivity read from directory (fail closed: an error is
// returned, nothing is recommended).
type Recommendations interface {
	// RecommendationSignals returns the categories and organisers of the
	// events userID held a place in (confirmed or checked in: a no-show,
	// a waitlisted, cancelled or rejected registration is no signal) that
	// started within RecommendSignalWindow before now or have yet to
	// start, most events first.
	RecommendationSignals(ctx context.Context, userID uuid.UUID) (RecommendationSignals, error)
	// RecommendableEvents returns up to 2 × q.Limit (q.Limit at most
	// MaxRecommendable) events a may see (03 §3.6, as a student and member:
	// no platform or staff path) that are published, start in [q.From,
	// q.To), are neither of a sensitive category nor organised by a
	// sensitive club or an org that is not active, and that a has no
	// registration of (whatever its status): up to q.Limit organised by one
	// of q.OrgIDs or of one of q.Categories, and up to q.Limit popular
	// (PopularThreshold) ones; soonest first. Rows only: the caller ranks
	// them and asks EventCards for the few it shows.
	RecommendableEvents(ctx context.Context, a authz.Actor, q RecommendQuery) ([]Event, error)
	// EventCards returns the list views of rows (events RecommendableEvents
	// returned) as listEvents builds them: the organiser, the cover URLs,
	// the shareable and targetable flags; in the order of rows, an event of
	// an organiser that is gone left out.
	EventCards(ctx context.Context, rows []Event) ([]EventView, error)
}

// RecommendationSignals are a student's own participation signals: most
// events first, then by category / org ID.
type RecommendationSignals struct {
	Categories []Category
	Organisers []uuid.UUID
}

// RecommendQuery narrows RecommendableEvents.
type RecommendQuery struct {
	OrgIDs     []uuid.UUID
	Categories []Category
	From, To   time.Time
	Limit      int
}

// SignalRow is one row of RecommendStore.RecommendSignals: the events of
// one category and organiser a user held a place in.
type SignalRow struct {
	Category Category
	OrgID    uuid.UUID
	Events   int
}

// RecommendFilter narrows RecommendStore.RecommendableEvents. The three
// matchers are ORed; none set matches nothing.
type RecommendFilter struct {
	Viewer       Viewer
	UserID       uuid.UUID // the events with a registration of this user are left out
	From, To     time.Time // starts_at in [From, To)
	OrgIDs       []uuid.UUID
	Categories   []Category
	MinConfirmed int // popular: at least this many held places (0: not this way)
}

// RecommendStore is the event reads of the recommendations (BE-174).
type RecommendStore interface {
	// RecommendSignals returns the (category, organiser) pairs of the
	// published or completed events, not of a sensitive category, starting
	// at or after since, that userID holds a confirmed or checked-in
	// registration of, with their number of events, most first, then by
	// category and org ID; at most 200 rows.
	RecommendSignals(ctx context.Context, userID uuid.UUID, since time.Time) ([]SignalRow, error)
	// RecommendableEvents returns up to limit published events visible to
	// f.Viewer, starting in [f.From, f.To), not of a sensitive category,
	// without a registration (any status) of f.UserID, that match one of
	// f's matchers, by starts_at then id.
	RecommendableEvents(ctx context.Context, f RecommendFilter, limit int) ([]Event, error)
}

var _ Recommendations = (*service)(nil)

// RecommendationSignals implements Recommendations.
func (s *service) RecommendationSignals(ctx context.Context, userID uuid.UUID) (RecommendationSignals, error) {
	out := RecommendationSignals{Categories: []Category{}, Organisers: []uuid.UUID{}}
	rows, err := s.d.Store.RecommendSignals(ctx, userID, s.now().Add(-RecommendSignalWindow))
	if err != nil || len(rows) == 0 {
		return out, err
	}
	var ids []uuid.UUID
	for _, r := range rows {
		if !slices.Contains(ids, r.OrgID) {
			ids = append(ids, r.OrgID)
		}
	}
	sensitive, err := s.sensitiveOrgs(ctx, ids)
	if err != nil {
		return RecommendationSignals{}, err
	}
	cats, orgIDs := map[Category]int{}, map[uuid.UUID]int{}
	for _, r := range rows {
		if sensitive[r.OrgID] {
			continue
		}
		cats[r.Category] += r.Events
		orgIDs[r.OrgID] += r.Events
	}
	out.Categories = byCount(cats, func(a, b Category) int { return cmp.Compare(a, b) })
	out.Organisers = byCount(orgIDs, func(a, b uuid.UUID) int { return a.Compare(b) })
	return out, nil
}

// byCount returns the keys of m, largest count first, then by key.
func byCount[K comparable](m map[K]int, key func(a, b K) int) []K {
	out := slices.Collect(maps.Keys(m))
	slices.SortFunc(out, func(a, b K) int { return cmp.Or(cmp.Compare(m[b], m[a]), key(a, b)) })
	return out
}

// RecommendableEvents implements Recommendations.
func (s *service) RecommendableEvents(ctx context.Context, a authz.Actor, q RecommendQuery) ([]Event, error) {
	if err := authz.Can(ctx, a, authz.EventsEventList, catalogResource); err != nil {
		return nil, err
	}
	out := []Event{}
	limit := min(max(q.Limit, 0), MaxRecommendable)
	if limit == 0 || !q.To.After(q.From) {
		return out, nil
	}
	f := RecommendFilter{Viewer: memberViewer(a), UserID: a.UserID, From: q.From, To: q.To}
	var rows []Event
	if len(q.OrgIDs) > 0 || len(q.Categories) > 0 {
		f.OrgIDs, f.Categories = q.OrgIDs, q.Categories
		got, err := s.d.Store.RecommendableEvents(ctx, f, limit)
		if err != nil {
			return nil, err
		}
		rows = got
	}
	f.OrgIDs, f.Categories, f.MinConfirmed = nil, nil, PopularThreshold
	popular, err := s.d.Store.RecommendableEvents(ctx, f, limit)
	if err != nil {
		return nil, err
	}
	for _, e := range popular {
		if !slices.ContainsFunc(rows, func(x Event) bool { return x.ID == e.ID }) {
			rows = append(rows, e)
		}
	}
	if len(rows) == 0 {
		return out, nil
	}
	sortByStart(rows)
	var ids []uuid.UUID
	for _, e := range rows {
		if !slices.Contains(ids, e.OrgID) {
			ids = append(ids, e.OrgID)
		}
	}
	sensitive, err := s.sensitiveOrgs(ctx, ids)
	if err != nil {
		return nil, err
	}
	os, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if o, ok := os[e.OrgID]; ok && !sensitive[e.OrgID] && o.Status == orgs.StatusActive {
			out = append(out, e)
		}
	}
	return out, nil
}

// EventCards implements Recommendations: listEvents' page builder over
// rows (one organiser read, one sensitivity read, the covers of rows).
func (s *service) EventCards(ctx context.Context, rows []Event) ([]EventView, error) {
	if len(rows) == 0 {
		return []EventView{}, nil
	}
	p, err := s.page(ctx, rows, len(rows), SortStartsAt, func(Event) bool { return false })
	if err != nil {
		return nil, err
	}
	return p.Items, nil
}
