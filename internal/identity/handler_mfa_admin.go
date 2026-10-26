package identity

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// registerMFAAndAdmin adds the BE-016 operations (04 §2.1, plus the
// four-eyes approval of 06 H-17 / T-79).
func registerMFAAndAdmin(api huma.API, h handler) {
	op.Register(api, "identity", huma.Operation{
		OperationID: "completeMfaLogin", Method: http.MethodPost, Path: "/auth/sessions/mfa",
		Summary: "Complete an MFA sign-in",
		Description: "Second step of an MFA account's login: the challenge of createSession (valid 5 minutes, single " +
			"use) with a TOTP code or a recovery code (exactly one). The session opens MFA-verified; web clients get " +
			"the cookie, mobile clients the token (the client kind of the password step). Any failure is 401 " +
			"identity.mfa_login_failed. A recovery code sign-in sends a security notice.",
	}, op.Meta{Action: string(authz.IdentitySessionCreate), Surface: op.SurfacePublic, RateLimit: op.RateAuthLogin,
		Idempotency: op.IdempotencyOptional}, h.completeMFALogin)

	op.Register(api, "identity", huma.Operation{
		OperationID: "enrollTotp", Method: http.MethodPost, Path: "/me/mfa/totp",
		Summary: "Start TOTP enrollment",
		Description: "reauth.password is the proof (06 H-46). Returns a new TOTP secret (only in this response; stored " +
			"encrypted). An earlier unconfirmed enrollment is replaced. 409 identity.mfa_already_enabled when TOTP is on.",
	}, op.Meta{Action: string(authz.IdentityMfaEnroll), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.enrollTOTP)

	op.Register(api, "identity", huma.Operation{
		OperationID: "confirmTotp", Method: http.MethodPost, Path: "/me/mfa/totp/confirm",
		Summary: "Confirm TOTP enrollment",
		Description: "reauth.password is the proof (06 H-46). The first code from the authenticator turns MFA on and " +
			"returns ten single-use recovery codes (shown once). The current session becomes MFA-verified. A wrong code " +
			"is 422 identity.invalid_mfa_code.",
	}, op.Meta{Action: string(authz.IdentityMfaConfirm), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.confirmTOTP)

	op.Register(api, "identity", huma.Operation{
		OperationID: "verifyMfa", Method: http.MethodPost, Path: "/me/mfa/verify", DefaultStatus: http.StatusNoContent,
		Summary: "Make the current session MFA-verified",
		Description: "Step-up after a role that requires MFA arrived (403 authz.mfa_required). A code is accepted " +
			"once; recovery codes are not accepted here.",
	}, op.Meta{Action: string(authz.IdentityMfaVerify), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}, h.verifyMFA)

	op.Register(api, "identity", huma.Operation{
		OperationID: "disableTotp", Method: http.MethodPost, Path: "/me/mfa/totp/disable", DefaultStatus: http.StatusNoContent,
		Summary: "Turn TOTP off",
		Description: "reauth.totp_code is the proof. Users holding a role that requires MFA (platform roles, " +
			"university_rep, company_admin/owner) get 409 identity.mfa_required_by_role.",
	}, op.Meta{Action: string(authz.IdentityMfaDisable), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.disableTOTP)

	op.Register(api, "identity", huma.Operation{
		OperationID: "regenerateRecoveryCodes", Method: http.MethodPost, Path: "/me/mfa/recovery-codes",
		Summary: "Replace my recovery codes", Description: "reauth.totp_code is the proof; the old codes stop working.",
	}, op.Meta{Action: string(authz.IdentityMfaRecoveryRegenerate), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.regenerateRecoveryCodes)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminQueryUsers", Method: http.MethodPost, Path: "/admin/users/search",
		Summary: "Search users",
		Description: "Read-only: the criteria are in the body so that an e-mail address or a name never reaches a URL " +
			"or an access log (BE-128). q is an exact e-mail address or a fragment of the name (2-100 characters); " +
			"without q every account, newest first. Every search is audited (query kind and the users found, not the " +
			"query). next_cursor goes back in the body.",
	}, op.Meta{Action: string(authz.IdentityUserSearch), Surface: op.SurfaceAdmin, RateLimit: op.RateSearch,
		Idempotency: op.IdempotencyOptional}, h.queryUsers)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminGetUser", Method: http.MethodGet, Path: "/admin/users/{user_id}",
		Summary: "Account summary for platform staff",
		Description: "Status, roles, student verification and non-club memberships; no profile content and never a " +
			"club membership (a sensitive one must not show). pending_actions lists the open four-eyes requests " +
			"against the account (06 H-47: action, requester, reason, expiry), so the second platform_admin sees them.",
	}, op.Meta{Action: string(authz.IdentityUserRead), Surface: op.SurfaceAdmin}, h.getUser)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminSuspendUser", Method: http.MethodPost, Path: "/admin/users/{user_id}/suspend",
		Summary: "Suspend an account",
		Description: "Every session closes (revoke_reason suspended). " + fourEyesAccountDoc + " The last active " +
			"holder of a privileged role cannot be suspended (409 identity.last_privileged_holder).",
	}, op.Meta{Action: string(authz.IdentityUserSuspend), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Responses: []op.Response{{Status: http.StatusAccepted, Body: AdminUserDTO{},
			Description: "A four-eyes request is open (pending_action); the account is unchanged until a second platform_admin repeats the call"}}},
		h.suspendUser)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminUnsuspendUser", Method: http.MethodPost, Path: "/admin/users/{user_id}/unsuspend",
		Summary: "Lift a suspension",
	}, op.Meta{Action: string(authz.IdentityUserSuspend), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.unsuspendUser)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminRevokeUserSessions", Method: http.MethodPost, Path: "/admin/users/{user_id}/sessions/revoke",
		DefaultStatus: http.StatusNoContent, Summary: "Close every session of a user",
		Description: "204 when done. " + fourEyesAccountDoc + " Only a platform_admin can be the second eye.",
	}, op.Meta{Action: string(authz.IdentityUserRevokeSessions), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Responses: fourEyesAccepted}, h.revokeUserSessions)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminResetUserMfa", Method: http.MethodPost, Path: "/admin/users/{user_id}/mfa/reset",
		DefaultStatus: http.StatusNoContent, Summary: "Reset a user's TOTP",
		Description: "Deletes the TOTP secret and recovery codes, closes every session and notifies the user (204). Not " +
			"for the own account (403). " + fourEyesAccountDoc,
	}, op.Meta{Action: string(authz.IdentityUserResetMfa), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional, Responses: fourEyesAccepted}, h.resetUserMFA)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminListPlatformRoles", Method: http.MethodGet, Path: "/admin/platform-roles",
		Summary: "List platform role assignments", Description: "Open assignments (pending, active, revoke_pending) unless include_revoked.",
	}, op.Meta{Action: string(authz.IdentityPlatformRoleList), Surface: op.SurfaceAdmin}, h.listPlatformRoles)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminGrantPlatformRole", Method: http.MethodPost, Path: "/admin/platform-roles", DefaultStatus: http.StatusCreated,
		Summary: "Grant a platform role",
		Description: "platform_admin, finance_admin and talent_auditor start as a pending request that a second " +
			"platform_admin approves (four eyes); moderator, support and dpo take effect at once and close the user's " +
			"sessions. Own account: 403. At most two talent auditors (409).",
	}, op.Meta{Action: string(authz.IdentityPlatformRoleGrant), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.grantPlatformRole)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminApprovePlatformRole", Method: http.MethodPost, Path: "/admin/platform-roles/{assignment_id}/approve",
		Summary: "Approve a pending platform role request",
		Description: "The second eye (06 H-17): a platform_admin other than the requester and the user. The " +
			"requester gets 403 authz.four_eyes_violation. The role takes effect and the user's sessions close.",
	}, op.Meta{Action: string(authz.IdentityPlatformRoleGrant), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.approvePlatformRole)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminRevokePlatformRole", Method: http.MethodPost, Path: "/admin/platform-roles/{assignment_id}/revoke",
		Summary: "Revoke a platform role",
		Description: "moderator, support, dpo: revoked at once. platform_admin, finance_admin, talent_auditor: the " +
			"first call requests the revocation (revoke_pending; the role stays in effect), a second platform_admin's " +
			"call confirms it (the requester gets 403 authz.four_eyes_violation). A pending request is rejected. The " +
			"last active platform_admin cannot be revoked (409).",
	}, op.Meta{Action: string(authz.IdentityPlatformRoleRevoke), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.revokePlatformRole)
}

