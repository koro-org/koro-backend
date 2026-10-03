package domainevent

import "uuid"

// Topics of the outreach module (01 §6.2). Owner: outreach tickets.
const (
	TopicOutreachDispatchScheduled = "outreach.dispatch_scheduled"
)

var _ = register(
	spec("outreach", Outbox, OutreachDispatchScheduled{}),
)

// OutreachDispatchScheduled: the worker streams the audience and sends.
type OutreachDispatchScheduled struct {
	DispatchID uuid.UUID `json:"dispatch_id"`
	OrgID      uuid.UUID `json:"org_id"`
	PostID     uuid.UUID `json:"post_id"`
}

// Topic implements Payload.
func (OutreachDispatchScheduled) Topic() string { return TopicOutreachDispatchScheduled }
