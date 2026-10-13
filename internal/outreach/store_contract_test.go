package outreach_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/outreach"
	"koro/internal/platform/db"
)

// world creates the parent rows a store needs: orgs and users (FKs in
// PostgreSQL; plain IDs in memory).
type world struct {
	org  func(t *testing.T) uuid.UUID
	user func(t *testing.T) uuid.UUID
	// post creates a post of org written by user (BE-035: a dispatch's FK).
	post func(t *testing.T, org, by uuid.UUID) uuid.UUID
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id, post: func(*testing.T, uuid.UUID, uuid.UUID) uuid.UUID { return uuid.NewV7() }}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (outreach.Store, world) {
		return outreach.NewMemStore(time.Now), memWorld()
	})
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (outreach.Store, world)) {
	t.Helper()
	dispatchContract(t, newStore)
	newSeg := func(org, by uuid.UUID, def outreach.Definition, count *int) outreach.Segment {
		at := t0
		return outreach.Segment{
			ID: uuid.NewV7(), OrgID: org, Name: "Hedef kitle", Definition: def, AudienceKeys: def.Keys(),
			AudienceDimCount: def.DimCount(), LastCount: count, LastCountedAt: &at, CreatedBy: by,
		}
	}
	def := outreach.Definition{Uni: []uuid.UUID{uniA, uniB}, Year: []int16{3, 4}, Club: []uuid.UUID{clubA}}

	t.Run("insert, get and org scope", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		in := newSeg(org, by, def, ptr(40))
		got, err := s.Insert(ctx, in)
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if got.ID != in.ID || got.CreatedAt.IsZero() || !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Fatalf("Insert returned %+v", got)
		}
		read, err := s.Get(ctx, org, in.ID)
		if err != nil || read.Name != in.Name || !slices.Equal(read.Definition.Uni, def.Uni) ||
			!slices.Equal(read.Definition.Year, def.Year) || !slices.Equal(read.Definition.Club, def.Club) ||
			read.Definition.Dept != nil || !slices.Equal(read.AudienceKeys, def.Keys()) || read.AudienceDimCount != 3 ||
			read.LastCount == nil || *read.LastCount != 40 || !read.LastCountedAt.Equal(t0) || read.CreatedBy != by ||
			read.ArchivedAt != nil {
			t.Fatalf("Get = %+v, %v", read, err)
		}
		if _, err := s.Get(ctx, other, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("Get under another org: %v", err)
		}
		if _, err := s.Lock(ctx, other, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("Lock under another org: %v", err)
		}
		if _, err := s.Get(ctx, uuid.Nil(), in.ID); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("Get without org: %v", err)
		}
		if l, err := s.Lock(ctx, org, in.ID); err != nil || l.ID != in.ID {
			t.Fatalf("Lock = %v, %v", l.ID, err)
		}
	})

	t.Run("checks", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		for name, mut := range map[string]func(*outreach.Segment){
			"last_count below 10": func(s *outreach.Segment) { s.LastCount = ptr(9) },
			"name too short":      func(s *outreach.Segment) { s.Name = "A" },
			"no dimension":        func(s *outreach.Segment) { s.AudienceDimCount = 0 },
			"five dimensions":     func(s *outreach.Segment) { s.AudienceDimCount = 5 },
		} {
			seg := newSeg(org, by, def, nil)
			mut(&seg)
			if _, err := s.Insert(ctx, seg); err == nil {
				t.Fatalf("%s: Insert accepted %+v", name, seg)
			}
		}
		ok, err := s.Insert(ctx, newSeg(org, by, def, nil))
		if err != nil {
			t.Fatal(err)
		}
		ok.LastCount = ptr(3)
		if _, err := s.Update(ctx, ok); err == nil {
			t.Fatal("Update accepted last_count 3")
		}
		if _, err := s.SetCount(ctx, org, ok.ID, ptr(5), t0); err == nil {
			t.Fatal("SetCount accepted 5")
		}
	})

	t.Run("update moves updated_at and stays in its org", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		seg, err := s.Insert(ctx, newSeg(org, by, def, nil))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // updated_at of the store's own clock
		next := seg
		next.Name, next.Definition = "Yeni ad", outreach.Definition{Dept: []uuid.UUID{uniA}}
		next.AudienceKeys, next.AudienceDimCount, next.LastCount = next.Definition.Keys(), 1, ptr(10)
		arch := t0.Add(time.Hour)
		next.ArchivedAt = &arch
		got, err := s.Update(ctx, next)
		if err != nil || got.Name != "Yeni ad" || got.AudienceDimCount != 1 || got.Definition.Uni != nil ||
			!slices.Equal(got.Definition.Dept, []uuid.UUID{uniA}) || *got.LastCount != 10 || got.ArchivedAt == nil ||
			!got.UpdatedAt.After(seg.UpdatedAt) || !got.CreatedAt.Equal(seg.CreatedAt) || got.CreatedBy != by {
			t.Fatalf("Update = %+v, %v", got, err)
		}
		moved := got
		moved.OrgID = other
		if _, err := s.Update(ctx, moved); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("Update under another org: %v", err)
		}
	})

	t.Run("list: org, archived, newest first, keyset", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		var ids []uuid.UUID
		for range 3 {
			seg, err := s.Insert(ctx, newSeg(org, by, def, nil))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, seg.ID)
		}
		archived := newSeg(org, by, def, nil)
		at := t0
		archived.ArchivedAt = &at
		if _, err := s.Insert(ctx, archived); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, newSeg(other, by, def, nil)); err != nil {
			t.Fatal(err)
		}
		page, err := s.List(ctx, org, false, nil, 2)
		if err != nil || len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
			t.Fatalf("List page 1 = %v, %v", idsOf(page), err)
		}
		page, err = s.List(ctx, org, false, &page[1].ID, 2)
		if err != nil || len(page) != 1 || page[0].ID != ids[0] {
			t.Fatalf("List page 2 = %v, %v", idsOf(page), err)
		}
		page, err = s.List(ctx, org, true, nil, 10)
		if err != nil || len(page) != 1 || page[0].ID != archived.ID {
			t.Fatalf("List archived = %v, %v", idsOf(page), err)
		}
		if _, err := s.List(ctx, uuid.Nil(), false, nil, 10); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("List without org: %v", err)
		}
	})

	t.Run("recount walk and set count", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		orgA, orgB, by := w.org(t), w.org(t), w.user(t)
		a, err := s.Insert(ctx, newSeg(orgA, by, def, ptr(20)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Insert(ctx, newSeg(orgB, by, def, nil))
		if err != nil {
			t.Fatal(err)
		}
		gone := newSeg(orgA, by, def, ptr(10))
		at := t0
		gone.ArchivedAt = &at
		if _, err := s.Insert(ctx, gone); err != nil {
			t.Fatal(err)
		}
		// The walk sees every org's active segments in id order; other
		// tests' rows may share a PostgreSQL database, so filter ours.
		var seen []uuid.UUID
		after := uuid.Nil()
		for {
			rows, err := s.ActiveAfter(ctx, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				break
			}
			after = rows[0].ID
			if rows[0].ArchivedAt != nil {
				t.Fatalf("ActiveAfter returned an archived segment %s", rows[0].ID)
			}
			if rows[0].ID == a.ID || rows[0].ID == b.ID || rows[0].ID == gone.ID {
				seen = append(seen, rows[0].ID)
			}
		}
		if !slices.Equal(seen, []uuid.UUID{a.ID, b.ID}) {
			t.Fatalf("ActiveAfter walk = %v, want %v", seen, []uuid.UUID{a.ID, b.ID})
		}
		later := t0.Add(24 * time.Hour)
		if ok, err := s.SetCount(ctx, orgA, a.ID, nil, later); err != nil || !ok {
			t.Fatalf("SetCount(nil) = %v, %v", ok, err)
		}
		if ok, err := s.SetCount(ctx, orgB, b.ID, ptr(30), later); err != nil || !ok {
			t.Fatalf("SetCount(30) = %v, %v", ok, err)
		}
		if ok, err := s.SetCount(ctx, orgB, a.ID, ptr(30), later); err != nil || ok {
			t.Fatalf("SetCount under another org = %v, %v", ok, err)
		}
		if ok, err := s.SetCount(ctx, orgA, gone.ID, ptr(30), later); err != nil || ok {
			t.Fatalf("SetCount of an archived segment = %v, %v", ok, err)
		}
		ra, _ := s.Get(ctx, orgA, a.ID)
		rb, _ := s.Get(ctx, orgB, b.ID)
		rg, _ := s.Get(ctx, orgA, gone.ID)
		if ra.LastCount != nil || !ra.LastCountedAt.Equal(later) || rb.LastCount == nil || *rb.LastCount != 30 ||
			rg.LastCount == nil || *rg.LastCount != 10 || !rg.LastCountedAt.Equal(t0) {
			t.Fatalf("after SetCount: a=%v b=%v gone=%v", ra.LastCount, rb.LastCount, rg.LastCount)
		}
	})
}