// fourEyesAccepted is the 202 of the admin account operations that answer
// 204 when done (06 H-47).
var fourEyesAccepted = []op.Response{{Status: http.StatusAccepted, Body: AccountActionDTO{},
	Description: "A four-eyes request is open; nothing changed until a second platform_admin repeats the call"}}

// fourEyesAccountDoc describes 06 H-47 in the admin account operations.
const fourEyesAccountDoc = "Against a holder of platform_admin, finance_admin or talent_auditor the first call only " +
	"opens a four-eyes request (202 with pending_action; valid 24 hours); the same call by a second platform_admin " +
	"approves and performs it (the requester gets 403 authz.four_eyes_violation)."

// ---- completeMfaLogin

type mfaLoginInput struct {
	Body struct {
		ChallengeID  string `json:"challenge_id" format:"uuid"`
		TOTPCode     string `json:"totp_code,omitempty" maxLength:"16"`
		RecoveryCode string `json:"recovery_code,omitempty" maxLength:"32"`
	}
}

func (h handler) completeMFALogin(ctx context.Context, in *mfaLoginInput) (*loginOutput, error) {
	id, err := uuid.Parse(in.Body.ChallengeID)
	if err != nil {
		return nil, problem.Field("body.challenge_id", "must be a UUID")
	}
	res, err := h.svc.CompleteMFALogin(ctx, authz.ActorOrAnonymous(ctx), MFALogin{
		ChallengeID: id, TOTPCode: in.Body.TOTPCode, RecoveryCode: in.Body.RecoveryCode,
	})
	if err != nil {
		return nil, err
	}
	return sessionOutput(res), nil
}

