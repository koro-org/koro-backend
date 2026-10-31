package stats

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

// Register adds the stats operations (04 §2.23, BE-163) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	const rangeNote = " The range is whole days in Europe/Istanbul (from and to inclusive, at most 366 days)."
	op.Register(api, "stats", huma.Operation{
		OperationID: "getAdminStats", Method: http.MethodGet, Path: "/admin/stats",
		Summary: "Get the platform summary",
		Description: "Operational counts of the platform (SR §8 Admin): accounts by status, sign-up role and " +
			"verification, orgs by kind and status, posts and content reports, events and their participation, and " +
			"the review queues. The *_in_range figures cover the range (default: the last 30 days)." + rangeNote,
	}, op.Meta{Action: string(authz.StatsPlatformRead), Surface: op.SurfaceAdmin}, h.platform)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getClubStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/club",
		Summary: "Get a club's panel figures",
		Description: "The club's members and their monthly growth, the registration and attendance of its events " +
			"and the orders companies placed on its offerings (SR §8 Kulüp). Default range: the last 12 calendar " +
			"months. Another kind of org is not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsClubRead), Surface: op.SurfaceOrg}, h.club)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getCompanyStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/company",
		Summary: "Get a company's panel figures",
		Description: "The company's saved audiences with their stored estimates, the reach of its posts and " +
			"dispatches, its contact requests with their acceptance rate and its package usage (SR §8 Şirket). " +
			"Numbers about students are estimates: null below 10, rounded down to their bucket. Default range: the " +
			"last 30 days. Another kind of org is not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsCompanyRead), Surface: op.SurfaceOrg}, h.company)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getUniversityStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/university",
		Summary: "Get a university's panel figures",
		Description: "The university's active students by degree level, class year and department (groups below 10 " +
			"left out, the others rounded down to their bucket), the event density of its clubs (sensitive clubs and " +
			"events of a sensitive category left out) and what waits for its decision (SR §8 Kurum). Default range: " +
			"the last 12 calendar months. Another kind of org is not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsUniversityRead), Surface: op.SurfaceOrg}, h.university)
}

type handler struct {
	svc Service
}

// RangeParams are the period query parameters of every panel (exported:
// huma reads the fields of an embedded struct only when it is exported).
type RangeParams struct {
	From string `query:"from" doc:"First day (YYYY-MM-DD, Europe/Istanbul), inclusive; default: the panel's"`
	To   string `query:"to" doc:"Last day (YYYY-MM-DD, Europe/Istanbul), inclusive; default: today"`
}

func (p RangeParams) query() RangeQuery { return RangeQuery(p) }

type platformInput struct {
	RangeParams
}

type platformOutput struct {
	Body PlatformStats
}

func (h handler) platform(ctx context.Context, in *platformInput) (*platformOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Platform(ctx, a, in.query())
	if err != nil {
		return nil, err
	}
	return &platformOutput{Body: st}, nil
}

type orgInput struct {
	OrgID string `path:"org_id" doc:"Org ID"`
	RangeParams
}

type clubOutput struct {
	Body ClubStats
}

func (h handler) club(ctx context.Context, in *orgInput) (*clubOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Club(ctx, a, httpx.ParseID(in.OrgID), in.query())
	if err != nil {
		return nil, err
	}
	return &clubOutput{Body: st}, nil
}

type companyOutput struct {
	Body CompanyStats
}

func (h handler) company(ctx context.Context, in *orgInput) (*companyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Company(ctx, a, httpx.ParseID(in.OrgID), in.query())
	if err != nil {
		return nil, err
	}
	return &companyOutput{Body: st}, nil
}

type universityOutput struct {
	Body UniversityStats
}

func (h handler) university(ctx context.Context, in *orgInput) (*universityOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.University(ctx, a, httpx.ParseID(in.OrgID), in.query())
	if err != nil {
		return nil, err
	}
	return &universityOutput{Body: st}, nil
}
