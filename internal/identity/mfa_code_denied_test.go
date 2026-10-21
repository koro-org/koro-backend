package identity_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/identity"
)

// BE-076: a refused second-factor code — the first code of confirmTotp,
// the step-up code of verifyMfa, the TOTP or recovery code of
// completeMfaLogin — is exactly one denied record of the operation with
// reason invalid_mfa_code, written in the lockout counter's transaction
// with the count (metadata.failed_attempts). The record of a signed-in
// user names that user (By): it is among the owner's security events
// (listMySecurityEvents), so a stolen session guessing codes shows there.
// The sign-in record has the request's actor — nobody is signed in, so
// anonymous — and the account as its resource; the answer stays the 401
// of an unknown challenge. An overload (the code read or the counter past
// the request budget) is no verdict: no record, nothing counted
// (BE-070/072). Before BE-076 each of these records was written without
// actor and reason.

// lateCodeStore is the identity store on an overloaded database for the
// steps of a code check: while one of its switches is on, that operation
// answers only once the request context has ended, with the context error
// wrapped like a store error.
type lateCodeStore struct {
	identity.Store
	totp, recovery, lock atomic.Bool
}

func waitLate(ctx context.Context, on *atomic.Bool) error {
	if !on.Load() {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *lateCodeStore) TOTP(ctx context.Context, userID uuid.UUID) (identity.TOTP, bool, error) {
	if err := waitLate(ctx, &s.totp); err != nil {
		return identity.TOTP{}, false, fmt.Errorf("identity: totp: %w", err)
	}
	return s.Store.TOTP(ctx, userID)
}

func (s *lateCodeStore) UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte, at time.Time) (bool, error) {
	if err := waitLate(ctx, &s.recovery); err != nil {
		return false, fmt.Errorf("identity: use recovery code: %w", err)
	}
	return s.Store.UseRecoveryCode(ctx, userID, hash, at)
}

func (s *lateCodeStore) LockUser(ctx context.Context, id uuid.UUID) (identity.User, error) {
	if err := waitLate(ctx, &s.lock); err != nil {
		return identity.User{}, fmt.Errorf("identity: lock user: %w", err)
	}
	return s.Store.LockUser(ctx, id)
}

// codeEnv is an identity module whose code checks can be overloaded.
type codeEnv struct {
	*env
	late *lateCodeStore
}

func newCodeEnv(t *testing.T) *codeEnv {
	t.Helper()
	ls := &lateCodeStore{}
	e := newEnv(t, func(d *identity.Deps) { ls.Store, d.Store = d.Store, ls })
	return &codeEnv{env: e, late: ls}
}

func (e *codeEnv) failures(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	return (&reauthEnv{env: e.env}).failures(t, userID)
}

func (e *codeEnv) wrongCode(t *testing.T, secret string) string {
	t.Helper()
	return (&reauthEnv{env: e.env}).wrongCode(t, secret)
}

// ownEvents returns the security events of userID's own account
// (listMySecurityEvents) over the module's audit store.
func (e *codeEnv) ownEvents(t *testing.T, userID uuid.UUID) []audit.SecurityEvent {
	t.Helper()
	svc, ok := e.audit.svc.(audit.Service)
	if !ok {
		t.Fatal("the env's audit logger is not the audit service")
	}
	p, err := svc.ListOwn(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: userID}, audit.OwnQuery{PageQuery: audit.PageQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

// countEvents counts the events of act with outcome among evs.
func countEvents(evs []audit.SecurityEvent, act authz.Action, outcome audit.Outcome) int {
	n := 0
	for _, ev := range evs {
		if ev.Action == string(act) && ev.Outcome == outcome {
			n++
		}
	}
	return n
}

// wantCodeDenied asserts that rows (everything one refused code wrote, of
// any action) are exactly one denied invalid_mfa_code record matching
// want — action, actor, resource, no org — whose metadata is want's plus
// failed_attempts = the lockout counter after the attempt.
func wantCodeDenied(t *testing.T, rows []audittest.Row, want audittest.Row, failures int) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s: %d audit records for a refused code, want exactly one denied record: %+v", want.Action, len(rows), rows)
	}
	r := rows[0]
	meta := map[string]any{"failed_attempts": failures}
	for k, v := range want.Metadata {
		meta[k] = v
	}
	sameID := func(a, b *uuid.UUID) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	switch {
	case r.Action != want.Action || r.Outcome != audittest.Denied || r.Reason != "invalid_mfa_code":
		t.Fatalf("record %s %s reason %q, want %s denied invalid_mfa_code", r.Action, r.Outcome, r.Reason, want.Action)
	case !sameID(r.ActorUserID, want.ActorUserID) || r.SystemJob != "":
		t.Fatalf("%s: actor %v, want %v", want.Action, r.ActorUserID, want.ActorUserID)
	case r.ResourceType != want.ResourceType || !sameID(r.ResourceID, want.ResourceID):
		t.Fatalf("%s: resource %s %v, want %s %v", want.Action, r.ResourceType, r.ResourceID, want.ResourceType, want.ResourceID)
	case r.OrgID != nil || r.ActorOrgID != nil:
		t.Fatalf("%s: org %v / actor org %v on an account record", want.Action, r.OrgID, r.ActorOrgID)
	case !reflect.DeepEqual(r.Metadata, meta):
		t.Fatalf("%s: metadata %v, want %v", want.Action, r.Metadata, meta)
	}
}

