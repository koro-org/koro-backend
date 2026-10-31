package messaging_test

import (
	"encoding/json"
	"strings"
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/messaging"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/students"
)

// Tests of BE-162b's conversation additions: the realtime signal, the
// student's level 3 name and the request's subject on the company's list,
// conversation_id on the requests.

// TestLive_SignalsWithoutContent: a message and a closing reach both
// parties' topics after the commit as {type, conversation_id, at}, never
// the text; a replayed Idempotency-Key and a refused write publish
// nothing; a block's closing signals too.
func TestLive_SignalsWithoutContent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	student, org := messaging.StudentLiveTopic(authztest.ID("student")), messaging.OrgLiveTopic(e.companyA)
	key := idempotency.WithKey(ctx, idempotency.Key{Scope: authztest.ID("student").String() + ":POST send", Value: "s-1", Status: 201})
	for range 2 {
		if _, err := e.svc.SendMessage(key, actor(t, "student"), asStudent, conv, "Gizli metin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, topic := range []string{student, org} {
		got := e.live.of(topic)
		if len(got) != 1 {
			t.Fatalf("%s: %d signals, want 1 (the replay publishes nothing): %v", topic, len(got), got)
		}
		var sig map[string]any
		if err := json.Unmarshal([]byte(got[0]), &sig); err != nil {
			t.Fatal(err)
		}
		if len(sig) != 3 || sig["type"] != "message" || sig["conversation_id"] != conv.String() || sig["at"] == nil ||
			strings.Contains(got[0], "Gizli") {
			t.Fatalf("%s signal = %s", topic, got[0])
		}
	}
	if _, err := e.svc.SendMessage(ctx, actor(t, "student"), asStudent, conv, ""); err == nil {
		t.Fatal("an empty message went through")
	}
	if _, err := e.svc.CloseConversation(ctx, actor(t, "company_admin"), e.asCompany(), conv); err != nil {
		t.Fatal(err)
	}
	if got := e.live.of(student); len(got) != 2 || !strings.Contains(got[1], `"type":"closed"`) {
		t.Fatalf("student signals = %v", got)
	}

	_, c2 := e.accepted(t, "student_granted")
	if err := e.svc.Block(ctx, actor(t, "student_granted"), e.companyA); err != nil {
		t.Fatal(err)
	}
	got := e.live.of(org)
	if last := got[len(got)-1]; !strings.Contains(last, `"type":"closed"`) || !strings.Contains(last, c2.Conversation.ID.String()) {
		t.Fatalf("the block's closing did not signal: %v", got)
	}
}

// TestLive_OpenAuthorisesLikeTheList: the student's WebSocket is their own
// topic; the company's is its recruiters' and admins' (403 for staff, 404
// for another company's member, a club or an unknown org).
func TestLive_OpenAuthorisesLikeTheList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if topic, err := e.svc.OpenLive(ctx, actor(t, "student"), asStudent); err != nil ||
		topic != messaging.StudentLiveTopic(authztest.ID("student")) {
		t.Fatalf("student = %q, %v", topic, err)
	}
	for _, p := range []string{"company_member", "company_admin"} {
		if topic, err := e.svc.OpenLive(ctx, actor(t, p), e.asCompany()); err != nil || topic != messaging.OrgLiveTopic(e.companyA) {
			t.Fatalf("%s = %q, %v", p, topic, err)
		}
	}
	_, err := e.svc.OpenLive(ctx, actor(t, "other_company_member"), e.asCompany())
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.OpenLive(ctx, actor(t, "moderator"), e.asCompany())
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.OpenLive(ctx, actor(t, "company_member"), messaging.CompanyParty(e.clubA))
	problemtest.WantErr(t, err, problem.ErrNotFound)
}

