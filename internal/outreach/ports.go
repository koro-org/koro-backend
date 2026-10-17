package outreach

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/ratelimit"
	"koro/internal/students"
)

// AuditLogger is what outreach needs from audit (L1): every segment write
// is logged in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Audience is what outreach needs from students (L7; 01 §4.9 Audience):
// the size of a targeting through count_segment. It counts only audience
// eligible students (an active verification, an adult, an active account
// and profile) and answers students.ErrAudienceTooSmall below 10 without a
// number. It is students.Service.
type Audience interface {
	// Count returns the number of eligible students spec targets or
	// students.ErrAudienceTooSmall.
	Count(ctx context.Context, spec students.AudienceSpec) (int, error)
}

// Catalog is what outreach needs from directory (L6; 01 §4.8 Catalog): the
// sensitive club flag (03 Y2) and a university's org (the university_rep's
// own university, 03 k16). It is directory.Service.
type Catalog interface {
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs.
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// GetUniversity returns a university or directory.ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
}

// Orgs is what outreach needs from orgs (L5; 01 §4.7 OrgDirectory): the
// org's existence and status (03 §2.3). It is orgs.Service.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
}

// Limiter counts the audience counts of an org (the daily budget,
// Targeting). It is the platform's rate limiter (Redis in production).
type Limiter interface {
	Allow(ctx context.Context, rule ratelimit.Rule, key string) (ratelimit.Result, error)
}

// ---- dispatch (BE-035)

// Entitlements is what outreach needs from entitlements (L2; 01 §4.4): the
// targeted_dispatch quota of a company, consumed when a dispatch is
// scheduled (01 §6.3 "Hedefli gönderi yayını": in the caller's
// transaction) and given back when it is cancelled or never runs.
type Entitlements interface {
	// Consume uses one unit of the org's feature for ref (the dispatch) in
	// the caller's transaction; no usable right is
	// entitlements.ErrNoEntitlement (403 entitlements.not_entitled).
	//
	// sync-write: entitlements.Entitlements.Consume
	Consume(ctx context.Context, orgID uuid.UUID, f entitlements.Feature, qty int, ref entitlements.Ref) (uuid.UUID, error)
	// Release gives the unit of ref back in the caller's transaction (a
	// reversal row); a ref that consumed nothing is a no-op.
	//
	// sync-write: entitlements.Entitlements.Release
	Release(ctx context.Context, ref entitlements.Ref) error
}

// Recipients is the recipient walk of students (L7; 01 §4.9 Audience.Stream):
// every audience eligible adult student a targeting reaches, in batches,
// k ≥ 10 first (students.ErrAudienceTooSmall, fn never called). Sensitive
// club keys never match (03 Y2). It is students.Service.
type Recipients interface {
	Stream(ctx context.Context, spec students.AudienceSpec, fn func(userID uuid.UUID) error) error
}

// Members lists a club's active members (orgs, L5; 01 §4.7 OrgDirectory):
// the recipients of a club's notice to its own members (T-258). It is
// orgs.Service.
type Members interface {
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
}

// Users is what outreach needs from identity (L4; 01 §4.6 Users) in the
// worker: whether an account is active and adult (checked again for every
// batch: a minor never receives a dispatch), the recipient's language and,
// for e-mail, the address (01 §5.3: notify does not resolve recipients).
type Users interface {
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error)
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// Consents is the commercial message filter of consent (L2; 01 §4.3
// Checker, 6563/İYS): of a batch, the users whose consent of a purpose is
// in force.
type Consents interface {
	FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose consent.Purpose) ([]uuid.UUID, error)
}

// UnsubscribeLinks mints the one-click unsubscribe link of a commercial
// e-mail's recipient (consent, L2; BE-062): a signed, expiring token bound
// to the user and the purpose, which notify puts in List-Unsubscribe,
// List-Unsubscribe-Post and the footer (6563 ret, RFC 8058). It is
// consent.Module.Links.
type UnsubscribeLinks interface {
	UnsubscribeURL(userID uuid.UUID, purpose consent.Purpose) (string, error)
}

// Sender is notify (L3; 01 §4.5): one notification per recipient and
// channel, idempotent by its dedupe key.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Posts is what a dispatch needs from the feed (L9: above outreach, bound
// late by cmd/koro; 01 §5.3 "feed ↔ outreach"): a post's org, status,
// targeting and notification title.
type Posts interface {
	// DispatchPost returns the facts of postID or ErrNotFound.
	DispatchPost(ctx context.Context, postID uuid.UUID) (PostInfo, error)
}
