package students

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
)

// registerViews adds the BE-025 operations (04 §2.4): the CVs, the member
// card and the admin view.
func registerViews(api huma.API, h handler) {
	tags := []string{"students"}
	reg := func(o huma.Operation, m op.Meta) huma.Operation {
		o.Tags = tags
		return op.Koro(o, m)
	}
	self := func(act authz.Action, idem op.Idempotency) op.Meta {
		return op.Meta{Action: string(act), Surface: op.SurfaceSelf, Idempotency: idem}
	}

	huma.Register(api, reg(huma.Operation{
		OperationID: "listMyCvs", Method: http.MethodGet, Path: "/me/cvs",
		Summary:     "List my CVs",
		Description: "At most 3, oldest first. The current CV is the one a company with level 3 access may download.",
	}, self(authz.StudentsCvReadOwn, "")), h.listCVs)

	huma.Register(api, reg(huma.Operation{
		OperationID: "createMyCv", Method: http.MethodPost, Path: "/me/cvs",
		Summary: "Add a CV", DefaultStatus: http.StatusCreated,
		Description: "file_id is a cv upload of yours (createUpload purpose cv: PDF only, 10 MB; the file is virus " +
			"scanned and its metadata removed before it is served). At most 3 CVs: a fourth is 409 students.cv_limit. " +
			"The first CV is current; make_current moves the mark to the new one.",
	}, self(authz.StudentsCvManage, op.IdempotencyOptional)), h.createCV)

	huma.Register(api, reg(huma.Operation{
		OperationID: "setMyCurrentCv", Method: http.MethodPost, Path: "/me/cvs/{cv_id}/current",
		Summary:     "Make a CV current",
		Description: "The CV becomes the only current one. Another user's CV is 404.",
	}, self(authz.StudentsCvManage, op.IdempotencyOptional)), h.setCurrentCV)

	huma.Register(api, reg(huma.Operation{
		OperationID: "deleteMyCv", Method: http.MethodDelete, Path: "/me/cvs/{cv_id}",
		Summary: "Delete a CV", DefaultStatus: http.StatusNoContent,
		Description: "Hard delete, the file included. A deleted current CV leaves no current one until you pick " +
			"another. Another user's CV is 404.",
	}, self(authz.StudentsCvManage, "")), h.deleteCV)

	huma.Register(api, reg(huma.Operation{
		OperationID: "downloadMyCv", Method: http.MethodGet, Path: "/me/cvs/{cv_id}/download",
		Summary: "Download a CV",
		Description: "A signed URL valid for 5 minutes (attachment). 409 files.not_clean while the virus scan is " +
			"pending. Every download is audited. Another user's CV is 404.",
	}, self(authz.StudentsCvReadOwn, "")), h.downloadCV)

	huma.Register(api, reg(huma.Operation{
		OperationID: "getMemberCard", Method: http.MethodGet, Path: "/orgs/{org_id}/members/{membership_id}/profile",
		Summary: "Get a club member's card",
		Description: "The club_admin's view of an active member of the club (no e-mail, CV, skills or other clubs). " +
			"A pending or ended membership, another club's member or a non-member caller is 404. A sensitive club's " +
			"card is audited.",
	}, op.Meta{Action: string(authz.StudentsProfileReadMemberCard), Surface: op.SurfaceOrg}), h.getMemberCard)

	huma.Register(api, reg(huma.Operation{
		OperationID: "adminGetStudentProfile", Method: http.MethodGet, Path: "/admin/users/{user_id}/student-profile",
		Summary: "Get a student's profile (staff)",
		Description: "support and platform_admin: break-glass (X-Koro-Access-Reason, 10–500 characters; audited, the " +
			"student is told). dpo: X-Koro-Access-Reason is the ID of an open data subject request of this student " +
			"(the birth date is shown only then). Never CV files, sensitive clubs, form answers or talent values.",
	}, op.Meta{Action: string(authz.StudentsProfileReadAdmin), Surface: op.SurfaceAdmin, BreakGlass: true}), h.adminGetProfile)
}

