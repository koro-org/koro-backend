package messaging_test

import (
	"encoding/json"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/messaging"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// Tests of the message reports (BE-162b; Q-43, T-405).

// moderatorReason is a valid X-Koro-Access-Reason.
const moderatorReason = "Taciz şikâyeti incelemesi (kuyruk)"

var harassment = messaging.MessageReportInput{Reason: feed.ReasonHarassment, Details: "Tekrar eden ısrarlı mesajlar"}

// thread writes m1 (company), s1 (student), then m2…m6 (company: five in a
// row, the most it may) and returns the seven messages in order.
func (e *env) thread(t *testing.T, conv uuid.UUID) []messaging.Message {
	t.Helper()
	out := []messaging.Message{e.say(t, "company_member", e.asCompany(), conv, "m1"), e.say(t, "student", asStudent, conv, "s1")}
	for _, text := range []string{"m2", "m3", "m4", "m5", "m6"} {
		out = append(out, e.say(t, "company_admin", e.asCompany(), conv, text))
	}
	return out
}

// TestMessageReport_KeepsReportedAndFiveBefore (Q-43): the student reports
// a company message: one report in the feed's queue (target the message),
// and in its transaction the copy of the reported message and the five
// before it (the oldest message stays out), with the conversation's
// company, student and the reported side; the audit record has no org
// scope (the company never learns who reported) and no text.
func TestMessageReport_KeepsReportedAndFiveBefore(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	msgs := e.thread(t, conv)
	reported := msgs[6]
	res, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, reported.ID, harassment)
	if err != nil || !res.Created {
		t.Fatalf("ReportMessage = %+v, %v", res, err)
	}
	filed := e.reports.all()
	if len(filed) != 1 || filed[0].id != res.ID || filed[0].in.MessageID != reported.ID || filed[0].in.Reason != feed.ReasonHarassment ||
		filed[0].in.Details != harassment.Details || filed[0].reporter != authztest.ID("student") {
		t.Fatalf("filed = %+v", filed)
	}
	r, kept, err := e.store.MessageReport(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.MessageID != reported.ID || r.ConversationID != conv || r.OrgID != e.companyA || r.StudentUserID != authztest.ID("student") ||
		r.ReportedSide != messaging.SideCompany || !r.TakenAt.Equal(t0) {
		t.Fatalf("report context = %+v", r)
	}
	if len(kept) != messaging.MessageReportContext+1 {
		t.Fatalf("kept %d messages, want %d", len(kept), messaging.MessageReportContext+1)
	}
	for i, m := range kept {
		want := msgs[i+1] // s1, m2…m6: m1 is out
		if m.MessageID != want.ID || m.Body != want.Body || m.SenderSide != want.SenderSide || m.SenderUserID != want.SenderUserID {
			t.Fatalf("kept[%d] = %+v, want %+v", i, m, want)
		}
	}
	a := e.audit.of(authz.MessagingMessageReport)
	if len(a) != 1 || a[0].ev.OrgID != nil || a[0].ev.Metadata["report_id"] != res.ID.String() ||
		a[0].ev.Metadata["message_id"] != reported.ID.String() || a[0].ev.Metadata["kept_messages"] != 6 {
		t.Fatalf("report audit = %+v", a)
	}
	raw, _ := json.Marshal(a[0].ev)
	if strings.Contains(string(raw), "m6") || strings.Contains(string(raw), harassment.Details) {
		t.Fatalf("the audit record carries the text: %s", raw)
	}
}

// TestMessageReport_AgainKeepsFirstCopy: reporting the same message again
// returns the open report (Created false) and writes nothing: the first
// copy and the single audit record stay. Another user's report of the same
// message is its own.
func TestMessageReport_AgainKeepsFirstCopy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	m := e.say(t, "company_member", e.asCompany(), conv, "Rahatsız edici mesaj")
	first, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, m.ID, harassment)
	if err != nil {
		t.Fatal(err)
	}
	e.say(t, "company_member", e.asCompany(), conv, "Sonraki mesaj")
	again, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, m.ID,
		messaging.MessageReportInput{Reason: feed.ReasonSpam})
	if err != nil || again.Created || again.ID != first.ID {
		t.Fatalf("again = %+v, %v (first %+v)", again, err, first)
	}
	if _, kept, _ := e.store.MessageReport(ctx, first.ID); len(kept) != 1 || kept[0].MessageID != m.ID {
		t.Fatalf("kept = %+v", kept)
	}
	if n := len(e.audit.of(authz.MessagingMessageReport)); n != 1 {
		t.Fatalf("%d report audits, want 1", n)
	}
}

