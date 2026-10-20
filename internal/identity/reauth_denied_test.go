package identity_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem"
)

// BE-075: identity's own R operations follow 03 §6.2 like every other
// module's (audit.ReauthFailed, BE-074). Only a verdict on the proof is
// audited: a wrong or a missing proof, and 429 once the account is locked
// or the reauth class is exhausted, is one denied reauth_failed record of
// the operation by its actor; an overloaded server (the hasher or the TOTP
// read past the request budget) writes nothing and counts nothing.
// Before BE-075 identity wrote the denied record of a wrong proof itself,
// inside the lockout counter: without reason and actor, and none at all
// for a missing proof or a 429.

// lateTOTPStore is the identity store on an overloaded database for the
// TOTP read of a re-authentication: while late is set it answers once the
// request context has ended, with the context error wrapped like a store
// error.
type lateTOTPStore struct {
	identity.Store
	late atomic.Bool
}

func (s *lateTOTPStore) TOTP(ctx context.Context, userID uuid.UUID) (identity.TOTP, bool, error) {
	if s.late.Load() {
		<-ctx.Done()
		return identity.TOTP{}, false, fmt.Errorf("identity: totp: %w", ctx.Err())
	}
	return s.Store.TOTP(ctx, userID)
}

// rows returns every accepted audit record (all actions) as audittest rows.
func (a *auditRec) rows() []audittest.Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]audittest.Row, len(a.events))
	for i, e := range a.events {
		out[i] = audittest.Row{Action: e.Action, Outcome: string(e.Outcome), Reason: e.Reason, ActorUserID: e.ActorUserID,
			ActorOrgID: e.ActorOrgID, SystemJob: e.SystemJob, OrgID: e.OrgID, ResourceType: e.ResourceType,
			ResourceID: e.ResourceID, Metadata: e.Metadata}
	}
	return out
}

// reauthEnv is an identity module whose hasher and TOTP read can be
// overloaded.
type reauthEnv struct {
	*env
	h    *lateHasher
	totp *lateTOTPStore
}

func newReauthEnv(t *testing.T) *reauthEnv {
	t.Helper()
	h, lt := &lateHasher{inner: sharedHasher()}, &lateTOTPStore{}
	e := newEnv(t, func(d *identity.Deps) {
		d.Hasher = h
		lt.Store, d.Store = d.Store, lt
	})
	return &reauthEnv{env: e, h: h, totp: lt}
}

