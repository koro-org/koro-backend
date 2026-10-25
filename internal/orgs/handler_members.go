package orgs

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

// registerMembers adds the 13 membership operations of BE-019 (04 §2.2,
// §2.3 joinClub).
func registerMembers(api huma.API, h handler) {
	op.Register(api, "orgs", huma.Operation{
		OperationID: "listOrgMembers", Method: http.MethodGet, Path: "/orgs/{org_id}/members",
		Summary: "List an organization's members",
		Description: "The org's admins (club_admin, university_rep, company roles) see the list; members do not see each " +
			"other. support and platform_admin need X-Koro-Access-Reason (break-glass, audited). A sensitive club's list " +
			"is its club_admin's only, every read is audited, and platform staff get 404.",
	}, op.Meta{Action: string(authz.OrgsMemberList), Surface: op.SurfaceOrg, BreakGlass: true}, h.listMembers)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "getOrgMember", Method: http.MethodGet, Path: "/orgs/{org_id}/members/{membership_id}",
		Summary:     "Get one membership of an organization",
		Description: "Same rules as listOrgMembers. Ended memberships are not found.",
	}, op.Meta{Action: string(authz.OrgsMemberRead), Surface: op.SurfaceOrg, BreakGlass: true}, h.getMember)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "approveJoinRequest", Method: http.MethodPost, Path: "/orgs/{org_id}/members/{membership_id}/approve",
		Summary:     "Approve a join request",
		Description: "club_admin only. pending → active; another status is 409 orgs.member_state.",
	}, op.Meta{Action: string(authz.OrgsMemberDecide), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.approve)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "rejectJoinRequest", Method: http.MethodPost, Path: "/orgs/{org_id}/members/{membership_id}/reject",
		Summary:     "Reject a join request",
		Description: "club_admin only. No reason is shown to the student; in a sensitive club the request is deleted.",
	}, op.Meta{Action: string(authz.OrgsMemberDecide), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.reject)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "removeOrgMember", Method: http.MethodPost, Path: "/orgs/{org_id}/members/{membership_id}/remove",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Remove a member",
		Description:   "The last club_admin and the company_owner cannot be removed (409 orgs.last_admin).",
	}, op.Meta{Action: string(authz.OrgsMemberRemove), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.remove)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "changeOrgMemberRole", Method: http.MethodPost, Path: "/orgs/{org_id}/members/{membership_id}/role",
		Summary: "Change a member's role",
		Description: "The old membership ends (role_changed) and a new one starts: the answer carries the new " +
			"membership_id. club_admin goes to verified adult students only (422 " +
			"orgs.club_admin_requires_adult_student); company_owner moves by ownership transfer only; the last " +
			"club_admin cannot be demoted (409 orgs.last_admin).",
	}, op.Meta{Action: string(authz.OrgsMemberChangeRole), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.changeRole)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "listMyMemberships", Method: http.MethodGet, Path: "/me/memberships",
		Summary:     "List my memberships",
		Description: "Sensitive club memberships included (only for their owner). Default: pending and active.",
	}, op.Meta{Action: string(authz.OrgsMembershipListOwn), Surface: op.SurfaceSelf}, h.listMine)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "leaveMyMembership", Method: http.MethodPost, Path: "/me/memberships/{membership_id}/leave",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Leave a membership (or withdraw a join request)",
		Description:   "The last club_admin and the company_owner cannot leave (409 orgs.last_admin).",
	}, op.Meta{Action: string(authz.OrgsMembershipLeave), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}, h.leave)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "createOrgInvitation", Method: http.MethodPost, Path: "/orgs/{org_id}/invitations",
		DefaultStatus: http.StatusCreated,
		Summary:       "Invite someone to an organization by e-mail",
		Description: "The invitation is valid for 7 days and can be accepted once, by the account with the invited " +
			"address only. company_owner is never invited; university roles are invited by platform_admin.",
	}, op.Meta{Action: string(authz.OrgsInvitationCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.invite)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "listOrgInvitations", Method: http.MethodGet, Path: "/orgs/{org_id}/invitations",
		Summary: "List an organization's open invitations",
	}, op.Meta{Action: string(authz.OrgsInvitationList), Surface: op.SurfaceOrg}, h.listInvitations)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "revokeOrgInvitation", Method: http.MethodDelete, Path: "/orgs/{org_id}/invitations/{invitation_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Revoke an open invitation",
	}, op.Meta{Action: string(authz.OrgsInvitationRevoke), Surface: op.SurfaceOrg}, h.revokeInvitation)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "acceptOrgInvitation", Method: http.MethodPost, Path: "/invitations/accept",
		Summary: "Accept an invitation",
		Description: "The token comes from the invitation e-mail. The invitation must be addressed to the signed-in " +
			"account's e-mail (otherwise 404); an expired one is 409 orgs.invitation_expired.",
	}, op.Meta{Action: string(authz.OrgsInvitationAccept), Surface: op.SurfaceSelf, RateLimit: op.RateTokenConfirm,
		Idempotency: op.IdempotencyOptional}, h.accept)

	op.Register(api, "orgs", huma.Operation{
		OperationID: "joinClub", Method: http.MethodPost, Path: "/clubs/{club_id}/join-requests",
		DefaultStatus: http.StatusCreated,
		Summary:       "Ask to join a club",
		Description: "Verified students only (403 students.verification_required). An open club makes the membership " +
			"active at once, an approval club leaves it pending for the club_admin.",
	}, op.Meta{Action: string(authz.OrgsMemberJoin), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.join)
}