// codeOperation is one operation that checks a second-factor code.
type codeOperation struct {
	user uuid.UUID // whose code it is
	// attempt prepares one attempt with code (the password step of a
	// sign-in runs here, outside any overload) and returns the call.
	attempt     func(t *testing.T, code string) func(ctx context.Context) error
	wrong, good func(t *testing.T) string
	refused     func(t *testing.T, err error) // the answer to a wrong code
	late        []*atomic.Bool                // the overload points of the check
	denied      audittest.Row                 // the record of a refused code
	ownEvent    bool                          // the record is among the owner's security events
}

// run checks op (BE-076): every overload point is no verdict (the bare
// request deadline after the whole budget, no record of any action,
// nothing counted); three wrong codes are three refusals of one record
// each, carrying the stored count; the owner's security events list them
// when the user was signed in; the right code afterwards is one success
// record.
func (op codeOperation) run(t *testing.T, e *codeEnv) {
	t.Helper()
	for i, late := range op.late {
		n := len(e.audit.rows())
		call := op.attempt(t, op.wrong(t))
		late.Store(true)
		audittest.Overloaded(t, call)
		late.Store(false)
		if rows := e.audit.rows()[n:]; len(rows) != 0 {
			t.Fatalf("%s overload %d: %d audit records; it is no verdict on the code: %+v", op.denied.Action, i+1, len(rows), rows)
		}
		if f := e.failures(t, op.user); f != 0 {
			t.Fatalf("%s overload %d counted as a wrong code: failed_logins=%d", op.denied.Action, i+1, f)
		}
	}

	for i := range 3 {
		n := len(e.audit.rows())
		err := op.attempt(t, op.wrong(t))(t.Context())
		op.refused(t, err)
		wantCodeDenied(t, e.audit.rows()[n:], op.denied, e.failures(t, op.user))
		if f := e.failures(t, op.user); f == 0 {
			t.Fatalf("%s wrong code %d was not counted", op.denied.Action, i+1)
		}
	}
	want := 0
	if op.ownEvent {
		want = 3
	}
	if got := countEvents(e.ownEvents(t, op.user), authz.Action(op.denied.Action), audit.Denied); got != want {
		t.Fatalf("%s: %d denied records among the owner's security events, want %d", op.denied.Action, got, want)
	}

	e.clk.Advance(16 * time.Minute) // the reauth class window (15 min) is over
	n := len(e.audit.rows())
	if err := op.attempt(t, op.good(t))(t.Context()); err != nil {
		t.Fatalf("%s with the right code after the refusals: %v", op.denied.Action, err)
	}
	if rows := e.audit.rows()[n:]; len(rows) != 1 || rows[0].Action != op.denied.Action || rows[0].Outcome != audittest.Success {
		t.Fatalf("%s: records of the right code %+v, want one success record", op.denied.Action, rows)
	}
}

// refusedAs422 is the answer of confirmTotp and verifyMfa to a wrong code.
func refusedAs422(t *testing.T, err error) {
	t.Helper()
	wantErr(t, err, identity.ErrInvalidMFACode)
	wantStatus(t, err, http.StatusUnprocessableEntity)
}