// failures is the lockout counter of userID.
func (e *reauthEnv) failures(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	u, err := e.store.User(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.FailedLogins
}

// wrongCode is a six-digit code that no step of the accepted window shows
// for secret at the env's time.
func (e *reauthEnv) wrongCode(t *testing.T, secret string) string {
	t.Helper()
	var window []string
	for d := -2; d <= 2; d++ {
		window = append(window, totpCode(t, secret, e.clk.Now().Add(time.Duration(d)*30*time.Second)))
	}
	for i := range 10 {
		c := fmt.Sprintf("%06d", i*111111)
		if !slices.Contains(window, c) {
			return c
		}
	}
	t.Fatal("no wrong code")
	return ""
}

// rOperation is one of identity's R operations under test.
type rOperation struct {
	call   func(ctx context.Context, p identity.ReauthProof) error
	user   uuid.UUID // whose proof it is (the actor's account)
	wrong  func() identity.ReauthProof
	good   func() identity.ReauthProof
	late   func(on bool) // overloads the proof check (hasher or TOTP read)
	denied audittest.Row // the record of a refused proof
	kind   string        // metadata.reauth of the success record
}

// password is an R operation of a password account: wrong and good
// passwords, the hasher overloaded.
func (e *reauthEnv) password(op rOperation) rOperation {
	op.wrong = func() identity.ReauthProof { return identity.ReauthProof{Password: "Yanlis-Parola-Deneme-1"} }
	op.good = func() identity.ReauthProof { return pwProof }
	op.late = func(on bool) {
		e.h.late = nil
		if on {
			e.h.late = map[string]bool{"verify": true}
		}
	}
	op.kind = "password"
	return op
}

// totpProof is an R operation of an MFA account: wrong and good codes of
// secret (with the password too when withPassword), the TOTP read
// overloaded.
func (e *reauthEnv) totpProof(t *testing.T, op rOperation, secret string, withPassword bool) rOperation {
	pw := ""
	if withPassword {
		pw = goodPassword
	}
	op.wrong = func() identity.ReauthProof {
		return identity.ReauthProof{Password: pw, TOTPCode: e.wrongCode(t, secret)}
	}
	op.good = func() identity.ReauthProof {
		return identity.ReauthProof{Password: pw, TOTPCode: totpCode(t, secret, e.clk.Now())}
	}
	op.late = e.totp.late.Store
	op.kind = "totp"
	return op
}

// run checks op (03 §6.2, BE-075):
//
//   - an overload is no verdict: the bare request deadline after the whole
//     budget (httpx: 503), no audit record of any action, nothing counted;
//   - a wrong and a missing proof are 403 identity.reauth_failed /
//     reauth_required and exactly one record each: the denied
//     reauth_failed record of the operation by its actor (no second record
//     of any action);
//   - wrong proofs until the account is locked or the reauth class is
//     exhausted: every refusal, the 429 included, is that one record;
//   - once the lock is over, the right proof is one success record naming
//     the proof's kind.
func (op rOperation) run(t *testing.T, e *reauthEnv) {
	t.Helper()
	n := len(e.audit.rows())
	op.late(true)
	audittest.Overloaded(t, func(ctx context.Context) error { return op.call(ctx, op.good()) })
	op.late(false)
	if rows := e.audit.rows()[n:]; len(rows) != 0 {
		t.Fatalf("%s: %d audit records on an overload; it is no verdict on the proof: %+v", op.denied.Action, len(rows), rows)
	}
	if f := e.failures(t, op.user); f != 0 {
		t.Fatalf("%s: the overload counted as a wrong proof: failed_logins=%d", op.denied.Action, f)
	}

	for _, refused := range []struct {
		name  string
		proof identity.ReauthProof
		want  error
	}{{"wrong proof", op.wrong(), identity.ErrReauthFailed}, {"missing proof", identity.ReauthProof{}, identity.ErrReauthRequired}} {
		n := len(e.audit.rows())
		err := op.call(t.Context(), refused.proof)
		if !errors.Is(err, refused.want) {
			t.Fatalf("%s %s: err %v, want %v", op.denied.Action, refused.name, err, refused.want)
		}
		wantStatus(t, err, http.StatusForbidden)
		audittest.WantDenied(t, e.audit.rows()[n:], op.denied)
	}

	for i := 0; ; i++ {
		if i == 10 {
			t.Fatalf("%s: no 429 after %d wrong proofs", op.denied.Action, i)
		}
		n := len(e.audit.rows())
		err := op.call(t.Context(), op.wrong())
		audittest.WantDenied(t, e.audit.rows()[n:], op.denied)
		if errors.Is(err, problem.ErrRateLimited) {
			wantStatus(t, err, http.StatusTooManyRequests)
			break
		}
		if !errors.Is(err, identity.ErrReauthFailed) {
			t.Fatalf("%s wrong proof %d: err %v", op.denied.Action, i+2, err)
		}
	}

	e.clk.Advance(16 * time.Minute) // the lock (1 min) and the reauth class window (15 min) are over
	n = len(e.audit.rows())
	if err := op.call(t.Context(), op.good()); err != nil {
		t.Fatalf("%s with the right proof after the refusals: %v", op.denied.Action, err)
	}
	rows := e.audit.rows()[n:]
	if len(rows) != 1 || rows[0].Action != op.denied.Action || rows[0].Outcome != audittest.Success ||
		rows[0].Metadata[audittest.MetaReauth] != op.kind {
		t.Fatalf("%s success records = %+v, want one naming the %s proof", op.denied.Action, rows, op.kind)
	}
}

// deniedRow is the denied record of act by actor on a resource.
func deniedRow(act authz.Action, actor uuid.UUID, resType string, resID *uuid.UUID, meta map[string]any) audittest.Row {
	return audittest.Row{Action: string(act), ActorUserID: &actor, ResourceType: resType, ResourceID: resID, Metadata: meta}
}

func TestReauthDenied_EveryIdentityROperation(t *testing.T) {
	t.Parallel()
	goodPasswordHash() // the shared hasher outside the bubbles
	const reason = "lost the authenticator phone"
	cases := []struct {
		name  string
		setup func(t *testing.T, e *reauthEnv) rOperation
	}{
		{"changeMyPassword", func(t *testing.T, e *reauthEnv) rOperation {
			u := e.activeUser(t, "pw@example.com")
			_, a := e.login(t, "pw@example.com", identity.ClientMobile)
			return e.password(rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					return e.svc.ChangePassword(ctx, a, identity.PasswordChange{CurrentPassword: p.Password, TOTPCode: p.TOTPCode, NewPassword: newPassword})
				},
				denied: deniedRow(authz.IdentityPasswordChange, u.ID, "user", &u.ID, nil)})
		}},
		{"changeMyPassword MFA account", func(t *testing.T, e *reauthEnv) rOperation {
			u, a, secret, _ := e.mfaUser(t, "pwmfa@example.com")
			return e.totpProof(t, rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					return e.svc.ChangePassword(ctx, a, identity.PasswordChange{CurrentPassword: p.Password, TOTPCode: p.TOTPCode, NewPassword: newPassword})
				},
				denied: deniedRow(authz.IdentityPasswordChange, u.ID, "user", &u.ID, nil)}, secret, true)
		}},
		{"requestEmailChange", func(t *testing.T, e *reauthEnv) rOperation {
			u := e.activeUser(t, "mail@example.com")
			_, a := e.login(t, "mail@example.com", identity.ClientMobile)
			return e.password(rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					return e.svc.RequestEmailChange(ctx, a, "new-mail@example.com", p)
				},
				denied: deniedRow(authz.IdentityEmailChangeRequest, u.ID, "user", &u.ID, nil)})
		}},
		{"enrollTotp", func(t *testing.T, e *reauthEnv) rOperation {
			u := e.activeUser(t, "enroll@example.com")
			_, a := e.login(t, "enroll@example.com", identity.ClientMobile)
			return e.password(rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.EnrollTOTP(ctx, a, p)
					return err
				},
				denied: deniedRow(authz.IdentityMfaEnroll, u.ID, "user", &u.ID, nil)})
		}},
		{"confirmTotp", func(t *testing.T, e *reauthEnv) rOperation {
			u := e.activeUser(t, "confirm@example.com")
			_, a := e.login(t, "confirm@example.com", identity.ClientMobile)
			enr, err := e.svc.EnrollTOTP(t.Context(), a, pwProof)
			if err != nil {
				t.Fatal(err)
			}
			return e.password(rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.ConfirmTOTP(ctx, a, totpCode(t, enr.Secret, e.clk.Now()), p)
					return err
				},
				denied: deniedRow(authz.IdentityMfaConfirm, u.ID, "user", &u.ID, nil)})
		}},
		{"disableTotp", func(t *testing.T, e *reauthEnv) rOperation {
			u, a, secret, _ := e.mfaUser(t, "disable@example.com")
			return e.totpProof(t, rOperation{user: u.ID,
				call:   func(ctx context.Context, p identity.ReauthProof) error { return e.svc.DisableTOTP(ctx, a, p) },
				denied: deniedRow(authz.IdentityMfaDisable, u.ID, "user", &u.ID, nil)}, secret, false)
		}},
		{"regenerateRecoveryCodes", func(t *testing.T, e *reauthEnv) rOperation {
			u, a, secret, _ := e.mfaUser(t, "codes@example.com")
			return e.totpProof(t, rOperation{user: u.ID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.RegenerateRecoveryCodes(ctx, a, p)
					return err
				},
				denied: deniedRow(authz.IdentityMfaRecoveryRegenerate, u.ID, "user", &u.ID, nil)}, secret, false)
		}},
		{"adminResetUserMfa", func(t *testing.T, e *reauthEnv) rOperation {
			s := &staffEnv{env: e.env}
			admin := s.admin(t, "admin-a@koro.test")
			target := e.activeUser(t, "target@example.com")
			return e.password(rOperation{user: admin.UserID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.ResetUserMFA(ctx, admin, target.ID, reason, p)
					return err
				},
				denied: deniedRow(authz.IdentityUserResetMfa, admin.UserID, "user", &target.ID, nil)})
		}},
		{"adminGrantPlatformRole", func(t *testing.T, e *reauthEnv) rOperation {
			s := &staffEnv{env: e.env}
			admin := s.admin(t, "admin-a@koro.test")
			target := e.activeUser(t, "target@example.com")
			return e.password(rOperation{user: admin.UserID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.GrantPlatformRole(ctx, admin, identity.RoleGrant{UserID: target.ID, Role: authz.PlatformModerator,
						Reason: "needs the role for the pilot", Proof: p})
					return err
				},
				denied: deniedRow(authz.IdentityPlatformRoleGrant, admin.UserID, "platform_role_assignment", nil, map[string]any{"step": "request"})})
		}},
		{"adminApprovePlatformRole", func(t *testing.T, e *reauthEnv) rOperation {
			s := &staffEnv{env: e.env}
			a, b := s.admin(t, "admin-a@koro.test"), s.admin(t, "admin-b@koro.test")
			target := e.activeUser(t, "target@example.com")
			ra, err := s.grant(t, a, target.ID, authz.PlatformFinanceAdmin)
			if err != nil || ra.Status != identity.RolePending {
				t.Fatalf("request: %+v %v", ra, err)
			}
			return e.password(rOperation{user: b.UserID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.ApprovePlatformRole(ctx, b, ra.ID, p)
					return err
				},
				denied: deniedRow(authz.IdentityPlatformRoleGrant, b.UserID, "platform_role_assignment", &ra.ID, map[string]any{"step": "approve"})})
		}},
		{"adminRevokePlatformRole", func(t *testing.T, e *reauthEnv) rOperation {
			s := &staffEnv{env: e.env}
			admin := s.admin(t, "admin-a@koro.test")
			target := e.activeUser(t, "target@example.com")
			ra, err := s.grant(t, admin, target.ID, authz.PlatformModerator)
			if err != nil || ra.Status != identity.RoleActive {
				t.Fatalf("grant: %+v %v", ra, err)
			}
			return e.password(rOperation{user: admin.UserID,
				call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.RevokePlatformRole(ctx, admin, ra.ID, "no longer needed for the pilot", p)
					return err
				},
				denied: deniedRow(authz.IdentityPlatformRoleRevoke, admin.UserID, "platform_role_assignment", &ra.ID, nil)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newReauthEnv(t)
			tc.setup(t, e).run(t, e)
		})
	}
}

