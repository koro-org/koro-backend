package audit

import (
	"context"
	"log/slog"

	"koro/internal/platform/problem"
)

// ReasonReauthFailed is the Reason of the denied record of a refused
// re-authentication proof (03 §6.2).
const ReasonReauthFailed = "reauth_failed"

// ReauthFailed audits the failed re-authentication step of an R operation
// (03 §6.2) and returns err unchanged. Every module checks the proof
// (identity's Verify through its own port, which audits nothing for its
// callers) after authz.Can and before any business write, and hands a
// failure here together with denied, the operation's record: Action, the
// resource type, its ID when one exists and the affected org when the
// module has already read it from a row (06 H-38); never the proof.
//
// Only a verdict is recorded (BE-072, BE-074): a refused proof — a 4xx
// (identity.reauth_required, identity.reauth_failed, 429 once the lockout
// or the reauth class is exhausted) — is written as denied with Outcome
// Denied and Reason reauth_failed. An error without a verdict — the
// request budget ran out before identity could check the proof
// (BE-070/071), or a dependency failed — writes nothing; httpx answers 503
// or 500 and logs it once (problem.IsClientError draws the line).
//
// A record that cannot be written does not change the answer: log gets
// "reauth failure not audited", at WARN when the request had already ended
// (an overload signal, like httpx's 503), at ERROR otherwise (an audit
// fault). Call it outside the business transaction: a row written in a
// transaction that rolls back with the refused operation is lost.
func ReauthFailed(ctx context.Context, l Logger, log *slog.Logger, denied Event, err error) error {
	if !problem.IsClientError(err) {
		return err
	}
	denied.Outcome, denied.Reason = Denied, ReasonReauthFailed
	if lerr := l.Log(ctx, denied); lerr != nil {
		level := slog.LevelError
		if ctx.Err() != nil {
			level = slog.LevelWarn
		}
		if log == nil {
			log = slog.Default()
		}
		log.LogAttrs(ctx, level, "reauth failure not audited", slog.String("action", denied.Action), slog.String("err", lerr.Error()))
	}
	return err
}
