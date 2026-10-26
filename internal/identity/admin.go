package identity

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Admin user operations (04 §2.1, §V.5; 03 §5.1). Every method decides
// with authz.Can before it reads anything (03 §3.2: /admin/** is visible to
// every user, so a caller without the role gets 403 and learns nothing).

// Reason limits (02: suspension_reason ≤ 500; role reasons 5–500).
const (
	minReasonLen = 5
	maxReasonLen = 500
	minQueryLen  = 2
	maxQueryLen  = 100
)

// AdminUserQuery is one page of adminQueryUsers.
type AdminUserQuery struct {
	Q      string // an exact e-mail address or a name fragment (2–100 characters); "" = all
	Status Status // "" = every status
	After  *uuid.UUID
	Limit  int
}

// AdminUserPage is one page of accounts, newest first.
type AdminUserPage struct {
	Items []User
	More  bool
}

// AdminUser is the platform staff view of an account (04 §V.5 AdminUserDTO:
// no profile content, no sensitive club membership).
type AdminUser struct {
	User          User
	PlatformRoles []authz.PlatformRole
	// Memberships are the org memberships except clubs: identity cannot
	// tell a sensitive club from another one, so no club membership is
	// shown at all (03 Y2, 04 §V.5 "hassas kulüp üyeliği yok").
	Memberships []authz.Membership
	Student     *EduVerification // nil: no active student verification
	// StudentStatus is none, verified or lapsed (03 §2.1).
	StudentStatus authz.StudentStatus
	// ShowBirthDate: only the dpo column of 04 §V.5 sees the birth date.
	ShowBirthDate bool
	// PendingActions are the open four-eyes requests against the account
	// (06 H-47): pending and not expired, oldest first. A second
	// platform_admin sees what waits for them (BE-128).
	PendingActions []AccountActionRequest
}

// targetUser is the authz view of an account an admin acts on (03 §3.5:
// user, OwnerUserID = the target; Y6 reads it).
func targetUser(id uuid.UUID) authz.Resource {
	t := id
	return authz.Resource{Type: authz.TypeUser, ID: t, OwnerUserID: &t}
}

// adminSurface is the resource of the admin lists (nothing is read before
// the decision).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// cleanReason normalizes a staff reason: 5–500 characters of plain text.
func cleanReason(raw, location string) (string, error) {
	r, err := textx.NormalizeText(raw)
	n := utf8.RuneCountInString(r)
	if err != nil || n < minReasonLen || n > maxReasonLen {
		return "", invalid(ErrInvalidReason, location, "must be 5-500 characters of plain text")
	}
	return r, nil
}

// SearchUsers implements Service (adminQueryUsers): rate class search,
// every search audited with what kind of query it was and whom it found
// (never the query itself: it may be an e-mail address).
func (s *service) SearchUsers(ctx context.Context, a authz.Actor, q AdminUserQuery) (AdminUserPage, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserSearch, adminSurface); err != nil {
		return AdminUserPage{}, err
	}
	search := UserSearch{Status: q.Status, After: q.After, Limit: page.ClampLimit(q.Limit) + 1}
	kind := "all"
	if raw := strings.TrimSpace(q.Q); raw != "" {
		if strings.Contains(raw, "@") {
			_, normalized, err := parseEmail(raw, "body.q")
			if err != nil {
				return AdminUserPage{}, err
			}
			search.Email, kind = normalized, "email"
		} else {
			name, err := textx.NormalizeText(raw)
			if n := utf8.RuneCountInString(name); err != nil || n < minQueryLen || n > maxQueryLen {
				return AdminUserPage{}, invalid(problem.ErrValidation, "body.q", "must be an e-mail address or 2-100 characters of a name")
			}
			search.Name, kind = name, "name"
		}
	}
	switch q.Status {
	case "", StatusPendingEmail, StatusActive, StatusSuspended, StatusErased:
	default:
		return AdminUserPage{}, invalid(problem.ErrValidation, "query.status", "must be pending_email, active, suspended or erased")
	}
	rows, err := s.d.Store.SearchUsers(ctx, search)
	if err != nil {
		return AdminUserPage{}, err
	}
	items, more := page.Cut(rows, search.Limit-1)
	found := make([]string, len(items))
	for i, u := range items {
		found[i] = u.ID.String()
	}
	meta := map[string]any{"query_kind": kind, "results": len(items), "result_user_ids": found}
	if q.Status != "" {
		meta["status"] = string(q.Status)
	}
	if q.After != nil {
		meta["page_after"] = q.After.String()
	}
	err = s.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityUserSearch), Outcome: audit.Success,
		ResourceType: "admin_surface", Metadata: meta}.By(a))
	if err != nil {
		return AdminUserPage{}, err
	}
	return AdminUserPage{Items: items, More: more}, nil
}

