package idempotency

import (
	"context"
	"net/http"
	"regexp"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

// Header names (04 §1.8).
const (
	HeaderKey      = "Idempotency-Key"
	HeaderReplayed = "Idempotent-Replayed"
)

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,255}$`)

// RequiredHeader documents the header of an x-koro-idempotency: required
// operation; embed it in the input struct (huma validates it too: a missing
// header is 422 validation_failed, 04 §1.2).
type RequiredHeader struct {
	IdempotencyKey string `header:"Idempotency-Key" required:"true" minLength:"8" maxLength:"255" pattern:"^[A-Za-z0-9_-]+$" doc:"Client-generated key; a retry with the same key and body replays the first response (Idempotent-Replayed: true)"`
}

// OptionalHeader documents the header of an x-koro-idempotency: optional
// operation.
type OptionalHeader struct {
	IdempotencyKey string `header:"Idempotency-Key" minLength:"8" maxLength:"255" pattern:"^[A-Za-z0-9_-]+$" doc:"Optional client-generated key; a retry with the same key and body replays the first response (Idempotent-Replayed: true)"`
}

// Key is the parsed Idempotency-Key of a request.
type Key struct {
	Scope  string // "<actor_user_id>:<METHOD> <path template>" (02 §1.3)
	Value  string // the header value
	Status int    // the operation's success status, stored with the response
	// replayed sets the Idempotent-Replayed response header.
	replayed func()
}

func (k Key) markReplayed() {
	if k.replayed != nil {
		k.replayed()
	}
}

type keyCtx struct{}

// WithKey returns ctx carrying k (the middleware; service tests).
func WithKey(ctx context.Context, k Key) context.Context { return context.WithValue(ctx, keyCtx{}, k) }

// KeyFrom returns the key of the request.
func KeyFrom(ctx context.Context) (Key, bool) {
	k, ok := ctx.Value(keyCtx{}).(Key)
	return k, ok
}

// Middleware parses Idempotency-Key on operations that declare
// x-koro-idempotency (op.Koro) and puts it into the request context. It
// runs after authentication (the scope needs the actor). Nothing is stored
// here: the service calls Do inside its transaction (06 H-5).
func Middleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		o := ctx.Operation()
		policy, _ := o.Extensions[op.ExtIdempotency].(string)
		if policy == "" {
			next(ctx)
			return
		}
		v := ctx.Header(HeaderKey)
		switch {
		case v == "" && policy == string(op.IdempotencyRequired):
			writeInvalid(api, ctx, "required header is missing")
			return
		case v == "":
			next(ctx)
			return
		case !keyRe.MatchString(v):
			writeInvalid(api, ctx, "expected 8-255 characters of A-Z a-z 0-9 _ -")
			return
		}
		actor := authz.ActorOrAnonymous(ctx.Context())
		if !actor.IsUser() {
			next(ctx) // anonymous: no owner to scope the key to
			return
		}
		status := o.DefaultStatus
		if status == 0 {
			status = http.StatusOK
		}
		k := Key{
			Scope: actor.UserID.String() + ":" + o.Method + " " + o.Path, Value: v, Status: status,
			replayed: func() { ctx.SetHeader(HeaderReplayed, "true") },
		}
		next(huma.WithContext(ctx, WithKey(ctx.Context(), k)))
	}
}

func writeInvalid(api huma.API, ctx huma.Context, msg string) {
	err := problem.ErrValidation.WithFields(problem.FieldError{Location: "header." + HeaderKey, Message: msg})
	_ = huma.WriteErr(api, ctx, http.StatusUnprocessableEntity, "", err)
}
