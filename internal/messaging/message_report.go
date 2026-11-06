package messaging

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Message reports (BE-162b; user decision Q-43, the lawyer's opinion of
// 2026-10-31, T-405). A party of a conversation reports a message of the
// other side: the student a company message (/me), the company's recruiter
// or admin a student message (/orgs/{org_id}). The report is a content
// report of the platform's moderation queue (feed, target_type message;
// worked with adminListReports and adminResolveReport like any other); the
// reported message and at most MessageReportContext messages before it are
// copied here in the report's transaction, because the conversation goes
// 12 months after its last activity (conversation_closed) while the report
// lives until 12 months after its resolution (content_report). A moderator
// reads that copy only, never the conversation, one report at a time, each
// read with a justification and an audit record (adminGetReportedMessages).

// MessageReportInput is report{Org,My}ConversationMessage's body.
type MessageReportInput struct {
	Reason  feed.ReportReason
	Details string
}

// ReportedMessagesView is a message report as a moderator reads it: the
// conversation context, the company (zero when gone) and the kept
// messages, oldest first.
type ReportedMessagesView struct {
	Report   MessageReport
	Company  orgs.OrgSummary
	Messages []ReportedMessage
}

// MessageReporting is the message report part of Service (BE-162b).
type MessageReporting interface {
	// ReportMessage is reportOrgConversationMessage /
	// reportMyConversationMessage (messaging.message.report): Created is
	// false when the caller's open report on the message is returned (its
	// first copy stays).
	ReportMessage(ctx context.Context, a authz.Actor, p Party, conversationID, messageID uuid.UUID, in MessageReportInput) (feed.ReportResult, error)
	// ReportedMessages is adminGetReportedMessages
	// (messaging.message_report.read): reason is the X-Koro-Access-Reason
	// justification (10–500 characters, 422 without it), audited with the
	// read.
	ReportedMessages(ctx context.Context, a authz.Actor, reportID uuid.UUID, reason string) (ReportedMessagesView, error)
	// PartiesOfMessageReports implements feed.MessageReportParties (BE-183):
	// a module call, no actor; the feed decides.
	PartiesOfMessageReports(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]feed.MessageReportParty, error)
}

// ReportMessage implements Service. Order: authz on the path and the
// conversation (404 for another one's; a closed conversation is reported
// too) → the message of this conversation (404) of the other side (422) →
// the copy (the message and the MessageReportContext before it) → in one
// transaction with the optional Idempotency-Key: the feed's report (or the
// caller's open one: nothing else is written) → the copy → the audit
// record (IDs and the reason only; the company's own act in its org scope,
// the student's outside it: the company never learns who reported).
func (s *service) ReportMessage(ctx context.Context, a authz.Actor, p Party, conversationID, messageID uuid.UUID,
	in MessageReportInput,
) (feed.ReportResult, error) {
	c, err := s.open(ctx, a, authz.MessagingMessageReport, p, conversationID)
	if err != nil {
		return feed.ReportResult{}, err
	}
	if s.d.Reports == nil {
		return feed.ReportResult{}, problem.ErrUnavailable
	}
	m, err := s.conversations().MessageIn(ctx, c.ID, messageID)
	if err != nil {
		return feed.ReportResult{}, err
	}
	if m.SenderSide == p.Side { // the student's own message, or the company's
		return feed.ReportResult{}, problem.Field("path.message_id", "must be a message of the other side of the conversation")
	}
	before, err := s.conversations().Messages(ctx, c.ID, &m.ID, MessageReportContext)
	if err != nil {
		return feed.ReportResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		ID      uuid.UUID
		Message uuid.UUID
		Side    Side
		In      MessageReportInput
	}{"report_conversation_message", c.ID, m.ID, p.Side, in})
	if err != nil {
		return feed.ReportResult{}, err
	}
	taken := s.now()
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (feed.ReportResult, error) {
		res, err := s.d.Reports.FileMessageReport(ctx, a, feed.MessageReportInput{MessageID: m.ID, Reason: in.Reason,
			Details: in.Details})
		if err != nil || !res.Created {
			return res, err
		}
		kept := []ReportedMessage{reportedCopy(res.ID, m)}
		for _, b := range before {
			kept = append(kept, reportedCopy(res.ID, b))
		}
		if err := s.messageReports().InsertMessageReport(ctx, MessageReport{
			ReportID: res.ID, MessageID: m.ID, ConversationID: c.ID, OrgID: c.OrgID, StudentUserID: c.StudentUserID,
			ReportedSide: m.SenderSide, TakenAt: taken,
		}, kept); err != nil {
			return feed.ReportResult{}, err
		}
		ev := conversationEvent(authz.MessagingMessageReport, c, p, map[string]any{
			"report_id": res.ID.String(), "message_id": m.ID.String(), "reason": string(in.Reason), "kept_messages": len(kept),
		})
		return res, s.d.Audit.Log(ctx, ev.By(a))
	})
	return res, err
}

