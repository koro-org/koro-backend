package identity_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/token"
)

// storeWorld is one fresh store.
type storeWorld struct {
	s identity.Store
}

var contractBase = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func contractUser(email string) identity.User {
	b, _ := civil.New(2001, time.June, 2)
	return identity.User{
		ID: uuid.NewV7(), Status: identity.StatusActive, Email: email, EmailNormalized: email, PasswordHash: "$argon2id$x",
		FirstName: "Ada", LastName: "Yılmaz", BirthDate: &b, Persona: identity.PersonaStudent, Locale: identity.LocaleTR,
		CreatedAt: contractBase, UpdatedAt: contractBase,
	}
}

func contractSession(userID uuid.UUID, at time.Time) identity.Session {
	_, hash, _ := token.New()
	return identity.Session{ID: uuid.NewV7(), UserID: userID, TokenHash: hash, ClientKind: identity.ClientWeb,
		CreatedAt: at, LastSeenAt: at, IdleExpiresAt: at.Add(time.Hour), AbsoluteExpiresAt: at.Add(2 * time.Hour), IP: "203.0.113.9", UserAgent: "UA"}
}

// storeContract is the contract both stores pass (05 §13.3): the in-memory
// one here, the GORM one against PostgreSQL in store_integration_test.go.
func storeContract(t *testing.T, newWorld func(t *testing.T) storeWorld) {
	t.Helper()
	t.Run("users", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		u := contractUser(uuid.NewV7().String() + "@contract.test")
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		dup := contractUser(u.Email)
		if err := s.InsertUser(ctx, dup); !errors.Is(err, identity.ErrEmailTaken) {
			t.Fatalf("duplicate address = %v, want ErrEmailTaken", err)
		}
		got, err := s.User(ctx, u.ID)
		if err != nil || got.Email != u.Email || got.BirthDate == nil || *got.BirthDate != *u.BirthDate || got.Persona != u.Persona {
			t.Fatalf("User = %+v, %v", got, err)
		}
		if _, found, _ := s.UserByEmail(ctx, u.EmailNormalized); !found {
			t.Fatal("UserByEmail missed")
		}
		if _, err := s.User(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("unknown user = %v", err)
		}
		lock := contractBase.Add(time.Hour)
		got.FailedLogins, got.LockedUntil, got.Locale, got.FirstName = 3, &lock, identity.LocaleEN, "Ayşe"
		if err := s.UpdateUser(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, _ := s.LockUser(ctx, u.ID)
		if again.FailedLogins != 3 || again.LockedUntil == nil || !again.LockedUntil.Equal(lock) || again.Locale != identity.LocaleEN || again.FirstName != "Ayşe" {
			t.Fatalf("after update = %+v", again)
		}
		other := contractUser(uuid.NewV7().String() + "@contract.test")
		if err := s.InsertUser(ctx, other); err != nil {
			t.Fatal(err)
		}
		other.Email, other.EmailNormalized = u.Email, u.EmailNormalized
		if err := s.UpdateUser(ctx, other); !errors.Is(err, identity.ErrEmailTaken) {
			t.Fatalf("update to a taken address = %v", err)
		}
		us, _ := s.Users(ctx, []uuid.UUID{u.ID, uuid.NewV7()})
		if len(us) != 1 || us[0].ID != u.ID {
			t.Fatalf("Users = %+v", us)
		}
		// Erasure: a tombstone, idempotent, the address is free again.
		if ok, err := s.EraseUser(ctx, u.ID, contractBase.Add(2*time.Hour)); err != nil || !ok {
			t.Fatalf("EraseUser = %v, %v", ok, err)
		}
		if ok, err := s.EraseUser(ctx, u.ID, contractBase.Add(3*time.Hour)); err != nil || ok {
			t.Fatalf("second EraseUser = %v, %v", ok, err)
		}
		tomb, _ := s.User(ctx, u.ID)
		if tomb.Status != identity.StatusErased || tomb.Email != "" || tomb.PasswordHash != "" || tomb.FirstName != "" || tomb.BirthDate != nil || tomb.ErasedAt == nil {
			t.Fatalf("tombstone = %+v", tomb)
		}
		if _, found, _ := s.UserByEmail(ctx, u.EmailNormalized); found {
			t.Fatal("a tombstone is still found by address")
		}
	})

	t.Run("sessions", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		u := contractUser(uuid.NewV7().String() + "@contract.test")
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		a := contractSession(u.ID, contractBase)
		b := contractSession(u.ID, contractBase.Add(time.Minute))
		c := contractSession(u.ID, contractBase.Add(2*time.Minute))
		for _, x := range []identity.Session{a, b, c} {
			if err := s.InsertSession(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		got, found, err := s.SessionByTokenHash(ctx, a.TokenHash)
		if err != nil || !found || got.ID != a.ID || got.IP != "203.0.113.9" || !bytes.Equal(got.TokenHash, a.TokenHash) {
			t.Fatalf("SessionByTokenHash = %+v, %v, %v", got, found, err)
		}
		if _, found, _ := s.SessionByTokenHash(ctx, token.Hash("nope")); found {
			t.Fatal("unknown hash found")
		}
		// Touch never passes the absolute expiry.
		if err := s.TouchSession(ctx, a.ID, contractBase.Add(30*time.Minute), contractBase.Add(10*time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Session(ctx, a.ID)
		if !got.IdleExpiresAt.Equal(a.AbsoluteExpiresAt) || !got.LastSeenAt.Equal(contractBase.Add(30*time.Minute)) {
			t.Fatalf("touched = %+v", got)
		}
		// Revoke: only the owner's, only once.
		if ok, _ := s.RevokeSession(ctx, uuid.NewV7(), a.ID, contractBase, identity.RevokeLogout); ok {
			t.Fatal("revoked through another user")
		}
		if ok, err := s.RevokeSession(ctx, u.ID, a.ID, contractBase.Add(time.Minute), identity.RevokeLogout); !ok || err != nil {
			t.Fatalf("RevokeSession = %v, %v", ok, err)
		}
		if ok, _ := s.RevokeSession(ctx, u.ID, a.ID, contractBase.Add(time.Minute), identity.RevokeLogout); ok {
			t.Fatal("revoked twice")
		}
		live, _ := s.ActiveSessions(ctx, u.ID, contractBase.Add(3*time.Minute), nil, 10)
		if len(live) != 2 || live[0].ID != c.ID || live[1].ID != b.ID {
			t.Fatalf("ActiveSessions = %v", ids(live))
		}
		page2, _ := s.ActiveSessions(ctx, u.ID, contractBase.Add(3*time.Minute), &c.ID, 10)
		if len(page2) != 1 || page2[0].ID != b.ID {
			t.Fatalf("ActiveSessions after = %v", ids(page2))
		}
		if n, err := s.RevokeUserSessions(ctx, u.ID, &c.ID, contractBase.Add(4*time.Minute), identity.RevokePasswordChange); err != nil || n != 1 {
			t.Fatalf("RevokeUserSessions = %d, %v", n, err)
		}
		got, _ = s.Session(ctx, b.ID)
		if got.RevokeReason != identity.RevokePasswordChange {
			t.Fatalf("b = %+v", got.RevokeReason)
		}
		// Cleanup: ended before the cutoff (a revoked at +1 min, b at +4 min).
		n, err := s.DeleteEndedSessions(ctx, contractBase.Add(2*time.Minute), 100)
		if err != nil || n != 1 {
			t.Fatalf("DeleteEndedSessions = %d, %v", n, err)
		}
		all, _ := s.SessionsOf(ctx, u.ID)
		if len(all) != 2 {
			t.Fatalf("SessionsOf = %d", len(all))
		}
		if n, _ := s.DeleteUserSessions(ctx, u.ID); n != 2 {
			t.Fatalf("DeleteUserSessions = %d", n)
		}
	})

	t.Run("email tokens", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		u := contractUser(uuid.NewV7().String() + "@contract.test")
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		mk := func(p identity.TokenPurpose, target string) identity.EmailToken {
			_, h, _ := token.New()
			return identity.EmailToken{ID: uuid.NewV7(), UserID: u.ID, Purpose: p, TargetEmail: target, KeyID: "k1", TokenHash: h,
				ExpiresAt: contractBase.Add(time.Hour), CreatedAt: contractBase}
		}
		t1, t2 := mk(identity.PurposeAccountEmail, ""), mk(identity.PurposeEmailChange, "n@contract.test")
		for _, x := range []identity.EmailToken{t1, t2} {
			if err := s.InsertEmailToken(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CloseOpenTokens(ctx, u.ID, identity.PurposeAccountEmail, contractBase.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		got, found, err := s.EmailTokenByHash(ctx, t1.TokenHash)
		if err != nil || !found || got.ConsumedAt == nil {
			t.Fatalf("closed token = %+v, %v", got, err)
		}
		if ok, _ := s.ConsumeEmailToken(ctx, t1.ID, contractBase); ok {
			t.Fatal("a closed token was consumed")
		}
		if ok, err := s.ConsumeEmailToken(ctx, t2.ID, contractBase.Add(2*time.Minute)); !ok || err != nil {
			t.Fatalf("ConsumeEmailToken = %v, %v", ok, err)
		}
		if ok, _ := s.ConsumeEmailToken(ctx, t2.ID, contractBase.Add(3*time.Minute)); ok {
			t.Fatal("consumed twice")
		}
		locked, err := s.LockEmailToken(ctx, t2.ID)
		if err != nil || locked.TargetEmail != "n@contract.test" {
			t.Fatalf("LockEmailToken = %+v, %v", locked, err)
		}
		if _, err := s.EmailToken(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("unknown token = %v", err)
		}
		t3 := mk(identity.PurposePasswordReset, "")
		if err := s.InsertEmailToken(ctx, t3); err != nil {
			t.Fatal(err)
		}
		if n, err := s.DeleteEndedEmailTokens(ctx, contractBase.Add(5*time.Minute), 100); err != nil || n != 2 {
			t.Fatalf("DeleteEndedEmailTokens = %d, %v", n, err)
		}
		if n, _ := s.DeleteUserEmailTokens(ctx, u.ID); n != 1 {
			t.Fatalf("DeleteUserEmailTokens = %d", n)
		}
	})

	t.Run("platform roles in effect (06 H-33)", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		var us []identity.User
		for range 3 {
			u := contractUser(uuid.NewV7().String() + "@contract.test")
			if err := s.InsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			us = append(us, u)
		}
		target, a, b := us[0], us[1], us[2]
		now := contractBase
		rows := []identity.RoleAssignment{
			{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformAdmin, Reason: "awaiting approval", Status: identity.RolePending, RequestedBy: &a.ID, RequestedAt: now},
			{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformSupport, Reason: "support team", Status: identity.RoleActive, RequestedBy: &a.ID, RequestedAt: now, GrantedAt: &now},
			{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformFinanceAdmin, Reason: "finance team", Status: identity.RoleRevokePending,
				RequestedBy: &a.ID, RequestedAt: now, ApprovedBy: &b.ID, ApprovedAt: &now, GrantedAt: &now,
				RevokeReason: "leaving the team", RevokeRequestedBy: &a.ID, RevokeRequestedAt: &now},
			{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformDPO, Reason: "former dpo", Status: identity.RoleRevoked,
				RequestedBy: &a.ID, RequestedAt: now, GrantedAt: &now, RevokeReason: "role ended", RevokedBy: &b.ID, RevokedAt: &now},
			{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformModerator, Reason: "refused request", Status: identity.RoleRejected,
				RequestedBy: &a.ID, RequestedAt: now, RejectedBy: &b.ID, RejectedAt: &now},
		}
		for _, r := range rows {
			if err := s.InsertRoleAssignment(ctx, r); err != nil {
				t.Fatalf("%s %s: %v", r.Role, r.Status, err)
			}
		}
		got, err := s.PlatformRoles(ctx, target.ID)
		want := []authz.PlatformRole{authz.PlatformFinanceAdmin, authz.PlatformSupport}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("PlatformRoles = %v, %v; want %v", got, err, want)
		}
		if h, _ := s.UsersWithRole(ctx, authz.PlatformAdmin); len(h) != 0 {
			t.Fatalf("a pending platform_admin request counts: %v", h)
		}
		// A second open row of (user, role) is refused (…_open_uq).
		dup := identity.RoleAssignment{ID: uuid.NewV7(), UserID: target.ID, Role: authz.PlatformAdmin, Reason: "second request",
			Status: identity.RolePending, RequestedBy: &b.ID, RequestedAt: now}
		if err := s.InsertRoleAssignment(ctx, dup); !errors.Is(err, identity.ErrRoleAssignmentOpen) {
			t.Fatalf("second open row = %v", err)
		}
		// A privileged role in effect without a second approver is refused,
		// except the bootstrap row (no requester at all).
		bad := identity.RoleAssignment{ID: uuid.NewV7(), UserID: a.ID, Role: authz.PlatformAdmin, Reason: "single approver",
			Status: identity.RoleActive, RequestedBy: &b.ID, RequestedAt: now, GrantedAt: &now}
		if err := s.InsertRoleAssignment(ctx, bad); err == nil {
			t.Fatal("platform_admin in effect without approval accepted")
		}
		boot := identity.RoleAssignment{ID: uuid.NewV7(), UserID: a.ID, Role: authz.PlatformAdmin, Reason: "bootstrap",
			Status: identity.RoleActive, RequestedAt: now, GrantedAt: &now}
		if err := s.InsertRoleAssignment(ctx, boot); err != nil {
			t.Fatalf("bootstrap row: %v", err)
		}
		if h, _ := s.UsersWithRole(ctx, authz.PlatformAdmin); !slices.Equal(h, []uuid.UUID{a.ID}) {
			t.Fatalf("UsersWithRole = %v", h)
		}
		if all, _ := s.RoleAssignmentsOf(ctx, target.ID); len(all) != len(rows) {
			t.Fatalf("RoleAssignmentsOf = %d", len(all))
		}
	})
	t.Run("four-eyes role flow (06 H-17)", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		var us [4]identity.User
		for i := range us {
			us[i] = contractUser(uuid.NewV7().String() + "@contract.test")
			if err := s.InsertUser(ctx, us[i]); err != nil {
				t.Fatal(err)
			}
		}
		a, b, c, target := us[0].ID, us[1].ID, us[2].ID, us[3].ID
		now := contractBase
		ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: target, Role: authz.PlatformFinanceAdmin, Reason: "needs finance",
			Status: identity.RolePending, RequestedBy: &a, RequestedAt: now}
		if err := s.InsertRoleAssignment(ctx, ra); err != nil {
			t.Fatal(err)
		}
		// approved_by = requested_by: …_four_eyes_chk → the authz problem code.
		bad := ra
		bad.Status, bad.ApprovedBy, bad.ApprovedAt, bad.GrantedAt = identity.RoleActive, &a, &now, &now
		if err := s.UpdateRoleAssignment(ctx, bad); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("self approval = %v, want ErrFourEyes", err)
		}
		// active without any approver: …_privileged_chk.
		bad.ApprovedBy, bad.ApprovedAt = nil, nil
		if err := s.UpdateRoleAssignment(ctx, bad); err == nil {
			t.Fatal("privileged role in effect without approver accepted")
		}
		ok := ra
		ok.Status, ok.ApprovedBy, ok.ApprovedAt, ok.GrantedAt = identity.RoleActive, &b, &now, &now
		if err := s.UpdateRoleAssignment(ctx, ok); err != nil {
			t.Fatalf("approval by b: %v", err)
		}
		got, err := s.RoleAssignment(ctx, ra.ID)
		if err != nil || got.Status != identity.RoleActive || *got.ApprovedBy != b || got.GrantedAt == nil {
			t.Fatalf("RoleAssignment = %+v, %v", got, err)
		}
		if eff, pend, rp, err := s.CountRoleAssignments(ctx, authz.PlatformFinanceAdmin); err != nil || eff != 1 || pend != 0 || rp != 0 {
			t.Fatalf("Count = %d %d %d %v", eff, pend, rp, err)
		}
		// Revocation: request by b, confirmation by the same b refused (…_revoke_four_eyes_chk).
		got.Status, got.RevokeReason, got.RevokeRequestedBy, got.RevokeRequestedAt = identity.RoleRevokePending, "left the team", &b, &now
		if err := s.UpdateRoleAssignment(ctx, got); err != nil {
			t.Fatalf("revoke request: %v", err)
		}
		if roles, _ := s.PlatformRoles(ctx, target); len(roles) != 1 {
			t.Fatalf("revoke_pending not in effect: %v", roles)
		}
		if _, _, rp, _ := s.CountRoleAssignments(ctx, authz.PlatformFinanceAdmin); rp != 1 {
			t.Fatalf("revoke_pending count %d", rp)
		}
		self := got
		self.Status, self.RevokedBy, self.RevokedAt = identity.RoleRevoked, &b, &now
		if err := s.UpdateRoleAssignment(ctx, self); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("self-confirmed revocation = %v, want ErrFourEyes", err)
		}
		got.Status, got.RevokedBy, got.RevokedAt = identity.RoleRevoked, &c, &now
		if err := s.UpdateRoleAssignment(ctx, got); err != nil {
			t.Fatalf("confirmation by c: %v", err)
		}
		if roles, _ := s.PlatformRoles(ctx, target); len(roles) != 0 {
			t.Fatalf("revoked role in effect: %v", roles)
		}
		// Lists: open ones by default, everything with IncludeEnded; newest first; paging.
		ra2 := identity.RoleAssignment{ID: uuid.NewV7(), UserID: target, Role: authz.PlatformSupport, Reason: "support desk",
			Status: identity.RoleActive, RequestedBy: &a, RequestedAt: now, GrantedAt: &now}
		if err := s.InsertRoleAssignment(ctx, ra2); err != nil {
			t.Fatal(err)
		}
		open, _ := s.RoleAssignments(ctx, identity.RoleQuery{Limit: 10})
		all, _ := s.RoleAssignments(ctx, identity.RoleQuery{IncludeEnded: true, Limit: 10})
		fin, _ := s.RoleAssignments(ctx, identity.RoleQuery{Role: authz.PlatformFinanceAdmin, IncludeEnded: true, Limit: 10})
		page2, _ := s.RoleAssignments(ctx, identity.RoleQuery{IncludeEnded: true, After: &ra2.ID, Limit: 10})
		if len(open) != 1 || len(all) != 2 || all[0].ID != ra2.ID || len(fin) != 1 || len(page2) != 1 || page2[0].ID != ra.ID {
			t.Fatalf("lists: open %d all %d fin %d page2 %d", len(open), len(all), len(fin), len(page2))
		}
		// A self-assignment: …_self_chk → 403.
		self2 := identity.RoleAssignment{ID: uuid.NewV7(), UserID: a, Role: authz.PlatformSupport, Reason: "self service",
			Status: identity.RoleActive, RequestedBy: &a, RequestedAt: now, GrantedAt: &now}
		if err := s.InsertRoleAssignment(ctx, self2); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("self assignment = %v, want ErrForbidden", err)
		}
		if _, err := s.RoleAssignment(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("unknown assignment = %v", err)
		}
	})
	t.Run("mfa secrets and recovery codes", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		u := contractUser(uuid.NewV7().String() + "@contract.test")
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.TOTP(ctx, u.ID); found || err != nil {
			t.Fatalf("TOTP before enrollment: %v %v", found, err)
		}
		if used, _ := s.UseTOTPStep(ctx, u.ID, 10); used {
			t.Fatal("a step was used without a TOTP")
		}
		if err := s.PutTOTP(ctx, identity.TOTP{UserID: u.ID, SecretEnc: []byte("sealed-1"), CreatedAt: contractBase}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutTOTP(ctx, identity.TOTP{UserID: u.ID, SecretEnc: []byte("sealed-2"), CreatedAt: contractBase}); err != nil {
			t.Fatalf("re-enrollment: %v", err)
		}
		if used, _ := s.UseTOTPStep(ctx, u.ID, 10); used {
			t.Fatal("an unconfirmed TOTP accepted a step")
		}
		if ok, err := s.ConfirmTOTP(ctx, u.ID, 100, contractBase); !ok || err != nil {
			t.Fatalf("ConfirmTOTP = %v %v", ok, err)
		}
		if ok, _ := s.ConfirmTOTP(ctx, u.ID, 101, contractBase); ok {
			t.Fatal("confirmed twice")
		}
		got, found, _ := s.TOTP(ctx, u.ID)
		if !found || string(got.SecretEnc) != "sealed-2" || got.ConfirmedAt == nil || got.LastUsedStep == nil || *got.LastUsedStep != 100 {
			t.Fatalf("TOTP = %+v", got)
		}
		if err := s.PutTOTP(ctx, identity.TOTP{UserID: u.ID, SecretEnc: []byte("sealed-3"), CreatedAt: contractBase}); !errors.Is(err, identity.ErrMFAAlreadyEnabled) {
			t.Fatalf("enrollment over a confirmed TOTP = %v", err)
		}
		// Replay guard: a step ≤ the last used one is refused, a later one once.
		for _, c := range []struct {
			step int64
			want bool
		}{{100, false}, {99, false}, {101, true}, {101, false}, {103, true}, {102, false}} {
			if used, err := s.UseTOTPStep(ctx, u.ID, c.step); used != c.want || err != nil {
				t.Fatalf("UseTOTPStep(%d) = %v %v, want %v", c.step, used, err, c.want)
			}
		}
		hashes := [][]byte{token.Hash("code-1"), token.Hash("code-2")}
		if err := s.ReplaceRecoveryCodes(ctx, u.ID, hashes, contractBase); err != nil {
			t.Fatal(err)
		}
		if used, _ := s.UseRecoveryCode(ctx, u.ID, hashes[0], contractBase); !used {
			t.Fatal("recovery code not accepted")
		}
		if used, _ := s.UseRecoveryCode(ctx, u.ID, hashes[0], contractBase); used {
			t.Fatal("recovery code accepted twice")
		}
		if left, _ := s.RecoveryCodesLeft(ctx, u.ID); left != 1 {
			t.Fatalf("left %d", left)
		}
		if err := s.ReplaceRecoveryCodes(ctx, u.ID, [][]byte{token.Hash("code-3")}, contractBase); err != nil {
			t.Fatal(err)
		}
		if used, _ := s.UseRecoveryCode(ctx, u.ID, hashes[1], contractBase); used {
			t.Fatal("a replaced code still works")
		}
		if n, _ := s.DeleteRecoveryCodes(ctx, u.ID); n != 1 {
			t.Fatalf("DeleteRecoveryCodes = %d", n)
		}
		if n, _ := s.DeleteTOTP(ctx, u.ID); n != 1 {
			t.Fatalf("DeleteTOTP = %d", n)
		}
	})
	t.Run("four-eyes account actions (06 H-47)", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		var us [3]identity.User
		for i := range us {
			us[i] = contractUser(uuid.NewV7().String() + "@contract.test")
			if err := s.InsertUser(ctx, us[i]); err != nil {
				t.Fatal(err)
			}
		}
		a, b, target := us[0].ID, us[1].ID, us[2].ID
		r := identity.AccountActionRequest{ID: uuid.NewV7(), TargetUserID: target, Action: identity.AccountSuspend, Reason: "suspicious",
			Status: identity.AccountActionPending, RequestedBy: a, RequestedAt: contractBase, ExpiresAt: contractBase.Add(24 * time.Hour)}
		if err := s.InsertAccountAction(ctx, r); err != nil {
			t.Fatal(err)
		}
		dup := r
		dup.ID, dup.RequestedBy = uuid.NewV7(), b
		if err := s.InsertAccountAction(ctx, dup); !errors.Is(err, identity.ErrAccountActionOpen) {
			t.Fatalf("second pending request = %v", err)
		}
		other := dup
		other.ID, other.Action = uuid.NewV7(), identity.AccountResetMFA
		if err := s.InsertAccountAction(ctx, other); err != nil {
			t.Fatalf("another action: %v", err)
		}
		self := r
		self.ID, self.Action, self.RequestedBy = uuid.NewV7(), identity.AccountRevokeSessions, target
		if err := s.InsertAccountAction(ctx, self); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("self request = %v", err)
		}
		got, found, err := s.LockOpenAccountAction(ctx, target, identity.AccountSuspend)
		if err != nil || !found || got.ID != r.ID || got.RequestedBy != a {
			t.Fatalf("LockOpenAccountAction = %+v %v %v", got, found, err)
		}
		at := contractBase.Add(time.Hour)
		got.Status, got.ApprovedBy, got.ApprovedAt = identity.AccountActionApproved, &a, &at
		if err := s.UpdateAccountAction(ctx, got); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("self approval = %v", err)
		}
		got.ApprovedBy = &b
		if err := s.UpdateAccountAction(ctx, got); err != nil {
			t.Fatalf("approval: %v", err)
		}
		if _, found, _ := s.LockOpenAccountAction(ctx, target, identity.AccountSuspend); found {
			t.Fatal("an approved request is still open")
		}
		if err := s.InsertAccountAction(ctx, dup); err != nil {
			t.Fatalf("new request after the approval: %v", err)
		}
		all, _ := s.AccountActionsOf(ctx, target)
		if len(all) != 3 || all[0].Status != identity.AccountActionApproved || *all[0].ApprovedBy != b || all[0].ApprovedAt == nil {
			t.Fatalf("AccountActionsOf = %+v", all)
		}
	})
	t.Run("admin search, mfa flag, suspension, session mfa", func(t *testing.T) {
		t.Parallel()
		s, ctx := newWorld(t).s, t.Context()
		tag := uuid.NewV7().String()[:8]
		u := contractUser(tag + "-x@contract.test")
		u.FirstName, u.LastName = "Zeynep"+tag, "Öztürk"
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		v := contractUser(tag + "-y@contract.test")
		v.FirstName = "Zeynep" + tag
		if err := s.InsertUser(ctx, v); err != nil {
			t.Fatal(err)
		}
		byMail, _ := s.SearchUsers(ctx, identity.UserSearch{Email: u.EmailNormalized, Limit: 10})
		byName, _ := s.SearchUsers(ctx, identity.UserSearch{Name: "zeynep" + tag, Limit: 10})
		one, _ := s.SearchUsers(ctx, identity.UserSearch{Name: "zeynep" + tag, Limit: 1})
		next, _ := s.SearchUsers(ctx, identity.UserSearch{Name: "zeynep" + tag, After: &v.ID, Limit: 10})
		if len(byMail) != 1 || byMail[0].ID != u.ID || len(byName) != 2 || byName[0].ID != v.ID || len(one) != 1 || len(next) != 1 || next[0].ID != u.ID {
			t.Fatalf("search: mail %d name %d one %d next %d", len(byMail), len(byName), len(one), len(next))
		}
		if got, _ := s.SearchUsers(ctx, identity.UserSearch{Name: "zeynep" + tag + "%", Limit: 10}); len(got) != 0 {
			t.Fatal("LIKE wildcards are not escaped")
		}
		cur, _ := s.User(ctx, u.ID)
		at := contractBase.Add(time.Minute)
		cur.MFAEnabled, cur.Status, cur.SuspendedAt, cur.SuspensionReason = true, identity.StatusSuspended, &at, "spam"
		if err := s.UpdateUser(ctx, cur); err != nil {
			t.Fatal(err)
		}
		got, _ := s.User(ctx, u.ID)
		if !got.MFAEnabled || got.Status != identity.StatusSuspended || got.SuspendedAt == nil || got.SuspensionReason != "spam" {
			t.Fatalf("UpdateUser lost columns: %+v", got)
		}
		if sus, _ := s.SearchUsers(ctx, identity.UserSearch{Name: "zeynep" + tag, Status: identity.StatusSuspended, Limit: 10}); len(sus) != 1 {
			t.Fatalf("status filter: %d", len(sus))
		}
		sess := contractSession(v.ID, contractBase)
		if err := s.InsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.MarkSessionMFA(ctx, u.ID, sess.ID, at); ok {
			t.Fatal("marked another user's session")
		}
		if ok, _ := s.MarkSessionMFA(ctx, v.ID, sess.ID, at); !ok {
			t.Fatal("MarkSessionMFA missed")
		}
		if x, _ := s.Session(ctx, sess.ID); x.MFAVerifiedAt == nil || !x.MFAVerifiedAt.Equal(at) {
			t.Fatalf("mfa_verified_at %v", x.MFAVerifiedAt)
		}
	})
}

func ids(ss []identity.Session) []uuid.UUID {
	out := make([]uuid.UUID, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) storeWorld { return storeWorld{s: identity.NewMemStore()} })
}
