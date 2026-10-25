package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/token"
)

// BE-070: on an overloaded server the request budget (httpx, 05 §10.6)
// can run out before the password hasher answers: platform/passwd then
// returns the context error of its semaphore (05 §14.1). That is neither
// a server bug (500) nor a wrong password (lockout): every password path
// writes nothing, counts nothing and returns the context error unchanged,
// which httpx answers with 503 unavailable (T-315, BE-071: one conversion
// point for every request that runs out of time; see
// TestWire_ChangePasswordPastRequestBudget_503).

// lateHasher is the real argon2id hasher on an overloaded server. An
// operation named in late reaches it only once the request context has
// ended (every computation slot stayed taken until the deadline, or a
// CPU-starved request got there too late), so the error the service sees
// is the one platform/passwd itself returns. Use it inside a synctest
// bubble: the wait for the deadline costs no wall-clock time.
type lateHasher struct {
	inner identity.PasswordHasher
	late  map[string]bool // "hash", "verify", "dummy"
}

func (h *lateHasher) wait(ctx context.Context, op string) {
	if h.late[op] {
		<-ctx.Done()
	}
}

func (h *lateHasher) Hash(ctx context.Context, pw string) (string, error) {
	h.wait(ctx, "hash")
	return h.inner.Hash(ctx, pw)
}

func (h *lateHasher) Verify(ctx context.Context, pw, encoded string) (bool, bool, error) {
	h.wait(ctx, "verify")
	return h.inner.Verify(ctx, pw, encoded)
}

func (h *lateHasher) VerifyDummy(ctx context.Context, pw string) error {
	h.wait(ctx, "dummy")
	return h.inner.VerifyDummy(ctx, pw)
}

