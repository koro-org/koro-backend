package recommend

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
)

// Register adds listMyRecommendations (04 §2.24, BE-174) to api. svc may be
// nil (`koro openapi`): the handler only touches it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	op.Register(api, "recommend", huma.Operation{
		OperationID: "listMyRecommendations", Method: http.MethodGet, Path: "/me/recommendations",
		Summary: "List my recommended events and clubs",
		Description: "Rule-based recommendations from your own data, shown to you only: upcoming " +
			"events (next 30 days) you have not registered for, of the clubs and orgs you follow or belong to, like the " +
			"events you attended in the last year, of your university or popular (at least 10 registrations); clubs that " +
			"organised your events or have at least 10 members in your department or university. Every item carries the " +
			"reason codes of the rules that picked it; no score or count. Sensitive clubs and events of a sensitive " +
			"category are never a signal nor a recommendation. Turn them off with updateMyPrivacy " +
			"recommendation_choice: disabled (enabled=false, empty lists; default: on for an adult, off for a minor). " +
			"Verified students only (403 otherwise).",
	}, op.Meta{Action: string(authz.RecommendRecommendationListOwn), Surface: op.SurfaceSelf}, h.list)
}

type handler struct {
	svc Service
}

type listInput struct {
	Limit int `query:"limit" minimum:"1" maximum:"20" default:"10" doc:"Items per list (events and clubs each)"`
}

type listOutput struct {
	Body RecommendationsDTO
}

func (h handler) list(ctx context.Context, in *listInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.List(ctx, a, Query{Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	return &listOutput{Body: toDTO(r)}, nil
}
