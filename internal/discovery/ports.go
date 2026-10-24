package discovery

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/students"
	"koro/internal/talent"
)

// Students is what discovery needs from students (L7; 01 §4.9): the level
// 2 contract (Discoverable) and its bulk side (DiscoveryIndex, BE-051).
// None of these methods authorises: discovery decides who searches.
type Students interface {
	// IsDiscoverable reports whether userID may be found at level 2 now.
	IsDiscoverable(ctx context.Context, userID uuid.UUID) (bool, error)
	// DiscoveryMatches returns every discoverable student matching q with
	// the relevance inputs, at most most of them (T-68: fewer than 10 →
	// BelowThreshold).
	DiscoveryMatches(ctx context.Context, q students.DiscoveryQuery, most int) (students.DiscoveryMatchSet, error)
	// AnonProfiles returns the level 2 projections of the users among ids
	// who are discoverable now (at most 100).
	AnonProfiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]students.AnonProfile, error)
	// DiscoveryCandidatesAfter walks the profiles with level 2 on (no
	// read-time checks), up to 1000 user IDs > after per call.
	DiscoveryCandidatesAfter(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Ranking is talent's band read (L8; 01 §4.11), whose only consumer is
// discovery. It must be called outside a transaction.
type Ranking interface {
	// Bands returns the band of the users above neutral; a missing user is
	// neutral (no score, neutralised, collection off).
	Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]talent.Band, error)
	// BandMultipliers returns the multipliers of the bands of the talent
	// config version in force (bands[].mult_bp; BE-099); found is false
	// without one (discovery then applies ADR-0015's).
	BandMultipliers(ctx context.Context) (m talent.BandMultipliers, found bool, err error)
}

// Entitlements is the access right check of entitlements (L2; 03 §3.3:
// read endpoints check Available, k20).
type Entitlements interface {
	// Available returns what orgID may still use of f now.
	Available(ctx context.Context, orgID uuid.UUID, f entitlements.Feature) (entitlements.Availability, error)
}

// Orgs is orgs' org directory (L5): the company's kind and status (k20).
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
}

// AuditLogger is what discovery needs from audit (L1): every search and
// every profile read, in a transaction of their own.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}