// TestReauthVerify_PortAuditsNothing_CallerWritesTheOneRecord: identity's
// Verify, the port every other module checks its proof with, writes no
// record of any action for a wrong or a missing proof or a 429, on a
// password and on an MFA account (failures still count); the denied record
// is the calling module's (audit.ReauthFailed), so a refused proof of
// another module's R operation is exactly one record (BE-074, BE-075; over
// HTTP with payments and orgs: TestWire_IdentityReauthDenied_OneRecordPerRefusal).
func TestReauthVerify_PortAuditsNothing_CallerWritesTheOneRecord(t *testing.T) {
	t.Parallel()
	e := newReauthEnv(t)
	pw := e.activeUser(t, "port@example.com")
	mfa, _, secret, _ := e.mfaUser(t, "portmfa@example.com")
	for _, acct := range []struct {
		name         string
		id           uuid.UUID
		wrong, right func() identity.ReauthProof
	}{
		{"password account", pw.ID,
			func() identity.ReauthProof { return identity.ReauthProof{Password: "Yanlis-Parola-Deneme-1"} },
			func() identity.ReauthProof { return pwProof }},
		{"MFA account", mfa.ID,
			func() identity.ReauthProof { return identity.ReauthProof{TOTPCode: e.wrongCode(t, secret)} },
			func() identity.ReauthProof { return identity.ReauthProof{TOTPCode: totpCode(t, secret, e.clk.Now())} }},
	} {
		// A module's R operation over the port (payments, orgs …: reauth then
		// audit.ReauthFailed with its own record).
		denied := audit.Event{Action: string(authz.PaymentsCollectionAccountCreate), ResourceType: authz.TypeCollectionAccount}.
			By(authz.Actor{Kind: authz.KindUser, UserID: acct.id})
		module := func(p identity.ReauthProof) error {
			_, err := e.svc.Verify(t.Context(), acct.id, p)
			if err != nil {
				return audit.ReauthFailed(t.Context(), e.audit, nil, denied, err)
			}
			return nil
		}
		want := audittest.Row{Action: denied.Action, ActorUserID: &acct.id, ResourceType: denied.ResourceType}
		for i, step := range []struct {
			proof identity.ReauthProof
			want  error
		}{
			{acct.wrong(), identity.ErrReauthFailed}, {identity.ReauthProof{}, identity.ErrReauthRequired},
			{acct.wrong(), identity.ErrReauthFailed}, {acct.wrong(), identity.ErrReauthFailed},
			{acct.wrong(), identity.ErrReauthFailed}, {acct.wrong(), identity.ErrReauthFailed}, // the fifth failure locks
			{acct.right(), problem.ErrRateLimited},
		} {
			n := len(e.audit.rows())
			_, err := e.svc.Verify(t.Context(), acct.id, step.proof)
			if !errors.Is(err, step.want) {
				t.Fatalf("%s Verify %d: err %v, want %v", acct.name, i+1, err, step.want)
			}
			if rows := e.audit.rows()[n:]; len(rows) != 0 {
				t.Fatalf("%s Verify %d wrote %d records; the calling module audits its own action: %+v", acct.name, i+1, len(rows), rows)
			}
		}
		if f := e.failures(t, acct.id); f != 5 {
			t.Fatalf("%s: failed_logins=%d after five wrong proofs, want 5", acct.name, f)
		}
		n := len(e.audit.rows())
		if err := module(acct.right()); !errors.Is(err, problem.ErrRateLimited) {
			t.Fatalf("%s: the module's R operation on a locked account: %v", acct.name, err)
		}
		audittest.WantDenied(t, e.audit.rows()[n:], want)
		e.clk.Advance(16 * time.Minute)
		if err := module(acct.right()); err != nil {
			t.Fatalf("%s: the right proof after the lock: %v", acct.name, err)
		}
		n = len(e.audit.rows())
		if err := module(acct.wrong()); !errors.Is(err, identity.ErrReauthFailed) {
			t.Fatalf("%s: the module's R operation with a wrong proof: %v", acct.name, err)
		}
		audittest.WantDenied(t, e.audit.rows()[n:], want)
	}
}
