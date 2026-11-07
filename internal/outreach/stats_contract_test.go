package outreach_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/outreach"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (outreach.Store, world) {
		return outreach.NewMemStore(time.Now), memWorld()
	})
}

// statsContract is the figures of an org's panel (BE-163): its active
// segments, largest stored estimate first (none last), bounded, and the
// dispatches created in a range by status with the sums of their stored
// counts; another org's rows are left out.
func statsContract(t *testing.T, newStore func(t *testing.T) (outreach.Store, world)) {
	t.Helper()
	t.Run("outreach stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		def := outreach.Definition{Uni: []uuid.UUID{uniA}}
		seg := func(o uuid.UUID, name string, count *int, archived bool) outreach.Segment {
			t.Helper()
			at := t0
			in := outreach.Segment{ID: uuid.NewV7(), OrgID: o, Name: name, Definition: def, AudienceKeys: def.Keys(),
				AudienceDimCount: def.DimCount(), LastCount: count, LastCountedAt: &at, CreatedBy: by}
			if archived {
				in.ArchivedAt = &at
			}
			got, err := s.Insert(ctx, in)
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			return got
		}
		small, large := seg(org, "Küçük", ptr(40), false), seg(org, "Büyük", ptr(120), false)
		none := seg(org, "Eşik altı", nil, false)
		seg(org, "Arşiv", ptr(500), true)
		seg(other, "Başkası", ptr(900), false)
		dispatch := func(o uuid.UUID, c *outreach.DispatchCounts) {
			t.Helper()
			d, err := s.InsertDispatch(ctx, outreach.Dispatch{ID: uuid.NewV7(), OrgID: o, PostID: w.post(t, o, by),
				AudienceKeys: []string{"uni:" + uniA.String()}, AudienceDimCount: 1, Channels: []string{"email"},
				TitleSnapshot: "Duyuru", DeepLink: "/posts/x", Commercial: true, Status: outreach.DispatchScheduled, CreatedBy: by})
			if err != nil {
				t.Fatalf("InsertDispatch: %v", err)
			}
			if c == nil {
				return
			}
			if _, ok, err := s.TransitionDispatch(ctx, o, d.ID, outreach.DispatchScheduled, outreach.DispatchRunning, nil, t0); err != nil || !ok {
				t.Fatalf("TransitionDispatch: %v %v", ok, err)
			}
			if ok, err := s.CompleteDispatch(ctx, o, d.ID, *c, t0); err != nil || !ok {
				t.Fatalf("CompleteDispatch: %v %v", ok, err)
			}
		}
		dispatch(org, &outreach.DispatchCounts{Eligible: ptr(40), Consented: ptr(30), Sent: ptr(30)})
		dispatch(org, &outreach.DispatchCounts{Eligible: ptr(20)})
		dispatch(org, nil)
		dispatch(other, &outreach.DispatchCounts{Eligible: ptr(900), Sent: ptr(900)})

		wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		c, err := s.OutreachStats(ctx, org, wideFrom, wideTo, 2, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.Active != 3 || len(c.Segments) != 2 || c.Segments[0].ID != large.ID || c.Segments[1].ID != small.ID {
			t.Fatalf("segments = %d active, %+v", c.Active, c.Segments)
		}
		if c.ByStatus[outreach.DispatchCompleted] != 2 || c.ByStatus[outreach.DispatchScheduled] != 1 || c.Eligible != 60 || c.Sent != 30 {
			t.Fatalf("dispatches = %+v", c)
		}
		all, err := s.OutreachStats(ctx, org, wideFrom, wideTo, 10, nil)
		if err != nil || len(all.Segments) != 3 || all.Segments[2].ID != none.ID {
			t.Fatalf("all segments = %+v, %v", all.Segments, err)
		}
		empty, err := s.OutreachStats(ctx, org, wideFrom, wideFrom.Add(time.Hour), 10, nil)
		if err != nil || len(empty.ByStatus) != 0 || empty.Sent != 0 || empty.Active != 3 {
			t.Fatalf("an empty range = %+v, %v", empty, err)
		}
	})
	t.Run("audience key groups", func(t *testing.T) {
		// BE-190: a row counts when its audience keys hold a key of every
		// group (OR inside a group, AND across the groups).
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		dept := uuid.NewV7()
		seg := func(name string, def outreach.Definition, count int) outreach.Segment {
			t.Helper()
			at := t0
			got, err := s.Insert(ctx, outreach.Segment{ID: uuid.NewV7(), OrgID: org, Name: name, Definition: def, AudienceKeys: def.Keys(),
				AudienceDimCount: def.DimCount(), LastCount: &count, LastCountedAt: &at, CreatedBy: by})
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			return got
		}
		y3 := []int16{3}
		uniSeg := seg("Üniversite A", outreach.Definition{Uni: []uuid.UUID{uniA}}, 300)
		deptSeg := seg("Bölüm", outreach.Definition{Dept: []uuid.UUID{dept}, Year: y3}, 200)
		bSeg := seg("Üniversite B", outreach.Definition{Uni: []uuid.UUID{uniB}, Year: y3}, 100)
		dispatch := func(def outreach.Definition, eligible int) {
			t.Helper()
			d, err := s.InsertDispatch(ctx, outreach.Dispatch{ID: uuid.NewV7(), OrgID: org, PostID: w.post(t, org, by),
				AudienceKeys: def.Keys(), AudienceDimCount: def.DimCount(), Channels: []string{"email"}, TitleSnapshot: "Duyuru",
				DeepLink: "/posts/x", Commercial: true, Status: outreach.DispatchScheduled, CreatedBy: by})
			if err != nil {
				t.Fatalf("InsertDispatch: %v", err)
			}
			if _, ok, err := s.TransitionDispatch(ctx, org, d.ID, outreach.DispatchScheduled, outreach.DispatchRunning, nil, t0); err != nil || !ok {
				t.Fatalf("TransitionDispatch: %v %v", ok, err)
			}
			if ok, err := s.CompleteDispatch(ctx, org, d.ID, outreach.DispatchCounts{Eligible: ptr(eligible), Sent: ptr(eligible)}, t0); err != nil || !ok {
				t.Fatalf("CompleteDispatch: %v %v", ok, err)
			}
		}
		dispatch(outreach.Definition{Uni: []uuid.UUID{uniA}}, 70)
		dispatch(outreach.Definition{Dept: []uuid.UUID{dept}, Year: y3}, 40)
		dispatch(outreach.Definition{Uni: []uuid.UUID{uniB}}, 20)

		wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			name     string
			groups   [][]string
			segments []uuid.UUID
			eligible int
		}{
			{"university or its department", [][]string{{"uni:" + uniA.String(), "dept:" + dept.String()}},
				[]uuid.UUID{uniSeg.ID, deptSeg.ID}, 110},
			{"class year", [][]string{{"year:3"}}, []uuid.UUID{deptSeg.ID, bSeg.ID}, 40},
			{"university and class year", [][]string{{"uni:" + uniB.String()}, {"year:3"}}, []uuid.UUID{bSeg.ID}, 0},
			{"nothing named", [][]string{{"year:8"}}, nil, 0},
		} {
			c, err := s.OutreachStats(ctx, org, wideFrom, wideTo, 10, tc.groups)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]uuid.UUID, len(c.Segments))
			for i, sg := range c.Segments {
				got[i] = sg.ID
			}
			if !slices.Equal(got, tc.segments) || c.Active != len(tc.segments) || c.Eligible != tc.eligible {
				t.Fatalf("%s: segments %v (active %d), eligible %d; want %v, %d", tc.name, got, c.Active, c.Eligible, tc.segments, tc.eligible)
			}
		}
	})
}
