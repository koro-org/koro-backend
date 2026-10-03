package orgs

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
)

// Register adds the orgs operations of BE-017 (04 §2.2) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	reg(api, huma.Operation{
		OperationID: "getOrg", Method: http.MethodGet, Path: "/orgs/{org_id}",
		Summary: "Get an organization's public profile",
		Description: "Active and suspended orgs are public; a pending_review or closed org is visible to its members and " +
			"platform staff only (others get 404). The ETag is the If-Match value of updateOrg.",
	}, op.Meta{Action: string(authz.OrgsOrgRead), Surface: op.SurfacePublic}, h.getOrg)

	reg(api, huma.Operation{
		OperationID: "getOrgBySlug", Method: http.MethodGet, Path: "/orgs/by-slug/{slug}",
		Summary: "Get an organization's public profile by slug",
	}, op.Meta{Action: string(authz.OrgsOrgRead), Surface: op.SurfacePublic}, h.getOrgBySlug)

	reg(api, huma.Operation{
		OperationID: "searchOrgs", Method: http.MethodGet, Path: "/orgs",
		Summary:     "Search organizations",
		Description: "Active and suspended orgs only. q matches name and description (full text) or part of the name.",
	}, op.Meta{Action: string(authz.OrgsOrgSearch), Surface: op.SurfacePublic, RateLimit: op.RateSearch}, h.searchOrgs)

	reg(api, huma.Operation{
		OperationID: "createCompany", Method: http.MethodPost, Path: "/companies", DefaultStatus: http.StatusCreated,
		Summary: "Register a company",
		Description: "The company starts pending_review until a platform admin verifies it. The caller becomes its " +
			"company_owner; all of the caller's sessions are closed (role_change) so that the MFA policy of the role " +
			"applies at the next sign-in. Tax number and MERSİS are given later with the legal party (parties).",
	}, op.Meta{Action: string(authz.OrgsCompanyCreate), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}, h.createCompany)

	reg(api, huma.Operation{
		OperationID: "updateOrg", Method: http.MethodPatch, Path: "/orgs/{org_id}",
		Summary: "Update an organization's profile",
		Description: "If-Match with the ETag of getOrg is required: missing → 428, stale → 412. Company fields apply to " +
			"companies only. A suspended org cannot be changed by its members (403 orgs.org_suspended).",
	}, op.Meta{Action: string(authz.OrgsOrgUpdate), Surface: op.SurfaceOrg}, h.updateOrg)

	reg(api, huma.Operation{
		OperationID: "closeOrg", Method: http.MethodPost, Path: "/orgs/{org_id}/close",
		Summary: "Close an organization",
		Description: "Company owner or platform admin, with re-authentication. Open orders or payouts → 409 " +
			"orgs.open_commerce. Every membership ends (org_closed).",
	}, op.Meta{Action: string(authz.OrgsOrgClose), Surface: op.SurfaceOrg, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.closeOrg)

	reg(api, huma.Operation{
		OperationID: "transferOrgOwnership", Method: http.MethodPost, Path: "/orgs/{org_id}/ownership-transfer",
		Summary: "Transfer company ownership",
		Description: "The new owner must be an active company_admin of the company (422 orgs.transfer_target_invalid). " +
			"The former owner stays as company_admin; both users' sessions are closed.",
	}, op.Meta{Action: string(authz.OrgsOwnershipTransfer), Surface: op.SurfaceOrg, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.transfer)

	reg(api, huma.Operation{
		OperationID: "adminListOrgs", Method: http.MethodGet, Path: "/admin/orgs",
		Summary: "List organizations (platform staff)",
	}, op.Meta{Action: string(authz.OrgsOrgListAdmin), Surface: op.SurfaceAdmin}, h.adminList)

	reg(api, huma.Operation{
		OperationID: "adminVerifyOrg", Method: http.MethodPost, Path: "/admin/orgs/{org_id}/verify",
		Summary: "Verify an organization", Description: "pending_review → active; another status is 409 orgs.invalid_state.",
	}, op.Meta{Action: string(authz.OrgsOrgVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminVerify)

	reg(api, huma.Operation{
		OperationID: "adminSuspendOrg", Method: http.MethodPost, Path: "/admin/orgs/{org_id}/suspend",
		Summary: "Suspend an organization", Description: "active → suspended, with re-authentication.",
	}, op.Meta{Action: string(authz.OrgsOrgSuspend), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.adminSuspend)

	reg(api, huma.Operation{
		OperationID: "adminReactivateOrg", Method: http.MethodPost, Path: "/admin/orgs/{org_id}/reactivate",
		Summary: "Reactivate a suspended organization", Description: "suspended → active, with re-authentication.",
	}, op.Meta{Action: string(authz.OrgsOrgSuspend), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.adminReactivate)
}

// reg registers one orgs operation with its Koro metadata.
func reg[I, O any](api huma.API, o huma.Operation, m op.Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{"orgs"}
	huma.Register(api, op.Koro(o, m), fn)
}

type handler struct{ svc Service }

func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

// OrgPath is the org_id path parameter (exported: huma reads the fields
// of an embedded struct only when its type is exported). A malformed ID
// becomes the nil UUID, which no org has: it answers like a missing org (404), after the admin
// surface's 403 for callers without a platform role (03 §7.2).
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID"`
}

func (in OrgPath) id() uuid.UUID {
	id, err := uuid.Parse(in.OrgID)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

type publicOutput struct {
	ETag string `header:"ETag"`
	Body OrgPublicDTO
}

type orgOutput struct {
	ETag string `header:"ETag"`
	Body OrgDTO
}

func orgOut(v OrgView) *orgOutput { return &orgOutput{ETag: v.ETag, Body: toOrgDTO(v)} }

// ---- getOrg / getOrgBySlug

func (h handler) getOrg(ctx context.Context, in *OrgPath) (*publicOutput, error) {
	v, err := h.svc.GetOrg(ctx, authz.ActorOrAnonymous(ctx), in.id())
	if err != nil {
		return nil, err
	}
	return &publicOutput{ETag: v.ETag, Body: toPublicDTO(v)}, nil
}

type slugInput struct {
	Slug string `path:"slug" maxLength:"80" doc:"Organization slug"`
}

func (h handler) getOrgBySlug(ctx context.Context, in *slugInput) (*publicOutput, error) {
	v, err := h.svc.GetOrgBySlug(ctx, authz.ActorOrAnonymous(ctx), in.Slug)
	if err != nil {
		return nil, err
	}
	return &publicOutput{ETag: v.ETag, Body: toPublicDTO(v)}, nil
}

// ---- searchOrgs / adminListOrgs

type searchInput struct {
	page.Params
	Q            string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	Kind         string `query:"kind" enum:"platform,university,club,company"`
	UniversityID string `query:"university_id" format:"uuid" doc:"The university's own org and its clubs"`
	City         string `query:"city" maxLength:"60"`
	Sort         string `query:"sort" enum:"name" doc:"name (A-Z); default newest first"`
}

type searchOutput struct {
	Body OrgCardList
}

// cursorOf decodes a cursor of sort with the request's filter fingerprint.
func cursorOf(raw, sort, fp string) (*ListAfter, error) {
	n := 1
	if sort == SortName {
		n = 2
	}
	c, err := page.Decode(raw, sort, fp, n)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(n - 1)
	if err != nil {
		return nil, err
	}
	a := &ListAfter{ID: id}
	if sort == SortName {
		a.Name = c.Vals[0]
	}
	return a, nil
}

func nextCursor(p OrgPage, fp string) string {
	if p.Next == nil {
		return ""
	}
	if p.Sort == SortName {
		return page.Next(SortName, fp, p.Next.Name, page.UUIDVal(p.Next.ID))
	}
	return page.Next(SortNewest, fp, page.UUIDVal(p.Next.ID))
}

func (h handler) searchOrgs(ctx context.Context, in *searchInput) (*searchOutput, error) {
	q := SearchQuery{Q: in.Q, Kind: Kind(in.Kind), City: in.City, Sort: sortOf(in.Sort), Limit: in.Limit}
	if in.UniversityID != "" {
		u, err := uuid.Parse(in.UniversityID)
		if err != nil {
			return nil, fieldErr("query.university_id", "must be a UUID")
		}
		q.UniversityID = &u
	}
	fp := page.Fingerprint("searchOrgs", in.Q, in.Kind, in.UniversityID, in.City)
	after, err := cursorOf(in.Cursor, q.Sort, fp)
	if err != nil {
		return nil, err
	}
	q.After = after
	p, err := h.svc.SearchOrgs(ctx, authz.ActorOrAnonymous(ctx), q)
	if err != nil {
		return nil, err
	}
	out := &searchOutput{Body: OrgCardList{Items: make([]OrgCardDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toCardDTO(v)
	}
	out.Body.NextCursor = nextCursor(p, fp)
	return out, nil
}

type adminListInput struct {
	page.Params
	Kind   string `query:"kind" enum:"platform,university,club,company,ngo"`
	Status string `query:"status" enum:"pending_review,active,suspended,closed"`
	Q      string `query:"q" maxLength:"100"`
}

type adminListOutput struct {
	Body AdminOrgList
}

func (h handler) adminList(ctx context.Context, in *adminListInput) (*adminListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListOrgs", in.Kind, in.Status, in.Q)
	after, err := cursorOf(in.Cursor, SortNewest, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListOrgs(ctx, a, AdminQuery{Kind: Kind(in.Kind), Status: Status(in.Status), Q: in.Q, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &adminListOutput{Body: AdminOrgList{Items: make([]AdminOrgDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toAdminDTO(v)
	}
	out.Body.NextCursor = nextCursor(p, fp)
	return out, nil
}

// ---- createCompany

type createCompanyInput struct {
	Body struct {
		Name        string `json:"name" minLength:"2" maxLength:"200"`
		Slug        string `json:"slug,omitempty" maxLength:"80" doc:"Derived from the name when omitted"`
		SectorCode  string `json:"sector_code" pattern:"^[a-z_]{2,40}$"`
		SizeBand    string `json:"size_band" enum:"1-10,11-50,51-200,201-1000,1000+"`
		WebsiteURL  string `json:"website_url,omitempty" maxLength:"2048" doc:"https:// only"`
		EmailDomain string `json:"email_domain,omitempty" maxLength:"253" doc:"The company's e-mail domain (not verified yet)"`
		HQCity      string `json:"hq_city,omitempty" maxLength:"60"`
	}
}

func (h handler) createCompany(ctx context.Context, in *createCompanyInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.CreateCompany(ctx, a, CompanyInput{
		Name: b.Name, Slug: b.Slug, SectorCode: b.SectorCode, SizeBand: b.SizeBand, WebsiteURL: b.WebsiteURL,
		EmailDomain: b.EmailDomain, HQCity: b.HQCity,
	})
	if err != nil {
		return nil, err
	}
	return orgOut(v), nil
}

// ---- updateOrg

type updateOrgInput struct {
	OrgPath
	IfMatch string `header:"If-Match" doc:"ETag of getOrg (required: missing → 428, stale → 412)"`
	Body    struct {
		Name        *string `json:"name,omitempty" minLength:"2" maxLength:"200"`
		Description *string `json:"description,omitempty" maxLength:"5000" doc:"Empty string clears it"`
		LogoFileID  *string `json:"logo_file_id,omitempty" doc:"An org_logo upload (createUpload); empty string removes the logo"`
		WebsiteURL  *string `json:"website_url,omitempty" maxLength:"2048" doc:"https:// only; empty string clears it"`
		City        *string `json:"city,omitempty" maxLength:"60"`
		SectorCode  *string `json:"sector_code,omitempty" maxLength:"40" doc:"Companies only"`
		SizeBand    *string `json:"size_band,omitempty" maxLength:"10" doc:"Companies only: 1-10, 11-50, 51-200, 201-1000, 1000+"`
		EmailDomain *string `json:"email_domain,omitempty" maxLength:"253" doc:"Companies only; a change resets its verification"`
		HQCity      *string `json:"hq_city,omitempty" maxLength:"60" doc:"Companies only"`
	}
}

func (h handler) updateOrg(ctx context.Context, in *updateOrgInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id := in.id()
	b := in.Body
	p := OrgPatch{
		Name: b.Name, Description: b.Description, WebsiteURL: b.WebsiteURL, City: b.City,
		SectorCode: b.SectorCode, SizeBand: b.SizeBand, EmailDomain: b.EmailDomain, HQCity: b.HQCity,
	}
	if b.LogoFileID != nil {
		if *b.LogoFileID == "" {
			p.ClearLogo = true
		} else {
			f, err := uuid.Parse(*b.LogoFileID)
			if err != nil {
				return nil, fieldErr("body.logo_file_id", "must be a UUID")
			}
			p.Logo = &f
		}
	}
	v, err := h.svc.UpdateOrg(ctx, a, id, in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return orgOut(v), nil
}

// ---- closeOrg / transferOrgOwnership

type reasonReauthInput struct {
	OrgPath
	Body struct {
		Reason string             `json:"reason" minLength:"3" maxLength:"500"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) closeOrg(ctx context.Context, in *reasonReauthInput) (*orgOutput, error) {
	return h.reasonReauth(ctx, in, h.svc.CloseOrg)
}

func (h handler) adminSuspend(ctx context.Context, in *reasonReauthInput) (*orgOutput, error) {
	return h.reasonReauth(ctx, in, h.svc.AdminSuspendOrg)
}

func (h handler) adminReactivate(ctx context.Context, in *reasonReauthInput) (*orgOutput, error) {
	return h.reasonReauth(ctx, in, h.svc.AdminReactivateOrg)
}

func (h handler) reasonReauth(ctx context.Context, in *reasonReauthInput,
	fn func(context.Context, authz.Actor, uuid.UUID, string, identity.ReauthProof) (OrgView, error)) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id := in.id()
	v, err := fn(ctx, a, id, in.Body.Reason, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return orgOut(v), nil
}

type transferInput struct {
	OrgPath
	Body struct {
		NewOwnerUserID string             `json:"new_owner_user_id" format:"uuid" doc:"An active company_admin of the company"`
		Reauth         *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) transfer(ctx context.Context, in *transferInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id := in.id()
	owner, err := uuid.Parse(in.Body.NewOwnerUserID)
	if err != nil {
		return nil, fieldErr("body.new_owner_user_id", "must be a UUID")
	}
	v, err := h.svc.TransferOwnership(ctx, a, id, owner, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return orgOut(v), nil
}

// ---- adminVerifyOrg

type verifyInput struct {
	OrgPath
	Body *struct {
		Note string `json:"note,omitempty" maxLength:"500" doc:"Internal note (audited)"`
	}
}

func (h handler) adminVerify(ctx context.Context, in *verifyInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id := in.id()
	var note string
	if in.Body != nil {
		note = in.Body.Note
	}
	v, err := h.svc.AdminVerifyOrg(ctx, a, id, note)
	if err != nil {
		return nil, err
	}
	return orgOut(v), nil
}
