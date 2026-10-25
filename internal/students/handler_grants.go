package students

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// sortGrants is the cursor sort key of the grant lists (granted_at DESC,
// id DESC).
const sortGrants = "granted_desc"

// registerGrants adds the BE-046 operations (04 §2.4): the student's level
// 3 grants and the company's views of its granted students; BE-088 adds
// the student's resume of a suspended grant.
func registerGrants(api huma.API, h handler) {
	self := func(act authz.Action, idem op.Idempotency) op.Meta {
		return op.Meta{Action: string(act), Surface: op.SurfaceSelf, Idempotency: idem}
	}
	org := func(act authz.Action, idem op.Idempotency) op.Meta {
		return op.Meta{Action: string(act), Surface: op.SurfaceOrg, Idempotency: idem}
	}

	op.Register(api, "students", huma.Operation{
		OperationID: "listMyAccessGrants", Method: http.MethodGet, Path: "/me/access-grants",
		Summary: "List the companies that see my profile",
		Description: "The companies you gave level 3 access to your full profile and current CV (you accepted their " +
			"contact request), newest first. A grant is suspended while your student verification has lapsed: the " +
			"company sees nothing, and the grant never reopens by itself; once verified again you may resume it until " +
			"resumable_until, after which it ends. Any account lists its own grants, without a student verification.",
	}, self(authz.StudentsAccessGrantListOwn, ""), h.listMyGrants)

	op.Register(api, "students", huma.Operation{
		OperationID: "revokeMyAccessGrant", Method: http.MethodPost, Path: "/me/access-grants/{grant_id}/revoke",
		Summary: "Revoke a company's access", DefaultStatus: http.StatusNoContent,
		Description: "The company loses level 3 access at once; a suspended grant ends too. No student verification " +
			"is needed. The reason is optional and never shown to the company. Revoking a revoked grant changes " +
			"nothing; another user's grant is 404.",
	}, self(authz.StudentsAccessGrantRevoke, op.IdempotencyOptional), h.revokeMyGrant)

	op.Register(api, "students", huma.Operation{
		OperationID: "resumeMyAccessGrant", Method: http.MethodPost, Path: "/me/access-grants/{grant_id}/resume",
		Summary: "Resume a suspended company access",
		Description: "Gives the named company level 3 access again to a grant suspended when your student " +
			"verification lapsed: an explicit consent, possible only with an active student verification (403 " +
			"students.verification_required) and only until resumable_until (409 students.resume_window_expired; " +
			"the company then needs a new contact request). A grant that is not suspended is 409 " +
			"students.invalid_state; another user's grant is 404. The company is not notified.",
	}, self(authz.StudentsAccessGrantResume, op.IdempotencyOptional), h.resumeMyGrant)

	op.Register(api, "students", huma.Operation{
		OperationID: "listCompanyGrantedStudents", Method: http.MethodGet, Path: "/orgs/{org_id}/students",
		Summary: "List the students who granted the company access",
		Description: "Students with an active level 3 grant of this company (they accepted its contact request) and " +
			"an active student verification, newest grant first; q filters by name. Minors never appear.",
	}, org(authz.StudentsAccessGrantListCompany, ""), h.listCompanyStudents)

	op.Register(api, "students", huma.Operation{
		OperationID: "getCompanyStudentProfile", Method: http.MethodGet, Path: "/orgs/{org_id}/students/{user_id}",
		Summary: "Get a granted student's profile",
		Description: "The level 3 profile. Without an active grant of this company, or while the student's " +
			"verification has lapsed, the student is not found (404, as an unknown student). The e-mail only when " +
			"the student shares it; clubs only when the student shares them (never a sensitive club). Every read " +
			"is audited.",
	}, org(authz.StudentsProfileReadCompany, ""), h.getCompanyStudent)

	op.Register(api, "students", huma.Operation{
		OperationID: "downloadCompanyStudentCv", Method: http.MethodGet, Path: "/orgs/{org_id}/students/{user_id}/cv",
		Summary: "Download a granted student's CV",
		Description: "A signed URL of the student's current CV, valid for 1 minute. Without an active grant, while " +
			"the student's verification has lapsed, or without a current CV, 404; 409 files.not_clean while the " +
			"virus scan is pending. Every call is audited.",
	}, org(authz.StudentsCvReadCompany, ""), h.downloadCompanyCV)

	op.Register(api, "students", huma.Operation{
		OperationID: "revokeCompanyAccessGrant", Method: http.MethodPost, Path: "/orgs/{org_id}/students/{user_id}/access-grant/revoke",
		Summary: "Give up access to a student", DefaultStatus: http.StatusNoContent,
		Description: "The company ends its level 3 access. The reason is optional and never shown to the student. " +
			"Without an active grant (a suspended one included), 404.",
	}, org(authz.StudentsAccessGrantRevoke, op.IdempotencyOptional), h.revokeCompanyGrant)
}

