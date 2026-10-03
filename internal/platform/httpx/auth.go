package httpx

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/problem"
)

// SessionCookieName is the web session cookie (04 §1.5).
const SessionCookieName = "__Host-koro_session"

// Security scheme names in the OpenAPI document.
const (
	SchemeSessionCookie = "sessionCookie"
	SchemeBearer        = "bearerAuth"
)

// SessionOrBearer is the Security requirement of every protected operation:
// cookie OR bearer (05 §10.1). op.Koro sets it from the operation surface.
var SessionOrBearer = []map[string][]string{{SchemeSessionCookie: {}}, {SchemeBearer: {}}}

// maxTokenLen bounds credential tokens before hashing.
const maxTokenLen = 512

// Transport tells how a credential arrived; the session's client kind must
// match it (web → cookie, mobile → bearer; 03 §2.2-1).
type Transport uint8

// Credential transports.
const (
	TransportCookie Transport = iota + 1
	TransportBearer
)

// Credential is what the auth middleware hands to the ActorResolver. The raw
// token never leaves the middleware; only its SHA-256 does.
type Credential struct {
	Transport Transport
	TokenHash [sha256.Size]byte
}

// ActorResolver authenticates a credential (implemented by cmd/koro/actor.go,
// BE-014; 03 §2.2).
//
// Resolve returns ctx enriched with the resolved actor (authz.WithActor and
// obs.WithActorID). For a missing, expired, revoked or transport-mismatched
// session it returns an error matching problem.ErrUnauthenticated — the same
// error for every case. Any other error is treated as a server failure.
type ActorResolver interface {
	Resolve(ctx context.Context, cred Credential) (context.Context, error)
}

// authMiddleware runs before huma parses the input, so unauthenticated
// callers learn nothing about the schema (05 §5.2).
//
//   - Protected operation (Security set): a missing or rejected credential
//     is 401 unauthenticated.
//   - Public operation: no credential → anonymous; a valid credential
//     resolves the actor; a rejected credential is ignored (anonymous), so a
//     stale cookie never blocks login or public pages.
func authMiddleware(api huma.API, actors ActorResolver) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		protected := len(ctx.Operation().Security) > 0
		cred, present := credentialFrom(ctx)
		if !present || actors == nil {
			if protected {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "", problem.ErrUnauthenticated)
				return
			}
			next(ctx)
			return
		}
		rctx, err := actors.Resolve(ctx.Context(), cred)
		switch {
		case err == nil:
			next(huma.WithContext(ctx, rctx))
		case errors.Is(err, problem.ErrUnauthenticated):
			if protected {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "", problem.ErrUnauthenticated)
				return
			}
			next(ctx)
		default:
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "", err)
		}
	}
}

// credentialFrom extracts the bearer token (preferred) or the session
// cookie. A malformed Authorization header yields an empty-hash credential
// that no session can match, so the resolver rejects it uniformly.
func credentialFrom(ctx huma.Context) (Credential, bool) {
	if h := ctx.Header("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > maxTokenLen {
			return Credential{Transport: TransportBearer}, true
		}
		return Credential{Transport: TransportBearer, TokenHash: sha256.Sum256([]byte(token))}, true
	}
	c, err := huma.ReadCookie(ctx, SessionCookieName)
	if err != nil || c.Value == "" {
		return Credential{}, false
	}
	if len(c.Value) > maxTokenLen {
		return Credential{Transport: TransportCookie}, true
	}
	return Credential{Transport: TransportCookie, TokenHash: sha256.Sum256([]byte(c.Value))}, true
}
