package authz

import (
	"context"

	"koro/internal/platform/problem"
)

type ctxKey struct{}

// WithActor returns ctx carrying a. The ActorResolver calls it (with
// obs.WithActorID) for every authenticated request (05 §5.2).
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// ActorFrom returns the actor of a protected operation. Without one it
// returns problem.ErrUnauthenticated (the auth middleware answers 401 before
// a protected handler runs, so this is a wiring error). Handlers pass the
// actor to the service explicitly; services never read it from ctx.
func ActorFrom(ctx context.Context) (Actor, error) {
	if a, ok := ctx.Value(ctxKey{}).(Actor); ok {
		return a, nil
	}
	return Actor{}, problem.ErrUnauthenticated
}

// ActorOrAnonymous returns the actor of a public operation: the resolved
// user when a valid credential was sent, Anonymous otherwise (06 H-25).
func ActorOrAnonymous(ctx context.Context) Actor {
	if a, ok := ctx.Value(ctxKey{}).(Actor); ok {
		return a
	}
	return Anonymous()
}
