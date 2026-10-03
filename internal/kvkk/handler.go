package kvkk

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the 22 kvkk operations (04 §2.22) to api: the 13 data
// subject request operations and the 9 records operations of kvkk-2.
// svc may be nil (`koro openapi`): handlers only touch it when called. clk
// computes days_left (nil → real clock).
func Register(api huma.API, svc Service, clk clock.Clock) {
	if clk == nil {
		clk = clock.Real()
	}
	h := handler{svc: svc, clk: clk}

	huma.Register(api, kop(huma.Operation{
		OperationID: "createMyDataRequest", Method: http.MethodPost, Path: "/me/data-requests",
		Summary: "Submit a data subject request", DefaultStatus: http.StatusCreated,
		Description: "KVKK md. 11 application. The answer is due within 30 days (due_at). An erasure (account deletion) " +
			"request needs the re-authentication proof in reauth (403 identity.reauth_required otherwise). One open " +
			"request per type (409 kvkk.duplicate_request).",
	}, op.Meta{Action: string(authz.KvkkDsrCreate), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional,
		Reauth: true, RateLimit: op.RateExport}), h.createMine)

	huma.Register(api, kop(huma.Operation{
		OperationID: "listMyDataRequests", Method: http.MethodGet, Path: "/me/data-requests",
		Summary: "List my data subject requests",
	}, op.Meta{Action: string(authz.KvkkDsrListOwn), Surface: op.SurfaceSelf}), h.listMine)

	huma.Register(api, kop(huma.Operation{
		OperationID: "getMyDataRequest", Method: http.MethodGet, Path: "/me/data-requests/{dsr_id}",
		Summary: "Get my data subject request and its answer",
	}, op.Meta{Action: string(authz.KvkkDsrReadOwn), Surface: op.SurfaceSelf}), h.getMine)

	huma.Register(api, kop(huma.Operation{
		OperationID: "cancelMyDataRequest", Method: http.MethodPost, Path: "/me/data-requests/{dsr_id}/cancel",
		Summary:     "Withdraw my data subject request",
		Description: "Only while the request is received or under identity check (409 kvkk.invalid_state otherwise).",
	}, op.Meta{Action: string(authz.KvkkDsrCancelOwn), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}), h.cancelMine)

	huma.Register(api, kop(huma.Operation{
		OperationID: "downloadMyDataExport", Method: http.MethodGet, Path: "/me/data-requests/{dsr_id}/export",
		Summary: "Get a download link of my data export",
		Description: "A presigned URL valid for at most 5 minutes; every call is audited. The file is deleted 7 days " +
			"after the request was answered (410 kvkk.export_expired); 404 kvkk.export_not_ready while it is built.",
	}, op.Meta{Action: string(authz.KvkkDsrExportDownload), Surface: op.SurfaceSelf}), h.downloadMine)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminListDataRequests", Method: http.MethodGet, Path: "/admin/data-requests",
		Summary:     "List the data subject request queue",
		Description: "Soonest deadline first. platform_admin sees only requests awaiting the second approval (03 k32).",
	}, op.Meta{Action: string(authz.KvkkDsrList), Surface: op.SurfaceAdmin}), h.adminList)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminGetDataRequest", Method: http.MethodGet, Path: "/admin/data-requests/{dsr_id}",
		Summary: "Get a data subject request", Description: "The ETag is the If-Match of adminUpdateDataRequest.",
	}, op.Meta{Action: string(authz.KvkkDsrRead), Surface: op.SurfaceAdmin}), h.adminGet)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminCreateDataRequest", Method: http.MethodPost, Path: "/admin/data-requests",
		Summary: "Record a postal, KEP, e-mail or in-person application", DefaultStatus: http.StatusCreated,
		Description: "user_id for an account holder, requester_email (and requester_name) otherwise. The identity " +
			"check is recorded with adminUpdateDataRequest before the request can be completed.",
	}, op.Meta{Action: string(authz.KvkkDsrCreateAdmin), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}), h.adminCreate)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminUpdateDataRequest", Method: http.MethodPatch, Path: "/admin/data-requests/{dsr_id}",
		Summary: "Assign, record the identity check, set the working status or draft the answer",
	}, op.Meta{Action: string(authz.KvkkDsrUpdate), Surface: op.SurfaceAdmin}), h.adminUpdate)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminApproveDataRequest", Method: http.MethodPost, Path: "/admin/data-requests/{dsr_id}/approve",
		Summary: "Approve the release of a profiling report",
		Description: "profiling_access only. The second approver must differ from the first (403 " +
			"authz.four_eyes_violation); platform_admin may give the second approval only.",
	}, op.Meta{Action: string(authz.KvkkDsrApprove), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}), h.approve)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminCompleteDataRequest", Method: http.MethodPost, Path: "/admin/data-requests/{dsr_id}/complete",
		Summary: "Answer and close a data subject request", DefaultStatus: http.StatusAccepted,
		Description: "Re-authentication required. Queues what the answer sets in motion: the data export (access, " +
			"export, profiling_access) or the account erasure; the applicant is notified when it is done.",
	}, op.Meta{Action: string(authz.KvkkDsrComplete), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Reauth: true, RateLimit: op.RateReauth}), h.complete)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminRejectDataRequest", Method: http.MethodPost, Path: "/admin/data-requests/{dsr_id}/reject",
		Summary: "Reject a data subject request with a reason",
	}, op.Meta{Action: string(authz.KvkkDsrReject), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}), h.reject)

	huma.Register(api, kop(huma.Operation{
		OperationID: "adminDownloadDataExport", Method: http.MethodGet, Path: "/admin/data-requests/{dsr_id}/export",
		Summary: "Get a download link of a data export", Description: "A presigned URL valid for at most 5 minutes; audited.",
	}, op.Meta{Action: string(authz.KvkkDsrExportDownload), Surface: op.SurfaceAdmin}), h.adminDownload)

	registerRecords(api, h)
}