// AdminUser implements Service (adminGetUser).
func (s *service) AdminUser(ctx context.Context, a authz.Actor, id uuid.UUID) (AdminUser, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserRead, targetUser(id)); err != nil {
		return AdminUser{}, err
	}
	u, err := s.d.Store.User(ctx, id)
	if err != nil {
		return AdminUser{}, err
	}
	return s.adminView(ctx, a, u)
}

// adminView completes an account with its roles, its non-club
// memberships and its student verification.
func (s *service) adminView(ctx context.Context, a authz.Actor, u User) (AdminUser, error) {
	roles, err := s.d.Store.PlatformRoles(ctx, u.ID)
	if err != nil {
		return AdminUser{}, err
	}
	ms, err := s.memberships(ctx, u.ID)
	if err != nil {
		return AdminUser{}, err
	}
	ms = slices.DeleteFunc(slices.Clone(ms), func(m authz.Membership) bool { return m.OrgKind == authz.OrgClub })
	out := AdminUser{User: u, PlatformRoles: roles, Memberships: ms, ShowBirthDate: a.HasPlatformRole(authz.PlatformDPO)}
	st, err := s.students.StudentStatus(ctx, u.ID)
	if err != nil {
		return AdminUser{}, err
	}
	out.Student, out.StudentStatus = st.Active, st.Status
	if out.PendingActions, err = s.openAccountActions(ctx, u.ID); err != nil {
		return AdminUser{}, err
	}
	return out, nil
}

// openAccountActions returns the four-eyes requests against userID that
// still wait for the second platform_admin: pending and not expired at
// now (an expired one is closed by the next call of its action), oldest
// first.
func (s *service) openAccountActions(ctx context.Context, userID uuid.UUID) ([]AccountActionRequest, error) {
	all, err := s.d.Store.AccountActionsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	return slices.DeleteFunc(all, func(r AccountActionRequest) bool {
		return r.Status != AccountActionPending || !now.Before(r.ExpiresAt)
	}), nil
}

// AdminActionResult is the outcome of an account action that may need a
// second platform_admin (06 H-47): Pending is set when this call only
// opened (or kept) the four-eyes request and nothing changed yet.
type AdminActionResult struct {
	User    AdminUser
	Pending *AccountActionRequest
}

// accountActionTTL is how long a four-eyes account action request waits
// for the second platform_admin.
const accountActionTTL = 24 * time.Hour

// accountResource is the authz view of an account an admin acts on with
// a four-eyes action (03 §3.5 user + Attrs requested_by: the first eye of
// a pending request, "" on a first step; 06 H-47).
func accountResource(id uuid.UUID, requestedBy *uuid.UUID) authz.Resource {
	r := targetUser(id)
	req := ""
	if requestedBy != nil {
		req = requestedBy.String()
	}
	r.Attrs = map[string]string{authz.AttrRequestedBy: req}
	return r
}

// holdsPrivileged reports whether userID holds a four-eyes role in effect
// (06 H-17 set: platform_admin, finance_admin, talent_auditor).
func (s *service) holdsPrivileged(ctx context.Context, userID uuid.UUID) ([]authz.PlatformRole, bool, error) {
	roles, err := s.d.Store.PlatformRoles(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	return roles, slices.ContainsFunc(roles, privilegedRole), nil
}

// accountFourEyes is 06 H-47, inside the business transaction: an action
// against the holder of a privileged role takes two platform admins. The
// first call opens a pending request (proceed=false); a later call by a
// different platform_admin (authz: Attrs requested_by → 403
// authz.four_eyes_violation; DB CHECK as second layer) approves it and the
// action runs (proceed=true). An expired request is closed and replaced by
// the caller's new one. A target without such a role proceeds at once.
func (s *service) accountFourEyes(ctx context.Context, a authz.Actor, act authz.Action, kind AccountActionKind, target uuid.UUID,
	reason string) (req *AccountActionRequest, proceed bool, err error) {
	if _, priv, err := s.holdsPrivileged(ctx, target); err != nil || !priv {
		return nil, err == nil, err
	}
	if target == a.UserID {
		return nil, false, ErrUserInvalidState.Wrap(errors.New("an admin cannot act on their own privileged account"))
	}
	now := s.now()
	open, found, err := s.d.Store.LockOpenAccountAction(ctx, target, kind)
	if err != nil {
		return nil, false, err
	}
	if found && !now.Before(open.ExpiresAt) {
		open.Status = AccountActionExpired
		if err := s.d.Store.UpdateAccountAction(ctx, open); err != nil {
			return nil, false, err
		}
		found = false
	}
	if !found {
		r := AccountActionRequest{ID: uuid.NewV7(), TargetUserID: target, Action: kind, Reason: reason, Status: AccountActionPending,
			RequestedBy: a.UserID, RequestedAt: now, ExpiresAt: now.Add(accountActionTTL)}
		if err := s.d.Store.InsertAccountAction(ctx, r); err != nil {
			return nil, false, err
		}
		err := s.d.Audit.Log(ctx, audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: resourceUser, ResourceID: &r.TargetUserID,
			Reason: reason, Metadata: map[string]any{"result": "four_eyes_requested", "request_id": r.ID.String(),
				audit.MetaSubjectUserID: target.String()}}.By(a))
		if err != nil {
			return nil, false, err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "four-eyes account action requested", slog.String("action", string(kind)),
			slog.String("user_id", target.String()), slog.String("by", a.UserID.String()))
		return &r, false, nil
	}
	if err := authz.Can(ctx, a, act, accountResource(target, &open.RequestedBy)); err != nil {
		return nil, false, err
	}
	me := a.UserID
	open.Status, open.ApprovedBy, open.ApprovedAt = AccountActionApproved, &me, &now
	if err := s.d.Store.UpdateAccountAction(ctx, open); err != nil {
		return nil, false, err
	}
	return &open, true, nil
}

