package events

import (
	"context"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/realtime"
)

// LivePath is the live board's route under /api/v1 (04 §2.0, §W): a gin
// route outside OpenAPI (authz matrix: non_api_routes, websocket: true).
const LivePath = "/events/{event_id}/check-ins/live"

// RegisterLive mounts the live board's WebSocket on api. hub nil (`koro
// openapi`) answers 503; actors authenticates the handshake (cookie or
// bearer, never the query string).
func RegisterLive(api huma.API, svc Service, hub *realtime.Hub, actors httpx.ActorResolver) {
	rt := realtime.Route{Path: LivePath, Hub: hub, Actors: actors}
	if svc != nil {
		rt.Open = func(ctx context.Context, a authz.Actor, param func(string) string) (realtime.Subscription, error) {
			return openLive(ctx, svc, a, param("event_id"))
		}
	}
	realtime.Register(api, rt)
}

func openLive(ctx context.Context, svc Service, a authz.Actor, rawID string) (realtime.Subscription, error) {
	id, err := uuid.Parse(rawID)
	if err != nil {
		return realtime.Subscription{}, ErrNotFound // a malformed id names no event
	}
	b, err := svc.OpenLiveBoard(ctx, a, id)
	if err != nil {
		return realtime.Subscription{}, err
	}
	snapshot, filter := LiveStream(b, func(ctx context.Context) ([]byte, error) { return svc.LiveSnapshot(ctx, b) })
	return realtime.Subscription{
		Topic:    b.Topic(),
		Snapshot: snapshot,
		Filter:   filter,
		Recheck: func(ctx context.Context, a authz.Actor) error {
			_, err := svc.OpenLiveBoard(ctx, a, id)
			return err
		},
	}, nil
}
