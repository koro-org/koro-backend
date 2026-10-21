package messaging_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/messaging"
)

// world creates the parent rows a store needs: company orgs, users and a
// contact_request usage (FKs in PostgreSQL; plain IDs in memory).
type world struct {
	org   func(t *testing.T) uuid.UUID
	user  func(t *testing.T) uuid.UUID
	usage func(t *testing.T, org uuid.UUID) uuid.UUID
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id, usage: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() }}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (messaging.Store, world) {
		return messaging.NewMemStore(time.Now), memWorld()
	})
}

// storeContract is the contract both stores pass (05 §13.3). The store
// clock is the wall clock (PostgreSQL stamps updated_at with now()).
func storeContract(t *testing.T, newStore func(t *testing.T) (messaging.Store, world)) {
	t.Helper()
	newReq := func(t *testing.T, w world, org, sender, student uuid.UUID) messaging.ContactRequest {
		return messaging.ContactRequest{
			ID: uuid.NewV7(), OrgID: org, SenderUserID: sender, StudentUserID: student, Via: messaging.ViaDiscovery,
			Subject: "Yaz stajı", Message: "Profilinizi gördük, görüşmek isteriz.", Status: messaging.StatusPending,
			ExpiresAt: time.Now().UTC().Add(messaging.RequestTTL).Truncate(time.Microsecond), EntitlementUsageID: w.usage(t, org),
		}
	}

	t.Run("insert, scope and one pending per pair", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, sender, student := w.org(t), w.org(t), w.user(t), w.user(t)
		in := newReq(t, w, org, sender, student)
		got, err := s.Insert(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != in.ID || got.CreatedAt.IsZero() || !got.UpdatedAt.Equal(got.CreatedAt) || got.Status != messaging.StatusPending ||
			!got.ExpiresAt.Equal(in.ExpiresAt) || got.RespondedAt != nil || got.OpportunityPostID != nil {
			t.Fatalf("Insert = %+v", got)
		}
		if _, err := s.Insert(ctx, newReq(t, w, org, sender, student)); !errors.Is(err, messaging.ErrRequestPending) {
			t.Fatalf("second pending = %v, want ErrRequestPending", err)
		}
		if _, err := s.Insert(ctx, newReq(t, w, other, sender, student)); err != nil {
			t.Fatalf("another company: %v", err)
		}
		if r, err := s.Get(ctx, org, in.ID); err != nil || r.ID != in.ID || r.Subject != in.Subject || r.Message != in.Message {
			t.Fatalf("Get = %+v, %v", r, err)
		}
		if _, err := s.Get(ctx, other, in.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("Get by another org = %v", err)
		}
		if _, err := s.GetForStudent(ctx, sender, in.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("GetForStudent by another user = %v", err)
		}
		if r, err := s.GetForStudent(ctx, student, in.ID); err != nil || r.ID != in.ID {
			t.Fatalf("GetForStudent = %+v, %v", r, err)
		}
		if r, err := s.Pending(ctx, org, student); err != nil || r.ID != in.ID {
			t.Fatalf("Pending = %+v, %v", r, err)
		}
		if r, err := s.Lock(ctx, in.ID); err != nil || r.ID != in.ID {
			t.Fatalf("Lock = %+v, %v", r, err)
		}
	})

	t.Run("transitions, declines and the 90-day read", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, sender, student := w.org(t), w.user(t), w.user(t)
		r, err := s.Insert(ctx, newReq(t, w, org, sender, student))
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		next, ok, err := s.Transition(ctx, r.ID, messaging.StatusPending, messaging.StatusDeclined, &at)
		if err != nil || !ok || next.Status != messaging.StatusDeclined || next.RespondedAt == nil || !next.RespondedAt.Equal(at) {
			t.Fatalf("Transition = %+v, %v, %v", next, ok, err)
		}
		if _, ok, err := s.Transition(ctx, r.ID, messaging.StatusPending, messaging.StatusAccepted, &at); ok || err != nil {
			t.Fatalf("a second transition from pending = %v, %v", ok, err)
		}
		if _, err := s.Pending(ctx, org, student); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("Pending after decline = %v", err)
		}
		if ok, err := s.DeclinedSince(ctx, org, student, at.Add(-time.Hour)); err != nil || !ok {
			t.Fatalf("DeclinedSince(before) = %v, %v", ok, err)
		}
		if ok, err := s.DeclinedSince(ctx, org, student, at); err != nil || ok {
			t.Fatalf("DeclinedSince(at) = %v, %v (strictly after)", ok, err)
		}
		if ok, _ := s.DeclinedSince(ctx, w.org(t), student, at.Add(-time.Hour)); ok {
			t.Fatal("DeclinedSince of another company")
		}
		// A new pending request of the same pair is allowed after the decline.
		n, err := s.Insert(ctx, newReq(t, w, org, sender, student))
		if err != nil {
			t.Fatal(err)
		}
		if w2, ok, err := s.Transition(ctx, n.ID, messaging.StatusPending, messaging.StatusWithdrawn, nil); err != nil || !ok ||
			w2.RespondedAt != nil {
			t.Fatalf("withdraw = %+v, %v, %v", w2, ok, err)
		}
	})

	t.Run("lists: newest first, effective status, keyset", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, sender := w.org(t), w.user(t)
		student := w.user(t)
		var ids []uuid.UUID
		for range 3 {
			r := newReq(t, w, org, sender, w.user(t))
			if len(ids) == 0 {
				r.StudentUserID = student
				r.ExpiresAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond) // past due, still pending
			}
			if _, err := s.Insert(ctx, r); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, r.ID)
		}
		now := time.Now().UTC()
		all, err := s.ListByOrg(ctx, org, messaging.ListQuery{Now: now, Limit: 10})
		if err != nil || len(all) != 3 || all[0].ID != ids[2] || all[2].ID != ids[0] {
			t.Fatalf("ListByOrg = %v, %v", all, err)
		}
		pg, _ := s.ListByOrg(ctx, org, messaging.ListQuery{Now: now, Limit: 1, After: &ids[2]})
		if len(pg) != 1 || pg[0].ID != ids[1] {
			t.Fatalf("keyset page = %v", pg)
		}
		pend, _ := s.ListByOrg(ctx, org, messaging.ListQuery{Status: messaging.StatusPending, Now: now, Limit: 10})
		exp, _ := s.ListByOrg(ctx, org, messaging.ListQuery{Status: messaging.StatusExpired, Now: now, Limit: 10})
		if len(pend) != 2 || len(exp) != 1 || exp[0].ID != ids[0] {
			t.Fatalf("pending %d, expired %v", len(pend), exp)
		}
		mine, _ := s.ListByStudent(ctx, student, messaging.ListQuery{Now: now, Limit: 10})
		if len(mine) != 1 || mine[0].ID != ids[0] {
			t.Fatalf("ListByStudent = %v", mine)
		}
		if other, _ := s.ListByOrg(ctx, w.org(t), messaging.ListQuery{Now: now, Limit: 10}); len(other) != 0 {
			t.Fatalf("another org's list = %v", other)
		}
		due, err := s.DueForExpiry(ctx, now, 10)
		if err != nil || len(due) != 1 || due[0].ID != ids[0] {
			t.Fatalf("DueForExpiry = %v, %v", due, err)
		}
		if pc, err := s.PendingOfCompany(ctx, org, student); err != nil || len(pc) != 1 {
			t.Fatalf("PendingOfCompany = %v, %v", pc, err)
		}
	})

	t.Run("blocks", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		student, a, b := w.user(t), w.org(t), w.org(t)
		if ok, err := s.InsertBlock(ctx, student, a); err != nil || !ok {
			t.Fatalf("InsertBlock = %v, %v", ok, err)
		}
		if ok, err := s.InsertBlock(ctx, student, a); err != nil || ok {
			t.Fatalf("InsertBlock again = %v, %v", ok, err)
		}
		time.Sleep(2 * time.Millisecond)
		if _, err := s.InsertBlock(ctx, student, b); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.Blocked(ctx, student, a); !ok {
			t.Fatal("not blocked")
		}
		if ok, _ := s.Blocked(ctx, w.user(t), a); ok {
			t.Fatal("another student blocked")
		}
		all, err := s.Blocks(ctx, student, nil, 10)
		if err != nil || len(all) != 2 || all[0].CompanyOrgID != b || all[0].CreatedAt.IsZero() {
			t.Fatalf("Blocks = %+v, %v", all, err)
		}
		next, _ := s.Blocks(ctx, student, &messaging.BlockKey{CreatedAt: all[0].CreatedAt, OrgID: all[0].CompanyOrgID}, 10)
		if len(next) != 1 || next[0].CompanyOrgID != a {
			t.Fatalf("Blocks after = %+v", next)
		}
		if ok, err := s.DeleteBlock(ctx, student, a); err != nil || !ok {
			t.Fatalf("DeleteBlock = %v, %v", ok, err)
		}
		if ok, err := s.DeleteBlock(ctx, student, a); err != nil || ok {
			t.Fatalf("DeleteBlock again = %v, %v", ok, err)
		}
		if n, err := s.DeleteBlocks(ctx, student); err != nil || n != 1 {
			t.Fatalf("DeleteBlocks = %d, %v", n, err)
		}
	})

	t.Run("privacy and retention", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, sender, student := w.org(t), w.user(t), w.user(t)
		closed := newReq(t, w, org, sender, student)
		open := newReq(t, w, w.org(t), sender, student)
		for _, r := range []messaging.ContactRequest{closed, open} {
			if _, err := s.Insert(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if _, ok, err := s.Transition(ctx, closed.ID, messaging.StatusPending, messaging.StatusAccepted, &at); !ok || err != nil {
			t.Fatalf("accept = %v, %v", ok, err)
		}
		if got, _ := s.ReceivedBy(ctx, student); len(got) != 2 {
			t.Fatalf("ReceivedBy = %d", len(got))
		}
		if got, _ := s.SentBy(ctx, sender); len(got) != 2 {
			t.Fatalf("SentBy = %d", len(got))
		}
		later := time.Now().Add(time.Hour)
		if n, err := s.CountClosedBefore(ctx, later); err != nil || n != 1 {
			t.Fatalf("CountClosedBefore(later) = %d, %v", n, err)
		}
		if n, _ := s.CountClosedBefore(ctx, time.Now().Add(-time.Hour)); n != 0 {
			t.Fatalf("CountClosedBefore(earlier) = %d", n)
		}
		if n, err := s.PurgeClosed(ctx, later, 10); err != nil || n != 1 {
			t.Fatalf("PurgeClosed = %d, %v", n, err)
		}
		if _, err := s.GetForStudent(ctx, student, open.ID); err != nil {
			t.Fatalf("a pending request was purged: %v", err)
		}
		if n, err := s.DeleteReceived(ctx, student); err != nil || n != 1 {
			t.Fatalf("DeleteReceived = %d, %v", n, err)
		}
		if got, _ := s.ReceivedBy(ctx, student); len(got) != 0 {
			t.Fatalf("after erase = %d", len(got))
		}
	})
}
