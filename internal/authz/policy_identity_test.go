package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// The resources the identity service builds for BE-016 (03 §3.5; 06 H-27).
func ownAccount(p authztest.Persona) authz.Resource {
	id := p.Actor.UserID
	return authz.Resource{Type: authz.TypeUser, ID: id, OwnerUserID: &id}
}

func targetUser(name string) authz.Resource {
	id := authztest.ID(name)
	return authz.Resource{Type: authz.TypeUser, ID: id, OwnerUserID: &id}
}

// accountAction is the resource of the four-eyes account actions (06 H-47).
func accountAction(name, requestedBy string) authz.Resource {
	r := targetUser(name)
	req := ""
	if requestedBy != "" {
		req = authztest.ID(requestedBy).String()
	}
	r.Attrs = map[string]string{authz.AttrRequestedBy: req}
	return r
}

func roleAssignment(target, role, requestedBy string) authz.Resource {
	id := authztest.ID(target)
	req := ""
	if requestedBy != "" {
		req = authztest.ID(requestedBy).String()
	}
	return authz.Resource{Type: authz.TypePlatformRoleAssignment, ID: uuid.NewV7(), OwnerUserID: &id,
		Attrs: map[string]string{"role": role, authz.AttrRequestedBy: req}}
}

// TestIdentityPolicy_MFAAndAdminMatrix pins the BE-016 rows of 03 §5.1 for
// the 18 personas (the HTTP matrix checks the same through the API).
func TestIdentityPolicy_MFAAndAdminMatrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden := authz.ErrForbidden
	self := func(p authztest.Persona) authz.Resource { return ownAccount(p) }
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     func(p authztest.Persona) authz.Resource
		want    map[string]error
		missing error
	}{
		{"mfa.enroll", authz.IdentityMfaEnroll, self, map[string]error{"anonymous": forbidden}, nil},
		{"mfa.confirm", authz.IdentityMfaConfirm, self, map[string]error{"anonymous": forbidden}, nil},
		{"mfa.verify", authz.IdentityMfaVerify, self, map[string]error{"anonymous": forbidden}, nil},
		{"mfa.disable", authz.IdentityMfaDisable, self, map[string]error{"anonymous": forbidden}, nil},
		{"mfa_recovery.regenerate", authz.IdentityMfaRecoveryRegenerate, self, map[string]error{"anonymous": forbidden}, nil},
		{"user.search", authz.IdentityUserSearch, func(authztest.Persona) authz.Resource { return adminSurface },
			map[string]error{"support": nil, "dpo": nil, "platform_admin": nil}, forbidden},
		{"user.read", authz.IdentityUserRead, func(authztest.Persona) authz.Resource { return targetUser("student_b") },
			map[string]error{"support": nil, "dpo": nil, "platform_admin": nil}, forbidden},
		{"user.suspend", authz.IdentityUserSuspend, func(authztest.Persona) authz.Resource { return accountAction("student_b", "") },
			map[string]error{"platform_admin": nil}, forbidden},
		{"user.revoke_sessions", authz.IdentityUserRevokeSessions, func(authztest.Persona) authz.Resource { return accountAction("student_b", "") },
			map[string]error{"support": nil, "platform_admin": nil}, forbidden},
		{"user.reset_mfa", authz.IdentityUserResetMfa, func(authztest.Persona) authz.Resource { return accountAction("student_b", "") },
			map[string]error{"platform_admin": nil}, forbidden},
		{"platform_role.list", authz.IdentityPlatformRoleList, func(authztest.Persona) authz.Resource { return adminSurface },
			map[string]error{"dpo": nil, "platform_admin": nil}, forbidden},
		{"platform_role.grant request", authz.IdentityPlatformRoleGrant,
			func(authztest.Persona) authz.Resource { return roleAssignment("student_b", "finance_admin", "") },
			map[string]error{"platform_admin": nil}, forbidden},
		{"platform_role.grant approval", authz.IdentityPlatformRoleGrant,
			func(authztest.Persona) authz.Resource {
				return roleAssignment("student_b", "finance_admin", "platform_admin_2")
			},
			map[string]error{"platform_admin": nil}, forbidden},
		{"platform_role.revoke", authz.IdentityPlatformRoleRevoke,
			func(authztest.Persona) authz.Resource { return roleAssignment("student_b", "moderator", "") },
			map[string]error{"platform_admin": nil}, forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.missing
				}
				if p.Name == "anonymous" && tc.missing == nil {
					want = forbidden
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res(p)), want, false)
				})
			}
		})
	}
}

func TestIdentityPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pad := persona(t, "platform_admin")

	// O: another user's account is invisible for the MFA actions, staff included.
	for _, act := range []authz.Action{authz.IdentityMfaEnroll, authz.IdentityMfaConfirm, authz.IdentityMfaVerify,
		authz.IdentityMfaDisable, authz.IdentityMfaRecoveryRegenerate} {
		wantErr(t, authz.Can(ctx, pad, act, targetUser("student")), authz.ErrNotFound, false)
	}
	// The MFA O cells never require an MFA session: an admin enrolls without one (03 §6.4-2).
	noMFA := pad
	noMFA.MFAVerified = false
	self := ownAccount(authztest.Persona{Actor: noMFA})
	for _, act := range []authz.Action{authz.IdentityMfaEnroll, authz.IdentityMfaConfirm, authz.IdentityMfaVerify} {
		wantErr(t, authz.Can(ctx, noMFA, act, self), nil, false)
	}
	// k-mfa: the staff cells need an MFA-verified session.
	wantErr(t, authz.Can(ctx, noMFA, authz.IdentityUserSearch, adminSurface), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, noMFA, authz.IdentityPlatformRoleGrant, roleAssignment("student_b", "moderator", "")), authz.ErrMFARequired, false)

	// Y6: nobody grants, approves or revokes their own role, or resets their own MFA.
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleGrant, roleAssignment("platform_admin", "finance_admin", "")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleGrant, roleAssignment("platform_admin", "finance_admin", "platform_admin_2")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleRevoke, roleAssignment("platform_admin", "platform_admin", "platform_admin_2")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, pad, authz.IdentityUserResetMfa, accountAction("platform_admin", "")), authz.ErrForbidden, false)

	// 06 H-47: approving a pending account action against a privileged
	// holder — the requester gets 403 four eyes, support never approves.
	for _, act := range []authz.Action{authz.IdentityUserSuspend, authz.IdentityUserRevokeSessions, authz.IdentityUserResetMfa} {
		wantErr(t, authz.Can(ctx, pad, act, accountAction("finance_admin", "platform_admin")), authz.ErrFourEyes, false)
		wantErr(t, authz.Can(ctx, persona(t, "platform_admin_2"), act, accountAction("finance_admin", "platform_admin")), nil, false)
		wantErr(t, authz.Can(ctx, persona(t, "support"), act, accountAction("finance_admin", "platform_admin_2")), authz.ErrForbidden, false)
		// The key is mandatory (fail closed on a service that forgot it).
		wantErr(t, authz.Can(ctx, pad, act, targetUser("finance_admin")), authz.ErrForbidden, true)
	}

	// Four eyes (06 H-17): the requester cannot be the approver, of a grant or of a revocation.
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleGrant, roleAssignment("student_b", "finance_admin", "platform_admin")), authz.ErrFourEyes, false)
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleRevoke, roleAssignment("finance_admin", "finance_admin", "platform_admin")), authz.ErrFourEyes, false)
	pad2 := persona(t, "platform_admin_2")
	wantErr(t, authz.Can(ctx, pad2, authz.IdentityPlatformRoleGrant, roleAssignment("student_b", "finance_admin", "platform_admin")), nil, false)

	// H-27: a role resource without its Attrs (or its target) is a programmer error.
	target := authztest.ID("student_b")
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleGrant,
		authz.Resource{Type: authz.TypePlatformRoleAssignment, OwnerUserID: &target, Attrs: map[string]string{"role": "moderator"}}), authz.ErrForbidden, true)
	wantErr(t, authz.Can(ctx, pad, authz.IdentityPlatformRoleGrant,
		authz.Resource{Type: authz.TypePlatformRoleAssignment, Attrs: map[string]string{"role": "moderator", authz.AttrRequestedBy: ""}}), authz.ErrNotFound, false)
	// The admin user cells need the target as owner.
	wantErr(t, authz.Can(ctx, pad, authz.IdentityUserSuspend, authz.Resource{Type: authz.TypeUser,
		Attrs: map[string]string{authz.AttrRequestedBy: ""}}), authz.ErrNotFound, false)
}
