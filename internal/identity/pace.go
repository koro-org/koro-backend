package identity

import (
	"context"
	"time"
)

// ResetAnswerFloor is the least time an answer of requestPasswordReset (both
// channels) and confirmPasswordResetSms takes, counted from the account
// lookup (BE-195). The answer of these operations is the same whatever the
// state of the account the address names; the floor makes their duration
// independent of that state too. It covers the work they keep on the
// request path (the lookup, the rows, the audit record, the commit); the
// e-mail and the SMS leave from the worker (identity.send_email_token,
// identity.send_sms_code), never from the request. What is refused before
// the lookup (a malformed address, the rate limits) does not depend on the
// account and answers at once.
//
// The value sits well above that work on a loaded server; the route's
// latency (koro_http_request_duration_seconds) shows whether it still does.
const ResetAnswerFloor = 500 * time.Millisecond

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

// resetFloor starts the answer floor of a password reset operation. The
// caller defers the wait it returns, so the floor ends the operation, after
// every transaction of it has committed.
func (s *service) resetFloor() func(context.Context) { return s.d.Pace.Start(ResetAnswerFloor) }
