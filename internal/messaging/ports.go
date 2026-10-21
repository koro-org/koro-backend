package messaging

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/students"
)

// AuditLogger is what messaging needs from audit (L1): every write in the
// business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Entitlements is what messaging needs from entitlements (L2; 01 §4.4):
// the company's contact_request quota, consumed in the request's
// transaction (01 §6.3 "İletişim talebi"). A unit is never given back: a
// withdrawn, declined or expired request keeps it used (04 §2.9).
type Entitlements interface {
	// Consume uses one unit of the org's feature for ref (the request) in
	// the caller's transaction; no usable right is
	// entitlements.ErrNoEntitlement (403 entitlements.not_entitled).
	//
	// sync-write: entitlements.Entitlements.Consume
	Consume(ctx context.Context, orgID uuid.UUID, f entitlements.Feature, qty int, ref entitlements.Ref) (uuid.UUID, error)
}

// AccessGrants is the level 3 contract of students (L7; 01 §4.9, §6.3
// "İletişim talebi kabulü"): the student's acceptance gives the company
// level 3 access in the acceptance's transaction.
type AccessGrants interface {
	// Grant gives companyOrgID level 3 access to studentID for the request
	// srcID in the transaction carried by ctx; a minor is 422
	// students.minor_restricted (and, from BE-077 on, an account without an
	// active student verification 403 students.verification_required).
	//
	// sync-write: students.AccessGrants.Grant
	Grant(ctx context.Context, studentID, companyOrgID uuid.UUID, src students.GrantSource, srcID uuid.UUID) error
}

// Discoverable is students' level 2 contract (L7; 01 §4.9): whether a
// student may be found now (level 2 on with its consent, an active
// verification, an adult, an active account and profile). It does not
// authorise: messaging decides who sends.
type Discoverable interface {
	IsDiscoverable(ctx context.Context, userID uuid.UUID) (bool, error)
}

// AnonResolver maps a company-scoped pseudonym (anon_id) to the student it
// was derived for, when that student is discoverable now (hooks.anonResolver,
// filled by discovery: 01 §5.3 "messaging ↔ discovery"). found=false for
// another company's pseudonym, a malformed one or a student who left level 2.
type AnonResolver func(ctx context.Context, companyOrgID uuid.UUID, anonID string) (userID uuid.UUID, found bool, err error)

// Pseudonyms derives the company's pseudonym of a student (discovery.Service.AnonID,
// L9): a company sees a request it sent under the pseudonym until the
// student accepts (04 §2.9).
type Pseudonyms interface {
	AnonID(companyOrgID, userID uuid.UUID) (string, error)
}

// Posts is what messaging needs from the feed (L9; 01 §4.14 Posts.Get): an
// opportunity call a request refers to.
type Posts interface {
	// Get returns the summary of a post (any status) or feed.ErrNotFound.
	Get(ctx context.Context, postID uuid.UUID) (feed.PostSummary, error)
}

// Orgs is orgs' org directory (L5; 01 §4.7 OrgDirectory): the company's
// kind and status (03 k19, §2.3), the company names of the student's views
// and the recipients of the company's notices.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
	// ActiveMemberIDs returns the active members of an org.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Sender is notify (L3; 01 §4.5): one in-app notice per recipient,
// idempotent by its dedupe key.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}
