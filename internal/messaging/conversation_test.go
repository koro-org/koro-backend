package messaging_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/messaging"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// Tests of the conversations of accepted contact requests (BE-162, Q-40).

var (
	asStudent = messaging.StudentParty()
)

func (e *env) asCompany() messaging.Party { return messaging.CompanyParty(e.companyA) }

// accepted sends company_a's request to student and accepts it; it returns
// the request and the conversation the acceptance opened.
func (e *env) accepted(t *testing.T, student string) (messaging.CompanyView, messaging.ConversationView) {
	t.Helper()
	ctx := t.Context()
	v := e.send(t, student)
	if _, err := e.svc.Respond(ctx, actor(t, student), v.Request.ID, accept); err != nil {
		t.Fatalf("accept: %v", err)
	}
	page, err := e.svc.ListConversations(ctx, actor(t, student), asStudent, messaging.ConversationListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Items {
		if c.Conversation.ContactRequestID != nil && *c.Conversation.ContactRequestID == v.Request.ID {
			return v, c
		}
	}
	t.Fatalf("no conversation of request %s: %+v", v.Request.ID, page.Items)
	return v, messaging.ConversationView{}
}

// say sends text as persona on p's path.
func (e *env) say(t *testing.T, persona string, p messaging.Party, conv uuid.UUID, text string) messaging.Message {
	t.Helper()
	m, err := e.svc.SendMessage(t.Context(), actor(t, persona), p, conv, text)
	if err != nil {
		t.Fatalf("%s says %q: %v", persona, text, err)
	}
	return m
}

// TestConversation_AcceptOpensConversation: the acceptance opens one open
// conversation in its transaction, with the company, the student and the
// requester of the request (06 H-38) and the request's audit names it; a
// decline opens none; the same answer again opens no second one; a refused
// acceptance (level 3 grant fails) leaves no conversation.
func TestConversation_AcceptOpensConversation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	req, c := e.accepted(t, "student")
	conv := c.Conversation
	if conv.OrgID != e.companyA || conv.StudentUserID != authztest.ID("student") || conv.CompanyUserID != authztest.ID("company_member") ||
		conv.Status != messaging.ConversationOpen || conv.ClosedBy != nil || conv.CompanyUnanswered != 0 || !c.CanSend ||
		c.Unread != 0 || c.Last != nil || c.Company.Name != "Şirket A" {
		t.Fatalf("conversation = %+v", c)
	}
	resp := e.audit.of(authz.MessagingContactRequestRespond)
	if len(resp) != 1 || resp[0].ev.Metadata["conversation_id"] != conv.ID.String() {
		t.Fatalf("respond audit = %+v", resp)
	}
	if _, err := e.svc.Respond(ctx, actor(t, "student"), req.Request.ID, accept); err != nil {
		t.Fatalf("the same answer again: %v", err)
	}
	all, err := e.svc.ListConversations(ctx, actor(t, "company_admin"), e.asCompany(), messaging.ConversationListQuery{})
	if err != nil || len(all.Items) != 1 || all.Items[0].Conversation.ID != conv.ID || all.Items[0].Company.ID != uuid.Nil() {
		t.Fatalf("company list = %+v, %v", all, err)
	}

	d := e.send(t, "student_granted")
	if _, err := e.svc.Respond(ctx, actor(t, "student_granted"), d.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	e.students.failGrant = problem.ErrConflict
	r, err := e.svc.Create(ctx, actor(t, "other_company_member"), e.companyB, request(e.companyB, "student_granted"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Respond(ctx, actor(t, "student_granted"), r.Request.ID, accept); err == nil {
		t.Fatal("an acceptance whose grant fails went through")
	}
	mine, err := e.svc.ListConversations(ctx, actor(t, "student_granted"), asStudent, messaging.ConversationListQuery{})
	if err != nil || len(mine.Items) != 0 {
		t.Fatalf("declined or refused requests opened conversations: %+v, %v", mine, err)
	}
}

// TestConversation_SendReadBothSides: the company writes, the student sees
// it unread with the newest message; marking read clears it (never shown to
// the company); the student's reply is unread for every company member,
// marks the conversation read for the student and resets the company's
// unanswered count; colleagues see who wrote (the student does not).
func TestConversation_SendReadBothSides(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	m1 := e.say(t, "company_member", e.asCompany(), id, "  Merhaba,\r\nyarın görüşelim mi?  ")
	if m1.Body != "Merhaba,\nyarın görüşelim mi?" || m1.SenderSide != messaging.SideCompany || m1.CreatedAt.IsZero() {
		t.Fatalf("message = %+v", m1)
	}
	m2 := e.say(t, "company_admin", e.asCompany(), id, "Ek bilgi.")

	v, err := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id)
	if err != nil || v.Unread != 2 || v.Last == nil || v.Last.ID != m2.ID || v.LastRead != nil {
		t.Fatalf("student view = %+v, %v", v, err)
	}
	if dto := messaging.ToConversationDTO(v, authztest.ID("student").String()); dto.LastMessage.SenderUserID != "" ||
		dto.LastMessage.Mine || dto.StudentUserID != "" || dto.Company == nil {
		t.Fatalf("student DTO = %+v", dto)
	}
	page, err := e.svc.ListMessages(ctx, actor(t, "student"), asStudent, id, nil, 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != m2.ID || page.Next == nil {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = e.svc.ListMessages(ctx, actor(t, "student"), asStudent, id, page.Next, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != m1.ID || page.Next != nil {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	if err := e.svc.MarkRead(ctx, actor(t, "student"), asStudent, id, &m1.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id); v.Unread != 1 || *v.LastRead != m1.ID {
		t.Fatalf("after marking m1 = %+v", v)
	}
	if err := e.svc.MarkRead(ctx, actor(t, "student"), asStudent, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MarkRead(ctx, actor(t, "student"), asStudent, id, &m1.ID); err != nil { // never moves back
		t.Fatal(err)
	}
	if v, _ = e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id); v.Unread != 0 || *v.LastRead != m2.ID {
		t.Fatalf("after marking all = %+v", v)
	}
	if n := len(e.audit.of(authz.MessagingConversationMarkRead)); n != 2 {
		t.Fatalf("%d mark_read audits, want 2 (the no-op writes none)", n)
	}

	reply := e.say(t, "student", asStudent, id, "Olur, saat 10 uygun.")
	cv, err := e.svc.GetConversation(ctx, actor(t, "company_admin"), e.asCompany(), id)
	if err != nil || cv.Unread != 1 || cv.Conversation.CompanyUnanswered != 0 || cv.Conversation.StudentUserID != authztest.ID("student") ||
		!cv.CanSend {
		t.Fatalf("company view = %+v, %v", cv, err)
	}
	if dto := messaging.ToConversationDTO(cv, authztest.ID("company_admin").String()); dto.LastMessage.SenderUserID != authztest.ID("student").String() ||
		dto.StudentUserID == "" || dto.Company != nil || dto.LastReadID == "" {
		t.Fatalf("company DTO = %+v", dto)
	}
	if v, _ = e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id); v.Unread != 0 || *v.LastRead != reply.ID {
		t.Fatalf("the reply did not mark the conversation read for the student: %+v", v)
	}
	if got := messaging.ToMessageDTO(m1, messaging.SideCompany, authztest.ID("company_member").String()); !got.Mine ||
		got.SenderUserID != authztest.ID("company_member").String() {
		t.Fatalf("colleague view = %+v", got)
	}
	err = e.svc.MarkRead(ctx, actor(t, "student"), asStudent, id, &c.Conversation.ID)
	problemtest.WantProblem(t, err, 422, "validation_failed")

	sent := e.audit.of(authz.MessagingMessageSend)
	if len(sent) != 3 || sent[0].ev.OrgID == nil || sent[2].ev.OrgID != nil ||
		sent[2].ev.Metadata["company_org_id"] != e.companyA.String() {
		t.Fatalf("send audits = %+v", sent)
	}
	b, _ := json.Marshal(sent)
	if strings.Contains(string(b), "görüşelim") {
		t.Fatalf("an audit record carries the text: %s", b)
	}
	ev := e.outbox.of(domainevent.TopicConversationMessageSent)
	if len(ev) != 3 || strings.Contains(string(ev[0].env.Payload), "görüşelim") {
		t.Fatalf("message events = %+v", ev)
	}
}

// TestConversation_Scope: the parties only. Another student's conversation
// and another company's path are 404 (IDOR), a club or another company's
// member is not a party.
func TestConversation_Scope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	_, err := e.svc.GetConversation(ctx, actor(t, "student_granted"), asStudent, id)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.SendMessage(ctx, actor(t, "other_company_member"), messaging.CompanyParty(e.companyB), id, "Merhaba")
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.GetConversation(ctx, actor(t, "other_company_member"), e.asCompany(), id)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ListConversations(ctx, actor(t, "platform_admin"), e.asCompany(), messaging.ConversationListQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.ListConversations(ctx, actor(t, "company_member"), messaging.CompanyParty(e.clubA), messaging.ConversationListQuery{})
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.ListConversations(ctx, actor(t, "student"), asStudent, messaging.ConversationListQuery{Status: "gone"})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	if l, err := e.svc.ListConversations(ctx, actor(t, "student"), asStudent,
		messaging.ConversationListQuery{Status: messaging.ConversationClosed}); err != nil || len(l.Items) != 0 {
		t.Fatalf("closed filter = %+v, %v", l, err)
	}
}

// TestConversation_CompanyAwaitsReply: the company sends
// MaxCompanyUnanswered messages in a row, the next is 409
// messaging.awaiting_reply (CanSend false on its side only); the
// student's reply opens it again.
func TestConversation_CompanyAwaitsReply(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	for i := range messaging.MaxCompanyUnanswered {
		e.say(t, []string{"company_member", "company_admin"}[i%2], e.asCompany(), id, "Mesaj")
	}
	_, err := e.svc.SendMessage(ctx, actor(t, "company_admin"), e.asCompany(), id, "Bir daha")
	problemtest.WantProblem(t, err, 409, "messaging.awaiting_reply")
	if v, _ := e.svc.GetConversation(ctx, actor(t, "company_member"), e.asCompany(), id); v.CanSend {
		t.Fatal("the company may send past the limit")
	}
	if v, _ := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id); !v.CanSend {
		t.Fatal("the student may not answer")
	}
	e.say(t, "student", asStudent, id, "Merhaba")
	e.say(t, "company_member", e.asCompany(), id, "Teşekkürler")
}

// TestConversation_Close: either side closes for good (closed_by); nobody
// writes after it (409 messaging.conversation_closed); closing again
// changes nothing; both still read it.
func TestConversation_Close(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	v, err := e.svc.CloseConversation(ctx, actor(t, "student"), asStudent, id)
	if err != nil || v.Conversation.Status != messaging.ConversationClosed || *v.Conversation.ClosedBy != messaging.SideStudent ||
		v.Conversation.ClosedAt == nil || v.CanSend {
		t.Fatalf("closed = %+v, %v", v, err)
	}
	if _, err := e.svc.CloseConversation(ctx, actor(t, "student"), asStudent, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CloseConversation(ctx, actor(t, "company_admin"), e.asCompany(), id); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.of(authz.MessagingConversationClose)); n != 1 {
		t.Fatalf("%d close audits, want 1", n)
	}
	_, err = e.svc.SendMessage(ctx, actor(t, "company_member"), e.asCompany(), id, "Merhaba")
	problemtest.WantProblem(t, err, 409, "messaging.conversation_closed")
	_, err = e.svc.SendMessage(ctx, actor(t, "student"), asStudent, id, "Merhaba")
	problemtest.WantProblem(t, err, 409, "messaging.conversation_closed")
	if cv, err := e.svc.GetConversation(ctx, actor(t, "company_member"), e.asCompany(), id); err != nil ||
		*cv.Conversation.ClosedBy != messaging.SideStudent {
		t.Fatalf("company read after close = %+v, %v", cv, err)
	}

	_, c2 := e.accepted(t, "student_granted")
	v, err = e.svc.CloseConversation(ctx, actor(t, "company_admin"), e.asCompany(), c2.Conversation.ID)
	if err != nil || *v.Conversation.ClosedBy != messaging.SideCompany {
		t.Fatalf("company close = %+v, %v", v, err)
	}
	if a := e.audit.of(authz.MessagingConversationClose); len(a) != 2 || a[1].ev.OrgID == nil || a[0].ev.OrgID != nil {
		t.Fatalf("close audits = %+v", a)
	}
}

// TestConversation_BlockClosesConversations (BE-056 + BE-162): a block
// closes the open conversations with the company in its transaction
// (closed_by student: the company cannot tell a block); unblocking does not
// reopen them; another company's conversation is untouched.
func TestConversation_BlockClosesConversations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	r, err := e.svc.Create(ctx, actor(t, "other_company_member"), e.companyB, request(e.companyB, "student"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Respond(ctx, actor(t, "student"), r.Request.ID, accept); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Block(ctx, actor(t, "student"), e.companyA); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.GetConversation(ctx, actor(t, "company_member"), e.asCompany(), c.Conversation.ID)
	if err != nil || v.Conversation.Status != messaging.ConversationClosed || *v.Conversation.ClosedBy != messaging.SideStudent {
		t.Fatalf("after the block = %+v, %v", v, err)
	}
	blocks := e.audit.of(authz.MessagingBlockManage)
	if len(blocks) != 1 || blocks[0].ev.Metadata["closed_conversations"] != 1 || blocks[0].ev.OrgID != nil {
		t.Fatalf("block audit = %+v", blocks)
	}
	if err := e.svc.Unblock(ctx, actor(t, "student"), e.companyA); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.svc.GetConversation(ctx, actor(t, "student"), asStudent, c.Conversation.ID); v.Conversation.Status != messaging.ConversationClosed {
		t.Fatal("unblocking reopened the conversation")
	}
	b, err := e.svc.ListConversations(ctx, actor(t, "student"), asStudent, messaging.ConversationListQuery{Status: messaging.ConversationOpen})
	if err != nil || len(b.Items) != 1 || b.Items[0].Conversation.OrgID != e.companyB {
		t.Fatalf("open conversations = %+v, %v", b, err)
	}
	if err := e.svc.Block(ctx, actor(t, "student"), e.companyA); err != nil { // nothing left to close: a new block row only
		t.Fatal(err)
	}
}

// TestConversation_CompanyStatus: a suspended company neither writes nor
// receives (403 orgs.org_suspended) and still reads; a closed company's
// conversation takes no message from the student (409).
func TestConversation_CompanyStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	e.orgs.status(e.companyA, orgs.StatusSuspended)
	_, err := e.svc.SendMessage(ctx, actor(t, "company_member"), e.asCompany(), id, "Merhaba")
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	_, err = e.svc.SendMessage(ctx, actor(t, "student"), asStudent, id, "Merhaba")
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	_, err = e.svc.CloseConversation(ctx, actor(t, "company_member"), e.asCompany(), id)
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	if v, err := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, id); err != nil || v.CanSend {
		t.Fatalf("read while suspended = %+v, %v", v, err)
	}
	if err := e.svc.SetMuted(ctx, actor(t, "company_member"), e.asCompany(), id, true); err != nil {
		t.Fatalf("a member's own mute while suspended: %v", err)
	}
	e.orgs.status(e.companyA, orgs.StatusClosed)
	_, err = e.svc.SendMessage(ctx, actor(t, "student"), asStudent, id, "Merhaba")
	problemtest.WantProblem(t, err, 409, "messaging.conversation_closed")
	_, err = e.svc.SendMessage(ctx, actor(t, "company_member"), e.asCompany(), id, "Merhaba")
	problemtest.WantErr(t, err, problem.ErrNotFound)
}

// TestConversation_TextRules: 1–2000 characters after trimming, no control
// or bidi characters (422 naming body.text); nothing is written.
func TestConversation_TextRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	for _, text := range []string{"", "   \n ", strings.Repeat("a", messaging.MaxMessageText+1), "kötü\u202emetin", "zil\x07"} {
		_, err := e.svc.SendMessage(ctx, actor(t, "student"), asStudent, c.Conversation.ID, text)
		problemtest.WantProblem(t, err, 422, "validation_failed")
	}
	e.say(t, "student", asStudent, c.Conversation.ID, strings.Repeat("ş", messaging.MaxMessageText))
	if n := len(e.outbox.of(domainevent.TopicConversationMessageSent)); n != 1 {
		t.Fatalf("%d messages written, want 1", n)
	}
}

// TestConversation_SendIdempotent: an Idempotency-Key replays the same
// message; reusing it for another text is refused.
func TestConversation_SendIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, c := e.accepted(t, "student")
	a := actor(t, "student")
	ctx := idempotency.WithKey(t.Context(), idempotency.Key{Scope: a.UserID.String() + ":POST /me/conversations/{conversation_id}/messages",
		Value: "k-1", Status: 201})
	m1, err := e.svc.SendMessage(ctx, a, asStudent, c.Conversation.ID, "Merhaba")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := e.svc.SendMessage(ctx, a, asStudent, c.Conversation.ID, "Merhaba")
	if err != nil || m2.ID != m1.ID {
		t.Fatalf("replay = %+v, %v", m2, err)
	}
	_, err = e.svc.SendMessage(ctx, a, asStudent, c.Conversation.ID, "Başka")
	problemtest.WantErr(t, err, idempotency.ErrKeyReused)
	if n := len(e.outbox.of(domainevent.TopicConversationMessageSent)); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}

// TestConversation_MessageNotice: the other side gets an in-app and a push
// notice (fixed template, the conversation's ID only, never the text; no
// e-mail): the student with the company as source, the company's requester
// and the members who wrote. An unread run is told once; reading opens the
// next notice; a muted conversation is silent for that person only; when
// no company party is left the owner and admins hear; a redelivery sends
// nothing twice. A fallback recipient who never read is told.
func TestConversation_MessageNotice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	run := func() {
		t.Helper()
		for _, ev := range e.outbox.of(domainevent.TopicConversationMessageSent) {
			for range 2 { // a redelivery
				if err := e.mod.HandleMessageSent(ctx, ev.env); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	notices := func(user string) []notify.Message {
		return slices.DeleteFunc(e.sender.all(), func(m notify.Message) bool {
			return *m.UserID != authztest.ID(user) || m.Template != "messaging.conversation_message"
		})
	}
	e.say(t, "company_member", e.asCompany(), id, "Gizli içerik bir")
	e.say(t, "company_member", e.asCompany(), id, "Gizli içerik iki")
	run()
	got := notices("student")
	if len(got) != 2 || got[0].Channel == got[1].Channel {
		t.Fatalf("student notices = %+v (one in-app and one push for the run)", got)
	}
	for _, m := range got {
		if m.Channel == notify.ChannelEmail || m.LinkPath.String() != "/me/conversations/"+id.String() || len(m.Data) != 1 ||
			m.Data["conversation_id"] != id.String() || m.Category != notify.CategoryTransactional {
			t.Fatalf("notice = %+v", m)
		}
		if m.Channel == notify.ChannelInApp && (m.SourceOrgID == nil || *m.SourceOrgID != e.companyA) {
			t.Fatalf("in-app source = %+v", m)
		}
	}
	b, _ := json.Marshal(e.sender.all())
	if strings.Contains(string(b), "Gizli") {
		t.Fatalf("a notice carries the text: %s", b)
	}
	if err := e.svc.MarkRead(ctx, actor(t, "student"), asStudent, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMuted(ctx, actor(t, "company_admin"), e.asCompany(), id, true); err != nil {
		t.Fatal(err)
	}
	e.say(t, "company_admin", e.asCompany(), id, "Üç")
	run()
	if n := len(notices("student")); n != 4 {
		t.Fatalf("student notices after reading = %d, want 4", n)
	}
	e.say(t, "student", asStudent, id, "Cevap")
	run()
	if n, m := len(notices("company_member")), len(notices("company_admin")); n != 2 || m != 0 {
		t.Fatalf("company notices: requester %d (want 2), muted writer %d (want 0)", n, m)
	}
	for _, m := range notices("company_member") {
		if m.LinkPath.String() != "/orgs/"+e.companyA.String()+"/conversations/"+id.String() || m.SourceOrgID != nil {
			t.Fatalf("company notice = %+v", m)
		}
	}

	// No company party left: the owner and the admin (muted: skipped).
	e.orgs.leave(e.companyA, authztest.ID("company_member"))
	if err := e.svc.SetMuted(ctx, actor(t, "company_admin"), e.asCompany(), id, false); err != nil {
		t.Fatal(err)
	}
	e.orgs.leave(e.companyA, authztest.ID("company_admin"))
	e.say(t, "student", asStudent, id, "Orada mısınız?")
	run()
	if n := len(notices("company_owner_a")); n != 2 {
		t.Fatalf("owner notices = %d, want 2", n)
	}
	if n := len(e.audit.of(authz.MessagingConversationMute)); n != 2 {
		t.Fatalf("%d mute audits, want 2", n)
	}
}

// TestConversation_RequestSweptConversationStays: the request's own
// retention deletes it 12 months after the acceptance; the conversation
// stays without it (ON DELETE SET NULL).
func TestConversation_RequestSweptConversationStays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	req, c := e.accepted(t, "student")
	e.store.Age(req.Request.ID, 400*24*time.Hour)
	p := privacy.RetentionPolicy{Code: messaging.PolicyContactRequestClosed, Retention: 365 * 24 * time.Hour, LegalApproved: true}
	if r, err := e.mod.Sweeper.Sweep(ctx, p, e.clk.Now()); err != nil || r.Count != 1 {
		t.Fatalf("sweep = %+v, %v", r, err)
	}
	v, err := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, c.Conversation.ID)
	if err != nil || v.Conversation.ContactRequestID != nil {
		t.Fatalf("conversation after the request went = %+v, %v", v, err)
	}
	e.say(t, "company_member", e.asCompany(), c.Conversation.ID, "Hâlâ buradayız")
}

// conversationAuditActions are the audit actions of the conversations.
var conversationAuditActions = []authz.Action{authz.MessagingMessageSend, authz.MessagingConversationMarkRead,
	authz.MessagingConversationClose, authz.MessagingConversationMute}

// TestConversation_AuditNamesTheStudentNeverTheText pins the audit shape:
// resource conversation, the student as subject, the company's scope only
// on the company's own actions.
func TestConversation_AuditNamesTheStudentNeverTheText(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	e.say(t, "company_member", e.asCompany(), id, "Metin")
	if err := e.svc.SetMuted(ctx, actor(t, "student"), asStudent, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CloseConversation(ctx, actor(t, "company_member"), e.asCompany(), id); err != nil {
		t.Fatal(err)
	}
	for _, act := range conversationAuditActions {
		for _, l := range e.audit.of(act) {
			ev := l.ev
			if ev.ResourceType != authz.TypeConversation || ev.ResourceID == nil || *ev.ResourceID != id ||
				ev.Metadata[audit.MetaSubjectUserID] != authztest.ID("student").String() || ev.Outcome != audit.Success {
				t.Fatalf("%s audit = %+v", act, ev)
			}
			if _, ok := ev.Metadata["text"]; ok {
				t.Fatalf("%s audit carries the text", act)
			}
		}
	}
}

// TestPrivacy_Conversations (KVKK; BE-162): the student's export holds its
// conversations and every message (a company message without the member
// who wrote it), the company member's export the messages it wrote (without
// the student) and its states; the student's erasure deletes the
// conversations with their messages and states; the member's erasure
// deletes its states and keeps its messages (the company's record, with a
// reason); both are idempotent.
func TestPrivacy_Conversations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	id := c.Conversation.ID
	e.say(t, "company_member", e.asCompany(), id, "Şirketten merhaba")
	e.say(t, "student", asStudent, id, "Öğrenciden merhaba")
	if err := e.svc.SetMuted(ctx, actor(t, "company_member"), e.asCompany(), id, true); err != nil {
		t.Fatal(err)
	}
	student, member := authztest.ID("student"), authztest.ID("company_member")

	sec, err := e.mod.Privacy.Export(ctx, student)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sec)
	var conv, msgs int
	for _, it := range sec.Items {
		switch it["record"] {
		case "conversation":
			conv++
			if it["company_org_id"] != e.companyA.String() || it["status"] != "open" {
				t.Fatalf("conversation item = %v", it)
			}
		case "conversation_message":
			msgs++
		}
	}
	if conv != 1 || msgs != 2 || strings.Contains(string(b), member.String()) || !strings.Contains(string(b), "Şirketten merhaba") {
		t.Fatalf("student export (%d conversations, %d messages, no member ID) = %s", conv, msgs, b)
	}
	sec, err = e.mod.Privacy.Export(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(sec)
	if !strings.Contains(string(b), `"conversation_message_sent"`) || !strings.Contains(string(b), `"conversation_state"`) ||
		strings.Contains(string(b), student.String()) || strings.Contains(string(b), "Öğrenciden") {
		t.Fatalf("member export = %s", b)
	}

	res, err := e.mod.Privacy.Erase(ctx, member)
	if err != nil || res.Deleted != 1 || res.Retained != 2 || res.RetainedReason == "" { // the request it sent, its message
		t.Fatalf("member erase = %+v, %v", res, err)
	}
	if page, err := e.svc.ListMessages(ctx, actor(t, "student"), asStudent, id, nil, 10); err != nil || len(page.Items) != 2 {
		t.Fatalf("the member's messages went: %+v, %v", page, err)
	}
	res, err = e.mod.Privacy.Erase(ctx, student)
	if err != nil || res.Deleted < 2 { // the conversation, the accepted request
		t.Fatalf("student erase = %+v, %v", res, err)
	}
	if _, err := e.svc.GetConversation(ctx, actor(t, "company_admin"), e.asCompany(), id); err == nil {
		t.Fatal("the student's conversation survived the erasure")
	}
	if left, _ := e.store.AllMessages(ctx, id); len(left) != 0 {
		t.Fatalf("messages left: %v", left)
	}
	if res, err = e.mod.Privacy.Erase(ctx, student); err != nil || res.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", res, err)
	}
}

// TestSweep_ConversationsAfter12Months (conversation_closed): an unapproved
// policy counts only; an approved one deletes the conversations whose last
// activity (closing or last message) is older than the retention, with
// their messages; a recent one stays.
func TestSweep_ConversationsAfter12Months(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, old := e.accepted(t, "student")
	_, recent := e.accepted(t, "student_granted")
	e.say(t, "student", asStudent, old.Conversation.ID, "Eski mesaj")
	e.store.AgeConversation(old.Conversation.ID, 400*24*time.Hour)
	p := privacy.RetentionPolicy{Code: messaging.PolicyConversationClosed, Retention: 365 * 24 * time.Hour}
	r, err := e.mod.Sweeper.Sweep(ctx, p, e.clk.Now())
	if err != nil || r.Count != 1 || r.Table != "conversations" || r.Method != privacy.MethodHardDelete {
		t.Fatalf("dry run = %+v, %v", r, err)
	}
	p.LegalApproved = true
	if r, err = e.mod.Sweeper.Sweep(ctx, p, e.clk.Now()); err != nil || r.Count != 1 {
		t.Fatalf("sweep = %+v, %v", r, err)
	}
	if _, err := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, old.Conversation.ID); err == nil {
		t.Fatal("the old conversation stayed")
	}
	if left, _ := e.store.AllMessages(ctx, old.Conversation.ID); len(left) != 0 {
		t.Fatalf("its messages stayed: %v", left)
	}
	if _, err := e.svc.GetConversation(ctx, actor(t, "student_granted"), asStudent, recent.Conversation.ID); err != nil {
		t.Fatalf("the recent conversation went: %v", err)
	}
}