// grantAfter decodes a grant list cursor.
func grantAfter(raw, fp string) (*GrantKey, error) {
	c, err := page.Decode(raw, sortGrants, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &GrantKey{GrantedAt: at, ID: id}, nil
}

func grantNext(k *GrantKey, fp string) string {
	if k == nil {
		return ""
	}
	return page.Next(sortGrants, fp, page.TimeVal(k.GrantedAt), page.UUIDVal(k.ID))
}

// accessGrantRevokeBody is the optional body of the revoke operations.
type accessGrantRevokeBody struct {
	Reason string `json:"reason,omitempty" maxLength:"300" doc:"Optional note (at most 300 characters); never shown to the other side"`
}

func (b *accessGrantRevokeBody) reason() string {
	if b == nil {
		return ""
	}
	return b.Reason
}

// ---- the student's grants

type myGrantsInput struct {
	page.Params
}

type myGrantsOutput struct {
	Body StudentAccessGrantList
}

func (h handler) listMyGrants(ctx context.Context, in *myGrantsInput) (*myGrantsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyAccessGrants")
	after, err := grantAfter(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyAccessGrants(ctx, a, after, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &myGrantsOutput{Body: StudentAccessGrantList{Items: make([]StudentAccessGrantDTO, len(p.Items)),
		NextCursor: grantNext(p.Next, fp)}}
	for i, v := range p.Items {
		out.Body.Items[i] = toAccessGrantDTO(v)
	}
	return out, nil
}

type revokeMyGrantInput struct {
	idempotency.OptionalHeader
	GrantID string `path:"grant_id" doc:"Grant ID (another user's or a malformed ID is 404)"`
	Body    *accessGrantRevokeBody
}

func (h handler) revokeMyGrant(ctx context.Context, in *revokeMyGrantInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.GrantID)
	if err != nil {
		id = uuid.Nil() // the role check decides first, then the unknown grant is 404
	}
	return nil, h.svc.RevokeMyAccessGrant(ctx, a, id, in.Body.reason())
}

type resumeMyGrantInput struct {
	idempotency.OptionalHeader
	GrantID string `path:"grant_id" doc:"Grant ID (another user's or a malformed ID is 404)"`
}

type resumeMyGrantOutput struct {
	Body StudentAccessGrantDTO
}

func (h handler) resumeMyGrant(ctx context.Context, in *resumeMyGrantInput) (*resumeMyGrantOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.GrantID)
	if err != nil {
		id = uuid.Nil() // the role check decides first, then the unknown grant is 404
	}
	v, err := h.svc.ResumeMyAccessGrant(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &resumeMyGrantOutput{Body: toAccessGrantDTO(v)}, nil
}

// ---- the company's views

// CompanyPath is the {org_id} path parameter of the company views.
type CompanyPath struct {
	OrgID string `path:"org_id" doc:"Company org ID"`
}

// CompanyStudentPath is {org_id}/students/{user_id}.
type CompanyStudentPath struct {
	OrgID  string `path:"org_id" doc:"Company org ID"`
	UserID string `path:"user_id" doc:"Student user ID (from an accepted contact request)"`
}

// ids parses the path; a malformed ID is not found (IDOR rule).
func (p CompanyStudentPath) ids() (org, user uuid.UUID, err error) {
	if org, err = pathID(p.OrgID); err != nil {
		return org, user, err
	}
	user, err = pathID(p.UserID)
	return org, user, err
}

type companyStudentsInput struct {
	CompanyPath
	Q string `query:"q" maxLength:"100" doc:"Name filter (case-insensitive)"`
	page.Params
}

type companyStudentsOutput struct {
	Body StudentL3CardList
}

func (h handler) listCompanyStudents(ctx context.Context, in *companyStudentsInput) (*companyStudentsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listCompanyGrantedStudents", org.String(), in.Q)
	after, err := grantAfter(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListCompanyStudents(ctx, a, org, CompanyStudentQuery{Q: in.Q, After: after, Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	return &companyStudentsOutput{Body: StudentL3CardList{Items: p.Items, NextCursor: grantNext(p.Next, fp)}}, nil
}

type companyStudentOutput struct {
	Body StudentProfileL3DTO
}

func (h handler) getCompanyStudent(ctx context.Context, in *CompanyStudentPath) (*companyStudentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, user, err := in.ids()
	if err != nil {
		return nil, err
	}
	d, err := h.svc.GetCompanyStudentProfile(ctx, a, org, user)
	if err != nil {
		return nil, err
	}
	return &companyStudentOutput{Body: d}, nil
}

func (h handler) downloadCompanyCV(ctx context.Context, in *CompanyStudentPath) (*cvDownloadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, user, err := in.ids()
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.DownloadCompanyStudentCV(ctx, a, org, user)
	if err != nil {
		return nil, err
	}
	return &cvDownloadOutput{Body: toDownloadDTO(dl)}, nil
}

type revokeCompanyGrantInput struct {
	idempotency.OptionalHeader
	CompanyStudentPath
	Body *accessGrantRevokeBody
}

func (h handler) revokeCompanyGrant(ctx context.Context, in *revokeCompanyGrantInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, user, err := in.ids()
	if err != nil {
		return nil, err
	}
	return nil, h.svc.RevokeCompanyAccessGrant(ctx, a, org, user, in.Body.reason())
}
