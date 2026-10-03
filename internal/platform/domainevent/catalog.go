package domainevent

import "uuid"

// Topics of the catalog module (01 §6.2). Owner: catalog tickets.
const (
	TopicOfferingSubmitted = "offering.submitted"
	TopicOfferingDecided   = "offering.decided"
	TopicOfferingCancelled = "offering.cancelled"
)

var _ = register(
	spec("catalog", AfterCommit, OfferingSubmitted{}),
	spec("catalog", AfterCommit, OfferingDecided{}),
	spec("catalog", Outbox, OfferingCancelled{}),
)

// OfferingSubmitted: approvers are notified.
type OfferingSubmitted struct {
	OfferingID uuid.UUID `json:"offering_id"`
	OrgID      uuid.UUID `json:"org_id"`
}

// Topic implements Payload.
func (OfferingSubmitted) Topic() string { return TopicOfferingSubmitted }

// OfferingDecided: the seller org is notified.
type OfferingDecided struct {
	OfferingID uuid.UUID `json:"offering_id"`
	OrgID      uuid.UUID `json:"org_id"`
	Outcome    string    `json:"outcome"`
}

// Topic implements Payload.
func (OfferingDecided) Topic() string { return TopicOfferingDecided }

// OfferingCancelled: unpaid orders are cancelled, paid ones queued for
// refund. Reason is a code.
type OfferingCancelled struct {
	OfferingID uuid.UUID `json:"offering_id"`
	OrgID      uuid.UUID `json:"org_id"`
	Reason     string    `json:"reason"`
}

// Topic implements Payload.
func (OfferingCancelled) Topic() string { return TopicOfferingCancelled }
