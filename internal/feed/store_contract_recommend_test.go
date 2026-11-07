package feed_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
	"koro/internal/platform/civil"
)

func TestStore_RecommendContract_Mem(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(*testing.T) (feed.Store, world) {
		s := feed.NewMemStore(func() time.Time { return t0 })
		return s, memWorld(s)
	})
}

// recommendContract is the opportunity read of the recommendations
// (BE-191): published opportunity calls the reader may see (the feed's
// visibility), published since a time, without a deadline or with one on
// or after today, of no org the reader muted, neither hidden, liked nor
// saved by the reader, matching an author org, the reader's class year
// (eligible_years) or the popularity floor (likes or saves), newest first.
func recommendContract(t *testing.T, newStore func(t *testing.T) (feed.Store, world)) {
	t.Helper()
	insert := func(t *testing.T, s feed.Store, p feed.Post) feed.Post {
		t.Helper()
		got, err := s.InsertPost(t.Context(), p)
		if err != nil {
			t.Fatalf("InsertPost: %v", err)
		}
		return got
	}
	ids := func(ps []feed.Post) []uuid.UUID {
		out := []uuid.UUID{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	today := civil.Date{Year: 2026, Month: time.October, Day: 5}
	at := func(i int) time.Time { return pub0.Add(time.Duration(i) * time.Minute) }

	t.Run("opportunities: visible, open, recent, untouched, matching", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club, company, other, muted, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.org(t, feed.AuthorClub),
			w.org(t, feed.AuthorClub), w.user(t)
		reader := w.user(t)
		call := func(org uuid.UUID, kind feed.AuthorKind, i int, opp string, edit func(*feed.Post)) feed.Post {
			t.Helper()
			p := post(org, kind, by)
			p.Kind, p.Opportunity = feed.KindOpportunityCall, feed.JSON(opp)
			p = publishedAt(p, at(i))
			if edit != nil {
				edit(&p)
			}
			return insert(t, s, p)
		}
		const (
			open    = `{"type":"internship","apply_via":"external_url","external_url":"https://a.example.test","deadline":"2026-10-10"}`
			noLimit = `{"type":"volunteer","apply_via":"external_url","external_url":"https://b.example.test"}`
			expired = `{"type":"job","apply_via":"external_url","external_url":"https://c.example.test","deadline":"2026-10-04"}`
			dueNow  = `{"type":"project","apply_via":"external_url","external_url":"https://d.example.test","deadline":"2026-10-05"}`
			years   = `{"type":"internship","apply_via":"external_url","external_url":"https://e.example.test","eligible_years":[3,4]}`
			perks   = `{"type":"internship","apply_via":"contact_request","deadline":"2026-10-20"}`
		)
		followed := call(club, feed.AuthorClub, 1, open, nil)
		noDeadline := call(club, feed.AuthorClub, 2, noLimit, nil)
		call(club, feed.AuthorClub, 3, expired, nil) // the deadline is over
		due := call(club, feed.AuthorClub, 4, dueNow, nil)
		call(club, feed.AuthorClub, -1, open, nil) // published before since
		forYear := call(other, feed.AuthorClub, 5, years, nil)
		ad := call(company, feed.AuthorCompany, 6, perks, nil)
		saved := call(club, feed.AuthorClub, 7, open, nil)
		liked := call(club, feed.AuthorClub, 8, open, nil)
		hidden := call(club, feed.AuthorClub, 9, open, nil)
		call(muted, feed.AuthorClub, 10, open, nil)
		fol := call(other, feed.AuthorClub, 11, open, func(p *feed.Post) { p.AudienceKind = feed.AudienceFollowers })
		popularSaves := call(other, feed.AuthorClub, 12, open, nil)
		seg := call(other, feed.AuthorClub, 13, open, func(p *feed.Post) {
			p.AudienceKind, p.AudienceKeys, p.AudienceDimCount = feed.AudienceSegment, []string{"year:3"}, 1
			p.AudienceSnapshot = feed.JSON(`{"year":[3]}`)
		})
		insert(t, s, publishedAt(post(club, feed.AuthorClub, by), at(14))) // a text post is no call
		draft := post(club, feed.AuthorClub, by)
		draft.Kind, draft.Opportunity = feed.KindOpportunityCall, feed.JSON(open)
		insert(t, s, draft) // a draft is never one
		for range feed.PopularThreshold {
			fan := w.user(t)
			if _, err := s.AddLike(ctx, fan, ad.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddSave(ctx, fan, popularSaves.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.AddSave(ctx, reader, saved.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddLike(ctx, reader, liked.ID); err != nil {
			t.Fatal(err)
		}
		w.hide(t, reader, hidden.ID)
		w.mute(t, reader, muted)

		r := feed.Reader{UserID: &reader, Keys: []string{"all"}}
		list := func(f feed.OpportunityFilter, limit int) []uuid.UUID {
			t.Helper()
			f.Since, f.Today = at(0), today
			ps, err := s.RecommendableOpportunities(ctx, f, limit)
			if err != nil {
				t.Fatalf("RecommendableOpportunities: %v", err)
			}
			return ids(ps)
		}
		if got := list(feed.OpportunityFilter{Reader: r, OrgIDs: []uuid.UUID{club, muted}}, 10); !slices.Equal(got, []uuid.UUID{due.ID, noDeadline.ID, followed.ID}) {
			t.Fatalf("by org = %v (expired, old, saved, liked, hidden, muted, text and draft left out)", got)
		}
		if got := list(feed.OpportunityFilter{Reader: r, OrgIDs: []uuid.UUID{club}}, 1); !slices.Equal(got, []uuid.UUID{due.ID}) {
			t.Fatalf("limit 1 = %v", got)
		}
		if got := list(feed.OpportunityFilter{Reader: r, ClassYear: 3}, 10); !slices.Equal(got, []uuid.UUID{forYear.ID}) {
			t.Fatalf("class year 3 = %v", got)
		}
		if got := list(feed.OpportunityFilter{Reader: r, ClassYear: 2}, 10); len(got) != 0 {
			t.Fatalf("class year 2 = %v", got)
		}
		if got := list(feed.OpportunityFilter{Reader: r, MinInterest: feed.PopularThreshold}, 10); !slices.Equal(got, []uuid.UUID{popularSaves.ID, ad.ID}) {
			t.Fatalf("popular = %v (10 saves, 10 likes)", got)
		}
		if got := list(feed.OpportunityFilter{Reader: r, MinInterest: feed.PopularThreshold + 1}, 10); len(got) != 0 {
			t.Fatalf("popular above the counts = %v", got)
		}
		minor := r
		minor.Minor = true
		if got := list(feed.OpportunityFilter{Reader: minor, MinInterest: feed.PopularThreshold}, 10); !slices.Equal(got, []uuid.UUID{popularSaves.ID}) {
			t.Fatalf("minor popular = %v (no commercial call)", got)
		}
		// Visibility: a followers call needs a follow, a segment call the keys.
		if got := list(feed.OpportunityFilter{Reader: r, OrgIDs: []uuid.UUID{other}}, 10); !slices.Equal(got, []uuid.UUID{popularSaves.ID, forYear.ID}) {
			t.Fatalf("other's calls for a stranger = %v", got)
		}
		w.follow(t, reader, other)
		third := feed.Reader{UserID: &reader, Keys: []string{"all", "year:3"}}
		if got := list(feed.OpportunityFilter{Reader: third, OrgIDs: []uuid.UUID{other}}, 10); !slices.Equal(got, []uuid.UUID{seg.ID, popularSaves.ID, fol.ID, forYear.ID}) {
			t.Fatalf("other's calls for a third-year follower = %v", got)
		}
		// No matcher, no signed-in reader: nothing.
		if got := list(feed.OpportunityFilter{Reader: r}, 10); len(got) != 0 {
			t.Fatalf("no matcher = %v", got)
		}
		if got := list(feed.OpportunityFilter{Reader: feed.Reader{Keys: []string{"all"}}, OrgIDs: []uuid.UUID{club}}, 10); len(got) != 0 {
			t.Fatalf("anonymous = %v", got)
		}
		// Another reader's likes and saves are not this reader's.
		someone := w.user(t)
		sr := feed.Reader{UserID: &someone, Keys: []string{"all"}}
		if got := list(feed.OpportunityFilter{Reader: sr, OrgIDs: []uuid.UUID{club}}, 10); !slices.Equal(got,
			[]uuid.UUID{hidden.ID, liked.ID, saved.ID, due.ID, noDeadline.ID, followed.ID}) {
			t.Fatalf("another reader = %v", got)
		}
	})
}
