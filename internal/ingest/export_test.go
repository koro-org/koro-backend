package ingest

import (
	"context"

	"koro/internal/platform/domainevent"
)

// ErrNoRow exposes the stores' not-found sentinel to the black-box tests.
var ErrNoRow = errNotFound

// HandleClaimDecidedForTest runs ingest.pause_claimed_sources.
func (m Module) HandleClaimDecidedForTest(ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleClaimDecided(ctx, e)
}

// PurgeForTest runs ingest.purge_expired.
func (m Module) PurgeForTest(ctx context.Context) error { return m.svc.purgeExpired(ctx) }
