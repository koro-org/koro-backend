package messaging

import (
	"context"
	"uuid"
)

// messageReportStore is the message report part of Store (BE-162b, Q-43;
// 000174): the reported messages a report keeps for the moderators. Rows
// are written once with their report and never updated (app_rw has no
// UPDATE); they go with the feed's report row (CASCADE).
type messageReportStore interface {
	// InsertMessageReport writes the report's context and its messages
	// (the reported one among them, at most MessageReportContext+1) in the
	// transaction carried by ctx. A report that already has its context is
	// a conflict (a programming error: the service writes it with a new
	// report only).
	InsertMessageReport(ctx context.Context, r MessageReport, msgs []ReportedMessage) error
	// MessageReport returns the context of report reportID and its
	// messages, oldest first, or ErrNotFound (not a message report, or
	// gone).
	MessageReport(ctx context.Context, reportID uuid.UUID) (MessageReport, []ReportedMessage, error)
	// CountReportedMessagesBy counts the kept messages the user wrote
	// (KVKK erasure: retained with their report).
	CountReportedMessagesBy(ctx context.Context, userID uuid.UUID) (int, error)
}
