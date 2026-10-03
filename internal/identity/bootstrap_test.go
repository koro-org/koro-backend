package identity_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

func newBootstrapper(t *testing.T, e *env) *identity.Bootstrapper {
	t.Helper()
	b, err := identity.NewBootstrapper(identity.BootstrapDeps{Store: e.store, Tx: db.NopTx{}, Audit: e.audit, Clock: e.clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBootstrapAdmin_Idempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	b := newBootstrapper(t, e)
	existing := e.activeUser(t, "ada@koro.test")
	res, _ := e.login(t, "ada@koro.test", identity.ClientWeb)
	admins := []identity.AdminSeed{
		{Email: "Ada@koro.test", FirstName: "Ada", LastName: "Yılmaz"},
		{Email: "can@koro.test", FirstName: "Can", LastName: "Demir"},
	}
	out, err := b.Run(ctx, admins)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].UserID != existing.ID || out[0].UserCreated || !out[1].UserCreated || !out[0].RoleGranted || !out[1].RoleGranted {
		t.Fatalf("first run = %+v", out)
	}
	holders, _ := e.store.UsersWithRole(ctx, authz.PlatformAdmin)
	if len(holders) != 2 {
		t.Fatalf("platform admins %v", holders)
	}
	as, _ := e.store.RoleAssignmentsOf(ctx, out[1].UserID)
	if len(as) != 1 || as[0].RequestedBy != nil || as[0].Reason != "bootstrap" || as[0].Status != identity.RoleActive || as[0].GrantedAt == nil {
		t.Fatalf("bootstrap row = %+v", as)
	}
	// 03 §2.2-6: the role change closed the existing admin's sessions.
	if s, _ := e.store.Session(ctx, res.Session.ID); s.RevokeReason != identity.RevokeRoleChange {
		t.Fatalf("session after role grant: %q", s.RevokeReason)
	}
	grants := len(e.audit.of(string(authz.IdentityPlatformRoleGrant)))
	if grants != 2 {
		t.Fatalf("%d audited grants", grants)
	}
	// A new user has no password (set through the reset flow) and is active.
	created, _ := e.store.User(ctx, out[1].UserID)
	if created.PasswordHash != "" || created.Status != identity.StatusActive || created.Persona != identity.PersonaPlatform {
		t.Fatalf("created admin = %+v", created)
	}

	// Second run: nothing changes.
	again, err := b.Run(ctx, admins)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].RoleGranted || again[1].RoleGranted || again[1].UserCreated || again[1].UserID != out[1].UserID {
		t.Fatalf("second run = %+v", again)
	}
	if n := len(e.audit.of(string(authz.IdentityPlatformRoleGrant))); n != grants {
		t.Fatalf("second run audited %d more grants", n-grants)
	}
	if as, _ := e.store.RoleAssignmentsOf(ctx, out[1].UserID); len(as) != 1 {
		t.Fatalf("second run added rows: %d", len(as))
	}

	// Other admins exist: the bootstrap refuses (the four-eyes flow applies).
	_, err = b.Run(ctx, []identity.AdminSeed{admins[0], {Email: "eve@koro.test", FirstName: "Eve", LastName: "Kara"}})
	wantErr(t, err, identity.ErrBootstrap)
	if _, found, _ := e.store.UserByEmail(ctx, "eve@koro.test"); found {
		t.Fatal("a refused bootstrap left an account behind (in production the transaction rolls back)")
	}
	// Exactly two distinct admins.
	wantErr(t, mustErr(b.Run(ctx, admins[:1])), identity.ErrBootstrap)
	wantErr(t, mustErr(b.Run(ctx, []identity.AdminSeed{admins[0], admins[0]})), identity.ErrBootstrap)
}

func mustErr(_ []identity.BootstrapOutcome, err error) error { return err }
