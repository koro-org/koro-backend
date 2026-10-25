package outbox

import (
	"context"
	"uuid"

	"koro/internal/platform/domainevent"
)

// Emit builds the envelope of p on the aggregate (aggregateType, id) and
// writes it with w.Add (mechanism O, inside the caller's transaction).
func Emit(ctx context.Context, w Writer, p domainevent.Payload, aggregateType string, id uuid.UUID) error {
	env, err := domainevent.New(p, aggregateType, id)
	if err != nil {
		return err
	}
	return w.Add(ctx, env)
}

// EmitAfterCommit is Emit for an A topic: w.AfterCommit enqueues it once
// the transaction commits.
func EmitAfterCommit(ctx context.Context, w Writer, p domainevent.Payload, aggregateType string, id uuid.UUID) error {
	env, err := domainevent.New(p, aggregateType, id)
	if err != nil {
		return err
	}
	return w.AfterCommit(ctx, env)
}
