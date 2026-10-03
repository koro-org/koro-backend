package authztest

import (
	"context"
	"crypto/sha256"
	"sync"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// Resolver is the test httpx.ActorResolver of the authz matrix: it maps
// bearer tokens to actors, so the full HTTP stack runs without identity
// (BE-014). It follows the production contract: unknown tokens and cookie
// credentials are problem.ErrUnauthenticated, and the actor goes into ctx
// with authz.WithActor and obs.WithActorID.
type Resolver struct {
	mu     sync.RWMutex
	actors map[[sha256.Size]byte]authz.Actor
}

// NewResolver returns a resolver that knows the tokens of ps (anonymous,
// which has no token, is skipped).
func NewResolver(ps ...Persona) *Resolver {
	r := &Resolver{actors: map[[sha256.Size]byte]authz.Actor{}}
	for _, p := range ps {
		if p.Token != "" {
			r.Add(p.Token, p.Actor)
		}
	}
	return r
}

// Add registers a bearer token for a (scenario variants: a session without
// MFA, a lapsed verification, ...). It is safe for concurrent use.
func (r *Resolver) Add(token string, a authz.Actor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actors[sha256.Sum256([]byte(token))] = a
}

// Resolve implements httpx.ActorResolver. Persona sessions are mobile
// sessions: a cookie credential never matches (03 §2.2-1).
func (r *Resolver) Resolve(ctx context.Context, cred httpx.Credential) (context.Context, error) {
	if cred.Transport != httpx.TransportBearer {
		return nil, problem.ErrUnauthenticated
	}
	r.mu.RLock()
	a, ok := r.actors[cred.TokenHash]
	r.mu.RUnlock()
	if !ok {
		return nil, problem.ErrUnauthenticated
	}
	return obs.WithActorID(authz.WithActor(ctx, a), a.UserID.String()), nil
}
