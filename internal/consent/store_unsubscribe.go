package consent

import (
	"context"
	"time"
	"uuid"
)

// UnsubscribeLinkStore is the unsubscribe_links part of Store (000162,
// BE-153): the opaque one-click unsubscribe links of commercial e-mail,
// addressed by the digest of their token. A link never changes; it goes
// with its account or after its expiry.
type UnsubscribeLinkStore interface {
	// InsertUnsubscribeLink writes l. A token digest already stored is a
	// conflict (unsubscribe_links_token_hash_uq).
	InsertUnsubscribeLink(ctx context.Context, l UnsubscribeToken) error
	// UnsubscribeLinkByHash returns the link whose token digest is hash, or
	// ErrNotFound.
	UnsubscribeLinkByHash(ctx context.Context, hash []byte) (UnsubscribeToken, error)
	// DeleteUnsubscribeLinksOf deletes every link of a user (KVKK erasure)
	// and returns how many went.
	DeleteUnsubscribeLinksOf(ctx context.Context, userID uuid.UUID) (int, error)
	// DeleteExpiredUnsubscribeLinks deletes up to limit links that expired
	// before cutoff, oldest expiry first, and returns how many went.
	DeleteExpiredUnsubscribeLinks(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