// kop tags an operation with the module and writes its Koro extensions.
func kop(o huma.Operation, m op.Meta) huma.Operation {
	o.Tags = []string{"kvkk"}
	return op.Koro(o, m)
}

type handler struct {
	svc Service
	clk clock.Clock
}

// parseID parses a path ID; a malformed one is simply not found.
func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

// ---- /me

type createMineInput struct {
	idempotency.OptionalHeader
	Body struct {
		Type    string        `json:"type" enum:"access,export,rectification,erasure,restriction,objection,profiling_access,profiling_objection,info"`
		Details string        `json:"details,omitempty" maxLength:"4000" doc:"Free text of the application (optional)"`
		Reauth  *DsrReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof; mandatory for erasure (03 k31)"`
	}
}

type selfOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DsrSelfDTO
}

func (h handler) createMine(ctx context.Context, in *createMineInput) (*selfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.CreateMine(ctx, a, MyRequest{Type: Type(in.Body.Type), Details: in.Body.Details, Proof: in.Body.Reauth.proof()})
	if err != nil {
		return nil, err
	}
	return &selfOutput{CacheControl: "no-store", Body: toSelfDTO(d)}, nil
}

type listMineInput struct {
	page.Params
}

type selfListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DsrSelfList
}

func (h handler) listMine(ctx context.Context, in *listMineInput) (*selfListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyDataRequests")
	after, err := afterOf(in.Cursor, sortOwn, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListMine(ctx, a, after, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &selfListOutput{CacheControl: "no-store", Body: DsrSelfList{Items: make([]DsrSelfDTO, len(p.Items)), NextCursor: cursorOf(sortOwn, fp, p.Next)}}
	for i, d := range p.Items {
		out.Body.Items[i] = toSelfDTO(d)
	}
	return out, nil
}

type mineIDInput struct {
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown, malformed or foreign ID is 404)"`
}

type selfDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DsrSelfDetailDTO
}

func (h handler) getMine(ctx context.Context, in *mineIDInput) (*selfDetailOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.GetMine(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &selfDetailOutput{CacheControl: "no-store", Body: DsrSelfDetailDTO{DsrSelfDTO: toSelfDTO(d),
		ResponseSummary: d.ResponseSummary, RejectionReason: d.RejectionReason}}, nil
}

type cancelInput struct {
	idempotency.OptionalHeader
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown, malformed or foreign ID is 404)"`
}

func (h handler) cancelMine(ctx context.Context, in *cancelInput) (*selfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.CancelMine(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &selfOutput{CacheControl: "no-store", Body: toSelfDTO(d)}, nil
}

type urlOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SignedURLDTO
}

func (h handler) downloadMine(ctx context.Context, in *mineIDInput) (*urlOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.DownloadMine(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &urlOutput{CacheControl: "no-store", Body: SignedURLDTO(dl)}, nil
}

// ---- /admin

type adminListInput struct {
	page.Params
	Status    string `query:"status" enum:"received,identity_check,in_progress,awaiting_second_approval,completed,rejected,cancelled"`
	Type      string `query:"type" enum:"access,export,rectification,erasure,restriction,objection,profiling_access,profiling_objection,info"`
	DueBefore string `query:"due_before" format:"date-time" doc:"Only requests due before this instant"`
}

type adminListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DsrAdminList
}

func (h handler) adminList(ctx context.Context, in *adminListInput) (*adminListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var f AdminFilter
	if in.Status != "" {
		f.Statuses = []Status{Status(in.Status)}
	}
	if in.Type != "" {
		t := Type(in.Type)
		f.Type = &t
	}
	if in.DueBefore != "" {
		at, err := time.Parse(time.RFC3339Nano, in.DueBefore)
		if err != nil {
			return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: "query.due_before", Message: "must be an RFC 3339 instant"})
		}
		f.DueBefore = &at
	}
	fp := page.Fingerprint("adminListDataRequests", in.Status, in.Type, in.DueBefore)
	after, err := afterOf(in.Cursor, sortAdmin, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminList(ctx, a, f, after, in.Limit)
	if err != nil {
		return nil, err
	}
	now := h.clk.Now()
	out := &adminListOutput{CacheControl: "no-store", Body: DsrAdminList{Items: make([]DsrAdminDTO, len(p.Items)), NextCursor: cursorOf(sortAdmin, fp, p.Next)}}
	for i, d := range p.Items {
		out.Body.Items[i] = toAdminDTO(d, now)
	}
	return out, nil
}