// TestMessageReport_CompanyReportsStudentMessage: the company's recruiter
// reports the student's message on its path; its audit record is in the
// company's org scope (the company's own act).
func TestMessageReport_CompanyReportsStudentMessage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	e.say(t, "company_member", e.asCompany(), conv, "Merhaba")
	m := e.say(t, "student", asStudent, conv, "Uygunsuz yanıt")
	res, err := e.svc.ReportMessage(ctx, actor(t, "company_member"), e.asCompany(), conv, m.ID, harassment)
	if err != nil || !res.Created {
		t.Fatalf("ReportMessage = %+v, %v", res, err)
	}
	r, kept, err := e.store.MessageReport(ctx, res.ID)
	if err != nil || r.ReportedSide != messaging.SideStudent || len(kept) != 2 {
		t.Fatalf("report = %+v %+v, %v", r, kept, err)
	}
	if a := e.audit.of(authz.MessagingMessageReport); len(a) != 1 || a[0].ev.OrgID == nil || *a[0].ev.OrgID != e.companyA {
		t.Fatalf("company report audit = %+v", a)
	}
}

// TestMessageReport_Scope: only the other side's message of one's own
// conversation: one's own side is 422; another student's or company's
// conversation, a message of another conversation and an unknown message
// are 404; staff on the company path is 403; a closed conversation's
// message is reported too. Nothing is filed for a refusal.
func TestMessageReport_Scope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	cm := e.say(t, "company_member", e.asCompany(), conv, "Şirket mesajı")
	sm := e.say(t, "student", asStudent, conv, "Öğrenci mesajı")
	_, other := e.accepted(t, "student_granted")
	om := e.say(t, "company_member", e.asCompany(), other.Conversation.ID, "Başka konuşma")

	_, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, sm.ID, harassment)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.ReportMessage(ctx, actor(t, "company_admin"), e.asCompany(), conv, cm.ID, harassment)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.ReportMessage(ctx, actor(t, "student_granted"), asStudent, conv, cm.ID, harassment)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ReportMessage(ctx, actor(t, "other_company_member"), messaging.CompanyParty(e.companyB), conv, sm.ID, harassment)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, om.ID, harassment)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, uuid.NewV7(), harassment)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ReportMessage(ctx, actor(t, "moderator"), e.asCompany(), conv, sm.ID, harassment)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if n := len(e.reports.all()); n != 0 {
		t.Fatalf("%d reports filed by refused calls", n)
	}

	if _, err := e.svc.CloseConversation(ctx, actor(t, "student"), asStudent, conv); err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, cm.ID, harassment); err != nil || !res.Created {
		t.Fatalf("a closed conversation's message = %+v, %v", res, err)
	}
}

// TestMessageReport_AllOrNothing: the feed's refusal leaves no copy and no
// audit record; without the feed a report is 503; an Idempotency-Key
// replay returns the same report and files nothing again.
func TestMessageReport_AllOrNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	m := e.say(t, "company_member", e.asCompany(), conv, "Mesaj")
	e.reports.fail = problem.Field("body.reason", "is not a report reason")
	_, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, m.ID, harassment)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	if n := len(e.audit.of(authz.MessagingMessageReport)); n != 0 {
		t.Fatalf("%d audits of a refused report", n)
	}
	e.reports.fail = nil

	key := idempotency.WithKey(ctx, idempotency.Key{Scope: authztest.ID("student").String() + ":POST report", Value: "k-1", Status: 201})
	first, err := e.svc.ReportMessage(key, actor(t, "student"), asStudent, conv, m.ID, harassment)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.ReportMessage(key, actor(t, "student"), asStudent, conv, m.ID, harassment)
	if err != nil || again != first || len(e.reports.all()) != 1 || len(e.audit.of(authz.MessagingMessageReport)) != 1 {
		t.Fatalf("replay = %+v, %v (first %+v)", again, err, first)
	}

	off := newEnv(t, func(d *messaging.Deps) { d.Reports = nil })
	_, oc := off.accepted(t, "student")
	om := off.say(t, "company_member", off.asCompany(), oc.Conversation.ID, "Mesaj")
	_, err = off.svc.ReportMessage(ctx, actor(t, "student"), asStudent, oc.Conversation.ID, om.ID, harassment)
	problemtest.WantErr(t, err, problem.ErrUnavailable)
}