// TestConversation_CompanySeesNameAndSubject: the company's list names the
// student while it holds level 3 access (students' LevelThreeNames, asked
// once per page with each student once) and shows the request's subject;
// the student's view has the subject and never a name; without level 3
// (a revoked grant) the name is gone.
func TestConversation_CompanySeesNameAndSubject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, c := e.accepted(t, "student")
	e.accepted(t, "student_granted")
	e.names.set(e.companyA, authztest.ID("student"), &students.GrantedName{FirstName: "Ayşe", LastName: "Yılmaz"})
	page, err := e.svc.ListConversations(ctx, actor(t, "company_member"), e.asCompany(), messaging.ConversationListQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("company list = %+v, %v", page, err)
	}
	for _, v := range page.Items {
		d := messaging.ToConversationDTO(v, authztest.ID("company_member").String())
		if d.Subject != "Yaz stajı" {
			t.Fatalf("subject = %q", d.Subject)
		}
		switch v.Conversation.StudentUserID {
		case authztest.ID("student"):
			if d.StudentName == nil || d.StudentName.FirstName != "Ayşe" || d.StudentName.LastName != "Yılmaz" {
				t.Fatalf("named student = %+v", d.StudentName)
			}
		default:
			if d.StudentName != nil {
				t.Fatalf("a student without level 3 is named: %+v", d.StudentName)
			}
		}
	}
	if asked := e.names.asked; len(asked) != 1 || len(asked[0]) != 2 {
		t.Fatalf("names asked = %v", asked)
	}
	mine, err := e.svc.GetConversation(ctx, actor(t, "student"), asStudent, c.Conversation.ID)
	if d := messaging.ToConversationDTO(mine, authztest.ID("student").String()); err != nil || d.StudentName != nil ||
		d.Subject != "Yaz stajı" || d.Company == nil {
		t.Fatalf("student view = %+v, %v", d, err)
	}
	e.names.set(e.companyA, authztest.ID("student"), nil)
	v, err := e.svc.GetConversation(ctx, actor(t, "company_admin"), e.asCompany(), c.Conversation.ID)
	if err != nil || v.StudentName != nil {
		t.Fatalf("after the revocation = %+v, %v", v.StudentName, err)
	}
}

// TestContactRequest_ConversationID: an accepted request names the
// conversation its acceptance opened on both sides (lists, reads, the
// answer itself); a pending or declined one names none.
func TestContactRequest_ConversationID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	answered, err := e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, accept)
	if err != nil || answered.ConversationID == nil {
		t.Fatalf("respond = %+v, %v", answered, err)
	}
	conv := *answered.ConversationID
	pending := e.send(t, "student_granted")
	if pending.ConversationID != nil {
		t.Fatalf("a new request names a conversation: %v", pending.ConversationID)
	}
	sent, err := e.svc.GetSent(ctx, actor(t, "company_admin"), e.companyA, v.Request.ID)
	if err != nil || sent.ConversationID == nil || *sent.ConversationID != conv ||
		messaging.ToCompanyDTO(sent).ConversationID != conv.String() {
		t.Fatalf("getSent = %+v, %v", sent, err)
	}
	list, err := e.svc.ListSent(ctx, actor(t, "company_member"), e.companyA, messaging.ListQuery{})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("listSent = %+v, %v", list, err)
	}
	for _, it := range list.Items {
		if (it.Request.ID == v.Request.ID) != (it.ConversationID != nil) {
			t.Fatalf("listSent item %+v", it)
		}
	}
	mine, err := e.svc.ListMine(ctx, actor(t, "student"), messaging.ListQuery{})
	if err != nil || len(mine.Items) != 1 || mine.Items[0].ConversationID == nil ||
		messaging.ToStudentDTO(mine.Items[0]).ConversationID != conv.String() {
		t.Fatalf("listMine = %+v, %v", mine, err)
	}
	if _, err := e.svc.Respond(ctx, actor(t, "student_granted"), pending.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetMine(ctx, actor(t, "student_granted"), pending.Request.ID)
	if err != nil || got.ConversationID != nil || messaging.ToStudentDTO(got).ConversationID != "" {
		t.Fatalf("declined = %+v, %v", got, err)
	}
}
