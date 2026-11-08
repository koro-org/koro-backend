package identity

import (
	"context"
	"time"
)

// AnswerFloor is the least time an answer of the operations that look an
// account up by an address takes, counted from that lookup:
// requestPasswordReset (both channels) and confirmPasswordResetSms
// (BE-195), resendEmailVerification, registerAccount and
// startEduVerification (BE-200). The answer of these operations is the
// same whatever the state of the account the address names; the floor
// makes their duration independent of that state too. It covers the work
// they keep on the request path after the lookup (the rows, the audit
// record, the commit); e-mails, SMS and security notices leave from the
// worker (identity.send_email_token, identity.send_sms_code,
// identity.send_security_notice), never from the request.
//
// What comes before the lookup does not depend on the account and stays
// out of the floor: a refusal there (a malformed body, an unknown
// university domain, the rate limits) answers at once, and the argon2id
// hash every registration pays before its lookup is not counted in the
// floor (it need not outlast a hash).
//
// The value sits well above that work on a loaded server; the routes'
// latency (koro_http_request_duration_seconds) shows whether it still does.
const AnswerFloor = 500 * time.Millisecond

// Pacer holds an answer back until a floor has passed (Deps.Pace).
type Pacer interface {
	// Start begins a floor of d and returns its wait, which blocks until d
	// has passed since Start or ctx ends.
	Start(d time.Duration) (wait func(ctx context.Context))
}

// timerPacer is the production Pacer: a runtime timer on the monotonic
// clock (a step of the wall clock neither shortens nor stretches a floor).
type timerPacer struct{}

func (timerPacer) Start(d time.Duration) func(context.Context) {
	t := time.NewTimer(d)
	return func(ctx context.Context) {
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done(): // the caller is gone: nobody waits for the answer
		}
	}
}

// answerFloor starts the answer floor (AnswerFloor) of an operation right
// before its account lookup. The caller defers the wait it returns, so the
// floor ends the operation, after every transaction of it has committed.
func (s *service) answerFloor() func(context.Context) { return s.d.Pace.Start(AnswerFloor) }