// TestMessageReport_ModeratorReadsTheCopyWithReason (Q-43): a moderator
// (or platform admin) reads the copy only, with a justification (422
// without one), oldest first with the reported message marked; every read
// is audited with the justification, outside the company's org scope and
// without the break_glass flag (the parties are not told); the copy
// outlives the conversation; parties, support and unknown reports get
// nothing.
func TestMessageReport_ModeratorReadsTheCopyWithReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	msgs := e.thread(t, conv)
	res, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, msgs[3].ID, harassment)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.svc.ReportedMessages(ctx, actor(t, "moderator"), res.ID, "")
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.ReportedMessages(ctx, actor(t, "moderator"), res.ID, "kısa")
	problemtest.WantProblem(t, err, 422, "validation_failed")
	for _, p := range []string{"student", "company_admin", "support"} {
		_, err = e.svc.ReportedMessages(ctx, actor(t, p), res.ID, moderatorReason)
		problemtest.WantErr(t, err, authz.ErrForbidden)
	}
	_, err = e.svc.ReportedMessages(ctx, actor(t, "moderator"), uuid.NewV7(), moderatorReason)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	if n := len(e.audit.of(authz.MessagingMessageReportRead)); n != 0 {
		t.Fatalf("%d read audits before a granted read", n)
	}

	// The conversation goes (here the student's erasure; the fake queue keeps
	// the report, which in the application the feed's provider erases with
	// the copy, CASCADE): the copy outlives the conversation.
	if _, err := e.mod.Privacy.Erase(ctx, authztest.ID("student")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"moderator", "platform_admin"} {
		v, err := e.svc.ReportedMessages(ctx, actor(t, p), res.ID, moderatorReason)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		d := messaging.ToReportedMessagesDTO(v)
		if d.ReportID != res.ID.String() || d.ConversationID != conv.String() || d.MessageID != msgs[3].ID.String() ||
			d.ReportedSide != "company" || d.Company.Name != "Şirket A" || d.CompanyStatus != "active" ||
			d.StudentUserID != authztest.ID("student").String() || len(d.Messages) != 4 {
			t.Fatalf("%s view = %+v", p, d)
		}
		for i, m := range d.Messages {
			if m.MessageID != msgs[i].ID.String() || m.Text != msgs[i].Body || m.Reported != (i == 3) {
				t.Fatalf("messages[%d] = %+v", i, m)
			}
		}
	}
	reads := e.audit.of(authz.MessagingMessageReportRead)
	if len(reads) != 2 {
		t.Fatalf("%d read audits, want 2", len(reads))
	}
	for _, r := range reads {
		ev := r.ev
		if ev.Reason != moderatorReason || ev.OrgID != nil || ev.ResourceType != "content_report" || *ev.ResourceID != res.ID ||
			ev.Metadata[audit.MetaBreakGlass] != nil || ev.Metadata[audit.MetaSubjectUserID] != authztest.ID("student").String() ||
			ev.Metadata["messages"] != 4 || r.tx == 0 {
			t.Fatalf("read audit = %+v (tx %d)", ev, r.tx)
		}
	}
}

// TestMessageReport_KVKK: the kept messages are counted retained on their
// writer's erasure (the report's evidence; they go with the report) and
// are in nobody's export.
func TestMessageReport_KVKK(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	e.say(t, "student", asStudent, conv, "Öğrenci mesajı")
	m := e.say(t, "company_member", e.asCompany(), conv, "Şirket mesajı")
	if _, err := e.svc.ReportMessage(ctx, actor(t, "student"), asStudent, conv, m.ID, harassment); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"student", "company_member"} {
		x, err := e.mod.Privacy.Export(ctx, authztest.ID(u))
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range x.Items {
			if strings.Contains(it["record"].(string), "report") {
				t.Fatalf("%s's export has a message report: %+v", u, it)
			}
		}
	}
	res, err := e.mod.Privacy.Erase(ctx, authztest.ID("company_member"))
	if err != nil {
		t.Fatal(err)
	}
	// The request company_member sent and its message (the company's
	// record) and the report's copy of that message.
	if res.Retained != 3 || !strings.Contains(res.RetainedReason, "message_report_messages") {
		t.Fatalf("erase = %+v", res)
	}
	res, err = e.mod.Privacy.Erase(ctx, authztest.ID("student"))
	if err != nil || res.Retained != 1 {
		t.Fatalf("student erase = %+v, %v", res, err)
	}
}

// TestMessageReport_DTO: the moderators' DTO marks the reported message and
// keeps every sender (the moderator may act on either party).
func TestMessageReport_DTO(t *testing.T) {
	t.Parallel()
	report, mid, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	d := messaging.ToReportedMessagesDTO(messaging.ReportedMessagesView{
		Report: messaging.MessageReport{ReportID: report, MessageID: mid, OrgID: uuid.NewV7(), ReportedSide: messaging.SideStudent,
			TakenAt: t0},
		Messages: []messaging.ReportedMessage{
			{MessageID: other, SenderSide: messaging.SideCompany, SenderUserID: authztest.ID("company_member"), Body: "a", SentAt: t0},
			{MessageID: mid, SenderSide: messaging.SideStudent, SenderUserID: authztest.ID("student"), Body: "b", SentAt: t0},
		},
	})
	if len(d.Messages) != 2 || d.Messages[0].Reported || !d.Messages[1].Reported ||
		d.Messages[0].SenderUserID != authztest.ID("company_member").String() || d.Company.Kind != "company" || d.CompanyStatus != "" {
		t.Fatalf("dto = %+v", d)
	}
}
