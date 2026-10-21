package realtime

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// maxTokenLen bounds a credential before hashing (as httpx).
const maxTokenLen = 512

// Subscription is what Route.Open grants an actor.
type Subscription struct {
	Topic    string
	Snapshot func(ctx context.Context) ([]byte, error)
	Filter   func(msg []byte) ([]byte, bool)
	// Recheck re-authorises the actor (its session was just re-validated;
	// see Stream.Recheck for the close codes).
	Recheck func(ctx context.Context, a authz.Actor) error
}

// Route is a WebSocket route: a raw gin route under the API group, outside
// OpenAPI (the authz matrix lists it in non_api_routes, websocket: true).
type Route struct {
	// Path under the group, e.g. "/events/{event_id}/check-ins/live".
	Path string
	// Hub serves the connections; nil (`koro openapi`) answers 503.
	Hub *Hub
	// Actors authenticates the handshake's credential; nil answers 401.
	Actors httpx.ActorResolver
	// Open authorises the actor for the path parameters (param) and returns
	// its subscription; an error is the HTTP answer (401/403/404) and
	// nothing is upgraded.
	Open func(ctx context.Context, a authz.Actor, param func(name string) string) (Subscription, error)
}

// Register mounts rt on api's router. huma's operation middlewares (auth,
// rate limit classes) do not run on it: the handler authenticates itself;
// the gin middlewares (request ID, access log, global IP limit, security
// headers) do.
func Register(api huma.API, rt Route) {
	api.Adapter().Handle(&huma.Operation{Method: http.MethodGet, Path: rt.Path}, func(hctx huma.Context) {
		serve(api, rt, hctx)
	})
}

// msgUpgradeRequired answers a plain GET on a WebSocket route (400).
const msgUpgradeRequired = "a WebSocket upgrade is required"

// serve is the handshake (04 §W): Origin → credential → authorisation →
// upgrade request → limits → upgrade.
func serve(api huma.API, rt Route, hctx huma.Context) {
	fail := func(status int, msg string, err error) {
		if err == nil {
			_ = huma.WriteErr(api, hctx, status, msg)
			return
		}
		_ = huma.WriteErr(api, hctx, status, msg, err)
	}
	gc := humagin.Unwrap(hctx)
	r := gc.Request
	if rt.Hub == nil || rt.Open == nil {
		fail(http.StatusServiceUnavailable, "", problem.ErrUnavailable)
		return
	}
	if err := rt.Hub.CheckOrigin(r); err != nil {
		fail(http.StatusForbidden, "", err)
		return
	}
	cred, ok := CredentialFrom(r)
	if !ok || rt.Actors == nil {
		fail(http.StatusUnauthorized, "", problem.ErrUnauthenticated)
		return
	}
	ctx, a, err := resolve(r.Context(), rt.Actors, cred)
	if err != nil {
		fail(http.StatusInternalServerError, "", err)
		return
	}
	sub, err := rt.Open(ctx, a, hctx.Param)
	if err != nil {
		fail(http.StatusInternalServerError, "", err)
		return
	}
	if !IsUpgrade(r) {
		fail(http.StatusBadRequest, msgUpgradeRequired, nil)
		return
	}
	lease, err := rt.Hub.Admit(ctx, sub.Topic, a.UserID)
	if err != nil {
		fail(http.StatusInternalServerError, "", err)
		return
	}
	s := Stream{Snapshot: sub.Snapshot, Filter: sub.Filter}
	if sub.Recheck != nil {
		s.Recheck = func(ctx context.Context) error {
			ctx, a, err := resolve(ctx, rt.Actors, cred)
			if err != nil {
				return err
			}
			return sub.Recheck(ctx, a)
		}
	}
	rt.Hub.Serve(gc.Writer, r.WithContext(ctx), lease, s)
}

// resolve authenticates cred; a rejected credential is
// problem.ErrUnauthenticated.
func resolve(ctx context.Context, actors httpx.ActorResolver, cred httpx.Credential) (context.Context, authz.Actor, error) {
	rctx, err := actors.Resolve(ctx, cred)
	if err != nil {
		return ctx, authz.Actor{}, err
	}
	a, err := authz.ActorFrom(rctx)
	if err != nil {
		return ctx, authz.Actor{}, err
	}
	return rctx, a, nil
}

// CredentialFrom extracts the handshake's credential like the API's auth
// middleware: the bearer header (native clients) first, then the session
// cookie (the browser sends it with the handshake). The query string is
// never read: a token there would end up in proxy and access logs (04 §W).
// A malformed Authorization header yields an empty-hash credential that no
// session matches.
func CredentialFrom(r *http.Request) (httpx.Credential, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > maxTokenLen {
			return httpx.Credential{Transport: httpx.TransportBearer}, true
		}
		return httpx.Credential{Transport: httpx.TransportBearer, TokenHash: sha256.Sum256([]byte(token))}, true
	}
	c, err := r.Cookie(httpx.SessionCookieName)
	if err != nil || c.Value == "" {
		return httpx.Credential{}, false
	}
	if len(c.Value) > maxTokenLen {
		return httpx.Credential{Transport: httpx.TransportCookie}, true
	}
	return httpx.Credential{Transport: httpx.TransportCookie, TokenHash: sha256.Sum256([]byte(c.Value))}, true
}

// IsUpgrade reports whether r asks for a WebSocket upgrade.
func IsUpgrade(r *http.Request) bool {
	return headerHas(r.Header, "Connection", "upgrade") && headerHas(r.Header, "Upgrade", "websocket")
}

func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
