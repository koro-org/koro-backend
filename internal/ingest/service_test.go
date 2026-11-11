package ingest_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/ingest"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

const (
	bounList   = "https://kultursanat.bogazici.edu.tr/tr/pages/kulupler-ve-topluluklar/2482"
	metuEvents = "https://kim.metu.edu.tr/tr/etkinlikler"
	ituEvents  = "https://etkinlik.itu.edu.tr/public/embed/etkinlikler.aspx"
)

func on() *bool { v := true; return &v }

func (e *env) source(t *testing.T, in ingest.SourceInput) ingest.Source {
	t.Helper()
	if in.Enabled == nil {
		in.Enabled = on()
	}
	if in.Label == "" {
		in.Label = "Kaynak"
	}
	v, err := e.svc.AdminCreateSource(t.Context(), admin(t), in)
	if err != nil {
		t.Fatalf("AdminCreateSource(%s): %v", in.URL, err)
	}
	return v.Source
}

func (e *env) runs(t *testing.T, id uuid.UUID) []ingest.Run {
	t.Helper()
	v, err := e.svc.AdminGetSource(t.Context(), admin(t), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Runs
}

// TestPipeline_ClubList_OpensUnclaimedClubsOnce (BE-210): an official list
// opens unclaimed clubs through directory (a club the catalog has is
// matched, not duplicated), keeps the import record (the group address on
// the university's domain only, the special category for review), turns a
// club's own site into an events source and keeps a social profile as a
// link; the same body again is "unchanged"; a club the list drops is
// marked missing.
func TestPipeline_ClubList_OpensUnclaimedClubsOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "BOUN", "Boğaziçi Üniversitesi", "example.org")
	existing, err := e.dir.AdminCreateClub(ctx, admin(t), directory.ClubInput{UniversityID: uni.ID, Name: "Bilim Topluluğu",
		Category: directory.CategoryAcademic, JoinPolicy: directory.JoinOpen})
	if err != nil {
		t.Fatal(err)
	}
	src := e.source(t, ingest.SourceInput{Kind: ingest.KindClubList, Adapter: ingest.AdapterClubListBOUN, URL: bounList,
		UniversityID: uni.ID})
	e.fixture(t, bounList, "boun_clubs.html", "text/html")

	if r := e.run(t); r.Runs != 1 {
		t.Fatalf("tick = %+v", r)
	}
	runs := e.runs(t, src.ID)
	if len(runs) != 1 || runs[0].Outcome != ingest.OutcomeOK || runs[0].Found != 5 || runs[0].Created != 4 || runs[0].Updated != 1 {
		t.Fatalf("run = %+v", runs)
	}
	clubs, err := e.svc.AdminListImportedClubs(ctx, admin(t), ingest.ClubQuery{Filter: ingest.ImportedClubFilter{UniversityID: &uni.ID}})
	if err != nil || len(clubs.Items) != 5 {
		t.Fatalf("imported clubs = %+v %v", clubs, err)
	}
	byName := map[string]ingest.ClubItem{}
	for _, c := range clubs.Items {
		byName[c.Name] = c
	}
	if c := byName["Bilim Topluluğu"]; c.Club.ClubOrgID != existing.Club.OrgID || c.Club.NeedsReview {
		t.Fatalf("the catalog's club was not matched: %+v", c)
	}
	bilisim := byName["Bilişim Kulübü (COMPEC)"]
	if bilisim.Club.ContactEmail == nil || *bilisim.Club.ContactEmail != "compec@example.org" {
		t.Fatalf("bilişim = %+v", bilisim.Club)
	}
	club, err := e.dir.GetClub(ctx, bilisim.Club.ClubOrgID)
	if err != nil || club.ClaimStatus != directory.ClaimUnclaimed || club.IsSensitive {
		t.Fatalf("directory club = %+v %v", club, err)
	}
	din := byName["Din ve İnanç Araştırmaları Kulübü (DİAK)"]
	if dc, _ := e.dir.GetClub(ctx, din.Club.ClubOrgID); !din.Club.NeedsReview || dc.Category != directory.CategoryOther {
		t.Fatalf("a religious name must start as other for review: %+v %+v", din.Club, dc)
	}
	if doga := byName["Doğa Sporları Kulübü (BÜDOSK)"]; doga.Club.ContactEmail != nil {
		t.Fatalf("an address outside the university's domain was kept: %v", *doga.Club.ContactEmail)
	}
	if bric, _ := e.dir.GetClub(ctx, byName["Briç Kulübü (BRIC)"].Club.ClubOrgID); bric.SocialLinks["instagram"] == "" {
		t.Fatalf("briç links = %v", bric.SocialLinks)
	}
	sites, _ := e.svc.AdminListSources(ctx, admin(t), ingest.SourceQuery{Filter: ingest.SourceFilter{ClubOrgID: &bilisim.Club.ClubOrgID}})
	if len(sites.Items) != 1 || sites.Items[0].URL != "https://compec.example.com/" || sites.Items[0].Adapter != ingest.AdapterHTMLPage {
		t.Fatalf("club site sources = %+v", sites.Items)
	}
	all, _ := e.svc.AdminListSources(ctx, admin(t), ingest.SourceQuery{Limit: 100})
	for _, s := range all.Items {
		if strings.Contains(s.Host, "instagram") {
			t.Fatalf("a social profile became a source: %s", s.URL)
		}
	}

	// Due again: the same body is unchanged, nothing is opened twice.
	if _, err := e.svc.AdminRunSource(ctx, admin(t), src.ID); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if runs := e.runs(t, src.ID); runs[0].Outcome != ingest.OutcomeUnchanged {
		t.Fatalf("second run = %+v", runs[0])
	}
	// A list without the Briç club marks it missing; nothing is opened twice.
	e.clk.Advance(time.Hour)
	body := strings.Replace(mustRead(t, "boun_clubs.html"), "BRİÇ KULÜBÜ", "", 1)
	e.fetch.Set(bounList, ingest.Page{ContentType: "text/html", Body: []byte(body)})
	if _, err := e.svc.AdminRunSource(ctx, admin(t), src.ID); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if c, _ := e.svc.AdminListImportedClubs(ctx, admin(t), ingest.ClubQuery{}); len(c.Items) != 5 {
		t.Fatalf("clubs after the drop = %d", len(c.Items))
	}
	bric := byName["Briç Kulübü (BRIC)"].Club.ClubOrgID
	if c, err := e.store.GetImportedClub(ctx, bric); err != nil || c.MissingSince == nil {
		t.Fatalf("dropped club = %+v %v", c, err)
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := readFixture(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPipeline_Events_MaskValidateDedupe (BE-210): the page text reaches
// the extractor masked (no address, no name after a title); a candidate
// whose evidence is not in the page is dropped; a valid one is stored with
// its club (named on the page, found by the import record) and its source;
// the same event from a second source is one row; an unchanged body is
// never sent to the extractor again.
func TestPipeline_Events_MaskValidateDedupe(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "ODTU", "Orta Doğu Teknik Üniversitesi", "metu.example.org")
	clubID, _, err := e.dir.ImportClub(ctx, directory.ImportedClub{UniversityID: uni.ID, Name: "Sinema Topluluğu",
		Category: directory.CategoryArts, Job: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: clubID, UniversityID: uni.ID,
		NameKey: directory.ClubNameKey("Sinema Topluluğu"), FirstSeenAt: t0, LastSeenAt: t0}); err != nil {
		t.Fatal(err)
	}
	e.ext.script = func(req ingest.ExtractRequest) (ingest.Extraction, error) {
		return ingest.Extraction{InputTokens: 900, OutputTokens: 60, Candidates: []ingest.Candidate{
			{Doc: 0, Title: "Film Gösterimi", StartDate: "2026-11-11", StartTime: "18:00", EndTime: "21:30", YearStated: true,
				Location: "Fizik U-2", Category: "culture", Organizer: "SİNEMA TOPLULUĞU",
				Evidence: `11 KASIM 2026 18:00-21:30 / ETKİNLİK: "FİLM GÖSTERİMİ"`, Confidence: 0.95},
			{Doc: 0, Title: "Uydurma Etkinlik", StartDate: "2026-11-20", YearStated: true,
				Evidence: "20 KASIM 2026 UYDURMA ETKİNLİK", Confidence: 0.9},
		}}, nil
	}
	a := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage, URL: metuEvents, UniversityID: uni.ID})
	e.fixture(t, metuEvents, "metu_events.html", "text/html")
	e.run(t)
	runs := e.runs(t, a.ID)
	if len(runs) != 1 || runs[0].Outcome != ingest.OutcomeOK || runs[0].Created != 1 || runs[0].Rejected != 1 ||
		runs[0].InputTokens != 900 {
		t.Fatalf("run = %+v", runs)
	}
	page, err := e.svc.ListImportedEvents(ctx, authz.Anonymous(), ingest.EventQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("public list = %+v %v", page, err)
	}
	v := page.Items[0]
	if v.Event.Title != "Film Gösterimi" || v.Club == nil || v.Club.ID != clubID || v.SourceHost != "kim.metu.edu.tr" ||
		v.Event.EndsAt == nil || *v.Event.LocationText != "Fizik U-2" || v.Event.Category != "culture" {
		t.Fatalf("event = %+v club %+v", v.Event, v.Club)
	}
	istanbul, _ := time.LoadLocation("Europe/Istanbul")
	if want := time.Date(2026, 11, 11, 18, 0, 0, 0, istanbul); !v.Event.StartsAt.Equal(want) {
		t.Fatalf("start = %s, want %s", v.Event.StartsAt, want)
	}
	if !v.Event.ExpiresAt.Equal(v.Event.EndsAt.Add(7 * 24 * time.Hour)) {
		t.Fatalf("expires = %s", v.Event.ExpiresAt)
	}

	// The same event from a second page is one row (a later sighting).
	copyURL := "https://kim.metu.edu.tr/tr/etkinlikler-arsiv"
	e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage, URL: copyURL, UniversityID: uni.ID})
	e.fixture(t, copyURL, "metu_events.html", "text/html")
	e.run(t) // the host gap holds the second page until the next minute
	e.run(t)
	if l, _ := e.svc.AdminListImportedEvents(ctx, admin(t), ingest.AdminEventQuery{}); len(l.Items) != 1 {
		t.Fatalf("events after the second source = %d", len(l.Items))
	}
	calls := e.ext.count()
	if _, err := e.svc.AdminRunSource(ctx, admin(t), a.ID); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	e.run(t)
	if e.ext.count() != calls {
		t.Fatalf("an unchanged body went to the extractor (%d → %d calls)", calls, e.ext.count())
	}

	// The JSON list: its text is masked before extraction.
	e.ext.script = nil
	e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterJSONAPI, URL: ituEvents, UniversityID: uni.ID})
	e.fixture(t, ituEvents, "itu_events.json", "application/json")
	e.run(t)
	e.ext.mu.Lock()
	last := e.ext.docs[len(e.ext.docs)-1]
	e.ext.mu.Unlock()
	for _, d := range last {
		if strings.Contains(d.Text, "Deneme") || strings.Contains(d.Text, "aikido@") {
			t.Fatalf("unmasked text reached the extractor: %q", d.Text)
		}
	}
}

