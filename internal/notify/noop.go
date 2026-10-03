package notify

import "context"

// NoopPush is the push adapter until BE-055: it accepts and drops. The
// delivery is recorded as sent with provider "noop", so the record shows that
// no device was reached.
type NoopPush struct{}

// Name implements PushSender.
func (NoopPush) Name() string { return "noop" }

// SendPush implements PushSender.
func (NoopPush) SendPush(context.Context, PushMessage) (string, error) { return "", nil }
