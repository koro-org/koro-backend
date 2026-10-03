package kvkk_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/kvkk"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// storeWorld is one fresh store; user creates a users row the FKs may point
// to (users belong to identity).
type storeWorld struct {
	s    kvkk.Store
	tx   db.TxManager
	user func(t *testing.T) uuid.UUID
	// file creates the files row an export_file_id points to (nil in the
	// in-memory world, which has no foreign keys).
	file func(t *testing.T, id uuid.UUID)
}

// storeContract is the contract both stores pass (05 §13.3): the in-memory
// one here, the GORM one against PostgreSQL in store_integration_test.go.
func storeContract(t *testing.T, newWorld func(t *testing.T) storeWorld) {
	t.Helper()
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	dsr := func(user *uuid.UUID, typ kvkk.Type, at time.Time) kvkk.DSR {
		return kvkk.DSR{ID: uuid.NewV7(), UserID: user, Type: typ, Channel: kvkk.ChannelInApp, Status: kvkk.StatusReceived,
			Details: "Ayrıntı", ReceivedAt: at, DueAt: at.Add(kvkk.LegalPeriod)}
	}

	t.Run("insert, get, own, update", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u, other := w.user(t), w.user(t)
		d := dsr(&u, kvkk.TypeExport, base)
		if err := w.s.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
		got, err := w.s.Get(ctx, d.ID)
		if err != nil || got.Details != "Ayrıntı" || !got.DueAt.Equal(base.Add(kvkk.LegalPeriod)) || *got.UserID != u ||
			got.Status != kvkk.StatusReceived || got.CreatedAt.IsZero() {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := w.s.GetOwn(ctx, d.ID, other); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("GetOwn(other) = %v", err)
		}
		if _, err := w.s.Get(ctx, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("Get(missing) = %v", err)
		}
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			l, err := w.s.Lock(ctx, d.ID)
			if err != nil {
				return err
			}
			at := base.Add(time.Hour)
			l.Status, l.CompletedAt, l.ResponseSummary, l.AssignedTo = kvkk.StatusCompleted, &at, "Yanıt", &other
			l.Details, l.Type = "ignored", kvkk.TypeInfo // immutable columns
			return w.s.Update(ctx, l)
		})
		if err != nil {
			t.Fatal(err)
		}
		got2, _ := w.s.GetOwn(ctx, d.ID, u)
		if got2.Status != kvkk.StatusCompleted || got2.ResponseSummary != "Yanıt" || *got2.AssignedTo != other ||
			got2.Details != "Ayrıntı" || got2.Type != kvkk.TypeExport || got2.UpdatedAt.Equal(got.UpdatedAt) {
			t.Fatalf("after Update = %+v", got2)
		}
	})

	t.Run("CHECK constraints", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u, a := w.user(t), w.user(t)
		if err := w.s.Insert(ctx, dsr(nil, kvkk.TypeInfo, base)); err == nil {
			t.Fatal("a request without user and address was stored")
		}
		d := dsr(&u, kvkk.TypeProfilingAccess, base)
		if err := w.s.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
		bad := d
		bad.SecondApprover = &a // without a first approver
		if err := w.s.Update(ctx, bad); err == nil {
			t.Fatal("second approval without a first was stored")
		}
		bad = d
		bad.FirstApprover, bad.SecondApprover = &a, &a
		if err := w.s.Update(ctx, bad); err == nil {
			t.Fatal("the same approver twice was stored")
		}
		at := base.Add(time.Hour)
		bad = d
		bad.Status, bad.CompletedAt = kvkk.StatusCompleted, &at
		if err := w.s.Update(ctx, bad); err == nil {
			t.Fatal("a profiling report completed without a second approval")
		}
		bad = d
		bad.Status, bad.CompletedAt = kvkk.StatusRejected, &at
		if err := w.s.Update(ctx, bad); err == nil {
			t.Fatal("a rejection without a reason was stored")
		}
	})

	t.Run("lists, open, due, exports", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u, v := w.user(t), w.user(t)
		a := dsr(&u, kvkk.TypeAccess, base)
		b := dsr(&u, kvkk.TypeErasure, base.Add(time.Hour))
		c := dsr(&v, kvkk.TypeAccess, base.Add(2*time.Hour))
		anon := dsr(nil, kvkk.TypeInfo, base.Add(3*time.Hour))
		anon.RequesterEmail, anon.RequesterName, anon.Channel = "basvuru@example.test", "Ayşe", kvkk.ChannelPost
		for _, d := range []kvkk.DSR{a, b, c, anon} {
			if err := w.s.Insert(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		own, _ := w.s.ListOwn(ctx, u, nil, 10)
		if len(own) != 2 || own[0].ID != b.ID || own[1].ID != a.ID {
			t.Fatalf("ListOwn = %v", ids(own))
		}
		page2, _ := w.s.ListOwn(ctx, u, &kvkk.After{At: own[0].ReceivedAt, ID: own[0].ID}, 10)
		if len(page2) != 1 || page2[0].ID != a.ID {
			t.Fatalf("ListOwn page 2 = %v", ids(page2))
		}
		all, _ := w.s.List(ctx, kvkk.AdminFilter{}, nil, 10)
		if !slices.Equal(ids(all), ids([]kvkk.DSR{a, b, c, anon})) {
			t.Fatalf("List = %v", ids(all))
		}
		typ := kvkk.TypeAccess
		f, _ := w.s.List(ctx, kvkk.AdminFilter{Type: &typ}, &kvkk.After{At: a.DueAt, ID: a.ID}, 10)
		if len(f) != 1 || f[0].ID != c.ID {
			t.Fatalf("List(type, after) = %v", ids(f))
		}
		got, _ := w.s.Get(ctx, anon.ID)
		if got.RequesterEmail != "basvuru@example.test" || got.UserID != nil {
			t.Fatalf("applicant without account = %+v", got)
		}
		if open, _ := w.s.HasOpen(ctx, u, kvkk.TypeErasure); !open {
			t.Fatal("HasOpen(erasure) = false")
		}
		if open, _ := w.s.HasOpen(ctx, v, kvkk.TypeErasure); open {
			t.Fatal("HasOpen(other user) = true")
		}
		if of, _ := w.s.AllOf(ctx, u); !slices.Equal(ids(of), ids([]kvkk.DSR{a, b})) {
			t.Fatalf("AllOf = %v", ids(of))
		}
		due, _ := w.s.DueBy(ctx, b.DueAt, nil, 10)
		if !slices.Equal(ids(due), ids([]kvkk.DSR{a, b})) {
			t.Fatalf("DueBy (inclusive) = %v", ids(due))
		}
		// An export file completed at the cutoff is purged.
		fileID := uuid.NewV7()
		if w.file != nil {
			w.file(t, fileID)
		}
		at := base.Add(time.Hour)
		cc, _ := w.s.Get(ctx, c.ID)
		cc.Status, cc.CompletedAt, cc.ExportFileID = kvkk.StatusCompleted, &at, &fileID
		if err := w.s.Update(ctx, cc); err != nil {
			t.Fatal(err)
		}
		if ex, _ := w.s.ExportsCompletedBefore(ctx, at.Add(-time.Second), uuid.Nil(), 10); len(ex) != 0 {
			t.Fatalf("exports before = %v", ids(ex))
		}
		if ex, _ := w.s.ExportsCompletedBefore(ctx, at, uuid.Nil(), 10); len(ex) != 1 || ex[0].ID != c.ID {
			t.Fatalf("exports at the cutoff = %v", ids(ex))
		}
		if due, _ := w.s.DueBy(ctx, c.DueAt, nil, 10); slices.Contains(ids(due), c.ID) {
			t.Fatal("DueBy lists a completed request")
		}
	})
}

func ids(rows []kvkk.DSR) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, d := range rows {
		out[i] = d.ID
	}
	return out
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) storeWorld {
		return storeWorld{s: kvkk.NewMemStore(time.Now), tx: db.NopTx{}, user: func(*testing.T) uuid.UUID { return uuid.NewV7() }}
	})
}