// fourEyesMeta adds the approved request to the audit metadata of the action.
func fourEyesMeta(meta map[string]any, req *AccountActionRequest) map[string]any {
	if req != nil {
		meta["request_id"], meta["requested_by"] = req.ID.String(), req.RequestedBy.String()
	}
	return meta
}

// keepActiveHolders refuses to suspend the last active holder of a
// privileged role (06 H-47): for every four-eyes role the target holds in
// effect, another holder of it whose account is active must remain. For
// platform_admin the four-eyes flow already implies two other active
// admins (the requester and the approver); the check is the safety net.
func (s *service) keepActiveHolders(ctx context.Context, target uuid.UUID) error {
	roles, err := s.d.Store.PlatformRoles(ctx, target)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if !privilegedRole(r) {
			continue
		}
		ids, err := s.d.Store.UsersWithRole(ctx, r)
		if err != nil {
			return err
		}
		users, err := s.d.Store.Users(ctx, ids)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(users, func(u User) bool { return u.ID != target && u.Status == StatusActive }) {
			return ErrLastPrivilegedHolder.Wrap(errors.New("last active " + string(r)))
		}
	}
	return nil
}

// SuspendUser implements Service (adminSuspendUser): the account becomes
// suspended and every open session closes with reason suspended (03 §2.2-3).
// Against a privileged role holder it takes two platform admins (06 H-47),
// and the last active holder of a privileged role cannot be suspended.
func (s *service) SuspendUser(ctx context.Context, a authz.Actor, id uuid.UUID, rawReason string) (AdminActionResult, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserSuspend, accountResource(id, nil)); err != nil {
		return AdminActionResult{}, err
	}
	reason, err := cleanReason(rawReason, "body.reason")
	if err != nil {
		return AdminActionResult{}, err
	}
	if id == a.UserID {
		return AdminActionResult{}, ErrUserInvalidState.Wrap(errors.New("an admin cannot suspend their own account"))
	}
	var out User
	var pending *AccountActionRequest
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		pending = nil
		u, err := s.d.Store.LockUser(ctx, id)
		if err != nil {
			return err
		}
		out = u
		if u.Status != StatusActive && u.Status != StatusPendingEmail {
			return ErrUserInvalidState
		}
		if err := s.keepActiveHolders(ctx, u.ID); err != nil {
			return err
		}
		req, proceed, err := s.accountFourEyes(ctx, a, authz.IdentityUserSuspend, AccountSuspend, u.ID, reason)
		if err != nil {
			return err
		}
		if !proceed {
			pending = req
			return nil
		}
		now := s.now()
		u.Status, u.SuspendedAt, u.SuspensionReason = StatusSuspended, &now, reason
		if err := s.d.Store.UpdateUser(ctx, u); err != nil {
			return err
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, u.ID, nil, now, RevokeSuspended)
		if err != nil {
			return err
		}
		out = u
		return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityUserSuspend), Outcome: audit.Success,
			ResourceType: resourceUser, ResourceID: &u.ID, Reason: reason,
			Metadata: fourEyesMeta(map[string]any{"result": "suspended", "sessions_revoked": n, audit.MetaSubjectUserID: u.ID.String()}, req)}.By(a))
	})
	if err != nil {
		return AdminActionResult{}, err
	}
	if pending == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "account suspended", slog.String("user_id", id.String()),
			slog.String("by", a.UserID.String()))
	}
	v, err := s.adminView(ctx, a, out)
	if err != nil {
		return AdminActionResult{}, err
	}
	return AdminActionResult{User: v, Pending: pending}, nil
}

