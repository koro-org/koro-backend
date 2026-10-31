package discovery

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// Register adds the discovery operations (04 §2.10) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	op.Register(api, "discovery", huma.Operation{
		OperationID: "searchDiscovery", Method: http.MethodGet, Path: "/orgs/{org_id}/discovery/profiles",
		Summary: "Search anonymous student profiles",
		Description: "Level 2 profiles of the students who chose to be discoverable, under this company's pseudonyms " +
			"(anon_id). OR inside a filter (repeat the parameter), AND across filters; there is no club filter. " +
			"Ordered by relevance to the skill and language filters, ties in an order fixed for the session; the " +
			"order is the only trace of the ranking. When fewer than 10 students match in total the list is empty " +
			"and below_threshold is true. Needs the discovery_access right; every search is audited.",
	}, op.Meta{Action: string(authz.DiscoveryProfileSearch), Surface: op.SurfaceOrg, RateLimit: op.RateSearch}, h.search)

	op.Register(api, "discovery", huma.Operation{
		OperationID: "getDiscoveryProfile", Method: http.MethodGet, Path: "/orgs/{org_id}/discovery/profiles/{anon_id}",
		Summary: "Get an anonymous student profile",
		Description: "The level 2 profile behind one of this company's pseudonyms. Another company's pseudonym, or a " +
			"student who is no longer discoverable, is not found. Needs the discovery_access right; every read is audited.",
	}, op.Meta{Action: string(authz.DiscoveryProfileRead), Surface: op.SurfaceOrg}, h.get)
}

type handler struct {
	svc Service
}

// DiscoveryProfileList is the body of searchDiscovery (04 §2.10, §V.2).
// It has no total, score, band or rank: the order of items is all a
// company learns of the ranking.
type DiscoveryProfileList struct {
	Items          []students.AnonProfileDTO `json:"items"`
	NextCursor     string                    `json:"next_cursor,omitempty"`
	BelowThreshold bool                      `json:"below_threshold" doc:"Fewer than 10 students match the filters: the list is empty"`
}

type searchInput struct {
	OrgID        string   `path:"org_id" doc:"Company org ID"`
	UniversityID []string `query:"university_id,explode" maxItems:"20" doc:"University IDs (repeat the parameter)"`
	DepartmentID []string `query:"department_id,explode" maxItems:"20" doc:"Department IDs (repeat the parameter)"`
	ClassYear    []int    `query:"class_year,explode" maxItems:"8" doc:"Class years 1–8 (repeat the parameter)"`
	DegreeLevel  string   `query:"degree_level" enum:"associate,bachelor,master,doctorate"`
	SkillID      []string `query:"skill_id,explode" maxItems:"20" doc:"Skill IDs (repeat the parameter)"`
	LanguageCode []string `query:"language_code,explode" maxItems:"20" doc:"ISO 639 language codes (repeat the parameter)"`
	page.Params
}

type searchOutput struct {
	Body DiscoveryProfileList
}

func (h handler) search(ctx context.Context, in *searchInput) (*searchOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	q := Query{DegreeLevel: students.DegreeLevel(in.DegreeLevel), LanguageCodes: in.LanguageCode,
		Cursor: in.Cursor, Limit: in.Limit}
	// A malformed filter is 422 only after the authorisation (404/403 first).
	var bad []error
	q.UniversityIDs, bad = queryIDs("query.university_id", in.UniversityID, bad)
	q.DepartmentIDs, bad = queryIDs("query.department_id", in.DepartmentID, bad)
	q.SkillIDs, bad = queryIDs("query.skill_id", in.SkillID, bad)
	for _, y := range in.ClassYear {
		if y < 1 || y > 8 {
			bad = append(bad, invalid("query.class_year", "must be between 1 and 8"))
			break
		}
		q.ClassYears = append(q.ClassYears, int16(y))
	}
	if len(bad) > 0 {
		q.invalid = bad[0]
	}
	p, err := h.svc.Search(ctx, a, org, q)
	if err != nil {
		return nil, err
	}
	httpx.MetricsFrom(ctx).ProfileReads(obs.ReadDiscoverySearch, org, len(p.Items))
	return &searchOutput{Body: DiscoveryProfileList(p)}, nil
}

type getInput struct {
	OrgID  string `path:"org_id" doc:"Company org ID"`
	AnonID string `path:"anon_id" doc:"The company's pseudonym of the student (from searchDiscovery)"`
}

type getOutput struct {
	Body students.AnonProfileDTO
}

func (h handler) get(ctx context.Context, in *getInput) (*getOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Get(ctx, a, org, in.AnonID)
	if err != nil {
		return nil, err
	}
	httpx.MetricsFrom(ctx).ProfileReads(obs.ReadDiscoveryProfile, org, 1)
	return &getOutput{Body: d}, nil
}

// pathID parses a path ID; a malformed one is not found (IDOR rule).
func pathID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

// queryIDs parses a repeated UUID filter; a malformed value adds a 422
// naming the parameter to bad.
func queryIDs(loc string, raw []string, bad []error) ([]uuid.UUID, []error) {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, append(bad, invalid(loc, "must be a UUID"))
		}
		out = append(out, id)
	}
	return out, bad
}

func invalid(loc, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: loc, Message: msg})
}