// ReportedMessages implements Service. Order: authz on the admin surface
// (moderators and platform admins; 403/404 before anything else) → the
// justification (422) → the report's copy (404 for a report that is no
// message report, or gone) → the decision on the report itself (BE-183, 403
// with a denied record: a stake in the report; a resolved report is a
// platform admin's) → the audit record of the read with the justification,
// in its own transaction, before anything is returned (no
// org scope: the company's trail never shows the moderators' reads; no
// break_glass flag: the parties are not told, which would tell them of the
// report and so of its reporter, Q-43). Only the copy is read: the live
// conversation stays out of the moderators' reach.
func (s *service) ReportedMessages(ctx context.Context, a authz.Actor, reportID uuid.UUID, reason string) (ReportedMessagesView, error) {
	if err := authz.Can(ctx, a, authz.MessagingMessageReportRead, authz.Resource{Type: authz.TypeAdminSurface}); err != nil {
		return ReportedMessagesView{}, err
	}
	reason, err := httpx.RequireAccessReason(reason)
	if err != nil {
		return ReportedMessagesView{}, err
	}
	if s.d.Reports == nil {
		return ReportedMessagesView{}, problem.ErrUnavailable
	}
	r, msgs, err := s.messageReports().MessageReport(ctx, reportID)
	if err != nil {
		return ReportedMessagesView{}, err
	}
	facts, err := s.d.Reports.MessageReportFacts(ctx, reportID)
	if err != nil {
		return ReportedMessagesView{}, err
	}
	if err := s.canReadReported(ctx, a, r, facts, reason); err != nil {
		return ReportedMessagesView{}, err
	}
	company, err := s.d.Orgs.Get(ctx, r.OrgID)
	if err != nil && !errors.Is(err, orgs.ErrNotFound) {
		return ReportedMessagesView{}, err
	}
	id := r.ReportID
	if err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.MessagingMessageReportRead), Outcome: audit.Success, ResourceType: authz.TypeContentReport,
			ResourceID: &id, Reason: reason, Metadata: map[string]any{
				"conversation_id": r.ConversationID.String(), "message_id": r.MessageID.String(),
				"company_org_id": r.OrgID.String(), audit.MetaSubjectUserID: r.StudentUserID.String(), "messages": len(msgs),
				"report_status": string(facts.Status),
			},
		}.By(a))
	}); err != nil {
		return ReportedMessagesView{}, err
	}
	slices.SortFunc(msgs, func(x, y ReportedMessage) int { return compareID(x.MessageID, y.MessageID) })
	return ReportedMessagesView{Report: r, Company: company, Messages: msgs}, nil
}

// canReadReported decides on the read of report r's copy. A refusal is
// recorded as denied in its own transaction, without org scope (the company's
// trail never shows a report).
func (s *service) canReadReported(ctx context.Context, a authz.Actor, r MessageReport, facts feed.MessageReportFacts, reason string) error {
	org, student := r.OrgID, r.StudentUserID
	err := authz.Can(ctx, a, authz.MessagingMessageReportRead,
		authz.ContentReportResource(r.ReportID, facts.ReporterUserID, string(facts.Status), &org, &student))
	if err == nil || !errors.Is(err, authz.ErrForbidden) {
		return err
	}
	id := r.ReportID
	if lerr := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.MessagingMessageReportRead), Outcome: audit.Denied, ResourceType: authz.TypeContentReport,
			ResourceID: &id, Reason: reason, Metadata: map[string]any{"report_status": string(facts.Status)},
		}.By(a))
	}); lerr != nil {
		return lerr
	}
	return err
}

// PartiesOfMessageReports implements MessageReporting.
func (s *service) PartiesOfMessageReports(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]feed.MessageReportParty, error) {
	rows, err := s.messageReports().MessageReportContexts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]feed.MessageReportParty, len(rows))
	for _, r := range rows {
		out[r.ReportID] = feed.MessageReportParty{OrgID: r.OrgID, StudentUserID: r.StudentUserID}
	}
	return out, nil
}
