package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/ingest"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

// useMemStore installs the in-memory ingest store, a fetcher without
// network and the fake extractor (05 §13.2). Only tickets of the ingest
// module edit this file (BE-001 R4).
func (w *ingestWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.fetcher = ingest.NewMemFetcher()
	w.extractor = ingest.FakeExtractor{}
	w.store = nil
	w.newStore = func(c *container) ingest.Store { return ingest.NewMemStore(c.infra.clock.Now) }
}

// TestWire_IngestBuildsJobsAndSubscription (BE-210): ingest builds over the
// in-memory stores, schedules ingest.fetch_due (every minute) and
// ingest.purge_expired (daily), subscribes club_claim.decided and wires
// events' drafts and notify's notices.
func TestWire_IngestBuildsJobsAndSubscription(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.ingest.svc == nil {
		t.Fatal("ingest did not set its service")
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "ingest.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	slices.Sort(jobs)
	want := []string{"ingest.fetch_due @every 1m low", "ingest.purge_expired 40 3 * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("jobs = %v, want %v", jobs, want)
	}
	found := false
	for _, s := range c.infra.subs.All() {
		if s.Name == ingest.PauseClaimedTaskName {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s subscription", ingest.PauseClaimedTaskName)
	}
	if err := c.ingest.metrics(obs.NewMetrics()); err != nil {
		t.Fatalf("metrics: %v", err)
	}
}

// TestWire_IngestExtractorSelectedByConfig: each INGEST_EXTRACTOR_PROVIDER
// name has one constructor; claude without a key is the fake extractor;
// claude with a key is the Claude adapter (no call is made).
func TestWire_IngestExtractorSelectedByConfig(t *testing.T) {
	t.Parallel()
	log := obs.Discard()
	if got, want := extractors(config.Ingest{}, log).names(), slices.Sorted(slices.Values(config.ExtractorProviders())); !slices.Equal(got, want) {
		t.Fatalf("wired extractors %v, configuration accepts %v", got, want)
	}
	x, err := extractors(config.Ingest{}, log).pick("INGEST_EXTRACTOR_PROVIDER", config.ExtractorClaude)
	if _, ok := x.(ingest.FakeExtractor); err != nil || !ok {
		t.Fatalf("claude without a key = %T %v, want the fake extractor", x, err)
	}
	x, err = extractors(config.Ingest{AnthropicAPIKey: config.NewSecret("test-key-not-a-real-one"),
		ExtractorModel: "claude-haiku-5-5"}, log).pick("INGEST_EXTRACTOR_PROVIDER", config.ExtractorClaude)
	if _, ok := x.(*ingest.ClaudeExtractor); err != nil || !ok {
		t.Fatalf("claude with a key = %T %v", x, err)
	}
	if _, err := extractors(config.Ingest{}, log).pick("INGEST_EXTRACTOR_PROVIDER", "gpt"); err == nil {
		t.Fatal("an unknown extractor was accepted")
	}
}

// TestWire_IngestPromotesThroughEvents (BE-210): over the real wiring a club
// of an official list (directory.ClubImports) gets its imported event as a
// suggestion, and its club_admin's promotion creates an ordinary events
// draft (events.event.create with the admin's own actor) in the same
// transaction that marks the imported event promoted.
func TestWire_IngestPromotesThroughEvents(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := c.directory.svc.ImportReference(ctx, directory.ReferenceData{Universities: []directory.RefUniversity{
		{YokCode: "ING-W", Name: "Ingest Wiring Üniversitesi", City: "Ankara", Type: directory.UniversityState}}}); err != nil {
		t.Fatal(err)
	}
	unis, err := c.directory.svc.ListUniversities(ctx, authz.Anonymous(), directory.UniversityQuery{Q: "Ingest Wiring"})
	if err != nil || len(unis.Items) != 1 {
		t.Fatalf("university = %+v %v", unis, err)
	}
	uni := unis.Items[0].ID
	club, created, err := c.directory.svc.ImportClub(ctx, directory.ImportedClub{UniversityID: uni, Name: "Kodlama Kulübü",
		Category: directory.CategoryTechnology, Job: "ingest.fetch_due"})
	if err != nil || !created {
		t.Fatalf("ImportClub = %v %v", created, err)
	}
	start := time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
	ev := ingest.ImportedEvent{ID: uuid.NewV7(), UniversityID: uni, ClubOrgID: &club, Status: ingest.StatusImported,
		Title: "Kodlama Atölyesi", StartsAt: start, Category: "workshop", SourceURL: "https://kodlama.example.org/",
		Evidence: "Kodlama Atölyesi 1 Aralık", Confidence: 0.9, Fingerprint: make([]byte, 32), ExpiresAt: start.Add(8 * 24 * time.Hour),
		LastSeenAt: start}
	if err := c.ingest.store.InsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	admin := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		Memberships: []authz.Membership{{OrgID: club, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	v, err := c.ingest.svc.PromoteSuggestion(ctx, admin, club, ev.ID)
	if err != nil {
		t.Fatalf("PromoteSuggestion: %v", err)
	}
	if v.Event.OrgID != club || v.Event.Title != "Kodlama Atölyesi" || v.Event.Status != "draft" {
		t.Fatalf("draft = %+v", v.Event)
	}
	got, err := c.ingest.store.GetEvent(ctx, ev.ID)
	if err != nil || got.Status != ingest.StatusPromoted || got.PromotedEventID == nil || *got.PromotedEventID != v.Event.ID {
		t.Fatalf("imported event = %+v %v", got, err)
	}
}
