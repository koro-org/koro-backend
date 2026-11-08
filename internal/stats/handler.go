package stats

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

// Register adds the stats operations (04 §2.23, BE-163, BE-196) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	const rangeNote = " The range is whole days in Europe/Istanbul (from and to inclusive, at most 366 days)."
	op.Register(api, "stats", huma.Operation{
		OperationID: "getAdminStats", Method: http.MethodGet, Path: "/admin/stats",
		Summary: "Get the platform summary",
		Description: "Operational counts of the platform (SR §8 Admin): accounts by status, sign-up role and " +
			"verification, orgs by kind and status, posts and content reports, events and their participation, and " +
			"the review queues. The KVKK request and student document queues are counted only for a caller who may " +
			"work that queue (kvkk.dsr.list; identity.edu_verification.list) and are null otherwise. " +
			"The *_in_range figures cover the range (default: the last 30 days)." + rangeNote,
	}, op.Meta{Action: string(authz.StatsPlatformRead), Surface: op.SurfaceAdmin, QuerySyntax: rangeSyntax}, h.platform)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getAdminQueues", Method: http.MethodGet, Path: "/admin/queues",
		Summary: "Get the review queue sizes the caller works",
		Description: "The number of items waiting now in each review queue of the admin surface, for every platform " +
			"role (BE-196). A queue is counted only for a caller who may work it, by its own list action, and is " +
			"null otherwise: org reviews (orgs.org.list_admin), club claims (directory.club_claim.list), content " +
			"reports (feed.report.list), KVKK requests (kvkk.dsr.list: every open request for the dpo, those " +
			"awaiting the second approval for platform_admin) and student documents " +
			"(identity.edu_verification.list). Counts only; the same figures as the queues of getAdminStats.",
	}, op.Meta{Action: string(authz.StatsQueuesRead), Surface: op.SurfaceAdmin}, h.queues)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getClubStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/club",
		Summary: "Get a club's panel figures",
		Description: "The club's members and their monthly growth, the registration and attendance of its events " +
			"and the orders companies placed on its offerings (SR §8 Kulüp). Default range: the last 12 calendar " +
			"months. event_id narrows the events to that event of the club, whatever its date (an event of another " +
			"org is 422 query.event_id); members and sponsorship keep the range (BE-190). Another kind of org is " +
			"not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsClubRead), Surface: op.SurfaceOrg, QuerySyntax: rangeSyntax}, h.club)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getCompanyStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/company",
		Summary: "Get a company's panel figures",
		Description: "The company's saved audiences with their stored estimates, the reach of its posts and " +
			"dispatches, its contact requests with their acceptance rate and its package usage (SR §8 Şirket). " +
			"Numbers about students are estimates: null below 10, rounded down to their bucket. university_id, " +
			"department_id and class_year narrow the audiences and dispatches to those whose audience names them (a " +
			"department names its university); their stored estimates pass through, nothing is counted again and " +
			"the daily count budget is not spent (BE-190). The other sections are not about an audience. Default " +
			"range: the last 30 days. Another kind of org is not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsCompanyRead), Surface: op.SurfaceOrg, QuerySyntax: companySyntax}, h.company)

	op.Register(api, "stats", huma.Operation{
		OperationID: "getUniversityStats", Method: http.MethodGet, Path: "/orgs/{org_id}/stats/university",
		Summary: "Get a university's panel figures",
		Description: "The university's active students by degree level, class year and department (groups below 10 " +
			"left out, the others rounded down to their bucket), the event density of its clubs (sensitive clubs and " +
			"events of a sensitive category left out) and what waits for its decision (SR §8 Kurum). by_faculty sums " +
			"the departments of each faculty. faculty_id or department_id (one of the university's, not both) " +
			"narrows the students, every group again at least 10; club_id (one of its clubs) narrows the club " +
			"activity, a sensitive club's staying empty; a filter that is not the university's is 422 (BE-190). " +
			"Default range: the last 12 calendar months. Another kind of org is not found." + rangeNote,
	}, op.Meta{Action: string(authz.StatsUniversityRead), Surface: op.SurfaceOrg, QuerySyntax: rangeSyntax}, h.university)
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

func (p RangeParams) query() RangeQuery { return RangeQuery{From: p.From, To: p.To} }

// rangeSyntax documents RangeParams as days; the service checks them after
// the authorisation (RangeQuery.resolve), so they carry no format tag.
var rangeSyntax = map[string]op.ParamSyntax{"from": op.Day, "to": op.Day}

// companySyntax adds class_year, checked by the service too (an "<x>_id"
// filter is documented as a UUID by httpx.DocumentParams).
var companySyntax = map[string]op.ParamSyntax{"from": op.Day, "to": op.Day, "class_year": {Pattern: "^[1-8]$"}}

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

type queuesOutput struct {
	Body QueueStats
}

func (h handler) queues(ctx context.Context, _ *struct{}) (*queuesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Queues(ctx, a)
	if err != nil {
		return nil, err
	}
	return &queuesOutput{Body: st}, nil
}

// The org panels' inputs: the org, the period and the panel's filters
// (BE-190), all checked by the service after the authorisation.
type clubInput struct {
	OrgID string `path:"org_id" doc:"Org ID"`
	RangeParams
	EventID string `query:"event_id" doc:"Only this event of the club, whatever its date"`
}

type companyInput struct {
	OrgID string `path:"org_id" doc:"Org ID"`
	RangeParams
	UniversityID string `query:"university_id" doc:"Audiences and dispatches that name this university or one of its departments"`
	DepartmentID string `query:"department_id" doc:"Audiences and dispatches that name this department"`
	ClassYear    string `query:"class_year" doc:"Audiences and dispatches that name this class year (1-8)"`
}

type universityInput struct {
	OrgID string `path:"org_id" doc:"Org ID"`
	RangeParams
	FacultyID    string `query:"faculty_id" doc:"Students of this faculty's departments only"`
	DepartmentID string `query:"department_id" doc:"Students of this department only"`
	ClubID       string `query:"club_id" doc:"Club activity of this club only (the club's org ID)"`
}

type clubOutput struct {
	Body ClubStats
}

func (h handler) club(ctx context.Context, in *clubInput) (*clubOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := in.query()
	q.EventID = in.EventID
	st, err := h.svc.Club(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	return &clubOutput{Body: st}, nil
}

type companyOutput struct {
	Body CompanyStats
}

func (h handler) company(ctx context.Context, in *companyInput) (*companyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := in.query()
	q.UniversityID, q.DepartmentID, q.ClassYear = in.UniversityID, in.DepartmentID, in.ClassYear
	st, err := h.svc.Company(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	return &companyOutput{Body: st}, nil
}

type universityOutput struct {
	Body UniversityStats
}

func (h handler) university(ctx context.Context, in *universityInput) (*universityOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := in.query()
	q.FacultyID, q.DepartmentID, q.ClubID = in.FacultyID, in.DepartmentID, in.ClubID
	st, err := h.svc.University(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	return &universityOutput{Body: st}, nil
}
