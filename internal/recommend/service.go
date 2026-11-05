package recommend

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/students"
)

// Limits of listMyRecommendations (04 §2.24).
const (
	// DefaultLimit and MaxLimit bound each list (events, clubs).
	DefaultLimit = 10
	MaxLimit     = 20
	// Horizon is how far ahead a recommended event may start.
	Horizon = 30 * 24 * time.Hour
	// Soon: an event starting within it carries starting_soon.
	Soon = 72 * time.Hour
	// Candidates bounds each candidate read of events (the matches of the
	// student's signals, the popular events).
	Candidates = events.MaxRecommendable
	// MaxFollows bounds the follows read as signals.
	MaxFollows = 200
	// maxCategories is how many attended categories are signals (most
	// events first).
	maxCategories = 3
	// maxPopularClubs bounds each popular club list read from students.
	maxPopularClubs = 20
)

// Reason is the code of a rule that picked an item (04 §2.24). The web and
// mobile apps show its Turkish label; the codes never carry a number.
type Reason string

// Reasons of an event.
const (
	// ReasonFollowedOrg: the organiser is a club or org the student follows
	// or is a member of ("Takip ettiğin kulüp").
	ReasonFollowedOrg Reason = "followed_org"
	// ReasonAttendedCategory: the category of events the student held a
	// place in during the last year ("Katıldığın etkinliklere benzer").
	ReasonAttendedCategory Reason = "attended_category"
	// ReasonSameUniversity: organised by the student's university or one of
	// its clubs ("Üniversitende").
	ReasonSameUniversity Reason = "same_university"
	// ReasonPopular: at least 10 held places ("Popüler"; k ≥ 10).
	ReasonPopular Reason = "popular"
	// ReasonStartingSoon: starts within Soon ("Yakında"); never alone.
	ReasonStartingSoon Reason = "starting_soon"
)

// Reasons of a club.
const (
	// ReasonAttendedOrganiser: the club organised an event the student held
	// a place in ("Etkinliğine katıldığın kulüp").
	ReasonAttendedOrganiser Reason = "attended_organiser"
	// ReasonPopularInDepartment: at least 10 students of the student's
	// department are members ("Bölümünde popüler").
	ReasonPopularInDepartment Reason = "popular_in_department"
	// ReasonPopularInUniversity: at least 10 students of the student's
	// university are members ("Üniversitende popüler").
	ReasonPopularInUniversity Reason = "popular_in_university"
)

// EventReasons and ClubReasons list the codes in the order an item shows
// them (the strongest rule first); weights rank the items.
var (
	EventReasons = []Reason{ReasonFollowedOrg, ReasonAttendedCategory, ReasonSameUniversity, ReasonPopular, ReasonStartingSoon}
	ClubReasons  = []Reason{ReasonAttendedOrganiser, ReasonPopularInDepartment, ReasonPopularInUniversity}
	weights      = map[Reason]int{
		ReasonFollowedOrg: 4, ReasonAttendedCategory: 2, ReasonSameUniversity: 2, ReasonPopular: 1, ReasonStartingSoon: 1,
		ReasonAttendedOrganiser: 3, ReasonPopularInDepartment: 2, ReasonPopularInUniversity: 1,
	}
)

// Query narrows List: Limit items per list (DefaultLimit when 0, at most
// MaxLimit).
type Query struct {
	Limit int
}

// Recommendations is the answer of List.
type Recommendations struct {
	Choice  students.RecommendationChoice
	Enabled bool
	Events  []EventItem
	Clubs   []ClubItem
}

// EventItem is a recommended event with the reasons that picked it.
type EventItem struct {
	View    events.EventView
	Reasons []Reason
}

// ClubItem is a recommended club with the reasons that picked it.
type ClubItem struct {
	Card    directory.ClubCard
	Reasons []Reason
}

