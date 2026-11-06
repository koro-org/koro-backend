package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// The message report part of the GORM store (messageReportStore; BE-162b).

const (
	messageReportCols   = `report_id, message_id, conversation_id, org_id, student_user_id, reported_side, taken_at`
	reportedMessageCols = `report_id, message_id, sender_side, sender_user_id, body, sent_at`
)

// InsertMessageReport implements Store in one statement (the context row
// and its messages together, or nothing; the composite FK binds the row to
// a content report of target_type message on the same message).
//
// cross-org: a report has no org scope (moderation is the platform's); the
// row's org_id is copied from the reported conversation.
//
// GORM ends a named parameter only at a space, a comma, a parenthesis, a
// quote or a semicolon: "@ids::uuid[]" would reach PostgreSQL as the
// column "ids", so the arrays are typed with CAST( @name AS type[]) (the
// feed and students stores' form).
func (s gormStore) InsertMessageReport(ctx context.Context, r MessageReport, msgs []ReportedMessage) error {
	if r.ReportID == uuid.Nil() || len(msgs) == 0 {
		return fmt.Errorf("messaging: insert message report: %w", problem.ErrInternal)
	}
	ids, senders := make(db.UUIDArray, len(msgs)), make(db.UUIDArray, len(msgs))
	sides, bodies, sent := make(db.TextArray, len(msgs)), make(db.TextArray, len(msgs)), make(db.TextArray, len(msgs))
	for i, m := range msgs {
		ids[i], senders[i], sides[i], bodies[i] = m.MessageID, m.SenderUserID, string(m.SenderSide), m.Body
		sent[i] = m.SentAt.UTC().Format(time.RFC3339Nano)
	}
	err := db.Conn(ctx, s.orm).Exec(`WITH r AS (
		  INSERT INTO message_reports (`+messageReportCols+`)
		  VALUES ( @report, @message, @conversation, @org_id, @student, @side, @at) RETURNING report_id)
		INSERT INTO message_report_messages (`+reportedMessageCols+`)
		SELECT r.report_id, m.message_id, m.sender_side, m.sender_user_id, m.body, CAST(m.sent_at AS timestamptz)
		FROM r, unnest(CAST( @ids AS uuid[]), CAST( @sides AS text[]), CAST( @senders AS uuid[]), CAST( @bodies AS text[]),
		  CAST( @sent AS text[])) AS m (message_id, sender_side, sender_user_id, body, sent_at)`, map[string]any{
		"report": r.ReportID, "message": r.MessageID, "conversation": r.ConversationID, "org_id": r.OrgID,
		"student": r.StudentUserID, "side": string(r.ReportedSide), "at": r.TakenAt,
		"ids": ids, "sides": sides, "senders": senders, "bodies": bodies, "sent": sent,
	}).Error
	if err != nil {
		return wrap("insert message report", err)
	}
	return nil
}

// MessageReport implements Store.
//
// cross-org: a report is addressed by its ID on the admin surface (the
// service authorises the moderator first).
func (s gormStore) MessageReport(ctx context.Context, reportID uuid.UUID) (MessageReport, []ReportedMessage, error) {
	conn := db.Conn(ctx, s.orm)
	var rows []MessageReport
	if err := conn.Raw(`SELECT `+messageReportCols+` FROM message_reports WHERE report_id = @id`,
		map[string]any{"id": reportID}).Scan(&rows).Error; err != nil {
		return MessageReport{}, nil, wrap("message report", err)
	}
	if len(rows) == 0 {
		return MessageReport{}, nil, ErrNotFound
	}
	var msgs []ReportedMessage
	if err := conn.Raw(`SELECT `+reportedMessageCols+` FROM message_report_messages WHERE report_id = @id ORDER BY message_id`,
		map[string]any{"id": reportID}).Scan(&msgs).Error; err != nil {
		return MessageReport{}, nil, wrap("reported messages", err)
	}
	return rows[0], msgs, nil
}

// MessageReportContexts implements Store.
//
// cross-org: reports are addressed by their IDs on the admin surface.
func (s gormStore) MessageReportContexts(ctx context.Context, ids []uuid.UUID) ([]MessageReport, error) {
	out := []MessageReport{}
	if len(ids) == 0 {
		return out, nil
	}
	if err := db.Conn(ctx, s.orm).Raw(`SELECT `+messageReportCols+` FROM message_reports WHERE report_id = ANY(CAST( @ids AS uuid[]))`,
		map[string]any{"ids": db.UUIDArray(ids)}).Scan(&out).Error; err != nil {
		return nil, wrap("message report contexts", err)
	}
	return out, nil
}

// CountReportedMessagesBy implements Store.
//
// cross-org: the data subject's own messages (KVKK erasure).
func (s gormStore) CountReportedMessagesBy(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM message_report_messages WHERE sender_user_id = @user`,
		map[string]any{"user": userID}).Scan(&n).Error; err != nil {
		return 0, wrap("count reported messages", err)
	}
	return int(n), nil
}
