package messaging

import (
	"context"
	"slices"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// InsertMessageReport implements Store with the CHECKs of 000174 (the
// feed's report row and the composite FK are not emulated: the in-memory
// stores are apart).
func (m *MemStore) InsertMessageReport(_ context.Context, r MessageReport, msgs []ReportedMessage) error {
	if r.ReportID == uuid.Nil() || len(msgs) == 0 || !validSide(r.ReportedSide) {
		return problem.ErrInternal
	}
	kept := make([]ReportedMessage, 0, len(msgs))
	for _, msg := range msgs {
		if n := utf8.RuneCountInString(msg.Body); n < MinMessageText || n > MaxMessageText || !validSide(msg.SenderSide) {
			return problem.ErrInternal // message_report_messages_body_chk, _sender_side_chk
		}
		msg.ReportID, msg.SentAt = r.ReportID, msg.SentAt.UTC()
		kept = append(kept, msg)
	}
	slices.SortFunc(kept, func(a, b ReportedMessage) int { return compareID(a.MessageID, b.MessageID) })
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.reports[r.ReportID]; dup {
		return problem.ErrConflict
	}
	r.TakenAt = r.TakenAt.UTC()
	m.reports[r.ReportID], m.reported[r.ReportID] = r, kept
	return nil
}

func validSide(s Side) bool { return s == SideStudent || s == SideCompany }

// MessageReport implements Store.
func (m *MemStore) MessageReport(_ context.Context, reportID uuid.UUID) (MessageReport, []ReportedMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reports[reportID]
	if !ok {
		return MessageReport{}, nil, ErrNotFound
	}
	return r, slices.Clone(m.reported[reportID]), nil
}

// CountReportedMessagesBy implements Store.
func (m *MemStore) CountReportedMessagesBy(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, msgs := range m.reported {
		for _, msg := range msgs {
			if msg.SenderUserID == userID {
				n++
			}
		}
	}
	return n, nil
}