// MemberPath is the org_id and membership_id path parameters.
type MemberPath struct {
	OrgPath
	MembershipID string `path:"membership_id" doc:"Membership ID"`
}

// AccessHeader is the break-glass header of listOrgMembers and
// getOrgMember (03 §6.3).
type AccessHeader struct {
	AccessReason string `header:"X-Koro-Access-Reason" maxLength:"500" doc:"Break-glass justification (support, platform_admin: 10–500 characters)"`
}

type listMembersInput struct {
	OrgPath
	AccessHeader
	page.Params
	Status string `query:"status" enum:"pending,active" doc:"Default: both"`
	Role   string `query:"role" enum:"member,club_admin,university_rep,career_staff,company_owner,company_admin,recruiter"`
	Sort   string `query:"sort" enum:"joined_at" doc:"joined_at (oldest first); default newest first"`
}

type memberListOutput struct {
	Body MemberList
}

func memberSort(s string) string {
	if s == "joined_at" {
		return SortMemberJoined
	}
	return SortMemberNewest
}

func (h handler) listMembers(ctx context.Context, in *listMembersInput) (*memberListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	sort := memberSort(in.Sort)
	fp := page.Fingerprint("listOrgMembers", in.OrgID, in.Status, in.Role)
	n := 1
	if sort == SortMemberJoined {
		n = 2
	}
	q := MemberQuery{Role: authz.MemberRole(in.Role), Sort: sort, Limit: in.Limit}
	if in.Status != "" {
		q.Statuses = []MemberStatus{MemberStatus(in.Status)}
	}
	c, err := page.Decode(in.Cursor, sort, fp, n)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(n - 1)
		if err != nil {
			return nil, err
		}
		q.After = &MemberAfter{ID: id}
		if sort == SortMemberJoined {
			if q.After.Since, err = c.Time(0); err != nil {
				return nil, err
			}
		}
	}
	p, err := h.svc.ListMembers(ctx, a, in.id(), q, Access{Reason: in.AccessReason})
	if err != nil {
		return nil, err
	}
	out := &memberListOutput{Body: MemberList{Items: make([]MemberDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toMemberDTO(v)
	}
	if p.Next != nil {
		if p.Sort == SortMemberJoined {
			out.Body.NextCursor = page.Next(sort, fp, page.TimeVal(p.Next.Since), page.UUIDVal(p.Next.ID))
		} else {
			out.Body.NextCursor = page.Next(sort, fp, page.UUIDVal(p.Next.ID))
		}
	}
	return out, nil
}

type getMemberInput struct {
	MemberPath
	AccessHeader
}

type memberOutput struct {
	Body MemberDTO
}

func (h handler) getMember(ctx context.Context, in *getMemberInput) (*memberOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetMember(ctx, a, in.id(), httpx.ParseID(in.MembershipID), Access{Reason: in.AccessReason})
	if err != nil {
		return nil, err
	}
	return &memberOutput{Body: toMemberDTO(v)}, nil
}

func (h handler) decide(ctx context.Context, in *MemberPath,
	fn func(context.Context, authz.Actor, uuid.UUID, uuid.UUID) (MemberView, error)) (*memberOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := fn(ctx, a, in.id(), httpx.ParseID(in.MembershipID))
	if err != nil {
		return nil, err
	}
	return &memberOutput{Body: toMemberDTO(v)}, nil
}

func (h handler) approve(ctx context.Context, in *MemberPath) (*memberOutput, error) {
	return h.decide(ctx, in, h.svc.ApproveJoinRequest)
}

func (h handler) reject(ctx context.Context, in *MemberPath) (*memberOutput, error) {
	return h.decide(ctx, in, h.svc.RejectJoinRequest)
}

type removeInput struct {
	MemberPath
	Body *struct {
		Reason string `json:"reason,omitempty" maxLength:"500" doc:"Internal note (audited)"`
	}
}

func (h handler) remove(ctx context.Context, in *removeInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var reason string
	if in.Body != nil {
		reason = in.Body.Reason
	}
	return nil, h.svc.RemoveMember(ctx, a, in.id(), httpx.ParseID(in.MembershipID), reason)
}

type roleInput struct {
	MemberPath
	Body struct {
		Role string `json:"role" enum:"member,club_admin,university_rep,career_staff,company_admin,recruiter"`
	}
}

func (h handler) changeRole(ctx context.Context, in *roleInput) (*memberOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ChangeMemberRole(ctx, a, in.id(), httpx.ParseID(in.MembershipID), authz.MemberRole(in.Body.Role))
	if err != nil {
		return nil, err
	}
	return &memberOutput{Body: toMemberDTO(v)}, nil
}

type listMineInput struct {
	page.Params
	Status string `query:"status" enum:"pending,active,ended" doc:"Default: pending and active"`
}

type myListOutput struct {
	Body MyMembershipList
}

func (h handler) listMine(ctx context.Context, in *listMineInput) (*myListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyMemberships", in.Status)
	q := MyMembershipQuery{Limit: in.Limit}
	if in.Status != "" {
		q.Statuses = []MemberStatus{MemberStatus(in.Status)}
	}
	if q.After, err = afterID(in.Cursor, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyMemberships(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &myListOutput{Body: MyMembershipList{Items: make([]MyMembershipDTO, len(p.Items))}}
	for i, x := range p.Items {
		m := x.Membership
		out.Body.Items[i] = MyMembershipDTO{
			MembershipID: m.ID.String(), Role: string(m.Role), Status: string(m.Status), JoinedAt: utc(m.JoinedAt),
			Org: MembershipOrgDTO{OrgID: m.OrgID.String(), Kind: string(m.OrgKind), Name: x.Org.Name, LogoURL: x.LogoURL},
		}
	}
	out.Body.NextCursor = nextID(p.Next, fp)
	return out, nil
}

// afterID decodes an id-keyed cursor (newest first).
func afterID(raw, fp string) (*uuid.UUID, error) {
	c, err := page.Decode(raw, SortNewest, fp, 1)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(0)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func nextID(next *uuid.UUID, fp string) string {
	if next == nil {
		return ""
	}
	return page.Next(SortNewest, fp, page.UUIDVal(*next))
}

type leaveInput struct {
	MembershipID string `path:"membership_id" doc:"Membership ID"`
}

func (h handler) leave(ctx context.Context, in *leaveInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.LeaveMembership(ctx, a, httpx.ParseID(in.MembershipID))
}

type inviteInput struct {
	OrgPath
	Body struct {
		Email string `json:"email" maxLength:"254" doc:"Address of the invited person; only the account with this address can accept"`
		Role  string `json:"role" enum:"member,club_admin,university_rep,career_staff,company_admin,recruiter"`
	}
}

type invitationOutput struct {
	Body InvitationDTO
}

func (h handler) invite(ctx context.Context, in *inviteInput) (*invitationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := h.svc.CreateInvitation(ctx, a, in.id(), in.Body.Email, authz.MemberRole(in.Body.Role))
	if err != nil {
		return nil, err
	}
	return &invitationOutput{Body: toInvitationDTO(inv)}, nil
}

type listInvitationsInput struct {
	OrgPath
	page.Params
}

type invitationListOutput struct {
	Body InvitationList
}

func (h handler) listInvitations(ctx context.Context, in *listInvitationsInput) (*invitationListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgInvitations", in.OrgID)
	after, err := afterID(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListInvitations(ctx, a, in.id(), InvitationQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &invitationListOutput{Body: InvitationList{Items: make([]InvitationDTO, len(p.Items))}}
	for i, inv := range p.Items {
		out.Body.Items[i] = toInvitationDTO(inv)
	}
	out.Body.NextCursor = nextID(p.Next, fp)
	return out, nil
}

type revokeInvitationInput struct {
	OrgPath
	InvitationID string `path:"invitation_id" doc:"Invitation ID"`
}

func (h handler) revokeInvitation(ctx context.Context, in *revokeInvitationInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.RevokeInvitation(ctx, a, in.id(), httpx.ParseID(in.InvitationID))
}

type acceptInput struct {
	Body struct {
		Token string `json:"token" minLength:"1" maxLength:"128" doc:"Token from the invitation e-mail"`
	}
}

type acceptOutput struct {
	Body AcceptedInvitationDTO
}

func (h handler) accept(ctx context.Context, in *acceptInput) (*acceptOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.AcceptInvitation(ctx, a, in.Body.Token)
	if err != nil {
		return nil, err
	}
	return &acceptOutput{Body: AcceptedInvitationDTO{MembershipID: m.ID.String(), OrgID: m.OrgID.String(), Role: string(m.Role)}}, nil
}

type joinInput struct {
	ClubID string `path:"club_id" doc:"The club's org ID"`
	Body   *struct {
		Message string `json:"message,omitempty" maxLength:"300" doc:"Optional note (not stored)"`
	}
}

type joinOutput struct {
	Body JoinRequestDTO
}

func (h handler) join(ctx context.Context, in *joinInput) (*joinOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var msg string
	if in.Body != nil {
		msg = in.Body.Message
	}
	m, err := h.svc.JoinClub(ctx, a, httpx.ParseID(in.ClubID), msg)
	if err != nil {
		return nil, err
	}
	return &joinOutput{Body: JoinRequestDTO{MembershipID: m.ID.String(), Status: string(m.Status)}}, nil
}
