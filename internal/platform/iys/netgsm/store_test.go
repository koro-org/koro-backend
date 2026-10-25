package netgsm_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/iys"
	"koro/internal/platform/iys/netgsm"
)

// TestMemStore_Contract: the in-memory store behaves like the PostgreSQL
// one (store_integration_test.go runs the same cases on it).
func TestMemStore_Contract(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(*testing.T) netgsm.Store { return netgsm.NewMemStore() })
}

func key(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hex[(n>>(i%8))&0xf]
	}
	b[0], b[1] = hex[n%16], hex[(n/16)%16]
	return string(b)
}

// runStoreContract is the behaviour every Store shows.
func runStoreContract(t *testing.T, newStore func(t *testing.T) netgsm.Store) {
	t.Helper()
	t0 := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	const feed, other = "netgsm:700123/700124", "netgsm:700123/700999"

	t.Run("append de-duplicates per feed, changes list in order", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := t.Context()
		ada := uuid.NewV7()
		events := []netgsm.Event{
			{Key: key(1), Kind: netgsm.KindChange, UserID: ada, Type: iys.Email, Status: iys.Refused, Source: "IYS_WEB",
				ConsentAt: t0, RecordedAt: t0.Add(time.Second), TransactionID: "aa11", ReceivedAt: t0.Add(time.Minute)},
			{Key: key(2), Kind: netgsm.KindChange, Type: iys.Email, Status: iys.Refused, Source: "IYS_MOBIL",
				RecordedAt: t0, ReceivedAt: t0.Add(2 * time.Minute)}, // a stranger's marker: no user, no consent date
			{Key: key(3), Kind: netgsm.KindResult, Ref: "ref-1", SubmitID: "sub-1", UserID: ada, Type: iys.Email,
				Status: iys.Approved, Source: "HS_WEB", ConsentAt: t0, Result: netgsm.ResultSuccess, TransactionID: "bb22",
				RecordedAt: t0, ReceivedAt: t0.Add(3 * time.Minute)},
			{Key: key(4), Kind: netgsm.KindChange, UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "IYS_3338",
				ConsentAt: t0.Add(time.Hour), ReceivedAt: t0.Add(time.Hour)},
		}
		if n, err := s.Append(ctx, feed, events); err != nil || n != 4 {
			t.Fatalf("Append = %d, %v", n, err)
		}
		if n, err := s.Append(ctx, feed, events[:2]); err != nil || n != 0 {
			t.Fatalf("Append again = %d, %v; want a no-op", n, err)
		}
		if n, err := s.Append(ctx, other, events[:1]); err != nil || n != 1 {
			t.Fatalf("Append to another feed = %d, %v", n, err)
		}
		got, err := s.Changes(ctx, feed, 0, t0.Add(30*time.Minute), 10)
		if err != nil || len(got) != 2 {
			t.Fatalf("Changes = %+v, %v; want the two settled changes", got, err)
		}
		a, m := got[0], got[1]
		if a.Seq <= 0 || m.Seq <= a.Seq || a.UserID != ada || a.Key != key(1) || a.Source != "IYS_WEB" || !a.ConsentAt.Equal(t0) ||
			!a.RecordedAt.Equal(t0.Add(time.Second)) || a.TransactionID != "aa11" || !a.ReceivedAt.Equal(t0.Add(time.Minute)) ||
			a.Kind != netgsm.KindChange || a.Status != iys.Refused || a.Type != iys.Email {
			t.Fatalf("first change = %+v", a)
		}
		if m.UserID != uuid.Nil() || !m.ConsentAt.IsZero() || m.Ref != "" {
			t.Fatalf("marker = %+v", m)
		}
		if page, err := s.Changes(ctx, feed, a.Seq, t0.Add(2*time.Hour), 1); err != nil || len(page) != 1 || page[0].Seq != m.Seq {
			t.Fatalf("Changes after %d limit 1 = %+v, %v", a.Seq, page, err)
		}
		if rest, err := s.Changes(ctx, feed, m.Seq, t0.Add(2*time.Hour), 10); err != nil || len(rest) != 1 || rest[0].Key != key(4) {
			t.Fatalf("Changes after the marker = %+v, %v", rest, err)
		}
	})

	t.Run("result finds the latest answer by reference or by its values", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := t.Context()
		ada, bea := uuid.NewV7(), uuid.NewV7()
		if _, err := s.Append(ctx, feed, []netgsm.Event{
			{Key: key(11), Kind: netgsm.KindResult, Ref: "ref-a", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
				ConsentAt: t0, Result: netgsm.ResultFailure, ErrCode: "H103", ReceivedAt: t0},
			{Key: key(12), Kind: netgsm.KindResult, Ref: "ref-a", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
				ConsentAt: t0, Result: netgsm.ResultSuccess, TransactionID: "cc33", RecordedAt: t0, ReceivedAt: t0.Add(time.Minute)},
			{Key: key(13), Kind: netgsm.KindResult, UserID: bea, Type: iys.Email, Status: iys.Refused, Source: "HS_EPOSTA",
				ConsentAt: t0, Result: netgsm.ResultFailure, ErrCode: "H174", ReceivedAt: t0}, // no user_refcode
		}); err != nil {
			t.Fatal(err)
		}
		r, ok, err := s.Result(ctx, feed, netgsm.ResultQuery{Ref: "ref-a"})
		if err != nil || !ok || r.Result != netgsm.ResultSuccess || r.TransactionID != "cc33" {
			t.Fatalf("Result(ref-a) = %+v, %v, %v; want the latest answer", r, ok, err)
		}
		q := netgsm.ResultQuery{Ref: "ref-b", UserID: bea, Type: iys.Email, Status: iys.Refused, ConsentAt: t0}
		if r, ok, err := s.Result(ctx, feed, q); err != nil || !ok || r.ErrCode != "H174" || r.Ref != "" {
			t.Fatalf("Result by values = %+v, %v, %v", r, ok, err)
		}
		for name, q := range map[string]netgsm.ResultQuery{
			"another date":   {Ref: "ref-b", UserID: bea, Type: iys.Email, Status: iys.Refused, ConsentAt: t0.Add(time.Second)},
			"another status": {Ref: "ref-b", UserID: bea, Type: iys.Email, Status: iys.Approved, ConsentAt: t0},
			"no reference":   {UserID: bea, Type: iys.Email, Status: iys.Refused, ConsentAt: t0},
		} {
			if r, ok, err := s.Result(ctx, feed, q); err != nil || ok {
				t.Errorf("%s: Result = %+v, %v, %v; want none", name, r, ok, err)
			}
		}
		if _, ok, err := s.Result(ctx, other, netgsm.ResultQuery{Ref: "ref-a"}); err != nil || ok {
			t.Fatalf("Result of another feed = %v, %v", ok, err)
		}
	})

	t.Run("purge deletes what is older than the cutoff, in batches", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := t.Context()
		var evs []netgsm.Event
		for i := range 5 {
			evs = append(evs, netgsm.Event{Key: key(20 + i), Kind: netgsm.KindChange, UserID: uuid.NewV7(), Type: iys.Email,
				Status: iys.Refused, ConsentAt: t0, ReceivedAt: t0.Add(time.Duration(i) * time.Hour)})
		}
		if _, err := s.Append(ctx, feed, evs); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Purge(ctx, t0.Add(3*time.Hour), 2); err != nil || n != 2 {
			t.Fatalf("Purge(limit 2) = %d, %v", n, err)
		}
		if n, err := s.Purge(ctx, t0.Add(3*time.Hour), 10); err != nil || n != 1 {
			t.Fatalf("Purge = %d, %v; want the third old one", n, err)
		}
		left, err := s.Changes(ctx, feed, 0, t0.Add(24*time.Hour), 10)
		if err != nil || len(left) != 2 || left[0].Key != key(23) {
			t.Fatalf("left = %+v, %v", left, err)
		}
	})
}
