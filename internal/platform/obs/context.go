// Package obs provides structured logging (log/slog), request/actor context
// fields, PII masking (05 §11.1–11.2) and the telemetry hooks (05 §11.3–11.4).
package obs

import "context"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	actorIDKey
	taskIDKey
)

// WithRequestID returns ctx carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID in ctx or "".
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// WithActorID returns ctx carrying the acting user's ID (a UUID string; never
// a name or e-mail).
func WithActorID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actorIDKey, id)
}

// ActorID returns the actor ID in ctx or "".
func ActorID(ctx context.Context) string {
	v, _ := ctx.Value(actorIDKey).(string)
	return v
}

// WithTaskID returns ctx carrying the background task ID (worker jobs log
// task_id instead of request_id).
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskIDKey, id)
}

// TaskID returns the task ID in ctx or "".
func TaskID(ctx context.Context) string {
	v, _ := ctx.Value(taskIDKey).(string)
	return v
}