func readFixture(name string) (string, error) {
	b, err := os.ReadFile("testdata/" + name)
	return string(b), err
}

// TestPipeline_RobotsFailuresAndHostGap (BE-210): robots.txt refusing a URL
// pauses its source without a fetch; failures in a row pause a source with
// an alarm; one fetch per host per tick and never within the host gap.
func TestPipeline_RobotsFailuresAndHostGap(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{FailLimit: 2})
	ctx := t.Context()
	uni := e.university(t, "YTU", "Yıldız Teknik Üniversitesi", "yildiz.example.org")
	denied := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
		URL: "https://denied.example.org/etkinlikler", UniversityID: uni.ID})
	e.fetch.Disallow(denied.URL)
	broken := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
		URL: "https://broken.example.org/etkinlikler", UniversityID: uni.ID})
	e.fetch.Fail(broken.URL, errors.New("connection reset"))
	e.run(t)
	if s, _ := e.svc.AdminGetSource(ctx, admin(t), denied.ID); s.Source.Enabled || *s.Source.PausedReason != ingest.PausedRobots ||
		s.Runs[0].Outcome != ingest.OutcomeRobotsDenied {
		t.Fatalf("denied = %+v", s)
	}
	if slices.Contains(e.fetch.Fetched(), denied.URL) {
		t.Fatal("a disallowed URL was fetched")
	}
	e.clk.Advance(7 * time.Hour) // the next run of broken
	e.run(t)
	b, _ := e.svc.AdminGetSource(ctx, admin(t), broken.ID)
	if b.Source.Enabled || *b.Source.PausedReason != ingest.PausedFailures || b.Source.FailCount != 2 || len(b.Runs) != 2 {
		t.Fatalf("broken = %+v", b.Source)
	}
	if b.Runs[0].ErrorCode == nil || *b.Runs[0].ErrorCode != "network" {
		t.Fatalf("error code = %v", b.Runs[0].ErrorCode)
	}
	if !hasCounter(e.mod.Counters(), "alarms", "paused_failures", 1) {
		t.Fatalf("counters = %+v", e.mod.Counters())
	}

	// Two pages of one host: one per tick, never within the gap.
	p1 := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
		URL: "https://same.example.org/a", UniversityID: uni.ID})
	p2 := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
		URL: "https://same.example.org/b", UniversityID: uni.ID})
	e.fetch.Set(p1.URL, ingest.Page{ContentType: "text/html", Body: []byte("<main><p>a</p></main>")})
	e.fetch.Set(p2.URL, ingest.Page{ContentType: "text/html", Body: []byte("<main><p>b</p></main>")})
	r, err := e.svc.RunDue(ctx)
	if err != nil || r.Runs != 1 || r.Skipped != 1 {
		t.Fatalf("first tick = %+v %v", r, err)
	}
	e.clk.Advance(5 * time.Second)
	if r, _ := e.svc.RunDue(ctx); r.Runs != 0 || r.Skipped != 1 {
		t.Fatalf("within the gap = %+v", r)
	}
	e.clk.Advance(10 * time.Second)
	if r, _ := e.svc.RunDue(ctx); r.Runs != 1 {
		t.Fatalf("after the gap = %+v", r)
	}
}