// Enabled reports whether a student with choice gets recommendations: the
// student's own answer, or for "default" the product setting: on for an
// adult, off for a minor (BE-174; a child's data stays out of
// personalisation until the student turns it on).
func Enabled(c students.RecommendationChoice, minor bool) bool {
	switch c {
	case students.RankingEnabled:
		return true
	case students.RankingDisabled:
		return false
	}
	return !minor
}

// List implements Service. Order: authz (403/404) → the choice → when on,
// the signals, the events and the clubs.
func (s *service) List(ctx context.Context, a authz.Actor, q Query) (Recommendations, error) {
	me := a.UserID
	if err := authz.Can(ctx, a, authz.RecommendRecommendationListOwn,
		authz.Resource{Type: authz.TypeMyRecommendations, OwnerUserID: &me}); err != nil {
		return Recommendations{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	choice, err := s.d.Students.RecommendationChoice(ctx, me)
	if err != nil {
		return Recommendations{}, err
	}
	out := Recommendations{Choice: choice, Enabled: Enabled(choice, a.IsMinor), Events: []EventItem{}, Clubs: []ClubItem{}}
	if !out.Enabled {
		return out, nil
	}
	sig, err := s.signals(ctx, a)
	if err != nil {
		return Recommendations{}, err
	}
	if out.Events, err = s.events(ctx, a, sig, limit); err != nil {
		return Recommendations{}, err
	}
	if out.Clubs, err = s.clubs(ctx, a, sig, limit); err != nil {
		return Recommendations{}, err
	}
	return out, nil
}

// signals are a student's own signals; a sensitive club is never one.
type signals struct {
	mine       map[uuid.UUID]bool // every org followed or a member of, sensitive included: never recommended as a club
	followed   []uuid.UUID        // those of mine that are not sensitive clubs: followed_org
	university []uuid.UUID        // the student's university org and its clubs: same_university
	categories []events.Category  // the attended categories, most events first: attended_category
	organisers []uuid.UUID        // the attended organisers (never sensitive): attended_organiser
}

func (s *service) signals(ctx context.Context, a authz.Actor) (signals, error) {
	sig := signals{mine: map[uuid.UUID]bool{}}
	follows, err := s.d.Follows.FollowedOrgIDs(ctx, a.UserID, MaxFollows)
	if err != nil {
		return signals{}, err
	}
	var own []uuid.UUID
	add := func(id uuid.UUID) {
		if !sig.mine[id] {
			sig.mine[id] = true
			own = append(own, id)
		}
	}
	for _, id := range follows {
		add(id)
	}
	for _, m := range a.Memberships {
		add(m.OrgID)
	}
	if len(own) > 0 {
		sensitive, err := s.d.Clubs.SensitiveOrgIDs(ctx, own)
		if err != nil {
			return signals{}, err
		}
		for _, id := range own {
			if !sensitive[id] {
				sig.followed = append(sig.followed, id)
			}
		}
	}
	if a.IsVerifiedStudent() && a.UniversityID != nil {
		if sig.university, err = s.d.Clubs.UniversityOrgs(ctx, *a.UniversityID); err != nil {
			return signals{}, err
		}
	}
	ev, err := s.d.Events.RecommendationSignals(ctx, a.UserID)
	if err != nil {
		return signals{}, err
	}
	sig.categories = ev.Categories[:min(maxCategories, len(ev.Categories))]
	sig.organisers = ev.Organisers
	return sig, nil
}

// events picks the events: the candidates of events (the student's
// matches and the popular ones), each with its reasons, by weight, then
// the soonest; only the chosen ones become cards (covers are read for
// them alone).
func (s *service) events(ctx context.Context, a authz.Actor, sig signals, limit int) ([]EventItem, error) {
	now := s.d.Clock.Now().UTC()
	orgIDs := slices.Clone(sig.followed)
	for _, id := range sig.university {
		if !slices.Contains(orgIDs, id) {
			orgIDs = append(orgIDs, id)
		}
	}
	rows, err := s.d.Events.RecommendableEvents(ctx, a, events.RecommendQuery{
		OrgIDs: orgIDs, Categories: sig.categories, From: now, To: now.Add(Horizon), Limit: Candidates,
	})
	if err != nil {
		return nil, err
	}
	type scored struct {
		event   events.Event
		reasons []Reason
		score   int
	}
	var items []scored
	for _, e := range rows {
		rules := map[Reason]bool{
			ReasonFollowedOrg:      slices.Contains(sig.followed, e.OrgID),
			ReasonAttendedCategory: slices.Contains(sig.categories, e.Category),
			ReasonSameUniversity:   slices.Contains(sig.university, e.OrgID),
			ReasonPopular:          e.ConfirmedCount >= events.PopularThreshold,
		}
		reasons, score := pick(EventReasons, rules)
		if len(reasons) == 0 {
			continue // no rule of the student's picked it (a candidate is always one's match)
		}
		if e.StartsAt.Before(now.Add(Soon)) {
			reasons, score = append(reasons, ReasonStartingSoon), score+weights[ReasonStartingSoon]
		}
		items = append(items, scored{e, reasons, score})
	}
	slices.SortStableFunc(items, func(x, y scored) int {
		return cmp.Or(cmp.Compare(y.score, x.score), x.event.StartsAt.Compare(y.event.StartsAt), x.event.ID.Compare(y.event.ID))
	})
	items = items[:min(limit, len(items))]
	if len(items) == 0 {
		return []EventItem{}, nil
	}
	chosen := make([]events.Event, len(items))
	why := make(map[uuid.UUID][]Reason, len(items))
	for i, it := range items {
		chosen[i], why[it.event.ID] = it.event, it.reasons
	}
	views, err := s.d.Events.EventCards(ctx, chosen)
	if err != nil {
		return nil, err
	}
	out := make([]EventItem, 0, len(views))
	for _, v := range views {
		out = append(out, EventItem{View: v, Reasons: why[v.Event.ID]})
	}
	return out, nil
}

// clubs picks the clubs: the organisers of the student's events and the
// clubs popular in the student's department and university, without the
// student's own (followed or member), by weight, then in the order the
// rules named them; directory drops the sensitive and inactive ones.
func (s *service) clubs(ctx context.Context, a authz.Actor, sig signals, limit int) ([]ClubItem, error) {
	pop, err := s.d.Students.PopularClubs(ctx, a.UserID, maxPopularClubs)
	if err != nil {
		return nil, err
	}
	var order []uuid.UUID
	rules := map[uuid.UUID]map[Reason]bool{}
	name := func(ids []uuid.UUID, r Reason) {
		for _, id := range ids {
			if sig.mine[id] {
				continue
			}
			if rules[id] == nil {
				rules[id] = map[Reason]bool{}
				order = append(order, id)
			}
			rules[id][r] = true
		}
	}
	name(sig.organisers, ReasonAttendedOrganiser)
	name(pop.Department, ReasonPopularInDepartment)
	name(pop.University, ReasonPopularInUniversity)
	if len(order) == 0 {
		return []ClubItem{}, nil
	}
	score := func(id uuid.UUID) int {
		_, n := pick(ClubReasons, rules[id])
		return n
	}
	slices.SortStableFunc(order, func(x, y uuid.UUID) int { return cmp.Compare(score(y), score(x)) })
	cards, err := s.d.Clubs.RecommendableClubs(ctx, order[:min(len(order), directory.MaxRecommendableClubs)], limit)
	if err != nil {
		return nil, err
	}
	out := make([]ClubItem, 0, len(cards))
	for _, c := range cards {
		reasons, _ := pick(ClubReasons, rules[c.Row.OrgID])
		if len(reasons) > 0 {
			out = append(out, ClubItem{Card: c, Reasons: reasons})
		}
	}
	return out, nil
}

// pick returns the reasons of order that hold, in that order, and the sum
// of their weights.
func pick(order []Reason, hold map[Reason]bool) ([]Reason, int) {
	var out []Reason
	score := 0
	for _, r := range order {
		if hold[r] {
			out, score = append(out, r), score+weights[r]
		}
	}
	return out, score
}
