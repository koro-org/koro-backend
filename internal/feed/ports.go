package feed

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/orgs"
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
