package authz

import (
	"errors"

	"koro/internal/platform/problem"
)

// The return set of Can (03 §3.1, 06 H-13). Denials wrap a *Denial cause
// that explains the decision in logs; clients only see code and title.
var (
	// ErrForbidden: the resource is visible, the action is not granted (403).
	ErrForbidden = problem.ErrForbidden
	// ErrNotFound: the resource is not visible to the actor (404; nothing leaks).
	ErrNotFound = problem.ErrNotFound
	// ErrMFARequired: the grant comes from an MFA role (or an M cell) and the
	// session is not MFA verified (403).
	ErrMFARequired = problem.New(problem.Forbidden, "authz.mfa_required", "Multi-factor authentication required")
	// ErrFourEyes: the approver is the initiator or the first eye (03 §6.1, 403).
	ErrFourEyes = problem.New(problem.Forbidden, "authz.four_eyes_violation", "Four-eyes rule violated")
	// ErrBreakGlassReason: a break-glass read without a 10–500 character
	// X-Koro-Access-Reason (03 §6.3, 403).
	ErrBreakGlassReason = problem.New(problem.Forbidden, "authz.break_glass_reason_required", "Access reason required")
)

// Denial is the cause wrapped into every denial. It reaches logs through the
// error chain (problem.Error.Error includes it), never the client.
type Denial struct {
	Action Action
	Reason string
	// Internal marks a programmer error (unknown action or resource type,
	// missing Attrs key, unwritten policy): log at ERROR (03 §3.2-0).
	Internal bool
}

// Error returns the reason for logs.
func (d *Denial) Error() string {
	prefix := "authz: "
	if d.Internal {
		prefix = "authz: programmer error: "
	}
	return prefix + string(d.Action) + ": " + d.Reason
}

// DenialOf returns the Denial inside err, if any.
func DenialOf(err error) (*Denial, bool) {
	var d *Denial
	ok := errors.As(err, &d)
	return d, ok
}

func deny(sentinel *problem.Error, act Action, reason string) error {
	return sentinel.Wrap(&Denial{Action: act, Reason: reason})
}

func denyInternal(act Action, reason string) error {
	return ErrForbidden.Wrap(&Denial{Action: act, Reason: reason, Internal: true})
}
