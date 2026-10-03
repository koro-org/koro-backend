package notify

import "context"

// NoopPush is the push adapter until BE-055: it accepts and drops. The
// delivery is recorded as sent with provider "noop", so the record shows that
// no device was reached.
type NoopPush struct{}

// Name implements PushSender.
func (NoopPush) Name() string { return "noop" }

// SendPush implements PushSender.
func (NoopPush) SendPush(context.Context, Message) (string, error) { return "", nil }

// NoopInApp is the in-app adapter until BE-015: it accepts and drops.
type NoopInApp struct{}

// Name implements InAppSender.
func (NoopInApp) Name() string { return "noop" }

// SendInApp implements InAppSender.
func (NoopInApp) SendInApp(context.Context, Message) (string, error) { return "", nil }
