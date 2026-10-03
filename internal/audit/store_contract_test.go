package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/platform/db"
)

// storeFactory returns a fresh store and the transaction manager to use
// with it (db.NopTx for the in-memory store).
type storeFactory func(t *testing.T) (audit.Store, db.TxManager)

// runStoreContract is the one body that proves every audit.Store behaves
// the same (05 §13.3): the in-memory store in `make test`, the GORM store
// in `make test-integration`.
func runStoreContract(t *testing.T, newStore storeFactory) {
	t.Helper()
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	org, otherOrg, user := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	rec := func(i int, action string, o *uuid.UUID, meta string) audit.Record {
		u := user
		return audit.Record{ID: uuid.NewV7(), OccurredAt: at(i), ActorKind: "user", ActorUserID: &u, Action: action,
			Outcome: audit.Success, OrgID: o, ResourceType: "event", ResourceID: &u, IP: "192.0.2.1",
			UserAgent: "ua", RequestID: "req-1", Metadata: json.RawMessage(meta)}
	}
	insert := func(t *testing.T, s audit.Store, rs ...audit.Record) {
		t.Helper()
		for _, r := range rs {
			if err := s.Insert(t.Context(), r); err != nil {
				t.Fatalf("Insert() error = %v", err)
			}
		}
	}
	ids := func(rs []audit.Record) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.Action
		}
		return out
	}

	t.Run("lists filter, scope and page newest first", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		insert(t, s,
			rec(1, "events.event.create", &org, `{}`),
			rec(2, "events.event.update", &org, `{"sensitive": true}`),
			rec(3, "students.profile.read_admin", &otherOrg, `{"break_glass": true}`),
			rec(4, "events.event.delete", &org, `{"n": 1}`),
		)
		all, err := s.List(ctx, audit.Filter{}, nil, 10)
		if err != nil || len(all) != 4 || all[0].Action != "events.event.delete" || all[3].Seq >= all[0].Seq {
			t.Fatalf("List() = %v, %v", ids(all), err)
		}
		r := all[3]
		if r.IP != "192.0.2.1" || r.UserAgent != "ua" || r.RequestID != "req-1" || r.ChainSeq != nil ||
			!r.OccurredAt.Equal(at(1)) || r.ActorUserID == nil || *r.ActorUserID != user || string(r.Metadata) != "{}" {
			t.Fatalf("stored row = %+v", r)
		}
		orgRows, _ := s.ListOrg(ctx, org, audit.Filter{OrgID: &otherOrg}, nil, 10) // f.OrgID is ignored
		if len(orgRows) != 3 {
			t.Fatalf("ListOrg() = %v", ids(orgRows))
		}
		hidden, _ := s.ListOrg(ctx, org, audit.Filter{HideSensitive: true}, nil, 10)
		if len(hidden) != 2 {
			t.Fatalf("ListOrg(HideSensitive) = %v", ids(hidden))
		}
		bg, _ := s.List(ctx, audit.Filter{BreakGlass: ptr(true)}, nil, 10)
		nbg, _ := s.List(ctx, audit.Filter{BreakGlass: ptr(false)}, nil, 10)
		if len(bg) != 1 || len(nbg) != 3 {
			t.Fatalf("break_glass filter: %v / %v", ids(bg), ids(nbg))
		}
		win, _ := s.List(ctx, audit.Filter{From: at(2), To: at(4), Action: "events.event.update", Outcome: audit.Success,
			ActorUserID: &user, ResourceType: "event", ResourceID: &user}, nil, 10)
		if len(win) != 1 {
			t.Fatalf("combined filter = %v", ids(win))
		}
		p1, _ := s.List(ctx, audit.Filter{}, nil, 2)
		p2, _ := s.List(ctx, audit.Filter{}, &audit.After{At: p1[1].OccurredAt, ID: p1[1].ID}, 2)
		if len(p2) != 2 || p2[0].ID == p1[1].ID || !p2[0].OccurredAt.Before(p1[1].OccurredAt) {
			t.Fatalf("keyset pages %v then %v", ids(p1), ids(p2))
		}
	})

	t.Run("own security events", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		u := user
		other := uuid.NewV7()
		bgMe := `{"break_glass": true, "subject_user_id": "` + u.String() + `"}`
		bgOther := `{"break_glass": true, "subject_user_id": "` + other.String() + `"}`
		staff := rec(4, "students.profile.read_admin", nil, bgMe)
		staff.ActorUserID = &other
		staff2 := rec(5, "students.profile.read_admin", nil, bgOther)
		staff2.ActorUserID = &other
		insert(t, s, rec(1, "identity.session.create", nil, `{}`), rec(2, "consent.record.create", nil, `{}`),
			rec(3, "events.registration.create", nil, `{}`), staff, staff2)
		got, err := s.ListOwn(t.Context(), u, nil, 10)
		if err != nil || len(got) != 3 || got[0].Action != "students.profile.read_admin" {
			t.Fatalf("ListOwn() = %v, %v", ids(got), err)
		}
	})

	t.Run("chain primitives", func(t *testing.T) {
		t.Parallel()
		s, tx := newStore(t)
		ctx := t.Context()
		insert(t, s, rec(1, "a.b.c", nil, `{}`), rec(2, "a.b.d", nil, `{}`), rec(3, "a.b.e", nil, `{}`))
		if oldest, ok, err := s.OldestUnchained(ctx); err != nil || !ok || !oldest.Equal(at(1)) {
			t.Fatalf("OldestUnchained() = %v, %v, %v", oldest, ok, err)
		}
		h1, h2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
		var first []audit.Record
		err := tx.WithTx(ctx, func(ctx context.Context) error {
			head, err := s.BeginChain(ctx)
			if err != nil {
				return err
			}
			if head.Seq != 0 || !bytes.Equal(head.Hash, make([]byte, 32)) {
				t.Errorf("empty chain head = %+v", head)
			}
			if first, err = s.Unchained(ctx, 2); err != nil {
				return err
			}
			if err := s.SetChain(ctx, first[0].ID, 1, head.Hash, h1, at(10)); err != nil {
				return err
			}
			return s.SetChain(ctx, first[1].ID, 2, h1, h2, at(11))
		})
		if err != nil || len(first) != 2 || first[0].Action != "a.b.c" {
			t.Fatalf("chain tx: %v (%v)", err, ids(first))
		}
		err = tx.WithTx(ctx, func(ctx context.Context) error {
			head, err := s.BeginChain(ctx)
			if err == nil && (head.Seq != 2 || !bytes.Equal(head.Hash, h2)) {
				t.Errorf("head = %+v, want seq 2", head)
			}
			if err == nil {
				err = s.SetChain(ctx, first[0].ID, 3, h2, h1, at(12)) // already chained
			}
			return err
		})
		if err == nil {
			t.Fatal("re-chaining a chained row succeeded")
		}
		chained, _ := s.Chained(ctx, 1, 10)
		if len(chained) != 1 || *chained[0].ChainSeq != 2 || !bytes.Equal(chained[0].PrevHash, h1) || !chained[0].ChainedAt.Equal(at(11)) {
			t.Fatalf("Chained(after 1) = %+v", chained)
		}
		if head, _ := s.HeadAt(ctx, at(11)); head.Seq != 1 || !bytes.Equal(head.Hash, h1) {
			t.Fatalf("HeadAt(before the second link) = %+v", head)
		}
		if head, _ := s.HeadAt(ctx, at(1)); head.Seq != 0 {
			t.Fatalf("HeadAt(before any link) = %+v", head)
		}
		if oldest, ok, _ := s.OldestUnchained(ctx); !ok || !oldest.Equal(at(3)) {
			t.Fatalf("OldestUnchained() after chaining = %v %v", oldest, ok)
		}
	})

	t.Run("daily roots", func(t *testing.T) {
		t.Parallel()
		s, tx := newStore(t)
		ctx := t.Context()
		r1 := audit.DailyRoot{Day: day0, LastChainSeq: 7, RootHash: bytes.Repeat([]byte{7}, 32), Signature: []byte("sig"), KeyID: "k1", ExternalRef: "audit-roots/a.json"}
		r2 := audit.DailyRoot{Day: day0.AddDays(2), LastChainSeq: 9, RootHash: bytes.Repeat([]byte{9}, 32), Signature: []byte("sig"), KeyID: "k1", ExternalRef: "audit-roots/b.json"}
		if _, ok, err := s.LastRoot(ctx); ok || err != nil {
			t.Fatalf("LastRoot() on empty = %v, %v", ok, err)
		}
		for _, r := range []audit.DailyRoot{r2, r1} {
			if err := s.InsertRoot(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.InsertRoot(ctx, r1); !errors.Is(err, audit.ErrRootExists) {
			t.Fatalf("duplicate day: %v, want ErrRootExists", err)
		}
		last, ok, _ := s.LastRoot(ctx)
		before, ok2, _ := s.RootBefore(ctx, day0.AddDays(2))
		rng, _ := s.Roots(ctx, day0, day0.AddDays(1))
		if !ok || last.Day != r2.Day || !ok2 || before.Day != day0 || before.LastChainSeq != 7 || len(rng) != 1 || rng[0].KeyID != "k1" {
			t.Fatalf("LastRoot %+v, RootBefore %+v, Roots %+v", last, before, rng)
		}
		// With its rows purged, the chain continues from the newest root.
		_ = tx.WithTx(ctx, func(ctx context.Context) error {
			head, err := s.BeginChain(ctx)
			if err != nil || head.Seq != 9 || !bytes.Equal(head.Hash, r2.RootHash) {
				t.Errorf("BeginChain() after purge = %+v, %v; want the root of seq 9", head, err)
			}
			return err
		})
	})
}

func TestMemStore_Contract(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(*testing.T) (audit.Store, db.TxManager) { return audit.NewMemStore(), db.NopTx{} })
}
