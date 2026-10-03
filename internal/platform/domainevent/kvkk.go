package domainevent

import (
	"time"
	"uuid"
)

// Topics of the kvkk module (01 §6.2). Owner: kvkk tickets.
const (
	TopicDSRSubmitted          = "dsr.submitted"
	TopicDSRErasureApproved    = "dsr.erasure_approved"
	TopicDSRProfilingObjection = "dsr.profiling_objection"
	TopicDSRCompleted          = "dsr.completed"
	TopicBreachRecorded        = "breach.recorded"
)

var _ = register(
	spec("kvkk", Outbox, DSRSubmitted{}),
	spec("kvkk", Outbox, DSRErasureApproved{}),
	spec("kvkk", Outbox, DSRProfilingObjection{}),
	spec("kvkk", Outbox, DSRCompleted{}),
	spec("kvkk", Outbox, BreachRecorded{}),
)

// DSRSubmitted: the DPO is notified. Type is the request type.
type DSRSubmitted struct {
	DSRID uuid.UUID `json:"dsr_id"`
	Type  string    `json:"type"`
}

// Topic implements Payload.
func (DSRSubmitted) Topic() string { return TopicDSRSubmitted }

// DSRErasureApproved: every privacy.Provider erases, identity anonymises last.
type DSRErasureApproved struct {
	DSRID  uuid.UUID `json:"dsr_id"`
	UserID uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (DSRErasureApproved) Topic() string { return TopicDSRErasureApproved }

// DSRProfilingObjection: talent neutralises (reason=objection).
type DSRProfilingObjection struct {
	DSRID  uuid.UUID `json:"dsr_id"`
	UserID uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (DSRProfilingObjection) Topic() string { return TopicDSRProfilingObjection }

// DSRCompleted: the data subject is notified.
type DSRCompleted struct {
	DSRID   uuid.UUID `json:"dsr_id"`
	UserID  uuid.UUID `json:"user_id"`
	Outcome string    `json:"outcome"`
}

// Topic implements Payload.
func (DSRCompleted) Topic() string { return TopicDSRCompleted }

// BreachRecorded: the DPO and platform admins are alerted.
type BreachRecorded struct {
	IncidentID uuid.UUID `json:"incident_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// Topic implements Payload.
func (BreachRecorded) Topic() string { return TopicBreachRecorded }
