package feed

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/students"
)

// AuditLogger is what feed needs from audit (L1): every write is logged in
// the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is what feed needs from orgs (L5; 01 §4.7 OrgDirectory): the author
// org's kind and status (03 §2.3) and its public name for the DTOs.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
	// MembershipsOf returns the active memberships of a user (sensitive
	// clubs included): the membership events re-check it, so an automatic
	// follow follows the membership whatever order the events arrive in.
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)
	// ActiveMemberIDs returns the users with an active membership of orgID
	// (the moderation notices to the author org, BE-032).
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Clubs is what feed needs from directory (L6; 01 §4.6 Catalog): which
// clubs are sensitive. A sensitive club membership never creates an
// automatic follow (02 §12.7: the follow list must not reveal it).
type Clubs interface {
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs (only
	// those keys are present, with true).
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// Viewers is what feed needs from students (L7; 01 §4.9 Profiles): the
// audience keys of a reader, sensitive clubs included, computed at read
// time and never stored or shown.
type Viewers interface {
	// ViewerKeys returns the feed keys of userID: 'all' for everyone, the
	// profile keys of a student, every active club.
	ViewerKeys(ctx context.Context, userID uuid.UUID) (students.ViewerKeys, error)
}

// Events is what feed needs from events (L8; 01 §4.10): the shared event
// of an event_share post.
type Events interface {
	// Get returns the summary of an event (any status) or events.ErrNotFound.
	Get(ctx context.Context, eventID uuid.UUID) (events.EventSummary, error)
}

// Files is what feed needs from files (L2) for the images of a post (02
// §12.3, purpose post_media).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds an image to the post in the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// Targeting is what feed needs from outreach (L8; 01 §4.13): the audience
// of a targeted post, fixed in the post's transaction (01 §6.3 "Hedefli
// gönderi yayını"). Compile applies the actor's targeting scope (03 k16),
// the sensitive club ban (03 Y2), k ≥ 10 and the minor exclusion, and
// spends one unit of the org's daily count budget (06 H-61; 429/503).
type Targeting interface {
	Compile(ctx context.Context, a authz.Actor, spec outreach.SegmentSpec) (outreach.Compiled, error)
}

// Entitlements is what feed needs from entitlements (L2; 01 §4.4): the
// targeted_post quota of a company, consumed when the targeted post is
// created and given back when its draft is deleted or rejected (DECISIONS-
// LOG 2026-10-03), in the post's transaction.
type Entitlements interface {
	// Consume uses one unit of the org's feature for ref (the post) in the
	// caller's transaction; no usable right is entitlements.ErrNoEntitlement
	// (403 entitlements.not_entitled).
	//
	// sync-write: entitlements.Entitlements.Consume
	Consume(ctx context.Context, orgID uuid.UUID, f entitlements.Feature, qty int, ref entitlements.Ref) (uuid.UUID, error)
	// Release gives the unit of ref back in the caller's transaction (a
	// reversal row); a ref that consumed nothing is a no-op.
	//
	// sync-write: entitlements.Entitlements.Release
	Release(ctx context.Context, ref entitlements.Ref) error
	// Available returns what the org may still use of the feature.
	Available(ctx context.Context, orgID uuid.UUID, f entitlements.Feature) (entitlements.Availability, error)
}

// Sender is what feed needs from notify (L3; 01 §4.5): the in-app notices
// of moderation (moderators, the author org, the reporter).
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Staff lists the platform staff who moderate (03 §5.7 mod, pad): the
// recipients of the review queue and report notices (T-249). Both lists
// hold the roles in effect only (06 H-33).
type Staff interface {
	// Moderators returns the users holding moderator.
	Moderators(ctx context.Context) ([]uuid.UUID, error)
	// PlatformAdmins returns the users holding platform_admin.
	PlatformAdmins(ctx context.Context) ([]uuid.UUID, error)
}
