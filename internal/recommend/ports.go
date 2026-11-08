package recommend

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/students"
)

// The ports of recommend (05 §1.3: consumer side, narrow). Each is a system
// read of the module that owns the rows; recommend authorises its own
// caller first. The composition root (cmd/koro/wire_recommend.go) passes
// the modules' services.

// Students is the student's recommendation choice and the clubs popular in
// the student's department and university (students.Recommendations).
type Students interface {
	RecommendationChoice(ctx context.Context, userID uuid.UUID) (students.RecommendationChoice, error)
	PopularClubs(ctx context.Context, userID uuid.UUID, limit int) (students.PopularClubs, error)
}

// Events is the student's participation signals, the candidate events and
// the cards of the chosen ones (events.Recommendations).
type Events interface {
	RecommendationSignals(ctx context.Context, userID uuid.UUID) (events.RecommendationSignals, error)
	RecommendableEvents(ctx context.Context, a authz.Actor, q events.RecommendQuery) ([]events.Event, error)
	EventCards(ctx context.Context, rows []events.Event) ([]events.EventView, error)
}

// Clubs is the sensitivity of the student's clubs, the orgs of the
// student's university (directory.Catalog, directory.Service) and the cards
// of the recommended clubs (directory.Recommendations).
type Clubs interface {
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)
	RecommendableClubs(ctx context.Context, orgIDs []uuid.UUID, limit int) ([]directory.ClubCard, error)
}

// Follows is the orgs the student follows (feed.Follows).
type Follows interface {
	FollowedOrgIDs(ctx context.Context, userID uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Opportunities is the opportunity calls the student may see in the feed
// and the cards of the chosen ones (feed.Opportunities, BE-191).
type Opportunities interface {
	RecommendableOpportunities(ctx context.Context, a authz.Actor, q feed.OpportunityQuery) ([]feed.OpportunityCandidate, error)
	OpportunityCards(ctx context.Context, a authz.Actor, rows []feed.OpportunityCandidate) ([]feed.PostView, error)
}

// ContactRequests is the opportunity calls a company already contacted the
// student about (messaging.ContactedOpportunities, BE-198): left out of
// the recommendations. Call IDs only; messaging is never imported here.
type ContactRequests interface {
	ContactedOpportunityPostIDs(ctx context.Context, userID uuid.UUID, since time.Time, limit int) ([]uuid.UUID, error)
}
