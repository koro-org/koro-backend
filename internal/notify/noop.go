package notify

import "context"

// NoopPush is the push adapter when neither FCM nor APNs is configured
// (dev; PUSH_REQUIRED=false): it accepts and drops. The delivery is recorded
// as sent with provider "noop", so the record shows that no device was
// reached.
type NoopPush struct{}

// Name implements PushSender.
func (NoopPush) Name() string { return "noop" }

// SendPush implements PushSender.
func (NoopPush) SendPush(context.Context, PushMessage) (string, error) { return "", nil }