// goodPasswordHash is the stored hash of goodPassword, computed once
// outside any bubble (the shared hasher's semaphore must not belong to
// one).
var goodPasswordHash = sync.OnceValue(func() string {
	h, err := sharedHasher().Hash(context.Background(), goodPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// overload is an identity module with a lateHasher, one active account
// (password goodPassword, no MFA) and two of its sessions.
type overload struct {
	*env
	h      *lateHasher
	u      identity.User
	a      authz.Actor // the mobile session
	webSID uuid.UUID   // the other session
}

func newOverload(t *testing.T) *overload {
	t.Helper()
	h := &lateHasher{inner: sharedHasher()}
	e := newEnv(t, func(d *identity.Deps) { d.Hasher = h })
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	u := identity.User{
		ID: id, Status: identity.StatusActive, Email: id.String() + "@overload.koro.test", EmailNormalized: id.String() + "@overload.koro.test",
		EmailVerifiedAt: &now, PasswordHash: goodPasswordHash(), FirstName: "Ada", LastName: "Yılmaz",
		Persona: identity.PersonaCompany, Locale: identity.LocaleTR,
	}
	if err := e.store.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	o := &overload{env: e, h: h, u: u}
	for _, kind := range []identity.ClientKind{identity.ClientMobile, identity.ClientWeb} {
		_, hash, err := token.New()
		if err != nil {
			t.Fatal(err)
		}
		s := identity.Session{ID: uuid.NewV7(), UserID: u.ID, TokenHash: hash, ClientKind: kind,
			CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(2 * time.Hour)}
		if err := e.store.InsertSession(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		if kind == identity.ClientMobile {
			o.a = authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: s.ID, StudentStatus: authz.StudentNone}
		} else {
			o.webSID = s.ID
		}
	}
	return o
}

// request is a request context with the production budget
// (httpx.DefaultRequestTimeout, as in the authz matrix).
func request(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), httpx.DefaultRequestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// unchanged asserts that the overloaded request wrote nothing: same
// password hash, no failed attempt or lock, the other session alive, no
// audit event of action, no queued security notice.
func (o *overload) unchanged(t *testing.T, action authz.Action) {
	t.Helper()
	cur, err := o.store.User(t.Context(), o.u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.PasswordHash != o.u.PasswordHash {
		t.Fatal("the password changed although the request failed")
	}
	if cur.FailedLogins != 0 || cur.LockedUntil != nil {
		t.Fatalf("overload counted as a wrong password: failed_logins=%d locked_until=%v", cur.FailedLogins, cur.LockedUntil)
	}
	if s, _ := o.store.Session(t.Context(), o.webSID); s.RevokedAt != nil {
		t.Fatal("the other session was closed although the password did not change")
	}
	if evs := o.audit.of(string(action)); len(evs) != 0 {
		t.Fatalf("%s audited %d times (%v): an overload is no verdict", action, len(evs), evs[0].Outcome)
	}
	if n := len(o.q.Tasks()); n != 0 {
		t.Fatalf("%d tasks queued (security notice) although nothing changed", n)
	}
}

const newPassword = "Yeni-Ve-Uzun-Parola-4821"

// wantOverload asserts the error of an overloaded request: the request
// deadline, not wrapped in any problem (a *problem.Error would take
// precedence in httpx; T-315), so that httpx answers 503 unavailable.
func wantOverload(t *testing.T, err error) {
	t.Helper()
	problemtest.WantErr(t, err, context.DeadlineExceeded)
	if pe, ok := errors.AsType[*problem.Error](err); ok {
		t.Fatalf("overload became problem %q; httpx maps the bare deadline to 503", pe.Code())
	}
}

// TestChangePassword_PastRequestDeadline_503NoPartialWrite reproduces the
// BE-064 matrix failure (changeMyPassword 500 internal_error after 18–37 s
// on an overloaded -race run): the current password is verified, but the
// request budget is gone before the new hash can start. Before BE-070 the
// raw context.DeadlineExceeded of platform/passwd became 500; now nothing
// is written, the deadline reaches httpx (503) and a retry with a fresh
// budget succeeds.
func TestChangePassword_PastRequestDeadline_503NoPartialWrite(t *testing.T) {
	t.Parallel()
	goodPasswordHash() // outside the bubble
	synctest.Test(t, func(t *testing.T) {
		o := newOverload(t)
		o.h.late = map[string]bool{"hash": true}
		start := time.Now()
		err := o.svc.ChangePassword(request(t), o.a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: newPassword})
		wantOverload(t, err)
		if waited := time.Since(start); waited != httpx.DefaultRequestTimeout {
			t.Fatalf("the request ended after %v, want the whole budget %v", waited, httpx.DefaultRequestTimeout)
		}
		o.unchanged(t, authz.IdentityPasswordChange)

		// The same request with a fresh budget goes through.
		o.h.late = nil
		if err := o.svc.ChangePassword(request(t), o.a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: newPassword}); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if s, _ := o.store.Session(t.Context(), o.webSID); s.RevokeReason != identity.RevokePasswordChange {
			t.Fatalf("retry: other session %+v", s.RevokeReason)
		}
		if evs := o.audit.of(string(authz.IdentityPasswordChange)); len(evs) != 1 || evs[0].Outcome != audit.Success {
			t.Fatalf("retry: audit %+v", evs)
		}
	})
}

// TestPasswordPaths_PastRequestDeadline_503NoLockout covers every argon2id
// call of identity (BE-070 §4): an overloaded hasher is 503 on each path
// (through httpx, BE-071), and a reauth/login verification that did not
// run is never counted as a
// wrong password (before BE-070 it was: 403/401, failed_logins+1 and a
// denied audit — five overloaded logins locked the account).
func TestPasswordPaths_PastRequestDeadline_503NoLockout(t *testing.T) {
	t.Parallel()
	goodPasswordHash() // outside the bubbles
	cases := []struct {
		name   string
		late   string
		action authz.Action
		call   func(t *testing.T, o *overload) error
		after  func(t *testing.T, o *overload)
	}{
		{name: "changeMyPassword reauth", late: "verify", action: authz.IdentityPasswordChange,
			call: func(t *testing.T, o *overload) error {
				return o.svc.ChangePassword(request(t), o.a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: newPassword})
			}},
		{name: "reauth hook of other modules", late: "verify", action: authz.IdentityPasswordChange,
			call: func(t *testing.T, o *overload) error {
				_, err := o.svc.Verify(request(t), o.u.ID, identity.ReauthProof{Password: goodPassword})
				return err
			}},
		{name: "requestEmailChange reauth", late: "verify", action: authz.IdentityEmailChangeRequest,
			call: func(t *testing.T, o *overload) error {
				return o.svc.RequestEmailChange(request(t), o.a, "new-"+o.u.Email, identity.ReauthProof{Password: goodPassword})
			}},
		{name: "createSession", late: "verify", action: authz.IdentitySessionCreate,
			call: func(t *testing.T, o *overload) error {
				_, err := o.svc.CreateSession(request(t), authz.Anonymous(), identity.Login{Email: o.u.Email, Password: goodPassword})
				return err
			},
			after: func(t *testing.T, o *overload) {
				// Five overloaded logins in a row do not lock the account.
				for range 4 {
					_, err := o.svc.CreateSession(request(t), authz.Anonymous(), identity.Login{Email: o.u.Email, Password: goodPassword})
					wantOverload(t, err)
				}
				o.h.late = nil
				if _, err := o.svc.CreateSession(request(t), authz.Anonymous(), identity.Login{Email: o.u.Email, Password: goodPassword}); err != nil {
					t.Fatalf("login after the overload: %v", err)
				}
			}},
		{name: "createSession unknown e-mail", late: "dummy", action: authz.IdentitySessionCreate,
			call: func(t *testing.T, o *overload) error {
				_, err := o.svc.CreateSession(request(t), authz.Anonymous(), identity.Login{Email: "ghost@overload.koro.test", Password: goodPassword})
				return err
			}},
		{name: "register", late: "hash", action: authz.IdentityAccountRegister,
			call: func(t *testing.T, o *overload) error {
				return o.svc.Register(request(t), authz.Anonymous(), o.registration("fresh@overload.koro.test"))
			},
			after: func(t *testing.T, o *overload) {
				if _, found, err := o.svc.FindByEmail(t.Context(), "fresh@overload.koro.test"); err != nil || found {
					t.Fatalf("account created although the request failed: %v %v", found, err)
				}
			}},
		{name: "confirmPasswordReset", late: "hash", action: authz.IdentityPasswordReset,
			call: func(t *testing.T, o *overload) error {
				if err := o.svc.RequestPasswordReset(t.Context(), authz.Anonymous(), o.u.Email); err != nil {
					t.Fatal(err)
				}
				return o.svc.ConfirmPasswordReset(request(t), authz.Anonymous(), o.lastToken(t, "identity.password_reset"), newPassword)
			},
			after: func(t *testing.T, o *overload) {
				// The token was not consumed: the retry uses it.
				o.h.late = nil
				if err := o.svc.ConfirmPasswordReset(request(t), authz.Anonymous(), o.lastToken(t, "identity.password_reset"), newPassword); err != nil {
					t.Fatalf("retry with the same token: %v", err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				o := newOverload(t)
				o.h.late = map[string]bool{tc.late: true}
				err := tc.call(t, o)
				wantOverload(t, err)
				o.unchanged(t, tc.action)
				if tc.after != nil {
					tc.after(t, o)
				}
			})
		})
	}
}
