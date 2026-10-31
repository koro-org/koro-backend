package messaging

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
)

// registerMessageReports adds the 3 operations of the message reports
// (BE-162b, Q-43): the report on each path and the moderators' read.
func registerMessageReports(api huma.API, h handler) {
	reportOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "report" + s.opName + "ConversationMessage", Method: http.MethodPost,
			Path: s.prefix + "/conversations/{conversation_id}/messages/{message_id}/report", DefaultStatus: http.StatusCreated,
			Summary: "Report a message of the conversation",
			Description: "A message of the other side only (422 for your own side's). The report joins the platform's " +
				"moderation queue (adminListReports, target_type message); the reported message and the 5 messages before " +
				"it are kept with the report as they are now (they outlive the conversation until the report's own " +
				"retention), and a moderator reads only those, each time with a recorded justification. The other side is " +
				"never told who reported. A user has one open report per message: reporting it again returns that report " +
				"(200) and its first copy. A closed conversation's messages can be reported too."}
	}
	rep := op.Meta{Action: string(authz.MessagingMessageReport), Idempotency: op.IdempotencyOptional}
	ro, rm := rep, rep
	ro.Surface, rm.Surface = orgConversations.surface, myConversations.surface
	op.Register(api, "messaging", reportOp(orgConversations), ro, reportMessage[orgReportInput](h))
	op.Register(api, "messaging", reportOp(myConversations), rm, reportMessage[myReportInput](h))

	op.Register(api, "messaging", huma.Operation{
		OperationID: "adminGetReportedMessages", Method: http.MethodGet, Path: "/admin/reports/{report_id}/messages",
		Summary: "Read the messages of a message report",
		Description: "The moderators' only view of a conversation (Q-43): the reported message and at most 5 before it, " +
			"as they stood when the report was filed (oldest first), never the rest of the conversation. Every read " +
			"needs X-Koro-Access-Reason and is recorded with it (audit, messaging.message_report.read); the " +
			"conversation's parties are not notified (that would reveal the report). A report that is not a message " +
			"report, or is gone, is 404. Work the report itself with adminGetReport / adminResolveReport.",
	}, op.Meta{Action: string(authz.MessagingMessageReportRead), Surface: op.SurfaceAdmin, RateLimit: op.RateBreakGlass},
		h.reportedMessages)
}

// ---- report{Org,My}ConversationMessage

// OrgMessagePath is the org_id, conversation_id and message_id path
// parameters.
type OrgMessagePath struct {
	OrgConversationPath
	MessageID string `path:"message_id" doc:"Message ID (an unknown, malformed or other conversation's ID is 404)"`
}

// MyMessagePath is the conversation_id and message_id path parameters of /me.
type MyMessagePath struct {
	MyConversationPath
	MessageID string `path:"message_id" doc:"Message ID (an unknown, malformed or other conversation's ID is 404)"`
}

type orgReportInput struct {
	idempotency.OptionalHeader
	OrgMessagePath
	Body feed.ReportReasonBody
}

func (in orgReportInput) report() (string, feed.ReportReasonBody) { return in.MessageID, in.Body }

type myReportInput struct {
	idempotency.OptionalHeader
	MyMessagePath
	Body feed.ReportReasonBody
}

func (in myReportInput) report() (string, feed.ReportReasonBody) { return in.MessageID, in.Body }

type reportOutput struct {
	Status int
	Body   feed.ReportCreatedDTO
}

func reportMessage[I interface {
	targeted
	report() (string, feed.ReportReasonBody)
}](h handler) func(context.Context, *I) (*reportOutput, error) {
	return func(ctx context.Context, in *I) (*reportOutput, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		mid, body := (*in).report()
		res, err := h.svc.ReportMessage(ctx, a, p, id, httpx.ParseID(mid),
			MessageReportInput{Reason: feed.ReportReason(body.Reason), Details: body.Details})
		if err != nil {
			return nil, err
		}
		status := http.StatusCreated
		if !res.Created {
			status = http.StatusOK
		}
		return &reportOutput{Status: status, Body: feed.ReportCreatedDTO{ReportID: res.ID.String()}}, nil
	}
}

// ---- adminGetReportedMessages

// ReportedMessageDTO is one kept message of a message report.
type ReportedMessageDTO struct {
	MessageID    string    `json:"message_id" format:"uuid"`
	SenderSide   string    `json:"sender_side" enum:"student,company"`
	SenderUserID string    `json:"sender_user_id" format:"uuid" doc:"Who wrote it (the student or the company member)"`
	Reported     bool      `json:"reported" doc:"The reported message"`
	Text         string    `json:"text"`
	SentAt       time.Time `json:"sent_at"`
}

// ReportedMessagesDTO is a message report as a moderator reads it
// (adminGetReportedMessages; BE-162b).
type ReportedMessagesDTO struct {
	ReportID       string               `json:"report_id" format:"uuid"`
	ConversationID string               `json:"conversation_id" format:"uuid" doc:"The conversation (it may be gone: it is kept 12 months after its last activity)"`
	MessageID      string               `json:"message_id" format:"uuid" doc:"The reported message"`
	ReportedSide   string               `json:"reported_side" enum:"student,company" doc:"Who wrote the reported message"`
	Company        ContactCompanyDTO    `json:"company" doc:"The conversation's company"`
	CompanyStatus  string               `json:"company_status,omitempty" enum:"pending_review,active,suspended,closed"`
	StudentUserID  string               `json:"student_user_id" format:"uuid" doc:"The conversation's student"`
	TakenAt        time.Time            `json:"taken_at" doc:"When the report was filed (the messages as they stood then)"`
	Messages       []ReportedMessageDTO `json:"messages" doc:"The reported message and at most 5 before it, oldest first"`
}

// ReportPath is the report_id path parameter.
type ReportPath struct {
	ReportID string `path:"report_id" doc:"Content report ID (an unknown, malformed or non-message report's ID is 404)"`
}

type reportedMessagesInput struct {
	ReportPath
	httpx.AccessReasonHeader
}

type reportedMessagesOutput struct{ Body ReportedMessagesDTO }

func (h handler) reportedMessages(ctx context.Context, in *reportedMessagesInput) (*reportedMessagesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReportedMessages(ctx, a, httpx.ParseID(in.ReportID), in.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return &reportedMessagesOutput{Body: ToReportedMessagesDTO(v)}, nil
}

// ToReportedMessagesDTO is the moderators' view of v.
func ToReportedMessagesDTO(v ReportedMessagesView) ReportedMessagesDTO {
	r := v.Report
	d := ReportedMessagesDTO{
		ReportID: r.ReportID.String(), ConversationID: r.ConversationID.String(), MessageID: r.MessageID.String(),
		ReportedSide: string(r.ReportedSide), Company: companyCard(v.Company, r.OrgID.String()),
		CompanyStatus: string(v.Company.Status), StudentUserID: r.StudentUserID.String(), TakenAt: r.TakenAt.UTC(),
		Messages: make([]ReportedMessageDTO, len(v.Messages)),
	}
	for i, m := range v.Messages {
		d.Messages[i] = ReportedMessageDTO{
			MessageID: m.MessageID.String(), SenderSide: string(m.SenderSide), SenderUserID: m.SenderUserID.String(),
			Reported: m.MessageID == r.MessageID, Text: m.Body, SentAt: m.SentAt.UTC(),
		}
	}
	return d
}
