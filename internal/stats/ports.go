package stats

import (
	"context"
	"time"
	"uuid"

	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/identity"
	"koro/internal/messaging"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/students"
)

// The ports of stats (05 §1.3: consumer side, narrow). Each is a system
// read of the module that owns the rows (its Statistics read, BE-163, or
// its directory read); stats authorises its own caller first. The
// composition root (cmd/koro/wire_stats.go) passes the modules' services.

// Orgs reads the org of the path (orgs.OrgDirectory).
type Orgs interface {
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
}

// Clubs reads a club's catalog row: its sensitivity (directory.Catalog).
type Clubs interface {
	GetClub(ctx context.Context, orgID uuid.UUID) (directory.Club, error)
}

// Users is the account counts (identity.UserStatistics).
type Users interface {
	UserStats(ctx context.Context, from, to time.Time) (identity.UserStats, error)
}

// OrgCounts is the org and membership counts (orgs.Statistics).
type OrgCounts interface {
	OrgStats(ctx context.Context, from, to time.Time) (orgs.OrgStats, error)
	MemberStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (orgs.MemberStats, error)
}

// Content is the post and report counts (feed.Statistics).
type Content interface {
	ContentStats(ctx context.Context, from, to time.Time) (feed.ContentStats, error)
	OrgPostStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (feed.OrgPostStats, error)
}

// Events is the event figures (events.Statistics).
type Events interface {
	EventStats(ctx context.Context, from, to time.Time) (events.EventStats, error)
	OrgEventFigures(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) ([]events.EventFigures, error)
	OrgEventTotals(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID]events.EventTotals, error)
}

// Sales is a seller's order counts (orders.Statistics).
type Sales interface {
	SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (orders.SalesStats, error)
}

// Outreach is an org's audiences and dispatch reach as estimates
// (outreach.Statistics).
type Outreach interface {
	OrgOutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (outreach.OutreachStats, error)
}

// Contacts is a company's contact request counts (messaging.Statistics).
type Contacts interface {
	ContactRequestStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (messaging.ContactRequestStats, error)
}

// Packages is a company's package usage (entitlements.Statistics).
type Packages interface {
	Usage(ctx context.Context, orgID uuid.UUID) ([]entitlements.FeatureUsage, error)
}

// Students is a university's student figures, groups below 10 suppressed
// (students.Statistics).
type Students interface {
	UniversityStudentStats(ctx context.Context, universityID uuid.UUID, from, to time.Time) (students.UniversityStudents, error)
}

// Universities is a university's clubs and claim queue, and the platform's
// claim queue (directory.Statistics).
type Universities interface {
	UniversityClubStats(ctx context.Context, universityOrgID uuid.UUID) (directory.UniversityClubs, error)
	PendingClaims(ctx context.Context) (int, error)
}

// Offerings is the university decision queue of a university's clubs
// (catalog.Statistics).
type Offerings interface {
	PendingUniversityDecisions(ctx context.Context, sellerOrgIDs []uuid.UUID) (int, error)
}
