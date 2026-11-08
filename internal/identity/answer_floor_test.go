package identity_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/sms/smsfake"
)

// answerTrace counts what an operation leaves behind: outbox events,
// queued tasks, audit records, e-mails and SMS handed to their providers.
type answerTrace struct {
	events, tasks, audits, mails, sms int
}

// answerTrace reads the counts; f may be nil (no SMS provider).
func (e *env) answerTrace(f *smsfake.Fake) answerTrace {
	tr := answerTrace{events: len(e.out.Events()), tasks: len(e.q.Tasks()), audits: e.audit.count(), mails: len(e.mail.sent())}
	if f != nil {
		tr.sms = len(f.Sent())
	}
	return tr
}

func (r answerTrace) minus(o answerTrace) answerTrace {
	return answerTrace{r.events - o.events, r.tasks - o.tasks, r.audits - o.audits, r.mails - o.mails, r.sms - o.sms}
}

// heldAnswer runs call in the bubble and returns what it left behind at
// the moment the floor held its answer (relative to before the call), how
// long the answer took and its error. lead is the time the call spends
// before its floor starts (registerAccount's hash). It fails if anything
// happened after the floor, or if the answer came before it.
func heldAnswer(t *testing.T, e *env, f *smsfake.Fake, lead time.Duration, call func(context.Context) error) (answerTrace, time.Duration, error) {
	t.Helper()
	before := e.answerTrace(f)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- call(t.Context()) }()
	if lead > 0 {
		synctest.Wait() // blocked before the floor
		time.Sleep(lead)
	}
	synctest.Wait() // the call is done with its work, or blocked on the floor
	held := e.answerTrace(f)
	select {
	case err := <-done:
		t.Fatalf("answered (%v) without waiting for the floor", err)
	default:
	}
	err := <-done
	took := time.Since(start)
	if after := e.answerTrace(f); after != held {
		t.Fatalf("work after the floor: %+v at the floor, %+v after the answer", held.minus(before), after.minus(before))
	}
	return held.minus(before), took, err
}

// TestPasswordReset_AnswerFloorOnEveryPath: requestPasswordReset (both
// channels) and confirmPasswordResetSms answer after exactly AnswerFloor
// (the production timer, measured in a synctest bubble), with the same
// answer whatever the state of the account the address names. While the
// floor holds the answer the path's work is done (rows, audit record,
// queued sending) and nothing has been handed to a provider: the e-mail
// and the SMS leave from the worker. An address without an account leaves
// nothing behind. A refusal before the lookup answers at once.
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
		check := func(name string, call func(context.Context) error, wantErr error, want answerTrace) {
			t.Helper()
			got, took, err := heldAnswer(t, e, f, 0, call)
			if wantErr == nil && err != nil {
				t.Errorf("%s = %v, want nil", name, err)
			} else if wantErr != nil {
				problemtest.WantErr(t, err, wantErr)
			}
			if took != identity.AnswerFloor {
				t.Errorf("%s answered after %v, want %v", name, took, identity.AnswerFloor)
			}
			if got != want {
				t.Errorf("%s left %+v at the floor, want %+v", name, got, want)
			}
		}

		link := answerTrace{events: 1, audits: 1} // identity.email_token_issued, sent by the worker
		check("e-mail, no account", email("nobody@example.com"), nil, answerTrace{})
		check("e-mail, active account", email("active@example.com"), nil, link)
		check("e-mail, unverified account", email("pending@example.com"), nil, link)
		check("SMS, no account", bySMS("nobody@example.com"), nil, answerTrace{})
		check("SMS, account without the SMS reset", bySMS("active@example.com"), nil, link)
		check("SMS, account with the SMS reset", bySMS("sms@example.com"), nil, answerTrace{tasks: 1, audits: 1}) // identity.send_sms_code

		e.sendSMS(t)
		code := lastSMSCode(t, f, testPhone)
		wrong := string(rune('0'+(code[0]-'0'+1)%10)) + code[1:]
		check("confirmation, no account", confirm("nobody@example.com", code), identity.ErrInvalidSMSCode, answerTrace{})
		check("confirmation, wrong code", confirm("sms@example.com", wrong), identity.ErrInvalidSMSCode, answerTrace{audits: 1})
		check("confirmation, right code", confirm("sms@example.com", code), nil, answerTrace{tasks: 1, audits: 1}) // the security notice

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

// slowHasher spends d (bubble time) on every hash before the real one: the
// argon2id time of a registration, which comes before its answer floor.
type slowHasher struct {
	identity.PasswordHasher
	d time.Duration
}

func (h slowHasher) Hash(ctx context.Context, pw string) (string, error) {
	time.Sleep(h.d)
	return h.PasswordHasher.Hash(ctx, pw)
}