func hasCounter(cs []ingest.Counter, name, label string, atLeast int64) bool {
	for _, c := range cs {
		if c.Name == name && c.Label == label && c.Value >= atLeast {
			return true
		}
	}
	return false
}

// TestPipeline_SitemapPagesFetchedOnce (BE-210): a sitemap adds its pages
// on its own host as one-off sources; each is fetched once.
func TestPipeline_SitemapPagesFetchedOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "BOUN2", "Boğaziçi Üniversitesi", "boun2.example.org")
	sm := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterSitemap,
		URL: "https://takvim.bogazici.edu.tr/tr/site/events", UniversityID: uni.ID})
	e.fixture(t, sm.URL, "boun_takvim_sitemap.xml", "text/xml")
	e.run(t)
	kids, _ := e.svc.AdminListSources(ctx, admin(t), ingest.SourceQuery{Filter: ingest.SourceFilter{Kind: ingest.KindEvents}, Limit: 50})
	var pages []ingest.Source
	for _, s := range kids.Items {
		if s.ParentID != nil {
			pages = append(pages, s)
		}
	}
	if len(pages) != 2 || pages[0].IntervalSeconds != 0 || !pages[0].Enabled {
		t.Fatalf("pages = %+v", pages)
	}
	for _, p := range pages {
		e.fetch.Set(p.URL, ingest.Page{ContentType: "text/html", Body: []byte("<main><p>Kış Konseri 15.12.2026</p></main>")})
	}
	e.run(t)
	e.run(t)
	e.run(t)
	for _, p := range pages {
		s, _ := e.svc.AdminGetSource(ctx, admin(t), p.ID)
		if len(s.Runs) != 1 || s.Source.NextRunAt != nil {
			t.Fatalf("one-off page %s: runs %d next %v", p.URL, len(s.Runs), s.Source.NextRunAt)
		}
	}
}