// refusedLikeUnknownChallenge: a wrong sign-in code gets exactly the
// answer of an unknown challenge (the same sentinel, unwrapped: 401
// identity.mfa_login_failed), so the record changes nothing a caller sees.
func refusedLikeUnknownChallenge(e *codeEnv) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		_, unknown := e.svc.CompleteMFALogin(t.Context(), authz.Anonymous(), identity.MFALogin{ChallengeID: uuid.NewV4(), TOTPCode: "123456"})
		if err != identity.ErrMFALoginFailed || unknown != identity.ErrMFALoginFailed { //nolint:errorlint // the identical answer is the point
			t.Fatalf("wrong code answered %v, unknown challenge %v; want the same identity.mfa_login_failed", err, unknown)
		}
		wantStatus(t, err, http.StatusUnauthorized)
	}
}

func TestMFACodeDenied_ActorReasonAndOwnSecurityEvents(t *testing.T) {
	t.Parallel()
	goodPasswordHash() // the shared hasher outside the bubbles
	signIn := func(e *codeEnv, email string, code func(string) identity.MFALogin) func(t *testing.T, c string) func(ctx context.Context) error {
		return func(t *testing.T, c string) func(ctx context.Context) error {
			ch := e.passwordStep(t, email, identity.ClientMobile)
			return func(ctx context.Context) error {
				in := code(c)
				in.ChallengeID = ch.ID
				_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), in)
				return err
			}
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, e *codeEnv) codeOperation
	}{
		{"confirmTotp", func(t *testing.T, e *codeEnv) codeOperation {
			u := e.activeUser(t, "confirm@example.com")
			_, a := e.login(t, "confirm@example.com", identity.ClientMobile)
			enr, err := e.svc.EnrollTOTP(t.Context(), a, pwProof)
			if err != nil {
				t.Fatal(err)
			}
			return codeOperation{user: u.ID,
				attempt: func(_ *testing.T, c string) func(ctx context.Context) error {
					return func(ctx context.Context) error {
						_, err := e.svc.ConfirmTOTP(ctx, a, c, pwProof)
						return err
					}
				},
				wrong:   func(t *testing.T) string { return e.wrongCode(t, enr.Secret) },
				good:    func(t *testing.T) string { return totpCode(t, enr.Secret, e.clk.Now()) },
				refused: refusedAs422, late: []*atomic.Bool{&e.late.totp, &e.late.lock},
				denied: audittest.Row{Action: string(authz.IdentityMfaConfirm), ActorUserID: &u.ID, ResourceType: "user",
					ResourceID: &u.ID},
				ownEvent: true}
		}},
		{"verifyMfa", func(t *testing.T, e *codeEnv) codeOperation {
			u, a, secret, _ := e.mfaUser(t, "verify@example.com")
			return codeOperation{user: u.ID,
				attempt: func(_ *testing.T, c string) func(ctx context.Context) error {
					return func(ctx context.Context) error { return e.svc.VerifyMFA(ctx, a, c) }
				},
				wrong:   func(t *testing.T) string { return e.wrongCode(t, secret) },
				good:    func(t *testing.T) string { return totpCode(t, secret, e.clk.Now()) },
				refused: refusedAs422, late: []*atomic.Bool{&e.late.totp, &e.late.lock},
				// The success record names the session it steps up; so does the refusal.
				denied: audittest.Row{Action: string(authz.IdentityMfaVerify), ActorUserID: &u.ID, ResourceType: "session",
					ResourceID: &a.SessionID},
				ownEvent: true}
		}},
		{"completeMfaLogin TOTP code", func(t *testing.T, e *codeEnv) codeOperation {
			u, _, secret, _ := e.mfaUser(t, "login@example.com")
			return codeOperation{user: u.ID,
				attempt: signIn(e, "login@example.com", func(c string) identity.MFALogin { return identity.MFALogin{TOTPCode: c} }),
				wrong:   func(t *testing.T) string { return e.wrongCode(t, secret) },
				good:    func(t *testing.T) string { return totpCode(t, secret, e.clk.Now()) },
				refused: refusedLikeUnknownChallenge(e), late: []*atomic.Bool{&e.late.totp, &e.late.lock},
				// Nobody is signed in: no actor, the account as the resource.
				denied: audittest.Row{Action: string(authz.IdentitySessionCreate), ResourceType: "user", ResourceID: &u.ID,
					Metadata: map[string]any{"mfa": "totp"}}}
		}},
		{"completeMfaLogin recovery code", func(t *testing.T, e *codeEnv) codeOperation {
			u, _, _, codes := e.mfaUser(t, "recovery@example.com")
			return codeOperation{user: u.ID,
				attempt: signIn(e, "recovery@example.com", func(c string) identity.MFALogin { return identity.MFALogin{RecoveryCode: c} }),
				wrong:   func(*testing.T) string { return "aaaa-bbbb-cccc-dddd" }, // well formed, never issued
				good:    func(*testing.T) string { return codes[0] },
				refused: refusedLikeUnknownChallenge(e), late: []*atomic.Bool{&e.late.recovery, &e.late.lock},
				denied: audittest.Row{Action: string(authz.IdentitySessionCreate), ResourceType: "user", ResourceID: &u.ID,
					Metadata: map[string]any{"mfa": "recovery_code"}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newCodeEnv(t)
			tc.setup(t, e).run(t, e)
		})
	}
}

// TestMFALoginCodeDenied_SignedInCallerIsTheActor: completeMfaLogin is a
// public operation; a caller already signed in to another account who
// sends a wrong code for this account's challenge is the actor of the
// denied record (it is among the caller's own security events, not the
// account owner's), the account its resource, the answer unchanged.
func TestMFALoginCodeDenied_SignedInCallerIsTheActor(t *testing.T) {
	t.Parallel()
	e := newCodeEnv(t)
	owner, _, secret, _ := e.mfaUser(t, "owner@example.com")
	caller := e.activeUser(t, "caller@example.com")
	_, a := e.login(t, "caller@example.com", identity.ClientMobile)

	ch := e.passwordStep(t, "owner@example.com", identity.ClientMobile)
	n := len(e.audit.rows())
	_, err := e.svc.CompleteMFALogin(t.Context(), a, identity.MFALogin{ChallengeID: ch.ID, TOTPCode: e.wrongCode(t, secret)})
	if !errors.Is(err, identity.ErrMFALoginFailed) {
		t.Fatalf("wrong code: %v", err)
	}
	wantCodeDenied(t, e.audit.rows()[n:], audittest.Row{Action: string(authz.IdentitySessionCreate), ActorUserID: &caller.ID,
		ResourceType: "user", ResourceID: &owner.ID, Metadata: map[string]any{"mfa": "totp"}}, e.failures(t, owner.ID))
	if got := countEvents(e.ownEvents(t, caller.ID), authz.IdentitySessionCreate, audit.Denied); got != 1 {
		t.Fatalf("%d denied sign-in records among the caller's security events, want 1", got)
	}
	if got := countEvents(e.ownEvents(t, owner.ID), authz.IdentitySessionCreate, audit.Denied); got != 0 {
		t.Fatalf("%d denied sign-in records among the owner's security events, want 0 (the caller acted)", got)
	}
}

// TestSignInPasswordDenied_RecordUnchanged: BE-076 changes only the
// records of a refused second-factor code; a refused password step keeps
// its record (recordFailure is shared): session.create denied without
// actor or reason, the account as the resource, metadata.result
// (invalid_credentials; locked once the fifth failure locked the account)
// and failed_attempts.
func TestSignInPasswordDenied_RecordUnchanged(t *testing.T) {
	t.Parallel()
	e := newCodeEnv(t)
	u := e.activeUser(t, "pw-step@example.com")
	for i := 1; i <= 6; i++ {
		n := len(e.audit.rows())
		_, err := e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: "pw-step@example.com", Password: "Yanlis-Parola-Deneme-1"})
		wantErr(t, err, identity.ErrInvalidCredentials)
		result := "invalid_credentials"
		if i == 6 {
			result = "locked"
		}
		want := audittest.Row{Action: string(authz.IdentitySessionCreate), Outcome: audittest.Denied, ResourceType: "user",
			ResourceID: &u.ID, Metadata: map[string]any{"result": result, "failed_attempts": i}}
		if rows := e.audit.rows()[n:]; len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
			t.Fatalf("password failure %d: records %+v, want exactly %+v", i, rows, want)
		}
	}
}
