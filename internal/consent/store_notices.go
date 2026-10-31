package consent

import (
	"context"
	"time"
	"uuid"
)

// noticeStore is notice_versions, the versioned privacy and terms notices
// (BE-129: the admin
// list).
type noticeStore interface {
	// InsertNotice writes a draft (published_at and retired_at are ignored).
	// A duplicate (kind, purpose, audience, locale, version) is
	// ErrNoticeVersionTaken; an unknown purpose ErrUnknownPurpose.
	InsertNotice(ctx context.Context, n Notice) error
	// Notice returns a notice or ErrNotFound.
	Notice(ctx context.Context, id uuid.UUID) (Notice, error)
	// LockNotice returns a notice locked for the transaction, or ErrNotFound.
	LockNotice(ctx context.Context, id uuid.UUID) (Notice, error)
	// LockCurrent returns the published, unretired notice of key locked
	// for the transaction; found=false when the key has none.
	LockCurrent(ctx context.Context, key NoticeKey) (n Notice, found bool, err error)
	// ListNotices returns up to limit notices matching f — drafts, current
	// and retired versions — newest first (id DESC: UUIDv7 creation
	// order), after the position after (nil: the first page; BE-129).
	ListNotices(ctx context.Context, f NoticeListFilter, after *uuid.UUID, limit int) ([]Notice, error)
	// CurrentNotices returns the published, unretired notices matching q,
	// ordered by kind, purpose, audience and locale.
	CurrentNotices(ctx context.Context, q CurrentQuery) ([]Notice, error)
	// Publish sets published_at of a draft. ErrNoticeState when the notice
	// is not a draft; ErrNoticeState too when another version of its key
	// is current (notice_versions_current_uq).
	Publish(ctx context.Context, id uuid.UUID, at time.Time) error
	// Retire sets retired_at of a published, unretired notice (the only
	// change notice_versions_guard allows); ErrNoticeState otherwise.
	Retire(ctx context.Context, id uuid.UUID, at time.Time) error
}