// TestPublic_RemovalRequestHidesAtOnce (BE-210): anyone sees the upcoming
// imported events; an anonymous removal request hides one at once
// (audited, reason only) and the public read is 404 until an admin
// restores it; an ended event is not public.
func TestPublic_RemovalRequestHidesAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "SU", "Sabancı Üniversitesi", "su.example.org")
	up := seedEvent(t, e, uni.ID, nil, t0.Add(48*time.Hour))
	past := seedEvent(t, e, uni.ID, nil, t0.Add(-72*time.Hour))
	anon := authz.Anonymous()
	if v, err := e.svc.GetImportedEvent(ctx, anon, up.ID); err != nil || v.Event.ID != up.ID {
		t.Fatalf("public read = %v", err)
	}
	if _, err := e.svc.GetImportedEvent(ctx, anon, past.ID); !errors.Is(err, ingest.ErrNotFound) {
		t.Fatalf("an ended event = %v", err)
	}
	problemtest.WantProblem(t, e.svc.RequestRemoval(ctx, anon, up.ID, "because"), 422, "validation_failed")
	if err := e.svc.RequestRemoval(ctx, anon, up.ID, ingest.RemovalPersonalData); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetImportedEvent(ctx, anon, up.ID); !errors.Is(err, ingest.ErrNotFound) {
		t.Fatalf("hidden event = %v", err)
	}
	if p, _ := e.svc.ListImportedEvents(ctx, anon, ingest.EventQuery{}); len(p.Items) != 0 {
		t.Fatalf("public list = %d", len(p.Items))
	}
	ev := e.audit.Of(authz.IngestImportedEventRequestRemoval)
	if len(ev) != 1 || ev[0].ActorUserID != nil || ev[0].Metadata["reason"] != "personal_data" {
		t.Fatalf("audit = %+v", ev)
	}
	hidden, _ := e.svc.AdminListImportedEvents(ctx, admin(t), ingest.AdminEventQuery{
		Filter: ingest.EventFilter{Statuses: []ingest.Status{ingest.StatusHidden}}})
	if len(hidden.Items) != 1 || *hidden.Items[0].Event.HiddenReason != ingest.HiddenByRequest {
		t.Fatalf("admin queue = %+v", hidden.Items)
	}
	if _, err := e.svc.AdminSetEventStatus(ctx, admin(t), up.ID, ingest.StatusImported); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetImportedEvent(ctx, anon, up.ID); err != nil {
		t.Fatalf("restored = %v", err)
	}
	problemtest.WantErr(t, e.svc.AdminDeleteImportedEvent(ctx, member(uuid.NewV7()), up.ID), authz.ErrForbidden)
	if err := e.svc.AdminDeleteImportedEvent(ctx, admin(t), up.ID); err != nil {
		t.Fatal(err)
	}
}

