package messaging_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/messaging"
)

func TestMessageReportStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	messageReportContract(t, func(t *testing.T) (messaging.Store, world) {
		return messaging.NewMemStore(time.Now), memWorld()
	})
}

// messageReportContract is the contract of the message report view both
// stores pass (05 §13.3; BE-162b): the copy is written once with its
// report, read back oldest first, outlives its conversation and counts the
// messages a user wrote.
func messageReportContract(t *testing.T, newStore func(t *testing.T) (messaging.Store, world)) {
	t.Helper()
	now := func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

	t.Run("written once, read oldest first, outlives the conversation", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, student, member := w.org(t), w.user(t), w.user(t)
		conv, err := s.InsertConversation(ctx, messaging.Conversation{ID: uuid.NewV7(), OrgID: org, StudentUserID: student,
			CompanyUserID: member, Status: messaging.ConversationOpen, LastActivityAt: now()})
		if err != nil {
			t.Fatal(err)
		}
		var msgs []messaging.Message
		for i, side := range []messaging.Side{messaging.SideCompany, messaging.SideStudent, messaging.SideCompany} {
			sender := map[messaging.Side]uuid.UUID{messaging.SideCompany: member, messaging.SideStudent: student}[side]
			m, err := s.InsertMessage(ctx, messaging.Message{ID: uuid.NewV7(), ConversationID: conv.ID, SenderSide: side,
				SenderUserID: sender, Body: strings.Repeat("ş", i+1), CreatedAt: now()})
			if err != nil {
				t.Fatal(err)
			}
			msgs = append(msgs, m)
		}
		reported := msgs[2]
		report := w.report(t, student, reported.ID)
		taken := now()
		ctxRow := messaging.MessageReport{ReportID: report, MessageID: reported.ID, ConversationID: conv.ID, OrgID: org,
			StudentUserID: student, ReportedSide: messaging.SideCompany, TakenAt: taken}
		// Newest first, as the service reads them: the store keeps message order.
		kept := []messaging.ReportedMessage{
			{MessageID: msgs[2].ID, SenderSide: msgs[2].SenderSide, SenderUserID: member, Body: msgs[2].Body, SentAt: msgs[2].CreatedAt},
			{MessageID: msgs[1].ID, SenderSide: msgs[1].SenderSide, SenderUserID: student, Body: msgs[1].Body, SentAt: msgs[1].CreatedAt},
			{MessageID: msgs[0].ID, SenderSide: msgs[0].SenderSide, SenderUserID: member, Body: msgs[0].Body, SentAt: msgs[0].CreatedAt},
		}
		if err := s.InsertMessageReport(ctx, ctxRow, kept); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertMessageReport(ctx, ctxRow, kept[:1]); err == nil {
			t.Fatal("a second copy of the same report was written")
		}
		if _, err := s.DeleteStudentConversations(ctx, student); err != nil {
			t.Fatal(err)
		}
		got, rows, err := s.MessageReport(ctx, report)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReportID != report || got.MessageID != reported.ID || got.ConversationID != conv.ID || got.OrgID != org ||
			got.StudentUserID != student || got.ReportedSide != messaging.SideCompany || !got.TakenAt.Equal(taken) {
			t.Fatalf("MessageReport = %+v", got)
		}
		if len(rows) != 3 {
			t.Fatalf("kept rows = %+v", rows)
		}
		for i, r := range rows {
			if r.ReportID != report || r.MessageID != msgs[i].ID || r.Body != msgs[i].Body || r.SenderSide != msgs[i].SenderSide ||
				!r.SentAt.Equal(msgs[i].CreatedAt) {
				t.Fatalf("rows[%d] = %+v, want %+v", i, r, msgs[i])
			}
		}
		if n, err := s.CountReportedMessagesBy(ctx, member); err != nil || n != 2 {
			t.Fatalf("CountReportedMessagesBy(member) = %d, %v", n, err)
		}
		if n, err := s.CountReportedMessagesBy(ctx, w.user(t)); err != nil || n != 0 {
			t.Fatalf("CountReportedMessagesBy(nobody) = %d, %v", n, err)
		}
		if _, _, err := s.MessageReport(ctx, uuid.NewV7()); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("unknown report = %v", err)
		}
		contexts, err := s.MessageReportContexts(ctx, []uuid.UUID{report, uuid.NewV7()})
		if err != nil || len(contexts) != 1 || contexts[0].ReportID != report || contexts[0].OrgID != org || contexts[0].StudentUserID != student {
			t.Fatalf("MessageReportContexts = %+v, %v", contexts, err)
		}
		if contexts, err := s.MessageReportContexts(ctx, nil); err != nil || len(contexts) != 0 {
			t.Fatalf("MessageReportContexts(nil) = %+v, %v", contexts, err)
		}
	})

	t.Run("checks", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, student, member := w.org(t), w.user(t), w.user(t)
		mid := uuid.NewV7()
		row := messaging.MessageReport{ReportID: w.report(t, student, mid), MessageID: mid, ConversationID: uuid.NewV7(), OrgID: org,
			StudentUserID: student, ReportedSide: messaging.SideCompany, TakenAt: now()}
		for name, m := range map[string]messaging.ReportedMessage{
			"empty":    {MessageID: mid, SenderSide: messaging.SideCompany, SenderUserID: member, Body: "", SentAt: now()},
			"too long": {MessageID: mid, SenderSide: messaging.SideCompany, SenderUserID: member, Body: strings.Repeat("a", 2001), SentAt: now()},
			"no side":  {MessageID: mid, SenderSide: "platform", SenderUserID: member, Body: "x", SentAt: now()},
		} {
			if err := s.InsertMessageReport(ctx, row, []messaging.ReportedMessage{m}); err == nil {
				t.Fatalf("%s: written", name)
			}
		}
		if err := s.InsertMessageReport(ctx, row, nil); err == nil {
			t.Fatal("a copy without messages was written")
		}
		if _, _, err := s.MessageReport(ctx, row.ReportID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("a refused copy is readable: %v", err)
		}
		// The control: the same row with a valid message is written, so the
		// refusals above are the checks, not the statement.
		ok := messaging.ReportedMessage{MessageID: mid, SenderSide: messaging.SideCompany, SenderUserID: member,
			Body: strings.Repeat("ş", 2000), SentAt: now()}
		if err := s.InsertMessageReport(ctx, row, []messaging.ReportedMessage{ok}); err != nil {
			t.Fatalf("a valid copy: %v", err)
		}
		if _, kept, err := s.MessageReport(ctx, row.ReportID); err != nil || len(kept) != 1 || kept[0].Body != ok.Body {
			t.Fatalf("the valid copy = %+v, %v", kept, err)
		}
	})
}
