package feed

import (
	"context"
	"uuid"
)

// moderationStore is moderation_actions (BE-032; 02 §12.10) and the review
// queue of pending posts (moderation.go, report.go, read.go). Moderation
// actions have no org scope: moderation is the platform's (02 §12.9 "Org
// kapsamı: yok"). The service authorises the admin surface before it
// reads them.
type moderationStore interface {
	// InsertModerationAction appends m (moderation_actions is append-only)
	// and returns it as stored.
	InsertModerationAction(ctx context.Context, m ModerationAction) (ModerationAction, error)
	// GetModerationAction returns the action or ErrNotFound.
	GetModerationAction(ctx context.Context, id uuid.UUID) (ModerationAction, error)
	// ListModerationActions returns up to limit actions of f, newest first
	// by id, after the id after.
	ListModerationActions(ctx context.Context, f ActionFilter, after *uuid.UUID, limit int) ([]ModerationAction, error)
	// LatestActions returns the newest action on each target of type t
	// among ids (one statement for a page).
	LatestActions(ctx context.Context, t TargetType, ids []uuid.UUID) (map[uuid.UUID]ModerationAction, error)
	// ReportActions counts the actions taken with reportID.
	ReportActions(ctx context.Context, reportID uuid.UUID) (int, error)
	// ActionsBy returns the actions taken by userID, oldest first (KVKK
	// export of a moderator).
	ActionsBy(ctx context.Context, userID uuid.UUID) ([]ModerationAction, error)

	// ListReview returns up to limit posts in pending_review of every org,
	// oldest first by (created_at, id), after the position after (the
	// review queue, posts_review_idx).
	ListReview(ctx context.Context, after *After, limit int) ([]Post, error)
}

// ActionFilter narrows ListModerationActions (adminListModerationActions).
type ActionFilter struct {
	TargetType TargetType // "": both
	TargetID   *uuid.UUID // nil: every target
}