// dispatchContract is the outreach_dispatches part of the contract
// (BE-035); storeContract runs it.
func dispatchContract(t *testing.T, newStore func(t *testing.T) (outreach.Store, world)) {
	t.Helper()
	newDispatch := func(t *testing.T, w world, org, by uuid.UUID) outreach.Dispatch {
		t.Helper()
		return outreach.Dispatch{
			ID: uuid.NewV7(), OrgID: org, PostID: w.post(t, org, by), AudienceKeys: []string{"uni:" + uniA.String(), "year:3"},
			AudienceDimCount: 2, Channels: []string{"email", "push"}, TitleSnapshot: "Yaz stajı", DeepLink: "/posts/x",
			Commercial: true, Status: outreach.DispatchScheduled, CreatedBy: by,
		}
	}

	t.Run("dispatch: insert, get, post, org scope, one per post", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		in := newDispatch(t, w, org, by)
		got, err := s.InsertDispatch(ctx, in)
		if err != nil {
			t.Fatalf("InsertDispatch: %v", err)
		}
		if got.ID != in.ID || got.Status != outreach.DispatchScheduled || got.CreatedAt.IsZero() ||
			!slices.Equal([]string(got.Channels), []string{"email", "push"}) || got.StartedAt != nil || got.EligibleCount != nil {
			t.Fatalf("inserted %+v", got)
		}
		read, err := s.GetDispatch(ctx, org, in.ID)
		if err != nil || read.PostID != in.PostID || !slices.Equal([]string(read.AudienceKeys), []string(in.AudienceKeys)) ||
			read.AudienceDimCount != 2 || !read.Commercial || read.TitleSnapshot != "Yaz stajı" || read.DeepLink != "/posts/x" {
			t.Fatalf("GetDispatch = %+v, %v", read, err)
		}
		for _, lock := range []bool{false, true} {
			if d, err := s.DispatchOfPost(ctx, org, in.PostID, lock); err != nil || d.ID != in.ID {
				t.Fatalf("DispatchOfPost(lock %v) = %v, %v", lock, d.ID, err)
			}
			if _, err := s.DispatchOfPost(ctx, other, in.PostID, lock); !errors.Is(err, outreach.ErrNotFound) {
				t.Fatalf("DispatchOfPost under another org: %v", err)
			}
		}
		if _, err := s.GetDispatch(ctx, other, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("GetDispatch under another org: %v", err)
		}
		if _, err := s.LockDispatch(ctx, other, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("LockDispatch under another org: %v", err)
		}
		if _, err := s.GetDispatch(ctx, uuid.Nil(), in.ID); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("GetDispatch without org: %v", err)
		}
		if d, err := s.DispatchByID(ctx, in.ID); err != nil || d.OrgID != org {
			t.Fatalf("DispatchByID = %v, %v", d.OrgID, err)
		}
		dup := in
		dup.ID = uuid.NewV7()
		if _, err := s.InsertDispatch(ctx, dup); !errors.Is(err, outreach.ErrDispatchExists) {
			t.Fatalf("a second dispatch of the post: %v", err)
		}
		bad := newDispatch(t, w, org, by)
		bad.Channels = []string{"sms"}
		if _, err := s.InsertDispatch(ctx, bad); err == nil {
			t.Fatal("an sms channel passed the CHECK")
		}
		if err := s.DeleteDispatch(ctx, other, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("DeleteDispatch under another org: %v", err)
		}
		if err := s.DeleteDispatch(ctx, org, in.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetDispatch(ctx, org, in.ID); !errors.Is(err, outreach.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})

	t.Run("dispatch: claim, touch, transition, complete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		d, err := s.InsertDispatch(ctx, newDispatch(t, w, org, by))
		if err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := s.ClaimDispatch(ctx, d.ID, t0, time.Hour)
		if err != nil || !ok || claimed.Status != outreach.DispatchRunning || claimed.StartedAt == nil || !claimed.StartedAt.Equal(t0) {
			t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
		}
		if _, ok, err := s.ClaimDispatch(ctx, d.ID, t0, time.Hour); err != nil || ok {
			t.Fatalf("a fresh claim was taken over: %v, %v", ok, err)
		}
		if err := s.TouchDispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		// An expired claim (lease 0) is taken over; started_at stays.
		again, ok, err := s.ClaimDispatch(ctx, d.ID, t0.Add(time.Hour), 0)
		if err != nil || !ok || !again.StartedAt.Equal(t0) {
			t.Fatalf("takeover = %+v, %v, %v", again.StartedAt, ok, err)
		}
		if _, ok, err := s.TransitionDispatch(ctx, org, d.ID, outreach.DispatchScheduled, outreach.DispatchCancelled, nil, t0); err != nil || ok {
			t.Fatalf("transition from the wrong status = %v, %v", ok, err)
		}
		back, ok, err := s.TransitionDispatch(ctx, org, d.ID, outreach.DispatchRunning, outreach.DispatchScheduled, nil, time.Time{})
		if err != nil || !ok || back.Status != outreach.DispatchScheduled || back.CompletedAt != nil {
			t.Fatalf("back to scheduled = %+v, %v, %v", back, ok, err)
		}
		if _, _, err := s.ClaimDispatch(ctx, d.ID, t0, time.Hour); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.CompleteDispatch(ctx, w.org(t), d.ID, outreach.DispatchCounts{}, t0); err != nil || ok {
			t.Fatalf("complete under another org = %v, %v", ok, err)
		}
		if ok, err := s.CompleteDispatch(ctx, org, d.ID, outreach.DispatchCounts{Eligible: ptr(40), Sent: ptr(10)}, t0.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("complete = %v, %v", ok, err)
		}
		done, _ := s.GetDispatch(ctx, org, d.ID)
		if done.Status != outreach.DispatchCompleted || done.CompletedAt == nil || !done.CompletedAt.Equal(t0.Add(time.Minute)) ||
			done.EligibleCount == nil || *done.EligibleCount != 40 || done.ConsentedCount != nil || *done.SentCount != 10 {
			t.Fatalf("completed %+v", done)
		}
		if ok, err := s.CompleteDispatch(ctx, org, d.ID, outreach.DispatchCounts{}, t0); err != nil || ok {
			t.Fatalf("complete twice = %v, %v", ok, err)
		}
		if _, ok, err := s.ClaimDispatch(ctx, d.ID, t0, 0); err != nil || ok {
			t.Fatalf("a completed dispatch was claimed: %v, %v", ok, err)
		}
		// A terminal transition stamps completed_at and keeps the reason.
		c, err := s.InsertDispatch(ctx, newDispatch(t, w, org, by))
		if err != nil {
			t.Fatal(err)
		}
		reason := "outreach.post_gone"
		got, ok, err := s.TransitionDispatch(ctx, org, c.ID, outreach.DispatchScheduled, outreach.DispatchCancelled, &reason, t0)
		if err != nil || !ok || got.CompletedAt == nil || got.LastError == nil || *got.LastError != reason {
			t.Fatalf("cancel = %+v, %v, %v", got, ok, err)
		}
		if _, ok, err := s.ClaimDispatch(ctx, uuid.NewV7(), t0, 0); err != nil || ok {
			t.Fatalf("claim of a missing dispatch = %v, %v", ok, err)
		}
	})

	t.Run("dispatch: list, pending, purge", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		orgA, orgB, by := w.org(t), w.org(t), w.user(t)
		var a []outreach.Dispatch
		for range 3 {
			d, err := s.InsertDispatch(ctx, newDispatch(t, w, orgA, by))
			if err != nil {
				t.Fatal(err)
			}
			a = append(a, d)
		}
		b, err := s.InsertDispatch(ctx, newDispatch(t, w, orgB, by))
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.ListDispatches(ctx, orgA, nil, 2)
		if err != nil || len(page) != 2 || page[0].ID != a[2].ID || page[1].ID != a[1].ID {
			t.Fatalf("first page = %v, %v", len(page), err)
		}
		rest, err := s.ListDispatches(ctx, orgA, &page[1].ID, 2)
		if err != nil || len(rest) != 1 || rest[0].ID != a[0].ID {
			t.Fatalf("second page = %v, %v", len(rest), err)
		}
		// Pending: scheduled ones created before the cutoff; a running one with an expired claim.
		future := time.Now().Add(time.Hour)
		pend, err := s.PendingDispatches(ctx, future, time.Hour, uuid.Nil(), 10)
		if err != nil || len(pend) != 4 {
			t.Fatalf("pending = %d, %v", len(pend), err)
		}
		if pend, err := s.PendingDispatches(ctx, time.Now().Add(-time.Hour), time.Hour, uuid.Nil(), 10); err != nil || len(pend) != 0 {
			t.Fatalf("pending before creation = %d, %v", len(pend), err)
		}
		if _, ok, err := s.ClaimDispatch(ctx, b.ID, t0, time.Hour); err != nil || !ok {
			t.Fatal(err)
		}
		if pend, err := s.PendingDispatches(ctx, time.Now().Add(-time.Hour), 0, uuid.Nil(), 10); err != nil || len(pend) != 1 || pend[0].ID != b.ID {
			t.Fatalf("stale running = %v, %v", len(pend), err)
		}
		if pend, err := s.PendingDispatches(ctx, future, time.Hour, a[1].ID, 10); err != nil || len(pend) != 1 || pend[0].ID != a[2].ID {
			// a[2] after a[1]; b is running with a fresh claim
			t.Fatalf("pending after a[1] = %d, %v", len(pend), err)
		}
		// Purge: finished dispatches only.
		for _, d := range a[:2] {
			if _, ok, err := s.TransitionDispatch(ctx, orgA, d.ID, outreach.DispatchScheduled, outreach.DispatchCancelled, nil, t0); err != nil || !ok {
				t.Fatal(err)
			}
		}
		if n, err := s.PurgeDispatches(ctx, future, 0, true); err != nil || n != 2 {
			t.Fatalf("dry purge = %d, %v", n, err)
		}
		if n, err := s.PurgeDispatches(ctx, time.Now().Add(-time.Hour), 10, false); err != nil || n != 0 {
			t.Fatalf("purge before the cutoff = %d, %v", n, err)
		}
		if n, err := s.PurgeDispatches(ctx, future, 1, false); err != nil || n != 1 {
			t.Fatalf("purge batch of 1 = %d, %v", n, err)
		}
		if n, err := s.PurgeDispatches(ctx, future, 10, false); err != nil || n != 1 {
			t.Fatalf("purge rest = %d, %v", n, err)
		}
		if _, err := s.GetDispatch(ctx, orgA, a[2].ID); err != nil {
			t.Fatalf("the scheduled dispatch was purged: %v", err)
		}
	})
}

func idsOf(rows []outreach.Segment) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
