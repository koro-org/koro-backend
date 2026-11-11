package ingest_test

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/ingest"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// t0 is the tests' clock (Europe/Istanbul 12:00 on 2026-11-11).
var t0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// world creates the parent rows the stores need (universities, clubs: FKs
// in PostgreSQL; free IDs in memory).
type world struct {
	uni  func(t *testing.T) uuid.UUID
	club func(t *testing.T, uni uuid.UUID) uuid.UUID
}

var seq atomic.Int64

func next() string { return strconv.FormatInt(seq.Add(1), 10) }

func memWorld() world {
	return world{
		uni:  func(*testing.T) uuid.UUID { return uuid.NewV7() },
		club: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
	}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (ingest.Store, world) {
		return ingest.NewMemStore(clock.NewFake(t0).Now), memWorld()
	})
}

func src(uni uuid.UUID, kind ingest.Kind, adapter ingest.AdapterName) ingest.Source {
	n := next()
	return ingest.Source{ID: uuid.NewV7(), Kind: kind, Adapter: adapter, URL: "https://h" + n + ".example.org/p",
		Host: "h" + n + ".example.org", Label: "Kaynak " + n, UniversityID: uni, IntervalSeconds: 6 * 3600}
}

func fp(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func importedEvent(uni uuid.UUID, club *uuid.UUID, start time.Time) ingest.ImportedEvent {
	n := next()
	return ingest.ImportedEvent{ID: uuid.NewV7(), UniversityID: uni, ClubOrgID: club, Status: ingest.StatusImported,
		Title: "Etkinlik " + n, StartsAt: start, Category: "talk", SourceURL: "https://e.example.org/" + n,
		Evidence: "Etkinlik " + n + " 11 Kasım", Confidence: 0.9, Fingerprint: fp(n), ExpiresAt: start.Add(8 * 24 * time.Hour),
		LastSeenAt: t0}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Status() != status {
		t.Fatalf("error = %v, want %d", err, status)
	}
}

func storeContract(t *testing.T, mk func(t *testing.T) (ingest.Store, world)) {
	t.Run("sources", func(t *testing.T) {
		t.Parallel()
		st, w := mk(t)
		ctx := t.Context()
		uni := w.uni(t)
		club := w.club(t, uni)
		now := t0.Truncate(time.Microsecond)

		a := src(uni, ingest.KindEvents, ingest.AdapterHTMLPage)
		a.Enabled, a.NextRunAt, a.ClubOrgID = true, &now, &club
		row, created, err := st.InsertSource(ctx, a)
		if err != nil || !created || row.ID != a.ID || row.CreatedAt.IsZero() {
			t.Fatalf("insert = %+v %v %v", row, created, err)
		}
		dup := src(uni, ingest.KindEvents, ingest.AdapterHTMLPage)
		dup.URL = a.URL
		if row, created, err := st.InsertSource(ctx, dup); err != nil || created || row.ID != a.ID {
			t.Fatalf("same URL = %v %v %v, want the existing row", row.ID, created, err)
		}
		if _, err := st.GetSource(ctx, uuid.NewV7()); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("missing = %v", err)
		}

		later := now.Add(time.Hour)
		b := src(uni, ingest.KindClubList, ingest.AdapterClubListYTU)
		b.Enabled, b.NextRunAt = true, &later
		c := src(uni, ingest.KindEvents, ingest.AdapterRSS) // disabled
		for _, s := range []ingest.Source{b, c} {
			if _, _, err := st.InsertSource(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		due, err := st.DueSources(ctx, now, 10)
		if err != nil || len(due) != 1 || due[0].ID != a.ID {
			t.Fatalf("due = %+v %v", due, err)
		}
		if due, _ := st.DueSources(ctx, later, 10); len(due) != 2 || due[0].ID != a.ID {
			t.Fatalf("due later = %d", len(due))
		}

		got, _ := st.GetSource(ctx, a.ID)
		fetched := now.Add(-time.Minute)
		status, yield := 200, 3
		got.LastFetchedAt, got.LastStatus, got.LastYield, got.FailCount = &fetched, &status, &yield, 2
		got.ContentHash, got.ETag = fp("body"), new("\"v1\"")
		if err := st.UpdateSource(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, _ := st.GetSource(ctx, a.ID)
		if back.FailCount != 2 || *back.LastStatus != 200 || *back.ETag != "\"v1\"" || len(back.ContentHash) != 32 ||
			!back.LastFetchedAt.Equal(fetched) {
			t.Fatalf("update = %+v", back)
		}
		bad := back
		reason := ingest.PausedAdmin
		bad.PausedReason = &reason // enabled and paused: CHECK
		wantStatus(t, st.UpdateSource(ctx, bad), 422)

		hosts, err := st.HostLastFetched(ctx, []string{a.Host, b.Host})
		if err != nil || !hosts[a.Host].Equal(fetched) || len(hosts) != 1 {
			t.Fatalf("hosts = %v %v", hosts, err)
		}
		list, err := st.ListSources(ctx, ingest.SourceFilter{UniversityID: &uni}, nil, 2)
		if err != nil || len(list) != 2 || list[0].ID != a.ID {
			t.Fatalf("list = %d %v", len(list), err)
		}
		if rest, _ := st.ListSources(ctx, ingest.SourceFilter{UniversityID: &uni}, &list[1].ID, 10); len(rest) != 1 {
			t.Fatalf("page 2 = %d", len(rest))
		}
		off := false
		if l, _ := st.ListSources(ctx, ingest.SourceFilter{Enabled: &off}, nil, 50); !containsSource(l, c.ID) || containsSource(l, a.ID) {
			t.Fatal("enabled filter")
		}
		if n, err := st.PauseClubSources(ctx, club, ingest.PausedClaimed); err != nil || n != 1 {
			t.Fatalf("pause = %d %v", n, err)
		}
		if p, _ := st.GetSource(ctx, a.ID); p.Enabled || p.PausedReason == nil || *p.PausedReason != ingest.PausedClaimed {
			t.Fatalf("paused = %+v", p)
		}

		// A sitemap's one-off page, fetched long ago, goes with its runs.
		child := src(uni, ingest.KindEvents, ingest.AdapterHTMLPage)
		child.ParentID, child.IntervalSeconds = &c.ID, 0
		if _, _, err := st.InsertSource(ctx, child); err != nil {
			t.Fatal(err)
		}
		cs, _ := st.GetSource(ctx, child.ID)
		old := now.Add(-40 * 24 * time.Hour)
		cs.LastFetchedAt = &old
		if err := st.UpdateSource(ctx, cs); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertRun(ctx, ingest.Run{ID: uuid.NewV7(), SourceID: child.ID, StartedAt: old, FinishedAt: old,
			Outcome: ingest.OutcomeOK}); err != nil {
			t.Fatal(err)
		}
		if n, err := st.DeleteFetchedChildren(ctx, now.Add(-30*24*time.Hour), 10); err != nil || n != 1 {
			t.Fatalf("children = %d %v", n, err)
		}
		if runs, _ := st.ListRuns(ctx, child.ID, 10); len(runs) != 0 {
			t.Fatal("the child's runs stayed")
		}
	})

	t.Run("runs", func(t *testing.T) {
		t.Parallel()
		st, w := mk(t)
		ctx := t.Context()
		s := src(w.uni(t), ingest.KindEvents, ingest.AdapterHTMLPage)
		if _, _, err := st.InsertSource(ctx, s); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			at := t0.Add(time.Duration(i) * time.Hour)
			code := "network"
			if err := st.InsertRun(ctx, ingest.Run{ID: uuid.NewV7(), SourceID: s.ID, StartedAt: at, FinishedAt: at.Add(time.Second),
				Outcome: ingest.OutcomeFetchError, ErrorCode: &code, InputTokens: 10}); err != nil {
				t.Fatal(err)
			}
		}
		runs, err := st.ListRuns(ctx, s.ID, 2)
		if err != nil || len(runs) != 2 || !runs[0].StartedAt.After(runs[1].StartedAt) || runs[0].InputTokens != 10 {
			t.Fatalf("runs = %+v %v", runs, err)
		}
		if n, err := st.DeleteRunsBefore(ctx, t0.Add(90*time.Minute), 10); err != nil || n != 2 {
			t.Fatalf("delete = %d %v", n, err)
		}
		if err := st.InsertRun(ctx, ingest.Run{ID: uuid.NewV7(), SourceID: s.ID, StartedAt: t0, FinishedAt: t0.Add(-time.Second),
			Outcome: ingest.OutcomeOK}); err == nil {
			t.Fatal("a run that ends before it starts was stored")
		}
	})

	t.Run("clubs", func(t *testing.T) {
		t.Parallel()
		st, w := mk(t)
		ctx := t.Context()
		uni := w.uni(t)
		c1, c2 := w.club(t, uni), w.club(t, uni)
		s := src(uni, ingest.KindClubList, ingest.AdapterClubListBOUN)
		if _, _, err := st.InsertSource(ctx, s); err != nil {
			t.Fatal(err)
		}
		email, label := "bilim@example.org", "Bilim"
		first := t0.Add(-48 * time.Hour)
		if err := st.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: c1, UniversityID: uni, SourceID: &s.ID,
			NameKey: "bilim", SourceCategory: &label, NeedsReview: true, ContactEmail: &email, FirstSeenAt: first, LastSeenAt: first}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: c2, UniversityID: uni, SourceID: &s.ID,
			NameKey: "dans", FirstSeenAt: first, LastSeenAt: first}); err != nil {
			t.Fatal(err)
		}
		// A second sighting keeps first_seen_at, needs_review and the address.
		if err := st.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: c1, UniversityID: uni, SourceID: &s.ID,
			NameKey: "bilim", FirstSeenAt: t0, LastSeenAt: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetImportedClub(ctx, c1)
		if err != nil || !got.NeedsReview || !got.FirstSeenAt.Equal(first) || !got.LastSeenAt.Equal(t0) ||
			got.ContactEmail == nil || *got.ContactEmail != email {
			t.Fatalf("club = %+v %v", got, err)
		}
		if f, err := st.FindImportedClub(ctx, uni, "bilim"); err != nil || f.ClubOrgID != c1 {
			t.Fatalf("find = %+v %v", f, err)
		}
		if _, err := st.FindImportedClub(ctx, uni, "yok"); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("find missing = %v", err)
		}
		if n, err := st.MarkMissingClubs(ctx, s.ID, t0.Add(-time.Hour), t0); err != nil || n != 1 {
			t.Fatalf("missing = %d %v", n, err)
		}
		if c, _ := st.GetImportedClub(ctx, c2); c.MissingSince == nil {
			t.Fatal("the dropped club is not marked")
		}
		yes := true
		if l, err := st.ListImportedClubs(ctx, ingest.ImportedClubFilter{UniversityID: &uni, NeedsReview: &yes}, nil, 10); err != nil ||
			len(l) != 1 || l[0].ClubOrgID != c1 {
			t.Fatalf("review list = %+v %v", l, err)
		}
		if err := st.MarkClubReviewed(ctx, c1); err != nil {
			t.Fatal(err)
		}
		if l, _ := st.ListImportedClubs(ctx, ingest.ImportedClubFilter{UniversityID: &uni, NeedsReview: &yes}, nil, 10); len(l) != 0 {
			t.Fatal("reviewed club still listed")
		}
		if err := st.MarkClubReviewed(ctx, uuid.NewV7()); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("review missing = %v", err)
		}
		// Another club cannot take a key of the university.
		if err := st.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: w.club(t, uni), UniversityID: uni,
			NameKey: "bilim", FirstSeenAt: t0, LastSeenAt: t0}); err == nil {
			t.Fatal("a second club took the key")
		}
	})

	t.Run("events", func(t *testing.T) {
		t.Parallel()
		st, w := mk(t)
		ctx := t.Context()
		uni := w.uni(t)
		club := w.club(t, uni)
		e1 := importedEvent(uni, &club, t0.Add(24*time.Hour))
		e2 := importedEvent(uni, nil, t0.Add(48*time.Hour))
		past := importedEvent(uni, &club, t0.Add(-72*time.Hour))
		past.ExpiresAt = t0.Add(-time.Hour)
		for _, e := range []ingest.ImportedEvent{e1, e2, past} {
			if err := st.InsertEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		dup := importedEvent(uni, nil, t0.Add(time.Hour))
		dup.Fingerprint = e1.Fingerprint
		wantStatus(t, st.InsertEvent(ctx, dup), 409)
		if got, err := st.EventByFingerprint(ctx, e1.Fingerprint); err != nil || got.ID != e1.ID {
			t.Fatalf("by fingerprint = %v %v", got.ID, err)
		}
		end := e1.StartsAt.Add(2 * time.Hour)
		loc := "Kültür Merkezi"
		e1.EndsAt, e1.LocationText, e1.Category, e1.LastSeenAt = &end, &loc, "culture", t0.Add(time.Hour)
		if err := st.RefreshEvent(ctx, e1); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.GetEvent(ctx, e1.ID); got.EndsAt == nil || *got.LocationText != loc || got.Category != "culture" {
			t.Fatalf("refresh = %+v", got)
		}
		now := t0
		page, err := st.ListEvents(ctx, ingest.EventFilter{Statuses: []ingest.Status{ingest.StatusImported}, UniversityID: &uni,
			EndsAfter: &now}, nil, 1)
		if err != nil || len(page) != 1 || page[0].ID != e1.ID {
			t.Fatalf("page 1 = %+v %v", page, err)
		}
		rest, _ := st.ListEvents(ctx, ingest.EventFilter{Statuses: []ingest.Status{ingest.StatusImported}, UniversityID: &uni,
			EndsAfter: &now}, &ingest.EventAfter{StartsAt: page[0].StartsAt, ID: page[0].ID}, 10)
		if len(rest) != 1 || rest[0].ID != e2.ID {
			t.Fatalf("page 2 = %+v", rest)
		}
		if l, _ := st.ListEvents(ctx, ingest.EventFilter{ClubOrgID: &club}, nil, 10); len(l) != 2 {
			t.Fatalf("club events = %d", len(l))
		}
		if n, err := st.CountClubEvents(ctx, club, ingest.StatusImported, now); err != nil || n != 1 {
			t.Fatalf("count = %d %v", n, err)
		}

		hidden := ingest.HiddenByRequest
		if err := st.SetEventStatus(ctx, e2.ID, ingest.StatusHidden, &hidden, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.GetEvent(ctx, e2.ID); got.Status != ingest.StatusHidden || *got.HiddenReason != hidden {
			t.Fatalf("hidden = %+v", got)
		}
		wantStatus(t, st.SetEventStatus(ctx, e2.ID, ingest.StatusHidden, nil, nil), 422)       // hidden without a reason
		wantStatus(t, st.SetEventStatus(ctx, e2.ID, ingest.StatusImported, &hidden, nil), 422) // a reason without hidden
		if err := st.SetEventStatus(ctx, e2.ID, ingest.StatusRejected, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.SetEventStatus(ctx, uuid.NewV7(), ingest.StatusRejected, nil, nil); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("status of a missing event = %v", err)
		}

		if n, err := st.DeleteExpiredEvents(ctx, t0, 10); err != nil || n != 1 {
			t.Fatalf("expired = %d %v", n, err)
		}
		if _, err := st.GetEvent(ctx, past.ID); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("expired event = %v", err)
		}
		if err := st.DeleteEvent(ctx, e1.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.LockEvent(ctx, e1.ID); !errors.Is(err, ingest.ErrNoRow) {
			t.Fatalf("deleted = %v", err)
		}
	})
}

func containsSource(l []ingest.Source, id uuid.UUID) bool {
	for _, s := range l {
		if s.ID == id {
			return true
		}
	}
	return false
}
