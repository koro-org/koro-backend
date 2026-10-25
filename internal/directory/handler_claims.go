package directory

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
)

// registerClaims adds the six club claim operations of BE-022 (04 §2.3).
func registerClaims(api huma.API, h handler) {
	op.Register(api, "directory", huma.Operation{
		OperationID: "createClubClaim", Method: http.MethodPost, Path: "/clubs/{club_id}/claims",
		DefaultStatus: http.StatusCreated,
		Summary:       "Claim a club",
		Description: "An adult student whose verified university is the club's asks to become its club admin " +
			"(403 students.verification_required, 403 directory.minor_restricted, 422 directory.university_mismatch). " +
			"Evidence: evidence_text and/or evidence_file_id (a club_claim_evidence upload). One pending claim per " +
			"club (409 directory.claim_pending). A club may have several admins, so a claimed club takes claims too.",
	}, op.Meta{Action: string(authz.DirectoryClubClaimCreate), Surface: op.SurfaceResource,
		Idempotency: op.IdempotencyOptional}, h.createClaim)

	op.Register(api, "directory", huma.Operation{
		OperationID: "listMyClubClaims", Method: http.MethodGet, Path: "/me/club-claims",
		Summary: "List my club claims", Description: "Newest first.",
	}, op.Meta{Action: string(authz.DirectoryClubClaimReadOwn), Surface: op.SurfaceSelf}, h.listMyClaims)

	op.Register(api, "directory", huma.Operation{
		OperationID: "withdrawMyClubClaim", Method: http.MethodPost, Path: "/me/club-claims/{claim_id}/withdraw",
		Summary:     "Withdraw my club claim",
		Description: "Only a pending claim (409 directory.claim_state).",
	}, op.Meta{Action: string(authz.DirectoryClubClaimWithdraw), Surface: op.SurfaceSelf,
		Idempotency: op.IdempotencyOptional}, h.withdrawClaim)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminListClubClaims", Method: http.MethodGet, Path: "/admin/club-claims",
		Summary: "List club claims",
		Description: "The review queue: pending claims oldest first (default), other statuses newest first. Support and " +
			"platform admins see every claim; a university representative the claims of their universities' " +
			"non-sensitive clubs (the claims they decide). Audited.",
	}, op.Meta{Action: string(authz.DirectoryClubClaimList), Surface: op.SurfaceAdmin}, h.adminListClaims)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminGetClubClaim", Method: http.MethodGet, Path: "/admin/club-claims/{claim_id}",
		Summary: "Get a club claim",
		Description: "With a 5-minute download of the evidence file once the virus scan cleared it, and the claim's " +
			"approver. A university representative reads only the claims they decide (others 404). Audited.",
	}, op.Meta{Action: string(authz.DirectoryClubClaimList), Surface: op.SurfaceAdmin}, h.adminGetClaim)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminDecideClubClaim", Method: http.MethodPost, Path: "/admin/club-claims/{claim_id}/decision",
		Summary: "Decide a club claim",
		Description: "The approver decides: the representative of the club's university when the university has " +
			"one and the club is not sensitive, a platform admin otherwise (403 for a platform admin where a " +
			"representative decides). Approval makes the claimant a club admin and marks the club claimed, in one " +
			"transaction (422 orgs.club_admin_requires_adult_student rolls it back). Nobody decides their own " +
			"claim (403). The decision note is never shown to the claimant.",
	}, op.Meta{Action: string(authz.DirectoryClubClaimDecide), Surface: op.SurfaceAdmin,
		Idempotency: op.IdempotencyOptional}, h.adminDecideClaim)
}

// Sort keys of the claim cursors.
const (
	claimSortNewest = "id_desc"
	claimSortOldest = "id_asc"
)

