package messaging

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

// Cursor sort keys.
const (
	sortNewest      = "-id"
	sortBlockNewest = "-created_at"
)

// Register adds the 10 messaging operations (04 §2.9) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	reg := func(o huma.Operation, m op.Meta) huma.Operation {
		o.Tags = []string{"messaging"}
		return op.Koro(o, m)
	}

	huma.Register(api, reg(huma.Operation{
		OperationID: "createContactRequest", Method: http.MethodPost, Path: "/orgs/{org_id}/contact-requests",
		DefaultStatus: http.StatusCreated,
		Summary:       "Send a contact request to a student",
		Description: "Asks the student behind one of this company's discovery pseudonyms (anon_id) to get in touch; " +
			"opportunity_post_id optionally names a published opportunity call of the company. Spends one " +
			"contact_request unit (403 entitlements.not_entitled without one; never given back). A pseudonym that " +
			"resolves to nobody, a student who is no longer discoverable, who blocked the company or declined it " +
			"within 90 days is not found (404). At most one pending request per student (409 " +
			"messaging.request_pending). The student answers within 14 days or the request expires.",
	}, op.Meta{
		Action: string(authz.MessagingContactRequestCreate), Surface: op.SurfaceOrg, RateLimit: op.RateContactRequest,
		Idempotency: op.IdempotencyRequired,
	}), h.create)

	huma.Register(api, reg(huma.Operation{
		OperationID: "listSentContactRequests", Method: http.MethodGet, Path: "/orgs/{org_id}/contact-requests",
		Summary: "List the company's contact requests", Description: "Newest first.",
	}, op.Meta{Action: string(authz.MessagingContactRequestListSent), Surface: op.SurfaceOrg}), h.listSent)

	huma.Register(api, reg(huma.Operation{
		OperationID: "getSentContactRequest", Method: http.MethodGet, Path: "/orgs/{org_id}/contact-requests/{request_id}",
		Summary: "Get a contact request of the company",
		Description: "The student appears under the company's pseudonym; once the student accepted, student_user_id " +
			"links the level 3 profile.",
	}, op.Meta{Action: string(authz.MessagingContactRequestRead), Surface: op.SurfaceOrg}), h.getSent)

	huma.Register(api, reg(huma.Operation{
		OperationID: "withdrawContactRequest", Method: http.MethodPost,
		Path:    "/orgs/{org_id}/contact-requests/{request_id}/withdraw",
		Summary: "Withdraw a pending contact request",
		Description: "Only a pending request (409 messaging.invalid_state otherwise); the contact_request unit is " +
			"not given back. Withdrawing a withdrawn request changes nothing.",
	}, op.Meta{
		Action: string(authz.MessagingContactRequestWithdraw), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional,
	}), h.withdraw)

	huma.Register(api, reg(huma.Operation{
		OperationID: "listMyContactRequests", Method: http.MethodGet, Path: "/me/contact-requests",
		Summary: "List the contact requests I received", Description: "Newest first.",
	}, op.Meta{Action: string(authz.MessagingContactRequestListReceived), Surface: op.SurfaceSelf}), h.listMine)

	huma.Register(api, reg(huma.Operation{
		OperationID: "getMyContactRequest", Method: http.MethodGet, Path: "/me/contact-requests/{request_id}",
		Summary: "Get a contact request I received",
	}, op.Meta{Action: string(authz.MessagingContactRequestRead), Surface: op.SurfaceSelf}), h.getMine)

	huma.Register(api, reg(huma.Operation{
		OperationID: "respondMyContactRequest", Method: http.MethodPost, Path: "/me/contact-requests/{request_id}/respond",
		Summary: "Accept or decline a contact request",
		Description: "accepted gives the company level 3 access to your profile (it can then see your full profile " +
			"and current CV; contact details only if you share them in your privacy settings) and needs an active " +
			"student verification (403 students.verification_required). declined needs no reason and the company " +
			"is never given one. A request that is no longer pending is 409 messaging.invalid_state; the same " +
			"answer again changes nothing.",
	}, op.Meta{
		Action: string(authz.MessagingContactRequestRespond), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional,
	}), h.respond)

	huma.Register(api, reg(huma.Operation{
		OperationID: "blockCompany", Method: http.MethodPut, Path: "/me/company-blocks/{org_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Block a company's contact requests",
		Description: "The company can no longer send you contact requests and its pending requests are declined. " +
			"The company is not told that you blocked it. Blocking twice changes nothing.",
	}, op.Meta{Action: string(authz.MessagingBlockManage), Surface: op.SurfaceSelf}), h.block)

	huma.Register(api, reg(huma.Operation{
		OperationID: "unblockCompany", Method: http.MethodDelete, Path: "/me/company-blocks/{org_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Unblock a company",
		Description:   "Removing a block that does not exist changes nothing.",
	}, op.Meta{Action: string(authz.MessagingBlockManage), Surface: op.SurfaceSelf}), h.unblock)

	huma.Register(api, reg(huma.Operation{
		OperationID: "listMyCompanyBlocks", Method: http.MethodGet, Path: "/me/company-blocks",
		Summary: "List the companies I blocked", Description: "Newest first.",
	}, op.Meta{Action: string(authz.MessagingBlockListOwn), Surface: op.SurfaceSelf}), h.listBlocks)
}