// pathID parses a path UUID; a malformed ID is simply not found (IDOR rule).
func pathID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

// ---- CVs

type cvListOutput struct {
	Body StudentCVList
}

func (h handler) listCVs(ctx context.Context, _ *struct{}) (*cvListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListMyCVs(ctx, a)
	if err != nil {
		return nil, err
	}
	return &cvListOutput{Body: StudentCVList{Items: cvDTOs(vs)}}, nil
}

type createCVInput struct {
	idempotency.OptionalHeader
	Body struct {
		FileID      string `json:"file_id" format:"uuid" doc:"A cv upload of yours (createUpload)"`
		MakeCurrent bool   `json:"make_current,omitempty" doc:"Make the new CV the current one (the first CV always is)"`
	}
}

type cvOutput struct {
	Body StudentCVDTO
}

func (h handler) createCV(ctx context.Context, in *createCVInput) (*cvOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.Body.FileID)
	if err != nil {
		return nil, fieldErr("body.file_id", "must be a UUID")
	}
	v, err := h.svc.CreateMyCV(ctx, a, id, in.Body.MakeCurrent)
	if err != nil {
		return nil, err
	}
	return &cvOutput{Body: toCVDTO(v)}, nil
}

// CVPath is the {cv_id} path parameter (exported: huma binds parameters of
// embedded structs only when the type is exported).
type CVPath struct {
	CVID string `path:"cv_id" doc:"CV ID (another user's or a malformed ID is 404)"`
}

type setCurrentCVInput struct {
	idempotency.OptionalHeader
	CVPath
}

func (h handler) setCurrentCV(ctx context.Context, in *setCurrentCVInput) (*cvOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := pathID(in.CVID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.SetMyCurrentCV(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &cvOutput{Body: toCVDTO(v)}, nil
}

func (h handler) deleteCV(ctx context.Context, in *CVPath) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := pathID(in.CVID)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.DeleteMyCV(ctx, a, id)
}

type cvDownloadOutput struct {
	Body StudentCVDownloadDTO
}

func (h handler) downloadCV(ctx context.Context, in *CVPath) (*cvDownloadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := pathID(in.CVID)
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.DownloadMyCV(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &cvDownloadOutput{Body: toDownloadDTO(dl)}, nil
}

// ---- member card

type memberCardInput struct {
	OrgID        string `path:"org_id" doc:"Club org ID"`
	MembershipID string `path:"membership_id" doc:"Membership ID (org_members.id)"`
}

type memberCardOutput struct {
	Body MemberCardDTO
}

func (h handler) getMemberCard(ctx context.Context, in *memberCardInput) (*memberCardOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	m, err := pathID(in.MembershipID)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.GetMemberCard(ctx, a, org, m)
	if err != nil {
		return nil, err
	}
	return &memberCardOutput{Body: toMemberCardDTO(c)}, nil
}

// ---- admin view

type adminProfileInput struct {
	UserID       string `path:"user_id" doc:"User ID"`
	AccessReason string `header:"X-Koro-Access-Reason" maxLength:"500" doc:"support, platform_admin: break-glass justification (10–500 characters); dpo: the ID of an open data subject request of this student"`
}

type adminProfileOutput struct {
	Body AdminStudentViewDTO
}

func (h handler) adminGetProfile(ctx context.Context, in *adminProfileInput) (*adminProfileOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.UserID)
	if err != nil {
		id = uuid.Nil() // the admin surface decides first: a non-staff caller gets 403, staff 404
	}
	v, err := h.svc.AdminGetStudentProfile(ctx, a, id, AdminAccess{Reason: in.AccessReason})
	if err != nil {
		return nil, err
	}
	return &adminProfileOutput{Body: toAdminStudentViewDTO(v)}, nil
}
