package talent

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/students"
)

// Students is what talent reads of the students module (01 §4.11:
// talent → students; students never knows talent). students.Service
// satisfies it. Both reads run on app_rw, before a talent transaction
// opens.
type Students interface {
	// Cohorts returns the cohort (with the minor flag) of the students
	// among userIDs; a user without a student profile is missing.
	Cohorts(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]students.Cohort, error)
	// RankingChoice returns the activity ranking choice of userID
	// (default without a settings row).
	RankingChoice(ctx context.Context, userID uuid.UUID) (students.RankingChoice, error)
}

// Directory reports sensitive clubs (01 §4.11: club membership signals
// leave sensitive clubs out). directory.Service satisfies it.
type Directory interface {
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// AuditLogger writes audit records; audit.Service satisfies it.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx (inside
	// a talent transaction: within Store.AsApp, on the session role).
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Reauthenticator verifies the re-authentication proof of the talent R
// operations (talent.neutralization.manage, talent.reversal.create,
// talent.config.manage; 03 §6.2): after authz.Can, before any business
// data. identity.Service satisfies it; it audits nothing for its caller
// (the refused proof is talent's denied record, audit.ReauthFailed).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Accounts reports when accounts were created (identity, 01 §4.6; BE-085):
// a like earns points only from an account at least
// min_account_age_days_for_likes days old at the like (02 §15.1, R-003
// §3.4). identity.Service satisfies it. Read on app_rw, before or between
// the talent transactions.
type Accounts interface {
	// AccountAge returns the creation instant of every account among
	// userIDs that exists.
	AccountAge(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]time.Time, error)
}

// Events is the participation read of the events module (BE-085): the
// organiser of an event and the figures behind min_event_confirmed and
// min_event_distinct_checkins (02 §15.1, R-003 §3.4). events is L8 like
// talent, so neither imports the other: the composition root adapts
// events.Participation. Read on app_rw, before or between the talent
// transactions.
type Events interface {
	// EventsOf maps registration and check-in IDs (the sources of the event
	// signals) to their event; an unknown ID is missing.
	EventsOf(ctx context.Context, sourceIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// EventFacts returns the figures of the events among eventIDs; an
	// unknown event is missing.
	EventFacts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]EventFacts, error)
}

// EventFacts are the participation figures of one event.
type EventFacts struct {
	OrgID uuid.UUID // the organiser
	// Confirmed counts the registrations that were confirmed and kept their
	// place (confirmed, checked in, no-show).
	Confirmed int
	// DistinctCheckins counts the distinct users with a valid check-in.
	DistinctCheckins int
}

// Memberships reports a user's active org memberships (orgs, 01 §4.7
// OrgDirectory.MembershipsOf; BE-085): an organiser of an event earns
// nothing at it (ADR-0015). orgs.Service satisfies it. Read on app_rw,
// before the talent transaction.
type Memberships interface {
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)
}
