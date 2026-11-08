package identity_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/sms/smsfake"
)

// resetTrace counts what a password reset operation leaves behind: outbox
// events, queued tasks, audit records of the two reset actions, e-mails
// and SMS handed to their providers.
type resetTrace struct {
	events, tasks, audits, mails, sms int
}

func (e *env) resetTrace(f *smsfake.Fake) resetTrace {
	return resetTrace{
		events: len(e.out.Events()), tasks: len(e.q.Tasks()),
		audits: len(e.audit.of(string(authz.IdentityPasswordResetRequest))) + len(e.audit.of(string(authz.IdentityPasswordReset))),
		mails:  len(e.mail.sent()), sms: len(f.Sent()),
	}
}

func (r resetTrace) minus(o resetTrace) resetTrace {
	return resetTrace{r.events - o.events, r.tasks - o.tasks, r.audits - o.audits, r.mails - o.mails, r.sms - o.sms}
}

// heldAnswer runs call in the bubble and returns what it left behind at
// the moment the floor held its answer (relative to before the call), how
// long the answer took and its error. It fails if anything happened after
// the floor, or if the answer came before it.
func heldAnswer(t *testing.T, e *env, f *smsfake.Fake, call func(context.Context) error) (resetTrace, time.Duration, error) {
	t.Helper()
	before := e.resetTrace(f)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- call(t.Context()) }()
	synctest.Wait() // the call is done with its work, or blocked on the floor
	held := e.resetTrace(f)
	select {
	case err := <-done:
		t.Fatalf("answered (%v) without waiting for the floor", err)
	default:
	}
	err := <-done
	took := time.Since(start)
	if after := e.resetTrace(f); after != held {
		t.Fatalf("work after the floor: %+v at the floor, %+v after the answer", held.minus(before), after.minus(before))
	}
	return held.minus(before), took, err
}

// TestPasswordReset_AnswerFloorOnEveryPath: requestPasswordReset (both
// channels) and confirmPasswordResetSms answer after exactly
// ResetAnswerFloor (the production timer, measured in a synctest bubble),
// with the same answer whatever the state of the account the address
// names. While the floor holds the answer the path's work is done (rows,
// audit record, queued sending) and nothing has been handed to a provider:
// the e-mail and the SMS leave from the worker. An address without an
// account leaves nothing behind. A refusal before the lookup answers at
// once.
func TestPasswordReset_AnswerFloorOnEveryPath(t *testing.T) {
	t.Parallel()
	sharedHasher() // outside the bubble: the shared hasher's semaphore must not belong to it
	synctest.Test(t, func(t *testing.T) {
		e, f := smsEnv(t, func(d *identity.Deps) { d.Pace = nil }) // nil → the production timer
		ctx := t.Context()
		anon := authz.Anonymous()
		e.activeUser(t, "active@example.com")
		if err := e.svc.Register(ctx, anon, e.registration("pending@example.com")); err != nil {
			t.Fatal(err)
		}
		e.smsResetUser(t, f, "sms@example.com", testPhone)

		email := func(addr string) func(context.Context) error {
			return func(ctx context.Context) error { return e.svc.RequestPasswordReset(ctx, anon, addr) }
		}
		bySMS := func(addr string) func(context.Context) error {
			return func(ctx context.Context) error { return e.svc.RequestPasswordResetSMS(ctx, anon, addr) }
		}
		confirm := func(addr, code string) func(context.Context) error {
			return func(ctx context.Context) error {
				return e.svc.ConfirmPasswordResetSMS(ctx, anon, identity.PasswordResetSMS{Email: addr, Code: code, NewPassword: newPassword})
			}
		}
		check := func(name string, call func(context.Context) error, wantErr error, want resetTrace) {
			t.Helper()
			got, took, err := heldAnswer(t, e, f, call)
			if wantErr == nil && err != nil {
				t.Errorf("%s = %v, want nil", name, err)
			} else if wantErr != nil {
				problemtest.WantErr(t, err, wantErr)
			}
			if took != identity.ResetAnswerFloor {
				t.Errorf("%s answered after %v, want %v", name, took, identity.ResetAnswerFloor)
			}
			if got != want {
				t.Errorf("%s left %+v at the floor, want %+v", name, got, want)
			}
		}

		link := resetTrace{events: 1, audits: 1} // identity.email_token_issued, sent by the worker
		check("e-mail, no account", email("nobody@example.com"), nil, resetTrace{})
		check("e-mail, active account", email("active@example.com"), nil, link)
		check("e-mail, unverified account", email("pending@example.com"), nil, link)
		check("SMS, no account", bySMS("nobody@example.com"), nil, resetTrace{})
		check("SMS, account without the SMS reset", bySMS("active@example.com"), nil, link)
		check("SMS, account with the SMS reset", bySMS("sms@example.com"), nil, resetTrace{tasks: 1, audits: 1}) // identity.send_sms_code

		e.sendSMS(t)
		code := lastSMSCode(t, f, testPhone)
		wrong := string(rune('0'+(code[0]-'0'+1)%10)) + code[1:]
		check("confirmation, no account", confirm("nobody@example.com", code), identity.ErrInvalidSMSCode, resetTrace{})
		check("confirmation, wrong code", confirm("sms@example.com", wrong), identity.ErrInvalidSMSCode, resetTrace{audits: 1})
		check("confirmation, right code", confirm("sms@example.com", code), nil, resetTrace{tasks: 1, audits: 1}) // the security notice

		// Refused before the lookup: no floor.
		start := time.Now()
		if err := e.svc.RequestPasswordReset(ctx, anon, "not an address"); err == nil {
			t.Fatal("a malformed address was accepted")
		}
		if err := e.svc.ConfirmPasswordResetSMS(ctx, anon, identity.PasswordResetSMS{Email: "not an address", Code: code}); err == nil {
			t.Fatal("a malformed address was accepted")
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("malformed addresses answered after %v, want at once", took)
		}
	})
}

// TestPasswordReset_FloorEndsWithTheRequest: when the request ends (the
// client went away), the floor lets the operation go at once.
func TestPasswordReset_FloorEndsWithTheRequest(t *testing.T) {
	t.Parallel()
	sharedHasher()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(d *identity.Deps) { d.Pace = nil })
		ctx, cancel := context.WithCancel(t.Context())
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "nobody@example.com") }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("answer = %v", err)
			}
		default:
			t.Fatal("still waiting for the floor after the request ended")
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("took %v, want at once", took)
		}
	})
}