func seedEvent(t *testing.T, e *env, uni uuid.UUID, club *uuid.UUID, start time.Time) ingest.ImportedEvent {
	t.Helper()
	ev := importedEvent(uni, club, start)
	ev.ExpiresAt = start.Add(7 * 24 * time.Hour)
	if err := e.store.InsertEvent(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestSuggestions_ClaimPromoteReject (BE-210): an approved claim pauses the
// club's sources and tells its new admin that suggestions wait; the club
// admin lists the club's imported events, promotes one to an ordinary
// draft (events decides with the admin's own actor) and rejects another;
// a member gets 403, an outsider 404, another club's event 404.
func TestSuggestions_ClaimPromoteReject(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "ITU", "İstanbul Teknik Üniversitesi", "itu.example.org")
	club, _, err := e.dir.ImportClub(ctx, directory.ImportedClub{UniversityID: uni.ID, Name: "Robotik Kulübü",
		Category: directory.CategoryTechnology, Job: "test"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := e.dir.ImportClub(ctx, directory.ImportedClub{UniversityID: uni.ID, Name: "Dans Kulübü",
		Category: directory.CategoryArts, Job: "test"})
	site := e.source(t, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
		URL: "https://robotik.example.org/", UniversityID: uni.ID, ClubOrgID: &club})
	a := seedEvent(t, e, uni.ID, &club, t0.Add(72*time.Hour))
	b := seedEvent(t, e, uni.ID, &club, t0.Add(96*time.Hour))
	foreign := seedEvent(t, e, uni.ID, &other, t0.Add(96*time.Hour))

	owner := uuid.NewV7()
	env, err := domainevent.New(domainevent.ClubClaimDecided{ClaimID: uuid.NewV7(), ClubOrgID: club, UserID: owner,
		Outcome: "approved"}, "club_claim", club)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mod.HandleClaimDecidedForTest(ctx, env); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.svc.AdminGetSource(ctx, admin(t), site.ID); s.Source.Enabled || *s.Source.PausedReason != ingest.PausedClaimed {
		t.Fatalf("site after the claim = %+v", s.Source)
	}
	if len(e.sender.msgs) != 1 || *e.sender.msgs[0].UserID != owner || e.sender.msgs[0].Template != "ingest.suggestions_ready" {
		t.Fatalf("notice = %+v", e.sender.msgs)
	}

	ca := clubAdmin(club)
	page, err := e.svc.ListSuggestions(ctx, ca, club, ingest.EventQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("suggestions = %+v %v", page, err)
	}
	problemtest.WantErr(t, func() error {
		_, err := e.svc.ListSuggestions(ctx, member(club), club, ingest.EventQuery{})
		return err
	}(), authz.ErrForbidden)
	problemtest.WantErr(t, func() error {
		_, err := e.svc.ListSuggestions(ctx, clubAdmin(other), club, ingest.EventQuery{})
		return err
	}(), authz.ErrNotFound)
	problemtest.WantErr(t, func() error { _, err := e.svc.PromoteSuggestion(ctx, member(club), club, a.ID); return err }(), authz.ErrForbidden)
	if _, err := e.svc.PromoteSuggestion(ctx, ca, club, foreign.ID); !errors.Is(err, ingest.ErrNotFound) {
		t.Fatalf("another club's event = %v", err)
	}

	v, err := e.svc.PromoteSuggestion(ctx, ca, club, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetEvent(ctx, a.ID)
	if got.Status != ingest.StatusPromoted || got.PromotedEventID == nil || *got.PromotedEventID != v.Event.ID {
		t.Fatalf("promoted = %+v", got)
	}
	in := e.drafts.inputs[0]
	if in.Title != a.Title || !in.StartsAt.Equal(a.StartsAt) || !in.EndsAt.After(in.StartsAt) || e.drafts.actors[0].UserID != ca.UserID {
		t.Fatalf("draft input = %+v", in)
	}
	if _, err := e.svc.PromoteSuggestion(ctx, ca, club, a.ID); !errors.Is(err, ingest.ErrInvalidState) {
		t.Fatalf("promote twice = %v", err)
	}
	if err := e.svc.RejectSuggestion(ctx, ca, club, b.ID); err != nil {
		t.Fatal(err)
	}
	if page, _ := e.svc.ListSuggestions(ctx, ca, club, ingest.EventQuery{}); len(page.Items) != 0 {
		t.Fatalf("suggestions left = %d", len(page.Items))
	}
	if len(e.audit.Of(authz.IngestSuggestionPromote)) != 1 || len(e.audit.Of(authz.IngestSuggestionReject)) != 1 {
		t.Fatal("suggestion decisions not audited")
	}
}

// TestAdmin_SourcesValidationPilotAndState (BE-210): a source URL the guard
// would not fetch is 422; the adapter must yield the kind; an adapter that
// never fetches cannot be enabled; the pilot set is added once; disabling
// records the admin's pause, enabling makes the source due; running a
// disabled source is 409; non-admins 403.
func TestAdmin_SourcesValidationPilotAndState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{SourceDefaultEnabled: false})
	ctx := t.Context()
	uni := e.university(t, "BOUN3", "Boğaziçi Üniversitesi", "boun3.example.org")
	pad := admin(t)
	for _, bad := range []string{"http://169.254.169.254/latest", "http://localhost:8080/", "ftp://x.example.org/", "https://u:p@x.example.org/"} {
		_, err := e.svc.AdminCreateSource(ctx, pad, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage,
			URL: bad, Label: "x", UniversityID: uni.ID})
		problemtest.WantProblem(t, err, 422, "ingest.url_not_allowed")
	}
	_, err := e.svc.AdminCreateSource(ctx, pad, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterClubListYTU,
		URL: "https://x.example.org/", Label: "x", UniversityID: uni.ID})
	problemtest.WantProblem(t, err, 422, "ingest.adapter_mismatch")
	_, err = e.svc.AdminCreateSource(ctx, pad, ingest.SourceInput{Kind: ingest.KindEvents, Adapter: ingest.AdapterInstagramGraph,
		URL: "https://y.example.org/", Label: "x", UniversityID: uni.ID, Enabled: on()})
	problemtest.WantProblem(t, err, 422, "ingest.not_fetchable")
	_, err = e.svc.AdminCreateSource(ctx, member(uuid.NewV7()), ingest.SourceInput{})
	problemtest.WantErr(t, err, authz.ErrForbidden)

	res, err := e.svc.AdminAddPilotSources(ctx, pad, ingest.PilotInput{Pilot: "boun", UniversityID: uni.ID})
	if err != nil || res.Created != 2 || res.Existing != 0 {
		t.Fatalf("pilot = %+v %v", res, err)
	}
	if res2, _ := e.svc.AdminAddPilotSources(ctx, pad, ingest.PilotInput{Pilot: "boun", UniversityID: uni.ID}); res2.Created != 0 || res2.Existing != 2 {
		t.Fatalf("pilot again = %+v", res2)
	}
	_, err = e.svc.AdminAddPilotSources(ctx, pad, ingest.PilotInput{Pilot: "harvard", UniversityID: uni.ID})
	problemtest.WantProblem(t, err, 422, "ingest.unknown_pilot")
	s := res.Sources[0]
	if s.Enabled || s.NextRunAt != nil {
		t.Fatalf("a prod-default source started enabled: %+v", s)
	}
	_, err = e.svc.AdminRunSource(ctx, pad, s.ID)
	problemtest.WantProblem(t, err, 409, "ingest.invalid_state")
	v, err := e.svc.AdminUpdateSource(ctx, pad, s.ID, ingest.SourcePatch{Enabled: on()})
	if err != nil || !v.Source.Enabled || v.Source.NextRunAt == nil {
		t.Fatalf("enable = %+v %v", v.Source, err)
	}
	off := false
	if v, err := e.svc.AdminUpdateSource(ctx, pad, s.ID, ingest.SourcePatch{Enabled: &off}); err != nil || v.Source.Enabled ||
		*v.Source.PausedReason != ingest.PausedAdmin {
		t.Fatalf("disable = %+v %v", v.Source, err)
	}
	if len(e.audit.Of(authz.IngestSourceUpdate)) != 2 || len(e.audit.Of(authz.IngestSourceCreate)) != 2 {
		t.Fatal("source writes not audited")
	}
}

// TestPurge_DeletesExpiredEventsAndOldRuns (BE-210): ingest.purge_expired
// deletes the imported events past expires_at and leaves the others.
func TestPurge_DeletesExpiredEventsAndOldRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	uni := e.university(t, "PRG", "Örnek Üniversitesi", "prg.example.org")
	gone := seedEvent(t, e, uni.ID, nil, t0.Add(-10*24*time.Hour))
	kept := seedEvent(t, e, uni.ID, nil, t0.Add(24*time.Hour))
	if err := e.mod.PurgeForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetEvent(ctx, gone.ID); !errors.Is(err, ingest.ErrNoRow) {
		t.Fatalf("expired event = %v", err)
	}
	if _, err := e.store.GetEvent(ctx, kept.ID); err != nil {
		t.Fatalf("kept event = %v", err)
	}
}