func claimCursor(raw, sort, fp string) (*uuid.UUID, error) {
	c, err := page.Decode(raw, sort, fp, 1)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(0)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func claimNext(next *uuid.UUID, sort, fp string) string {
	if next == nil {
		return ""
	}
	return page.Next(sort, fp, page.UUIDVal(*next))
}

// ClaimPath is the claim_id path parameter.
type ClaimPath struct {
	ClaimID string `path:"claim_id" doc:"Club claim ID"`
}

type createClaimInput struct {
	ClubPath
	Body struct {
		RoleInClub     string `json:"role_in_club" minLength:"2" maxLength:"100" doc:"The claimant's role in the club, e.g. Başkan"`
		EvidenceText   string `json:"evidence_text,omitempty" maxLength:"2000" doc:"Evidence in words; required without evidence_file_id"`
		EvidenceFileID string `json:"evidence_file_id,omitempty" format:"uuid" doc:"A completed club_claim_evidence upload of the caller"`
	}
}

type claimOutput struct{ Body ClubClaimDTO }

func (h handler) createClaim(ctx context.Context, in *createClaimInput) (*claimOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	file, err := httpx.OptID("body.evidence_file_id", in.Body.EvidenceFileID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CreateClubClaim(ctx, a, httpx.ParseID(in.ClubID), ClaimInput{
		RoleInClub: in.Body.RoleInClub, EvidenceText: in.Body.EvidenceText, EvidenceFileID: file,
	})
	if err != nil {
		return nil, err
	}
	return &claimOutput{Body: toClubClaimDTO(v)}, nil
}

type listMyClaimsInput struct {
	page.Params
}

type claimListOutput struct{ Body ClubClaimList }

func (h handler) listMyClaims(ctx context.Context, in *listMyClaimsInput) (*claimListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyClubClaims")
	after, err := claimCursor(in.Cursor, claimSortNewest, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyClubClaims(ctx, a, after, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &claimListOutput{Body: ClubClaimList{Items: make([]ClubClaimDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toClubClaimDTO(v)
	}
	out.Body.NextCursor = claimNext(p.Next, claimSortNewest, fp)
	return out, nil
}

func (h handler) withdrawClaim(ctx context.Context, in *ClaimPath) (*claimOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.WithdrawMyClubClaim(ctx, a, httpx.ParseID(in.ClaimID))
	if err != nil {
		return nil, err
	}
	return &claimOutput{Body: toClubClaimDTO(v)}, nil
}

type adminListClaimsInput struct {
	page.Params
	Status string `query:"status" enum:"pending,approved,rejected,withdrawn" doc:"Default pending"`
}

type adminClaimListOutput struct{ Body AdminClubClaimList }

func (h handler) adminListClaims(ctx context.Context, in *adminListClaimsInput) (*adminClaimListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	status := ClaimState(in.Status)
	if status == "" {
		status = ClaimPending
	}
	sort := claimSortNewest
	if status == ClaimPending {
		sort = claimSortOldest
	}
	fp := page.Fingerprint("adminListClubClaims", string(status))
	after, err := claimCursor(in.Cursor, sort, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListClubClaims(ctx, a, AdminClaimQuery{Status: status, After: after, Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	out := &adminClaimListOutput{Body: AdminClubClaimList{Items: make([]AdminClubClaimDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toAdminClaimDTO(v)
	}
	out.Body.NextCursor = claimNext(p.Next, sort, fp)
	return out, nil
}

type adminClaimOutput struct{ Body AdminClubClaimDTO }

func (h handler) adminGetClaim(ctx context.Context, in *ClaimPath) (*adminClaimOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminGetClubClaim(ctx, a, httpx.ParseID(in.ClaimID))
	if err != nil {
		return nil, err
	}
	return &adminClaimOutput{Body: toAdminClaimDTO(v)}, nil
}

type adminDecideClaimInput struct {
	ClaimPath
	Body struct {
		Decision     string `json:"decision" enum:"approved,rejected"`
		DecisionNote string `json:"decision_note,omitempty" maxLength:"1000" doc:"Shown to the reviewers only, never to the claimant"`
	}
}

func (h handler) adminDecideClaim(ctx context.Context, in *adminDecideClaimInput) (*adminClaimOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminDecideClubClaim(ctx, a, httpx.ParseID(in.ClaimID),
		ClaimDecision{Decision: ClaimState(in.Body.Decision), Note: in.Body.DecisionNote})
	if err != nil {
		return nil, err
	}
	return &adminClaimOutput{Body: toAdminClaimDTO(v)}, nil
}
