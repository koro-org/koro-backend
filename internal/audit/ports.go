package audit

import (
	"context"
	"uuid"
)

// ActorNames is what audit needs from identity (L4, above audit): display
// names of actors for AuditEventDTO.actor.display. The composition root
// binds it once identity exists (BE-014); unbound, display is omitted.
type ActorNames interface {
	DisplayNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}
