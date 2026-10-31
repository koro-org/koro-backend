package files

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Rejection is file.rejected (01 §6.2) as the owning module of the file
// sees it (T-176): the file it attached to its record EntityID was taken
// out of use after its upload (Reason) and its object is already gone. The
// module drops its reference and deletes the file through Files.Delete, as
// Actor, in one transaction; or, for a financial or legal record that must
// keep its reference, does nothing (BE-151).
type Rejection struct {
	FileID   uuid.UUID
	Purpose  Purpose
	Reason   RejectReason
	EntityID uuid.UUID
	// Actor is the system actor of the subscription (its task name).
	Actor authz.Actor
}

// Meta is the audit metadata of the dropped reference (IDs and codes only).
func (r Rejection) Meta() map[string]any {
	return map[string]any{"file_id": r.FileID.String(), "reason": string(r.Reason)}
}

// SubscribeRejected registers name, the file.rejected subscription of the
// owning module module (01 §6.2, T-176; queue default). The payload is
// decoded (a bad one is permanent) and drop runs only for a file module
// attached: a file of another module, or of none, is no task of module's.
// drop must be idempotent (at-least-once delivery): a second delivery finds
// the reference gone, and Files.Delete of a missing file is no error.
func SubscribeRejected(r *outbox.Registry, module, name string, drop func(context.Context, Rejection) error) {
	r.Subscribe(TopicRejected, name, func(ctx context.Context, e domainevent.Envelope) error {
		p, err := domainevent.Decode[domainevent.FileRejected](e)
		if err != nil {
			return queue.Permanent(err)
		}
		if p.AttachedModule != module || p.AttachedEntityID == nil {
			return nil
		}
		reason := RejectReason(p.Reason)
		if reason != RejectInfected && reason != RejectUnprocessable {
			return queue.Permanent(fmt.Errorf("files: file.rejected with unknown reason %q", p.Reason))
		}
		return drop(ctx, Rejection{
			FileID: p.FileID, Purpose: Purpose(p.Purpose), Reason: reason, EntityID: *p.AttachedEntityID,
			Actor: authz.SystemActor(name),
		})
	}, outbox.Queue(queue.Default))
}
