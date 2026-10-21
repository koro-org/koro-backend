package messaging

import (
	"context"

	"koro/internal/platform/domainevent"
)

// HandleCreated runs the contact_request.created subscription (tests).
func (m Module) HandleCreated(ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleCreated(ctx, e)
}

// HandleClosed runs the contact_request.closed subscription (tests).
func (m Module) HandleClosed(ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleClosed(ctx, e)
}

// ExpireDue runs messaging.expire_requests (tests).
func (m Module) ExpireDue(ctx context.Context) error { return m.svc.ExpireDue(ctx) }
