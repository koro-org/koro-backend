package consent

import (
	"context"
	"time"
)

// retentionStore is the consent proof purge of the retention sweeper
// (retention.go; 02 §24.2, BE-057).
type retentionStore interface {
	// PurgeProof deletes up to limit consent rows (with their İYS
	// submissions) of the chains whose proof retention ended: the latest
	// row of the (user, purpose, scope) chain is a withdrawal before cutoff,
	// or the user was erased before cutoff (consent_proof, 3 years; BE-057).
	// Oldest first, so a chain cut by the limit keeps its newest rows and
	// its current state. dryRun counts every such row and deletes nothing.
	// The GORM store goes through 000121's consents_purge (consents are
	// append-only): it never passes now − the policy's period and refuses a
	// real purge of an unapproved policy (55000).
	PurgeProof(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}