type adminIDInput struct {
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown or malformed ID is 404)"`
}

type adminOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         DsrAdminDTO
}

func (h handler) adminOut(d DSR) *adminOutput {
	return &adminOutput{ETag: ETag(d), CacheControl: "no-store", Body: toAdminDTO(d, h.clk.Now())}
}

func (h handler) adminGet(ctx context.Context, in *adminIDInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.AdminGet(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

type adminCreateInput struct {
	idempotency.OptionalHeader
	Body struct {
		UserID         string `json:"user_id,omitempty" format:"uuid" doc:"Account of the applicant, if any"`
		RequesterName  string `json:"requester_name,omitempty" maxLength:"200" doc:"Applicant without an account"`
		RequesterEmail string `json:"requester_email,omitempty" maxLength:"254" doc:"Applicant without an account (mandatory then)"`
		Type           string `json:"type" enum:"access,export,rectification,erasure,restriction,objection,profiling_access,profiling_objection,info"`
		Channel        string `json:"channel" enum:"email,post,kep,in_person"`
		Details        string `json:"details,omitempty" maxLength:"4000"`
	}
}

func (h handler) adminCreate(ctx context.Context, in *adminCreateInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r := AdminRequest{RequesterName: in.Body.RequesterName, RequesterEmail: in.Body.RequesterEmail,
		Type: Type(in.Body.Type), Channel: Channel(in.Body.Channel), Details: in.Body.Details}
	if in.Body.UserID != "" {
		id, err := uuid.Parse(in.Body.UserID)
		if err != nil {
			return nil, invalid("body.user_id", "must be a UUID")
		}
		r.UserID = &id
	}
	d, err := h.svc.AdminCreate(ctx, a, r)
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

type adminUpdateInput struct {
	DSRID   string `path:"dsr_id" doc:"Request ID (an unknown or malformed ID is 404)"`
	IfMatch string `header:"If-Match" maxLength:"100" doc:"ETag of adminGetDataRequest (428 without, 412 when stale)"`
	Body    struct {
		Status             *string    `json:"status,omitempty" enum:"identity_check,in_progress"`
		AssignedTo         *string    `json:"assigned_to,omitempty" format:"uuid" doc:"The DPO handling the request"`
		IdentityVerifiedAt *time.Time `json:"identity_verified_at,omitempty" doc:"When the applicant's identity was verified"`
		ResponseSummary    *string    `json:"response_summary,omitempty" maxLength:"4000" doc:"Draft of the answer"`
	}
}

func (h handler) adminUpdate(ctx context.Context, in *adminUpdateInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	p := Patch{IdentityVerifiedAt: in.Body.IdentityVerifiedAt, ResponseSummary: in.Body.ResponseSummary}
	if in.Body.Status != nil {
		st := Status(*in.Body.Status)
		p.Status = &st
	}
	if in.Body.AssignedTo != nil {
		uid, err := uuid.Parse(*in.Body.AssignedTo)
		if err != nil {
			return nil, invalid("body.assigned_to", "must be a UUID")
		}
		p.AssignedTo = &uid
	}
	d, err := h.svc.AdminUpdate(ctx, a, id, in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

type approveInput struct {
	idempotency.OptionalHeader
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown or malformed ID is 404)"`
	Body  struct {
		Stage string `json:"stage" enum:"first,second"`
	}
}

func (h handler) approve(ctx context.Context, in *approveInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Approve(ctx, a, id, Stage(in.Body.Stage))
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

type completeInput struct {
	idempotency.OptionalHeader
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown or malformed ID is 404)"`
	Body  struct {
		ResponseSummary string        `json:"response_summary" minLength:"1" maxLength:"4000" doc:"The answer to the applicant"`
		Reauth          *DsrReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) complete(ctx context.Context, in *completeInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Complete(ctx, a, id, in.Body.ResponseSummary, in.Body.Reauth.proof())
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

type rejectInput struct {
	idempotency.OptionalHeader
	DSRID string `path:"dsr_id" doc:"Request ID (an unknown or malformed ID is 404)"`
	Body  struct {
		RejectionReason string `json:"rejection_reason" minLength:"1" maxLength:"1000"`
	}
}

func (h handler) reject(ctx context.Context, in *rejectInput) (*adminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Reject(ctx, a, id, in.Body.RejectionReason)
	if err != nil {
		return nil, err
	}
	return h.adminOut(d), nil
}

func (h handler) adminDownload(ctx context.Context, in *adminIDInput) (*urlOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(in.DSRID)
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.AdminDownload(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &urlOutput{CacheControl: "no-store", Body: SignedURLDTO(dl)}, nil
}
