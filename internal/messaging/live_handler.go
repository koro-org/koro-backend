package messaging

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/realtime"
)

// RegisterLive mounts the conversations' two WebSockets (live.go; BE-162b)
// on api. hub nil (`koro openapi`) answers 503; actors authenticates the
// handshake (cookie or bearer, never the query string).
func RegisterLive(api huma.API, svc Service, hub *realtime.Hub, actors httpx.ActorResolver) {
	for _, r := range []struct {
		path  string
		party func(param func(string) string) Party
	}{
		{LivePath, func(func(string) string) Party { return StudentParty() }},
		{OrgLivePath, func(param func(string) string) Party { return CompanyParty(httpx.ParseID(param("org_id"))) }},
	} {
		rt := realtime.Route{Path: r.path, Hub: hub, Actors: actors}
		if svc != nil {
			party := r.party
			rt.Open = func(ctx context.Context, a authz.Actor, param func(string) string) (realtime.Subscription, error) {
				return openLive(ctx, svc, a, party(param))
			}
		}
		realtime.Register(api, rt)
	}
}

func openLive(ctx context.Context, svc Service, a authz.Actor, p Party) (realtime.Subscription, error) {
	topic, err := svc.OpenLive(ctx, a, p)
	if err != nil {
		return realtime.Subscription{}, err
	}
	return realtime.Subscription{
		Topic: topic,
		Recheck: func(ctx context.Context, a authz.Actor) error {
			_, err := svc.OpenLive(ctx, a, p)
			return err
		},
	}, nil
}
