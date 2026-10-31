package messaging

import "time"

// MessageDTO is a message of a conversation (BE-162). sender_user_id names
// the company member who wrote it on the company's view only (colleagues
// see who answered); the student's view tells only the side and whether
// the caller wrote it.
type MessageDTO struct {
	MessageID      string    `json:"message_id" format:"uuid"`
	ConversationID string    `json:"conversation_id" format:"uuid"`
	SenderSide     string    `json:"sender_side" enum:"student,company"`
	SenderUserID   string    `json:"sender_user_id,omitempty" format:"uuid" doc:"The company's view only: who wrote it"`
	Mine           bool      `json:"mine" doc:"The caller wrote it"`
	Text           string    `json:"text"`
	CreatedAt      time.Time `json:"created_at"`
}

// MessageList is a page of a conversation's messages, newest first.
type MessageList struct {
	Items      []MessageDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty" doc:"Older messages"`
}

// ConversationDTO is a conversation as its viewer sees it (BE-162).
type ConversationDTO struct {
	ConversationID   string             `json:"conversation_id" format:"uuid"`
	ContactRequestID string             `json:"contact_request_id,omitempty" format:"uuid" doc:"The accepted contact request it started from (absent once that request's retention deleted it)"`
	Company          *ContactCompanyDTO `json:"company,omitempty" doc:"The student's view: the company"`
	StudentUserID    string             `json:"student_user_id,omitempty" format:"uuid" doc:"The company's view: the student (getCompanyStudentProfile while the level 3 grant lasts)"`
	Status           string             `json:"status" enum:"open,closed"`
	ClosedBy         string             `json:"closed_by,omitempty" enum:"student,company" doc:"Who closed it"`
	ClosedAt         *time.Time         `json:"closed_at,omitempty"`
	CanSend          bool               `json:"can_send" doc:"The caller may write now (an open conversation, an active company; on the company's side fewer than 5 messages since the student's last one)"`
	Muted            bool               `json:"muted" doc:"The caller muted its notices"`
	UnreadCount      int                `json:"unread_count" doc:"The other side's messages after the caller's read marker"`
	LastReadID       string             `json:"last_read_message_id,omitempty" format:"uuid" doc:"The caller's read marker (never shown to the other side)"`
	LastMessage      *MessageDTO        `json:"last_message,omitempty"`
	LastActivityAt   time.Time          `json:"last_activity_at" doc:"The last message or the closing moment"`
	CreatedAt        time.Time          `json:"created_at"`
}

// ConversationList is a page of a conversation list, last activity first.
type ConversationList struct {
	Items      []ConversationDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// ToMessageDTO is the view of m on side's path for the caller callerID.
func ToMessageDTO(m Message, side Side, callerID string) MessageDTO {
	d := MessageDTO{
		MessageID: m.ID.String(), ConversationID: m.ConversationID.String(), SenderSide: string(m.SenderSide),
		Mine: m.SenderUserID.String() == callerID, Text: m.Body, CreatedAt: m.CreatedAt.UTC(),
	}
	if side == SideCompany {
		d.SenderUserID = m.SenderUserID.String()
	}
	return d
}

// ToConversationDTO is the view v for the caller callerID.
func ToConversationDTO(v ConversationView, callerID string) ConversationDTO {
	c := v.Conversation
	d := ConversationDTO{
		ConversationID: c.ID.String(), Status: string(c.Status), ClosedAt: utc(c.ClosedAt), CanSend: v.CanSend, Muted: v.Muted,
		UnreadCount: v.Unread, LastActivityAt: c.LastActivityAt.UTC(), CreatedAt: c.CreatedAt.UTC(),
	}
	if c.ContactRequestID != nil {
		d.ContactRequestID = c.ContactRequestID.String()
	}
	if c.ClosedBy != nil {
		d.ClosedBy = string(*c.ClosedBy)
	}
	if v.Side == SideCompany {
		d.StudentUserID = c.StudentUserID.String()
	} else {
		card := companyCard(v.Company, c.OrgID.String())
		d.Company = &card
	}
	if v.LastRead != nil {
		d.LastReadID = v.LastRead.String()
	}
	if v.Last != nil {
		m := ToMessageDTO(*v.Last, v.Side, callerID)
		d.LastMessage = &m
	}
	return d
}