type handler struct {
	svc Service
}

// parseID parses a path ID; a malformed one is not found (IDOR rule).
func parseID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

// OrgPath is the org_id path parameter of the company surface.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Company org ID (an unknown or malformed ID is 404)"`
}

// RequestPath is the org_id and request_id path parameters.
type RequestPath struct {
	OrgID     string `path:"org_id" doc:"Company org ID (an unknown or malformed ID is 404)"`
	RequestID string `path:"request_id" doc:"Contact request ID (an unknown, malformed or other org's ID is 404)"`
}

// MyRequestPath is the request_id path parameter of /me.
type MyRequestPath struct {
	RequestID string `path:"request_id" doc:"Contact request ID (an unknown, malformed or another user's ID is 404)"`
}

type companyOutput struct{ Body ContactRequestCompanyDTO }

type companyListOutput struct{ Body ContactRequestCompanyList }

type studentOutput struct{ Body ContactRequestStudentDTO }

type studentListOutput struct{ Body ContactRequestStudentList }

type blockListOutput struct{ Body CompanyBlockList }

// ---- createContactRequest

// CreateContactRequestBody is the request of createContactRequest.
type CreateContactRequestBody struct {
	AnonID            string `json:"anon_id" minLength:"1" maxLength:"64" doc:"The company's pseudonym of the student (from discovery)"`
	Subject           string `json:"subject" minLength:"3" maxLength:"120"`
	Message           string `json:"message" minLength:"10" maxLength:"1000" doc:"Shown to the student only; never in a notification"`
	OpportunityPostID string `json:"opportunity_post_id,omitempty" format:"uuid" doc:"A published opportunity call of this company"`
}

type createInput struct {
	idempotency.RequiredHeader
	OrgPath
	Body CreateContactRequestBody
}

func (h handler) create(ctx context.Context, in *createInput) (*companyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ci := CreateInput{AnonID: in.Body.AnonID, Subject: in.Body.Subject, Message: in.Body.Message}
	if in.Body.OpportunityPostID != "" {
		id := parseID(in.Body.OpportunityPostID)
		ci.OpportunityPostID = &id
	}
	v, err := h.svc.Create(ctx, a, parseID(in.OrgID), ci)
	if err != nil {
		return nil, err
	}
	return &companyOutput{Body: ToCompanyDTO(v)}, nil
}

// ---- listSentContactRequests

type listSentInput struct {
	OrgPath
	Status string `query:"status" enum:"pending,accepted,declined,expired,withdrawn"`
	page.Params
}

