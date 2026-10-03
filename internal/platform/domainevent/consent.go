package domainevent

import "uuid"

// Topics of the consent module (01 §6.2). Owner: consent tickets.
const (
	TopicConsentRecorded = "consent.recorded"
)

var _ = register(
	spec("consent", Outbox, ConsentRecorded{}),
)

// ConsentRecorded: a consent was given or withdrawn (İYS submission).
type ConsentRecorded struct {
	ConsentID uuid.UUID `json:"consent_id"`
	UserID    uuid.UUID `json:"user_id"`
	Purpose   string    `json:"purpose"`
	Action    string    `json:"action"`
}

// Topic implements Payload.
func (ConsentRecorded) Topic() string { return TopicConsentRecorded }
