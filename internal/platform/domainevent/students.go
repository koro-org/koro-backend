package domainevent

import "uuid"

// Topics of the students module (01 §6.2). Owner: students tickets.
const (
	TopicStudentProfileSectionCompleted = "student.profile_section_completed"
	TopicStudentRankingChoiceChanged    = "student.ranking_choice_changed"
	TopicStudentGraduated               = "student.graduated"
)

var _ = register(
	spec("students", AfterCommit, StudentProfileSectionCompleted{}),
	spec("students", Outbox, StudentRankingChoiceChanged{}),
	spec("students", Outbox, StudentGraduated{}),
)

// StudentProfileSectionCompleted: a profile section became complete.
type StudentProfileSectionCompleted struct {
	UserID  uuid.UUID `json:"user_id"`
	Section string    `json:"section"`
}

// Topic implements Payload.
func (StudentProfileSectionCompleted) Topic() string { return TopicStudentProfileSectionCompleted }

// StudentRankingChoiceChanged: the student opted in or out of ranking.
type StudentRankingChoiceChanged struct {
	UserID uuid.UUID `json:"user_id"`
	Choice string    `json:"choice"`
}

// Topic implements Payload.
func (StudentRankingChoiceChanged) Topic() string { return TopicStudentRankingChoiceChanged }

// StudentGraduated: the profile moved to graduated.
type StudentGraduated struct {
	UserID uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (StudentGraduated) Topic() string { return TopicStudentGraduated }