// UnsuspendUser implements Service (adminUnsuspendUser): back to active
// (pending_email when the address was never verified).
func (s *service) UnsuspendUser(ctx context.Context, a authz.Actor, id uuid.UUID, rawReason string) (AdminUser, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserSuspend, accountResource(id, nil)); err != nil {
		return AdminUser{}, err
	}
	reason, err := cleanReason(rawReason, "body.reason")
	if err != nil {
		return AdminUser{}, err
	}
	var out User
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUser(ctx, id)
		if err != nil {
			return err
		}
		if u.Status != StatusSuspended {
			return ErrUserInvalidState
		}
		u.Status, u.SuspendedAt, u.SuspensionReason = StatusActive, nil, ""
		if u.EmailVerifiedAt == nil {
			u.Status = StatusPendingEmail
		}
		if err := s.d.Store.UpdateUser(ctx, u); err != nil {
			return err
		}
		out = u
		return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityUserSuspend), Outcome: audit.Success,
			ResourceType: resourceUser, ResourceID: &u.ID, Reason: reason,
			Metadata: map[string]any{"result": "unsuspended", audit.MetaSubjectUserID: u.ID.String()}}.By(a))
	})
	if err != nil {
		return AdminUser{}, err
	}
	return s.adminView(ctx, a, out)
}

// RevokeUserSessions implements Service (adminRevokeUserSessions); four
// eyes against a privileged role holder (06 H-47): a non-nil request means
// the call only opened it.
func (s *service) RevokeUserSessions(ctx context.Context, a authz.Actor, id uuid.UUID, rawReason string) (*AccountActionRequest, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserRevokeSessions, accountResource(id, nil)); err != nil {
		return nil, err
	}
	reason, err := cleanReason(rawReason, "body.reason")
	if err != nil {
		return nil, err
	}
	var pending *AccountActionRequest
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		pending = nil
		u, err := s.d.Store.LockUser(ctx, id)
		if err != nil {
			return err
		}
		req, proceed, err := s.accountFourEyes(ctx, a, authz.IdentityUserRevokeSessions, AccountRevokeSessions, u.ID, reason)
		if err != nil {
			return err
		}
		if !proceed {
			pending = req
			return nil
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, u.ID, nil, s.now(), RevokeAdmin)
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityUserRevokeSessions), Outcome: audit.Success,
			ResourceType: resourceUser, ResourceID: &u.ID, Reason: reason,
			Metadata: fourEyesMeta(map[string]any{"sessions_revoked": n, audit.MetaSubjectUserID: u.ID.String()}, req)}.By(a))
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

// ResetUserMFA implements Service (adminResetUserMfa, R, k2): the TOTP
// secret and the recovery codes go, every session closes (mfa_reset) and
// the user gets a security notice. Idempotent on an account without TOTP.
// Four eyes against a privileged role holder (06 H-47): a non-nil request
// means the call only opened it.
func (s *service) ResetUserMFA(ctx context.Context, a authz.Actor, id uuid.UUID, rawReason string, proof ReauthProof) (*AccountActionRequest, error) {
	if err := authz.Can(ctx, a, authz.IdentityUserResetMfa, accountResource(id, nil)); err != nil {
		return nil, err
	}
	reason, err := cleanReason(rawReason, "body.reason")
	if err != nil {
		return nil, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityUserResetMfa, resourceUser, &id, nil))
	if err != nil {
		return nil, err
	}
	var pending *AccountActionRequest
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		pending = nil
		u, err := s.d.Store.LockUser(ctx, id)
		if err != nil {
			return err
		}
		req, proceed, err := s.accountFourEyes(ctx, a, authz.IdentityUserResetMfa, AccountResetMFA, u.ID, reason)
		if err != nil {
			return err
		}
		if !proceed {
			pending = req
			return nil
		}
		had := u.MFAEnabled
		if err := s.dropMFA(ctx, u.ID); err != nil {
			return err
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, u.ID, nil, s.now(), RevokeMFAReset)
		if err != nil {
			return err
		}
		if u.Status != StatusErased {
			s.notifyUser(ctx, u.ID, NoticeMFAReset)
		}
		return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityUserResetMfa), Outcome: audit.Success,
			ResourceType: resourceUser, ResourceID: &u.ID, Reason: reason,
			Metadata: fourEyesMeta(map[string]any{audit.MetaReauth: kind, "mfa_was_enabled": had, "sessions_revoked": n,
				audit.MetaSubjectUserID: u.ID.String()}, req)}.By(a))
	})
	if err != nil {
		return nil, err
	}
	if pending == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "mfa reset by admin", slog.String("user_id", id.String()),
			slog.String("by", a.UserID.String()))
	}
	return pending, nil
}