// TestAnswerFloor_RegisterResendAndEduStart (BE-200): resendEmailVerification,
// registerAccount and startEduVerification answer after exactly
// AnswerFloor from their lookup (the production timer, in a synctest
// bubble), with the same answer whatever the state of the account or the
// address: no account, active, unverified or suspended; a free .edu.tr
// address, one that verifies another account, the caller's own. While the
// floor holds the answer the path's work is done (rows, audit records,
// queued notice) and nothing has been handed to a provider. A
// registration hashes once on every path, before its floor: it answers
// after the hash plus the floor. A refusal before the lookup (malformed
// address, unknown university domain, rate limit) answers at once.
func TestAnswerFloor_RegisterResendAndEduStart(t *testing.T) {
	t.Parallel()
	sharedHasher() // outside the bubble: the shared hasher's semaphore must not belong to it
	const hashTime = 300 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		e := newEduEnv(t, func(d *identity.Deps) {
			d.Pace = nil // the production timer
			d.Hasher = slowHasher{PasswordHasher: d.Hasher, d: hashTime}
		})
		ctx, anon := t.Context(), authz.Anonymous()
		e.activeUser(t, "active@example.com")
		if err := e.svc.Register(ctx, anon, e.registration("pending@example.com")); err != nil {
			t.Fatal(err)
		}
		suspended := e.activeUser(t, "suspended@example.com")
		now := e.clk.Now()
		suspended.Status, suspended.SuspendedAt = identity.StatusSuspended, &now
		if err := e.store.UpdateUser(ctx, suspended); err != nil {
			t.Fatal(err)
		}
		_, ada := e.student(t, "ada@example.com")
		_, bora := e.student(t, "bora@example.com")

		check := func(name string, lead time.Duration, call func(context.Context) error, want answerTrace) {
			t.Helper()
			got, took, err := heldAnswer(t, e.env, nil, lead, call)
			if err != nil {
				t.Errorf("%s = %v, want nil (202)", name, err)
			}
			if took != lead+identity.AnswerFloor {
				t.Errorf("%s answered after %v, want %v", name, took, lead+identity.AnswerFloor)
			}
			if got != want {
				t.Errorf("%s left %+v at the floor, want %+v", name, got, want)
			}
		}
		resend := func(name, addr string, want answerTrace) {
			t.Helper()
			check("resend, "+name, 0, func(ctx context.Context) error { return e.svc.ResendVerification(ctx, anon, addr) }, want)
		}
		register := func(name, addr string, want answerTrace) {
			t.Helper()
			hashes := e.hasher.hash.Load()
			check("register, "+name, hashTime, func(ctx context.Context) error { return e.svc.Register(ctx, anon, e.registration(addr)) }, want)
			if n := e.hasher.hash.Load() - hashes; n != 1 {
				t.Errorf("register, %s hashed %d times, want 1", name, n)
			}
		}
		eduStart := func(name string, a authz.Actor, addr string, want answerTrace) {
			t.Helper()
			check("edu start, "+name, 0, func(ctx context.Context) error { return e.svc.StartEduVerification(ctx, a, addr) }, want)
		}

		token := answerTrace{events: 1} // identity.email_token_issued, sent by the worker
		resend("no account", "nobody@example.com", answerTrace{})
		resend("active account", "active@example.com", answerTrace{})
		resend("suspended account", "suspended@example.com", answerTrace{})
		resend("unverified account", "pending@example.com", token)

		register("new address", "new@example.com", answerTrace{events: 1, audits: 2})      // the account and the notice shown
		register("active account", "active@example.com", answerTrace{tasks: 1, audits: 1}) // the security notice
		register("unverified account", "pending@example.com", answerTrace{events: 1, audits: 1})
		register("suspended account", "suspended@example.com", answerTrace{audits: 1})

		eduStart("free address", ada, "ada@uni.edu.tr", answerTrace{events: 1, audits: 1})
		if err := e.svc.ConfirmEduVerification(ctx, anon, e.lastToken(t, "identity.edu_verify")); err != nil {
			t.Fatal(err)
		}
		eduStart("address of another account", bora, "ADA@uni.edu.tr", answerTrace{audits: 1})
		eduStart("own address", ada, "ada@uni.edu.tr", answerTrace{events: 1, audits: 1}) // a renewal

		// Refused before the lookup: no floor (and no hash).
		start, hashes := time.Now(), e.hasher.hash.Load()
		if err := e.svc.ResendVerification(ctx, anon, "not an address"); err == nil {
			t.Fatal("resend: a malformed address was accepted")
		}
		if err := e.svc.Register(ctx, anon, e.registration("not an address")); err == nil {
			t.Fatal("register: a malformed address was accepted")
		}
		problemtest.WantErr(t, e.svc.StartEduVerification(ctx, bora, "bora@bilinmeyen.edu.tr"), identity.ErrEduDomainUnknown)
		// The address's auth_email budget (3 an hour) is spent.
		if err := e.svc.StartEduVerification(ctx, bora, "ada@uni.edu.tr"); err == nil {
			t.Fatal("edu start: a fourth start for the address within the hour was accepted")
		} else {
			wantStatus(t, err, http.StatusTooManyRequests)
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("refusals before the lookup answered after %v, want at once", took)
		}
		if n := e.hasher.hash.Load() - hashes; n != 0 {
			t.Fatalf("refusals before the lookup hashed %d times", n)
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