// sessionOutput is the createSession / completeMfaLogin answer of an
// opened session: cookie for web, token in the body for mobile.
func sessionOutput(res LoginResult) *loginOutput {
	out := &loginOutput{}
	s := res.Session
	abs, idle := s.AbsoluteExpiresAt, s.IdleExpiresAt
	out.Body = SessionDTO{UserID: s.UserID.String(), ExpiresAt: &abs, IdleExpiresAt: &idle}
	if s.ClientKind == ClientMobile {
		out.Body.Token = res.Token
	} else {
		out.SetCookie = sessionCookie(res.Token, abs, int(abs.Sub(s.CreatedAt).Seconds()))
	}
	return out
}

// ---- MFA of the caller

type enrollOutput struct {
	Body TOTPEnrollmentDTO
}

func (h handler) enrollTOTP(ctx context.Context, in *reauthOnlyInput) (*enrollOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	e, err := h.svc.EnrollTOTP(ctx, a, reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &enrollOutput{Body: TOTPEnrollmentDTO{OTPAuthURI: e.URI, SecretBase32: e.Secret}}, nil
}

type codeInput struct {
	Body struct {
		Code string `json:"code" minLength:"1" maxLength:"16" doc:"The current 6-digit TOTP code"`
	}
}

type confirmInput struct {
	Body struct {
		Code   string             `json:"code" minLength:"1" maxLength:"16" doc:"The current 6-digit TOTP code"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (06 H-46): reauth.password"`
	}
}

type recoveryCodesOutput struct {
	Body RecoveryCodesDTO
}

func (h handler) confirmTOTP(ctx context.Context, in *confirmInput) (*recoveryCodesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := h.svc.ConfirmTOTP(ctx, a, in.Body.Code, reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &recoveryCodesOutput{Body: RecoveryCodesDTO{RecoveryCodes: codes}}, nil
}

func (h handler) verifyMFA(ctx context.Context, in *codeInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.VerifyMFA(ctx, a, in.Body.Code)
}

type reauthOnlyInput struct {
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2): totp_code on an MFA account"`
	}
}

