package messaging_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/messaging"
	"koro/internal/platform/problem"
)

func TestConversationStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	conversationContract(t, func(t *testing.T) (messaging.Store, world) {
		return messaging.NewMemStore(time.Now), memWorld()
	})
}

// conversationContract is the contract of the conversation view both stores
// pass (05 §13.3; BE-162).
func conversationContract(t *testing.T, newStore func(t *testing.T) (messaging.Store, world)) {
	t.Helper()
	now := func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
	type parties struct{ org, student, member, admin uuid.UUID }
	newParties := func(t *testing.T, w world) parties {
		return parties{org: w.org(t), student: w.user(t), member: w.user(t), admin: w.user(t)}
	}
	newConv := func(p parties, at time.Time) messaging.Conversation {
		return messaging.Conversation{ID: uuid.NewV7(), OrgID: p.org, StudentUserID: p.student, CompanyUserID: p.member,
			Status: messaging.ConversationOpen, LastActivityAt: at}
	}
	msg := func(c messaging.Conversation, side messaging.Side, sender uuid.UUID, body string) messaging.Message {
		return messaging.Message{ID: uuid.NewV7(), ConversationID: c.ID, SenderSide: side, SenderUserID: sender, Body: body, CreatedAt: now()}
	}

	t.Run("insert, scope, lock and one per request", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		p := newParties(t, w)
		in := newConv(p, now())
		got, err := s.InsertConversation(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != in.ID || got.OrgID != p.org || got.StudentUserID != p.student || got.CompanyUserID != p.member ||
			got.Status != messaging.ConversationOpen || got.ClosedBy != nil || got.ClosedAt != nil || got.CompanyUnanswered != 0 ||
			!got.LastActivityAt.Equal(in.LastActivityAt) || got.CreatedAt.IsZero() || got.ContactRequestID != nil {
			t.Fatalf("InsertConversation = %+v", got)
		}
		if c, err := s.ConversationOfOrg(ctx, p.org, in.ID); err != nil || c.ID != in.ID {
			t.Fatalf("ConversationOfOrg = %+v, %v", c, err)
		}
		if _, err := s.ConversationOfOrg(ctx, w.org(t), in.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("another org = %v", err)
		}
		if c, err := s.ConversationOfStudent(ctx, p.student, in.ID); err != nil || c.ID != in.ID {
			t.Fatalf("ConversationOfStudent = %+v, %v", c, err)
		}
		if _, err := s.ConversationOfStudent(ctx, p.member, in.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("another user = %v", err)
		}
		if c, err := s.LockConversation(ctx, in.ID); err != nil || c.ID != in.ID {
			t.Fatalf("LockConversation = %+v, %v", c, err)
		}
		if _, err := s.LockConversation(ctx, uuid.NewV7()); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("unknown lock = %v", err)
		}

		// A request's conversation: one only; the request's deletion leaves
		// the conversation without it (ON DELETE SET NULL).
		r, err := s.Insert(ctx, messaging.ContactRequest{ID: uuid.NewV7(), OrgID: p.org, SenderUserID: p.member,
			StudentUserID: p.student, Via: messaging.ViaDiscovery, Subject: "Yaz stajı", Message: "Profilinizi gördük, görüşelim.",
			Status: messaging.StatusPending, ExpiresAt: now().Add(messaging.RequestTTL), EntitlementUsageID: w.usage(t, p.org)})
		if err != nil {
			t.Fatal(err)
		}
		at := now()
		if _, ok, err := s.Transition(ctx, r.ID, messaging.StatusPending, messaging.StatusAccepted, &at); err != nil || !ok {
			t.Fatalf("accept: %v %v", ok, err)
		}
		withReq := newConv(p, now())
		withReq.ContactRequestID = &r.ID
		if _, err := s.InsertConversation(ctx, withReq); err != nil {
			t.Fatal(err)
		}
		again := newConv(p, now())
		again.ContactRequestID = &r.ID
		if _, err := s.InsertConversation(ctx, again); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("second conversation of a request = %v, want a conflict", err)
		}
		if n, err := s.DeleteReceived(ctx, p.student); err != nil || n != 1 {
			t.Fatalf("DeleteReceived = %d, %v", n, err)
		}
		if c, err := s.ConversationOfOrg(ctx, p.org, withReq.ID); err != nil || c.ContactRequestID != nil {
			t.Fatalf("after the request went = %+v, %v", c, err)
		}
	})

	t.Run("messages, counters, read markers, mute and rows", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		p := newParties(t, w)
		older := newConv(p, now().Add(-time.Hour))
		if _, err := s.InsertConversation(ctx, older); err != nil {
			t.Fatal(err)
		}
		c := newConv(p, now().Add(-time.Minute))
		if _, err := s.InsertConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		m1, err := s.InsertMessage(ctx, msg(c, messaging.SideCompany, p.member, "Merhaba"))
		if err != nil || m1.Body != "Merhaba" || m1.CreatedAt.IsZero() {
			t.Fatalf("InsertMessage = %+v, %v", m1, err)
		}
		m2, err := s.InsertMessage(ctx, msg(c, messaging.SideCompany, p.admin, "Ek"))
		if err != nil {
			t.Fatal(err)
		}
		if cur, _ := s.LockConversation(ctx, c.ID); cur.CompanyUnanswered != 2 || !cur.LastActivityAt.Equal(m2.CreatedAt) {
			t.Fatalf("after two company messages = %+v", cur)
		}
		m3, err := s.InsertMessage(ctx, msg(c, messaging.SideStudent, p.student, "Cevap"))
		if err != nil {
			t.Fatal(err)
		}
		if cur, _ := s.LockConversation(ctx, c.ID); cur.CompanyUnanswered != 0 {
			t.Fatalf("after the student's reply = %+v", cur)
		}
		if _, err := s.InsertMessage(ctx, msg(messaging.Conversation{ID: uuid.NewV7()}, messaging.SideStudent, p.student, "x")); err == nil {
			t.Fatal("a message of no conversation was written")
		}
		if got, err := s.MessageIn(ctx, c.ID, m2.ID); err != nil || got.ID != m2.ID || got.SenderUserID != p.admin {
			t.Fatalf("MessageIn = %+v, %v", got, err)
		}
		if _, err := s.MessageIn(ctx, older.ID, m2.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("MessageIn of another conversation = %v", err)
		}
		page, err := s.Messages(ctx, c.ID, nil, 2)
		if err != nil || len(page) != 2 || page[0].ID != m3.ID || page[1].ID != m2.ID {
			t.Fatalf("Messages = %+v, %v", page, err)
		}
		if page, err = s.Messages(ctx, c.ID, &m2.ID, 5); err != nil || len(page) != 1 || page[0].ID != m1.ID {
			t.Fatalf("Messages before m2 = %+v, %v", page, err)
		}
		if senders, err := s.CompanySenders(ctx, c.ID); err != nil || !slices.Equal(senders, []uuid.UUID{p.member, p.admin}) {
			t.Fatalf("CompanySenders = %v, %v", senders, err)
		}

		org, student := p.org, p.student
		rows := func(q messaging.ConversationQuery) []messaging.ConversationRow {
			t.Helper()
			out, err := s.ConversationRows(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		byStudent := rows(messaging.ConversationQuery{StudentUserID: &student, Viewer: student, ViewerSide: messaging.SideStudent})
		if len(byStudent) != 2 || byStudent[0].Conversation.ID != c.ID || byStudent[1].Conversation.ID != older.ID ||
			byStudent[0].Unread != 2 || byStudent[0].Last == nil || byStudent[0].Last.ID != m3.ID || byStudent[0].Muted ||
			byStudent[1].Last != nil || byStudent[1].Unread != 0 {
			t.Fatalf("student rows = %+v", byStudent)
		}
		memberQ := messaging.ConversationQuery{OrgID: &org, Viewer: p.member, ViewerSide: messaging.SideCompany}
		if r := rows(memberQ); len(r) != 2 || r[0].Unread != 1 || r[0].LastReadMessageID != nil {
			t.Fatalf("member rows = %+v", r)
		}
		if moved, err := s.MarkRead(ctx, c.ID, p.member, m3.ID, now()); err != nil || !moved {
			t.Fatalf("MarkRead = %v, %v", moved, err)
		}
		if moved, err := s.MarkRead(ctx, c.ID, p.member, m1.ID, now()); err != nil || moved {
			t.Fatalf("MarkRead back = %v, %v", moved, err)
		}
		if changed, err := s.SetMuted(ctx, c.ID, p.member, true, now()); err != nil || !changed {
			t.Fatalf("mute = %v, %v", changed, err)
		}
		if changed, err := s.SetMuted(ctx, c.ID, p.member, true, now()); err != nil || changed {
			t.Fatalf("mute again = %v, %v", changed, err)
		}
		if r := rows(memberQ); r[0].Unread != 0 || r[0].LastReadMessageID == nil || *r[0].LastReadMessageID != m3.ID || !r[0].Muted {
			t.Fatalf("member rows after reading and muting = %+v", r[0])
		}
		if st, found, err := s.State(ctx, c.ID, p.member); err != nil || !found || st.MutedAt == nil || st.ReadAt == nil {
			t.Fatalf("State = %+v, %v, %v", st, found, err)
		}
		if _, found, err := s.State(ctx, c.ID, p.admin); err != nil || found {
			t.Fatalf("State of a user without one = %v, %v", found, err)
		}
		if changed, err := s.SetMuted(ctx, c.ID, p.member, false, now()); err != nil || !changed {
			t.Fatalf("unmute = %v, %v", changed, err)
		}
		if changed, err := s.SetMuted(ctx, c.ID, p.admin, false, now()); err != nil || changed {
			t.Fatalf("unmute without a state = %v, %v", changed, err)
		}
		// The admin's view: its own marker (none) counts the student's message.
		if r := rows(messaging.ConversationQuery{OrgID: &org, ID: &c.ID, Viewer: p.admin, ViewerSide: messaging.SideCompany}); len(r) != 1 ||
			r[0].Unread != 1 || r[0].Muted {
			t.Fatalf("admin row = %+v", r)
		}
		// Paging and the status filter.
		first := rows(messaging.ConversationQuery{OrgID: &org, Viewer: p.member, ViewerSide: messaging.SideCompany, Limit: 1})
		if len(first) != 1 || first[0].Conversation.ID != c.ID {
			t.Fatalf("first page = %+v", first)
		}
		key := &messaging.ConversationKey{At: first[0].Conversation.LastActivityAt, ID: first[0].Conversation.ID}
		if r := rows(messaging.ConversationQuery{OrgID: &org, Viewer: p.member, ViewerSide: messaging.SideCompany, After: key}); len(r) != 1 ||
			r[0].Conversation.ID != older.ID {
			t.Fatalf("second page = %+v", r)
		}
		if r := rows(messaging.ConversationQuery{OrgID: &org, Viewer: p.member, ViewerSide: messaging.SideCompany,
			Status: messaging.ConversationClosed}); len(r) != 0 {
			t.Fatalf("closed rows = %+v", r)
		}
		if _, err := s.ConversationRows(ctx, messaging.ConversationQuery{Viewer: p.member}); err == nil {
			t.Fatal("a query without a scope ran")
		}
		other := w.org(t)
		if r := rows(messaging.ConversationQuery{OrgID: &other, Viewer: p.member, ViewerSide: messaging.SideCompany}); len(r) != 0 {
			t.Fatalf("another org's rows = %+v", r)
		}
	})

	t.Run("close, open of a pair and the block", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		p := newParties(t, w)
		a, b := newConv(p, now()), newConv(p, now())
		for _, c := range []messaging.Conversation{a, b} {
			if _, err := s.InsertConversation(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		if open, err := s.OpenConversationsOf(ctx, p.org, p.student); err != nil || len(open) != 2 {
			t.Fatalf("OpenConversationsOf = %+v, %v", open, err)
		}
		at := now()
		got, ok, err := s.CloseConversation(ctx, a.ID, messaging.SideCompany, at)
		if err != nil || !ok || got.Status != messaging.ConversationClosed || *got.ClosedBy != messaging.SideCompany ||
			!got.ClosedAt.Equal(at) || !got.LastActivityAt.Equal(at) {
			t.Fatalf("CloseConversation = %+v, %v, %v", got, ok, err)
		}
		if _, ok, err := s.CloseConversation(ctx, a.ID, messaging.SideStudent, now()); err != nil || ok {
			t.Fatalf("closing again = %v, %v", ok, err)
		}
		if open, err := s.OpenConversationsOf(ctx, p.org, p.student); err != nil || len(open) != 1 || open[0].ID != b.ID {
			t.Fatalf("OpenConversationsOf after a close = %+v, %v", open, err)
		}
		org := p.org
		if r, err := s.ConversationRows(ctx, messaging.ConversationQuery{OrgID: &org, Viewer: p.member, ViewerSide: messaging.SideCompany,
			Status: messaging.ConversationClosed}); err != nil || len(r) != 1 || r[0].Conversation.ID != a.ID {
			t.Fatalf("closed rows = %+v, %v", r, err)
		}
	})

	t.Run("KVKK export and erasure", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		p := newParties(t, w)
		c := newConv(p, now())
		if _, err := s.InsertConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		m1, _ := s.InsertMessage(ctx, msg(c, messaging.SideCompany, p.member, "Bir"))
		m2, _ := s.InsertMessage(ctx, msg(c, messaging.SideStudent, p.student, "İki"))
		if _, err := s.MarkRead(ctx, c.ID, p.member, m2.ID, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkRead(ctx, c.ID, p.student, m2.ID, now()); err != nil {
			t.Fatal(err)
		}
		if convs, err := s.StudentConversations(ctx, p.student); err != nil || len(convs) != 1 || convs[0].ID != c.ID {
			t.Fatalf("StudentConversations = %+v, %v", convs, err)
		}
		if all, err := s.AllMessages(ctx, c.ID); err != nil || len(all) != 2 || all[0].ID != m1.ID || all[1].ID != m2.ID {
			t.Fatalf("AllMessages = %+v, %v", all, err)
		}
		if sent, err := s.MessagesSentBy(ctx, p.member); err != nil || len(sent) != 1 || sent[0].ID != m1.ID {
			t.Fatalf("MessagesSentBy = %+v, %v", sent, err)
		}
		if states, err := s.UserStates(ctx, p.member); err != nil || len(states) != 1 || *states[0].LastReadMessageID != m2.ID {
			t.Fatalf("UserStates = %+v, %v", states, err)
		}
		if n, err := s.DeleteUserStates(ctx, p.member); err != nil || n != 1 {
			t.Fatalf("DeleteUserStates = %d, %v", n, err)
		}
		if n, err := s.DeleteStudentConversations(ctx, p.student); err != nil || n != 1 {
			t.Fatalf("DeleteStudentConversations = %d, %v", n, err)
		}
		if all, _ := s.AllMessages(ctx, c.ID); len(all) != 0 {
			t.Fatalf("messages left: %+v", all)
		}
		if states, _ := s.UserStates(ctx, p.student); len(states) != 0 {
			t.Fatalf("states left: %+v", states)
		}
		if n, err := s.DeleteStudentConversations(ctx, p.student); err != nil || n != 0 {
			t.Fatalf("second DeleteStudentConversations = %d, %v", n, err)
		}
	})

	t.Run("retention by last activity", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		p := newParties(t, w)
		old, recent := newConv(p, now().Add(-400*24*time.Hour)), newConv(p, now())
		for _, c := range []messaging.Conversation{old, recent} {
			if _, err := s.InsertConversation(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.InsertMessage(ctx, messaging.Message{ID: uuid.NewV7(), ConversationID: old.ID, SenderSide: messaging.SideStudent,
			SenderUserID: p.student, Body: "Eski", CreatedAt: now().Add(-401 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		cutoff := now().Add(-365 * 24 * time.Hour)
		if n, err := s.CountInactiveBefore(ctx, cutoff); err != nil || n < 1 {
			t.Fatalf("CountInactiveBefore = %d, %v", n, err)
		}
		if n, err := s.PurgeInactive(ctx, cutoff, 1000); err != nil || n < 1 {
			t.Fatalf("PurgeInactive = %d, %v", n, err)
		}
		if _, err := s.LockConversation(ctx, old.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("the old conversation stayed: %v", err)
		}
		if all, _ := s.AllMessages(ctx, old.ID); len(all) != 0 {
			t.Fatalf("its messages stayed: %+v", all)
		}
		if _, err := s.LockConversation(ctx, recent.ID); err != nil {
			t.Fatalf("the recent conversation went: %v", err)
		}
	})
}
