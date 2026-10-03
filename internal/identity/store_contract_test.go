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
		if err := s.InsertRoleAssignment(ctx, dup); !errors.Is(err, identity.ErrBootstrap) {
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
