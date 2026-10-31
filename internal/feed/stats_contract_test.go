package feed_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(*testing.T) (feed.Store, world) {
		s := feed.NewMemStore(func() time.Time { return t0 })
		return s, memWorld(s)
	})
}

// statsContract is the post counts of the panels (BE-163): posts by status
// and first published in a range, content reports by status and filed in
// a range (deltas: a fresh database may hold seed rows); one org's posts
// published in a range with their targeted share, likes and saves.
func statsContract(t *testing.T, newStore func(t *testing.T) (feed.Store, world)) {
	t.Helper()
	wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	insert := func(t *testing.T, s feed.Store, p feed.Post) feed.Post {
		t.Helper()
		got, err := s.InsertPost(t.Context(), p)
		if err != nil {
			t.Fatalf("InsertPost: %v", err)
		}
		return got
	}

	t.Run("content stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		before, err := s.ContentStats(ctx, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		org, by := w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		insert(t, s, post(org, feed.AuthorClub, by))
		if _, _, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: w.user(t), TargetType: feed.TargetPost,
			TargetID: p.ID, Reason: feed.ReasonSpam}); err != nil {
			t.Fatal(err)
		}
		after, err := s.ContentStats(ctx, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		for name, d := range map[string][2]int{
			"published":      {after.PostsByStatus[feed.StatusPublished] - before.PostsByStatus[feed.StatusPublished], 1},
			"draft":          {after.PostsByStatus[feed.StatusDraft] - before.PostsByStatus[feed.StatusDraft], 1},
			"published new":  {after.PublishedInRange - before.PublishedInRange, 1},
			"open reports":   {after.ReportsByStatus[feed.ReportOpen] - before.ReportsByStatus[feed.ReportOpen], 1},
			"reports in rng": {after.ReportsInRange - before.ReportsInRange, 1},
		} {
			if d[0] != d[1] {
				t.Errorf("%s: +%d, want +%d", name, d[0], d[1])
			}
		}
		empty, err := s.ContentStats(ctx, wideFrom, wideFrom.Add(time.Hour))
		if err != nil || empty.PublishedInRange != 0 || empty.ReportsInRange != 0 {
			t.Errorf("an empty range: %+v %v", empty, err)
		}
	})

	t.Run("org post stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		seg := post(org, feed.AuthorClub, by)
		seg.AudienceKind, seg.AudienceKeys, seg.AudienceDimCount = feed.AudienceSegment, []string{"uni:u1"}, 1
		seg.AudienceSnapshot = feed.JSON(`{"uni":["u1"]}`)
		insert(t, s, publishedAt(seg, pub0.Add(time.Hour)))
		insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0.AddDate(0, -2, 0))) // before the range
		insert(t, s, post(org, feed.AuthorClub, by))                                      // a draft
		insert(t, s, publishedAt(post(other, feed.AuthorClub, by), pub0))                 // another org
		for _, u := range []uuid.UUID{w.user(t), w.user(t)} {
			if _, err := s.AddLike(ctx, u, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.AddSave(ctx, w.user(t), p.ID); err != nil {
			t.Fatal(err)
		}
		st, err := s.OrgPostStats(ctx, org, pub0.AddDate(0, -1, 0), pub0.AddDate(0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		if want := (feed.OrgPostStats{Published: 2, Targeted: 1, Likes: 2, Saves: 1}); st != want {
			t.Fatalf("org post stats = %+v, want %+v", st, want)
		}
	})
}