func (h handler) listSent(ctx context.Context, in *listSentInput) (*companyListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID := parseID(in.OrgID)
	fp := page.Fingerprint("listSentContactRequests", orgID.String(), in.Status)
	after, err := afterID(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListSent(ctx, a, orgID, ListQuery{Status: Status(in.Status), After: after, Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	out := &companyListOutput{Body: ContactRequestCompanyList{Items: make([]ContactRequestCompanyDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = ToCompanyDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

// afterID decodes an id keyset cursor.
func afterID(cursor, fp string) (*uuid.UUID, error) {
	c, err := page.Decode(cursor, sortNewest, fp, 1)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(0)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ---- getSentContactRequest, withdrawContactRequest

type sentInput struct {
	RequestPath
}

func (h handler) getSent(ctx context.Context, in *sentInput) (*companyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetSent(ctx, a, parseID(in.OrgID), parseID(in.RequestID))
	if err != nil {
		return nil, err
	}
	return &companyOutput{Body: ToCompanyDTO(v)}, nil
}

type withdrawInput struct {
	idempotency.OptionalHeader
	RequestPath
}

func (h handler) withdraw(ctx context.Context, in *withdrawInput) (*companyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.Withdraw(ctx, a, parseID(in.OrgID), parseID(in.RequestID))
	if err != nil {
		return nil, err
	}
	return &companyOutput{Body: ToCompanyDTO(v)}, nil
}

// ---- listMyContactRequests, getMyContactRequest

type listMineInput struct {
	Status string `query:"status" enum:"pending,accepted,declined,expired,withdrawn"`
	page.Params
}

func (h handler) listMine(ctx context.Context, in *listMineInput) (*studentListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyContactRequests", a.UserID.String(), in.Status)
	after, err := afterID(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListMine(ctx, a, ListQuery{Status: Status(in.Status), After: after, Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	out := &studentListOutput{Body: ContactRequestStudentList{Items: make([]ContactRequestStudentDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = ToStudentDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

type mineInput struct {
	MyRequestPath
}

func (h handler) getMine(ctx context.Context, in *mineInput) (*studentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetMine(ctx, a, parseID(in.RequestID))
	if err != nil {
		return nil, err
	}
	return &studentOutput{Body: ToStudentDTO(v)}, nil
}

// ---- respondMyContactRequest

// RespondContactRequestBody is the request of respondMyContactRequest. A
// decline takes no reason (04 §2.9).
type RespondContactRequestBody struct {
	Decision string `json:"decision" enum:"accepted,declined"`
}

type respondInput struct {
	idempotency.OptionalHeader
	MyRequestPath
	Body RespondContactRequestBody
}

func (h handler) respond(ctx context.Context, in *respondInput) (*studentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.Respond(ctx, a, parseID(in.RequestID), Decision(in.Body.Decision))
	if err != nil {
		return nil, err
	}
	return &studentOutput{Body: ToStudentDTO(v)}, nil
}

// ---- blockCompany, unblockCompany, listMyCompanyBlocks

type blockInput struct {
	OrgID string `path:"org_id" doc:"Company org ID (an unknown, malformed or non-company ID is 404)"`
}

func (h handler) block(ctx context.Context, in *blockInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.Block(ctx, a, parseID(in.OrgID))
}

func (h handler) unblock(ctx context.Context, in *blockInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.Unblock(ctx, a, parseID(in.OrgID))
}

type listBlocksInput struct {
	page.Params
}

func (h handler) listBlocks(ctx context.Context, in *listBlocksInput) (*blockListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyCompanyBlocks", a.UserID.String())
	var after *BlockKey
	c, err := page.Decode(in.Cursor, sortBlockNewest, fp, 2)
	if err != nil {
		return nil, err
	}
	if c != nil {
		at, err := c.Time(0)
		if err != nil {
			return nil, err
		}
		id, err := c.UUID(1)
		if err != nil {
			return nil, err
		}
		after = &BlockKey{CreatedAt: at, OrgID: id}
	}
	p, err := h.svc.ListBlocks(ctx, a, after, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &blockListOutput{Body: CompanyBlockList{Items: make([]ContactCompanyDTO, len(p.Items))}}
	for i, b := range p.Items {
		out.Body.Items[i] = companyCard(b.Company, b.Block.CompanyOrgID.String())
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortBlockNewest, fp, page.TimeVal(p.Next.CreatedAt), page.UUIDVal(p.Next.OrgID))
	}
	return out, nil
}
