package messaging

import (
	"time"
	"uuid"
)

// MessageReportContext is how many messages before the reported one a
// message report keeps (BE-162b; Q-43: the moderator sees the reported
// message and the few before it, never the rest of the conversation).
const MessageReportContext = 5

// MessageReport is a message_reports row (BE-162b, Q-43; 000174): the
// conversation context of a content report on a conversation message,
// copied from the conversation when the report was filed (06 H-38) and
// never changed. ReportID is the feed's content_reports row (target_type
// message, target_id MessageID); the row goes with it (CASCADE: the
// reporter's erasure, content_report retention) and outlives the
// conversation (no FK to it).
type MessageReport struct {
	ReportID       uuid.UUID `gorm:"type:uuid;column:report_id;primaryKey"`
	MessageID      uuid.UUID `gorm:"type:uuid;column:message_id"`
	ConversationID uuid.UUID `gorm:"type:uuid;column:conversation_id"`
	OrgID          uuid.UUID `gorm:"type:uuid;column:org_id"`
	StudentUserID  uuid.UUID `gorm:"type:uuid;column:student_user_id"`
	ReportedSide   Side      `gorm:"column:reported_side"`
	TakenAt        time.Time `gorm:"column:taken_at"`
}

// TableName implements gorm's Tabler.
func (MessageReport) TableName() string { return "message_reports" }

// ReportedMessage is a message_report_messages row: one message of a
// report's context as it was written (the reported message and at most
// MessageReportContext before it).
type ReportedMessage struct {
	ReportID     uuid.UUID `gorm:"type:uuid;column:report_id;primaryKey"`
	MessageID    uuid.UUID `gorm:"type:uuid;column:message_id;primaryKey"`
	SenderSide   Side      `gorm:"column:sender_side"`
	SenderUserID uuid.UUID `gorm:"type:uuid;column:sender_user_id"`
	Body         string    `gorm:"column:body"`
	SentAt       time.Time `gorm:"column:sent_at"`
}

// TableName implements gorm's Tabler.
func (ReportedMessage) TableName() string { return "message_report_messages" }

// reportedCopy is m as a report keeps it.
func reportedCopy(reportID uuid.UUID, m Message) ReportedMessage {
	return ReportedMessage{ReportID: reportID, MessageID: m.ID, SenderSide: m.SenderSide, SenderUserID: m.SenderUserID,
		Body: m.Body, SentAt: m.CreatedAt}
}