func (h handler) disableTOTP(ctx context.Context, in *reauthOnlyInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.DisableTOTP(ctx, a, reauthProof(in.Body.Reauth))
}

func (h handler) regenerateRecoveryCodes(ctx context.Context, in *reauthOnlyInput) (*recoveryCodesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := h.svc.RegenerateRecoveryCodes(ctx, a, reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &recoveryCodesOutput{Body: RecoveryCodesDTO{RecoveryCodes: codes}}, nil
}

// ---- admin users

type queryUsersInput struct {
	Body struct {
		Q      string `json:"q,omitempty" maxLength:"254" doc:"An exact e-mail address or a fragment of the name (2-100 characters)"`
		Status string `json:"status,omitempty" enum:"pending_email,active,suspended,erased"`
		page.BodyParams
	}
}

type searchUsersOutput struct {
	Body AdminUserList
}

const userSort = "id_desc"

func (h handler) queryUsers(ctx context.Context, in *queryUsersInput) (*searchUsersOutput, error) {
	b := in.Body
	return h.userPage(ctx, "adminQueryUsers", b.Q, b.Status, b.Cursor, b.Limit)
}

// userPage runs one page of the admin user search; the cursor is bound to
// the operation and its criteria.
func (h handler) userPage(ctx context.Context, opID, query, status, cursor string, limit int) (*searchUsersOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint(opID, query, status)
	c, err := page.Decode(cursor, userSort, fp, 1)
	if err != nil {
		return nil, err
	}
	q := AdminUserQuery{Q: query, Status: Status(status), Limit: limit}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.SearchUsers(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &searchUsersOutput{Body: AdminUserList{Items: make([]AdminUserSummaryDTO, len(p.Items))}}
	for i, u := range p.Items {
		out.Body.Items[i] = toAdminSummaryDTO(u)
	}
	if p.More && len(p.Items) > 0 {
		out.Body.NextCursor = page.Next(userSort, fp, page.UUIDVal(p.Items[len(p.Items)-1].ID))
	}
	return out, nil
}

type userIDInput struct {
	UserID string `path:"user_id" doc:"User ID (an unknown or malformed ID is 404)"`
}

type adminUserOutput struct {
	Body AdminUserDTO
}

// adminActionOutput is a suspension: 200 done, 202 four-eyes request open.
type adminActionOutput struct {
	Status int
	Body   AdminUserDTO
}

// pendingOutput is a 204 operation that may answer 202 with the open
// four-eyes request instead (06 H-47).
type pendingOutput struct {
	Status int
	Body   *AccountActionDTO
}

func pendingResult(r *AccountActionRequest) *pendingOutput {
	if r == nil {
		return &pendingOutput{Status: http.StatusNoContent}
	}
	return &pendingOutput{Status: http.StatusAccepted, Body: toAccountActionDTO(r)}
}

// pathUser parses a user ID path parameter after the caller is known:
// malformed IDs are 404 like unknown ones.
func pathUser(ctx context.Context, raw string) (authz.Actor, uuid.UUID, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return authz.Actor{}, uuid.UUID{}, err
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		// Decide on a placeholder first: a caller without the role gets
		// 403, not a 404 that would tell IDs apart (03 §3.2).
		return a, uuid.UUID{}, ErrNotFound
	}
	return a, id, nil
}

func (h handler) getUser(ctx context.Context, in *userIDInput) (*adminUserOutput, error) {
	a, id, err := pathUser(ctx, in.UserID)
	if err != nil {
		return nil, notFoundAfterGate(ctx, a, authz.IdentityUserRead, err)
	}
	v, err := h.svc.AdminUser(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &adminUserOutput{Body: toAdminUserDTO(v)}, nil
}

type reasonInput struct {
	UserID string `path:"user_id" doc:"User ID (an unknown or malformed ID is 404)"`
	Body   struct {
		Reason reasonText `json:"reason" doc:"Audited"`
	}
}

func (h handler) suspendUser(ctx context.Context, in *reasonInput) (*adminActionOutput, error) {
	a, id, err := pathUser(ctx, in.UserID)
	if err != nil {
		return nil, notFoundAfterGate(ctx, a, authz.IdentityUserSuspend, err)
	}
	res, err := h.svc.SuspendUser(ctx, a, id, string(in.Body.Reason))
	if err != nil {
		return nil, err
	}
	out := &adminActionOutput{Status: http.StatusOK, Body: toAdminUserDTO(res.User)}
	if res.Pending != nil {
		out.Status, out.Body.PendingAction = http.StatusAccepted, toAccountActionDTO(res.Pending)
	}
	return out, nil
}

func (h handler) unsuspendUser(ctx context.Context, in *reasonInput) (*adminUserOutput, error) {
	a, id, err := pathUser(ctx, in.UserID)
	if err != nil {
		return nil, notFoundAfterGate(ctx, a, authz.IdentityUserSuspend, err)
	}
	v, err := h.svc.UnsuspendUser(ctx, a, id, string(in.Body.Reason))
	if err != nil {
		return nil, err
	}
	return &adminUserOutput{Body: toAdminUserDTO(v)}, nil
}

func (h handler) revokeUserSessions(ctx context.Context, in *reasonInput) (*pendingOutput, error) {
	a, id, err := pathUser(ctx, in.UserID)
	if err != nil {
		return nil, notFoundAfterGate(ctx, a, authz.IdentityUserRevokeSessions, err)
	}
	r, err := h.svc.RevokeUserSessions(ctx, a, id, string(in.Body.Reason))
	if err != nil {
		return nil, err
	}
	return pendingResult(r), nil
}

type resetMFAInput struct {
	UserID string `path:"user_id" doc:"User ID (an unknown or malformed ID is 404)"`
	Body   struct {
		Reason reasonText         `json:"reason" doc:"Audited"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof of the admin (03 §6.2)"`
	}
}

func (h handler) resetUserMFA(ctx context.Context, in *resetMFAInput) (*pendingOutput, error) {
	a, id, err := pathUser(ctx, in.UserID)
	if err != nil {
		return nil, notFoundAfterGate(ctx, a, authz.IdentityUserResetMfa, err)
	}
	r, err := h.svc.ResetUserMFA(ctx, a, id, string(in.Body.Reason), reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return pendingResult(r), nil
}

// notFoundAfterGate answers a malformed path ID: 403 for a caller the
// operation's grant does not cover, 404 otherwise (03 §3.2: the admin
// surface rejects before it reads anything). Errors other than
// ErrNotFound pass through.
func notFoundAfterGate(ctx context.Context, a authz.Actor, act authz.Action, err error) error {
	if !isNotFound(err) {
		return err
	}
	res := targetUser(uuid.Max())
	if act != authz.IdentityUserRead {
		res = accountResource(uuid.Max(), nil)
	}
	if gate := authz.Can(ctx, a, act, res); gate != nil {
		return gate
	}
	return ErrNotFound
}

// ---- platform roles

type listRolesInput struct {
	Role           string `query:"role" enum:"platform_admin,moderator,finance_admin,talent_auditor,support,dpo"`
	IncludeRevoked bool   `query:"include_revoked" doc:"Also revoked and rejected assignments"`
	page.Params
}

type listRolesOutput struct {
	Body PlatformRoleList
}

const roleSort = "id_desc"

func (h handler) listPlatformRoles(ctx context.Context, in *listRolesInput) (*listRolesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListPlatformRoles", in.Role, boolText(in.IncludeRevoked))
	c, err := page.Decode(in.Cursor, roleSort, fp, 1)
	if err != nil {
		return nil, err
	}
	q := RoleQuery{Role: authz.PlatformRole(in.Role), IncludeEnded: in.IncludeRevoked, Limit: in.Limit}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.PlatformRoleAssignments(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &listRolesOutput{Body: PlatformRoleList{Items: make([]PlatformRoleAssignmentDTO, len(p.Items))}}
	for i, r := range p.Items {
		out.Body.Items[i] = toRoleDTO(r, p.Users[r.UserID])
	}
	if p.More && len(p.Items) > 0 {
		out.Body.NextCursor = page.Next(roleSort, fp, page.UUIDVal(p.Items[len(p.Items)-1].ID))
	}
	return out, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type grantRoleInput struct {
	Body struct {
		UserID string             `json:"user_id" format:"uuid"`
		Role   string             `json:"role" enum:"platform_admin,moderator,finance_admin,talent_auditor,support,dpo"`
		Reason reasonText         `json:"reason" doc:"Audited"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof of the admin (03 §6.2)"`
	}
}

type roleOutput struct {
	Body PlatformRoleAssignmentDTO
}

func (h handler) roleOutput(ctx context.Context, ra RoleAssignment) (*roleOutput, error) {
	users, err := h.svc.GetMany(ctx, []uuid.UUID{ra.UserID})
	if err != nil {
		return nil, err
	}
	return &roleOutput{Body: toRoleDTO(ra, users[ra.UserID])}, nil
}

func (h handler) grantPlatformRole(ctx context.Context, in *grantRoleInput) (*roleOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	target, err := uuid.Parse(in.Body.UserID)
	if err != nil {
		return nil, problem.Field("body.user_id", "must be a UUID")
	}
	ra, err := h.svc.GrantPlatformRole(ctx, a, RoleGrant{
		UserID: target, Role: authz.PlatformRole(in.Body.Role), Reason: string(in.Body.Reason), Proof: reauthProof(in.Body.Reauth),
	})
	if err != nil {
		return nil, err
	}
	return h.roleOutput(ctx, ra)
}

type assignmentInput struct {
	AssignmentID string `path:"assignment_id" doc:"Assignment ID (an unknown or malformed ID is 404)"`
	Body         struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof of the admin (03 §6.2)"`
	}
}

func (h handler) approvePlatformRole(ctx context.Context, in *assignmentInput) (*roleOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.AssignmentID)
	if err != nil {
		if gate := authz.Can(ctx, a, authz.IdentityPlatformRoleGrant, roleGate(uuid.Max())); gate != nil {
			return nil, gate
		}
		return nil, ErrNotFound
	}
	ra, err := h.svc.ApprovePlatformRole(ctx, a, id, reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return h.roleOutput(ctx, ra)
}

type revokeRoleInput struct {
	AssignmentID string `path:"assignment_id" doc:"Assignment ID (an unknown or malformed ID is 404)"`
	Body         struct {
		Reason reasonText         `json:"reason" doc:"Audited"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof of the admin (03 §6.2)"`
	}
}

func (h handler) revokePlatformRole(ctx context.Context, in *revokeRoleInput) (*roleOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.AssignmentID)
	if err != nil {
		if gate := authz.Can(ctx, a, authz.IdentityPlatformRoleRevoke, roleGate(uuid.Max())); gate != nil {
			return nil, gate
		}
		return nil, ErrNotFound
	}
	ra, err := h.svc.RevokePlatformRole(ctx, a, id, string(in.Body.Reason), reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return h.roleOutput(ctx, ra)
}
