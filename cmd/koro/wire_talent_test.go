package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/events"
	"koro/internal/kvkk"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/talent"
)

// useMemStore installs the in-memory talent stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-047.
// Only tickets of the talent module edit this file (BE-001 R4).
func (w *talentWiring) useMemStore() {
	w.store = talent.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_TalentTasksAndHooks (BE-047): talent subscribes to the 14
// topics of 01 §4.11 (signals on low, the two neutralisations on default),
// registers its KVKK provider, fills hooks.talentAuditor and starts with
// collection off (T-32).
func TestWire_TalentTasksAndHooks(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.talent.mod.CollectionEnabled() {
		t.Fatal("talent collection is on by default; T-32 wants talent.default_enabled=false")
	}
	want := map[string]string{
		"registration.confirmed": talent.IngestRegistrationTaskName, "registration.cancelled": talent.ReverseRegistrationTaskName,
		"registration.no_show": talent.IngestNoShowTaskName, "checkin.recorded": talent.IngestCheckinTaskName,
		"checkin.voided": talent.ReverseCheckinTaskName, "membership.started": talent.IngestMembershipTaskName,
		"membership.ended": talent.ReverseMembershipTaskName, "student.profile_section_completed": talent.IngestProfileSectionTaskName,
		"student.ranking_choice_changed": talent.ApplyRankingChoiceTaskName, "post.liked": talent.IngestLikeTaskName,
		"post.unliked": talent.ReverseLikeTaskName, "post.saved": talent.IngestSaveTaskName,
		"user.active_day": talent.IngestActiveDayTaskName, "dsr.profiling_objection": talent.NeutralizeObjectionTaskName,
	}
	var names []string
	for _, s := range c.infra.subs.All() {
		if strings.HasPrefix(s.Name, "talent.") {
			names = append(names, s.Name)
		}
	}
	if len(names) != len(want) {
		t.Fatalf("talent subscriptions %q, want %d", names, len(want))
	}
	for topic, name := range want {
		q := queue.Low
		if name == talent.ApplyRankingChoiceTaskName || name == talent.NeutralizeObjectionTaskName {
			q = queue.Default
		}
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name && s.Queue == q }) {
			t.Errorf("%s: no %s subscription on %s", topic, name, q)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "talent" }) {
		t.Fatal("talent privacy provider not registered")
	}
	h := c.hooks.talentAuditor
	if h == nil || h.Report == nil || h.Neutralize == nil {
		t.Fatal("talent did not fill hooks.talentAuditor")
	}
	user := uuid.NewV7()
	raw, err := h.Report(workerContext(t), uuid.Nil(), user, "dsr:test")
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep["band"] != float64(0) || rep["collection_enabled"] != false || rep["user_id"] != user.String() {
		t.Fatalf("report = %v", rep)
	}
	if err := h.Neutralize(workerContext(t), user, "minor", nil); err == nil {
		t.Fatal("a reason outside 02 §15.8 was accepted")
	}
	// The worker registers koro_talent_ingest_total.
	m := obs.NewMetrics()
	if err := c.talent.metrics(m); err != nil {
		t.Fatal(err)
	}
}

// TestWire_TalentJobsAndRoutes (BE-052; 01 §7, 04 §T): talent schedules
// talent.recompute at 03:00 on the low queue, registers the seven
// talent_auditor operations under /admin/talent, closed by default
// (talent.auditor_api_enabled false), and gets identity's
// re-authentication.
func TestWire_TalentJobsAndRoutes(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "talent.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"talent.recompute 0 3 * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("talent jobs %q, want %q", jobs, want)
	}
	if c.talent.mod.AuditorAPIEnabled() {
		t.Fatal("the talent auditor endpoints are open by default; talent.auditor_api_enabled must default to false")
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion})
	if err != nil {
		t.Fatal(err)
	}
	c.talent.routes(c, app.V1)
	var ops []string
	for path, item := range app.API.OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Post} {
			if o != nil && slices.Contains(o.Tags, "talent") {
				if !strings.HasPrefix(path, httpx.APIPrefix+"/admin/talent/") {
					t.Errorf("talent operation %s outside /admin/talent: %s", o.OperationID, path)
				}
				ops = append(ops, o.OperationID)
			}
		}
	}
	slices.Sort(ops)
	want := []string{"createTalentConfigVersion", "createTalentNeutralization", "getTalentConfig", "getTalentDistribution",
		"getTalentReport", "liftTalentNeutralization", "reverseTalentPointEvent"}
	if !slices.Equal(ops, want) {
		t.Fatalf("talent operations %q, want %q", ops, want)
	}
}

// TestWire_TalentRetentionSweeper (BE-079; 00 T-323): talent registers
// one retention sweeper on c.sweepers for its four policies; it has no job
// of its own (kvkk.retention_sweep runs it, flag on or off).
func TestWire_TalentRetentionSweeper(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, sw := range c.sweepers {
		for _, code := range sw.PolicyCodes() {
			if strings.HasPrefix(code, "talent_") {
				codes = append(codes, code)
			}
		}
	}
	slices.Sort(codes)
	want := []string{talent.PolicyCounters, talent.PolicyLedger, talent.PolicyNeutralization, talent.PolicyRuns}
	if !slices.Equal(codes, want) {
		t.Fatalf("talent retention codes on c.sweepers = %q, want %q", codes, want)
	}
	if c.talent.mod.Sweeper == nil || !slices.Contains(c.sweepers, c.talent.mod.Sweeper) {
		t.Fatal("the talent sweeper is not the module's")
	}
}

// TestWire_TalentAuditorFlagFromConfig: TALENT_AUDITOR_API_ENABLED reaches
// the module.
func TestWire_TalentAuditorFlagFromConfig(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.infra.cfg.Talent.AuditorAPIEnabled = true
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if !c.talent.mod.AuditorAPIEnabled() || c.talent.mod.CollectionEnabled() {
		t.Fatal("talent flags not taken from the configuration")
	}
}

// TestOpenAPI_NoTalentFields (BE-047; 01 §0-7, 03 §3.4 Y1, 04 §T, §V.0-2):
// the full API, as `koro api` and `koro openapi` register it, exposes no
// talent*, band, score* or point* member or parameter outside the talent
// operations: registration itself would panic (httpx's talent field
// guard), and the whole rendered document is checked again here.
func TestOpenAPI_NoTalentFields(t *testing.T) {
	t.Parallel()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion})
	if err != nil {
		t.Fatal(err)
	}
	c := newContainer(infra{log: obs.Discard(), clock: clock.Real(), version: version})
	c.registerRoutes(app.V1)
	oapi := app.API.OpenAPI()
	if len(oapi.Paths) < 100 {
		t.Fatalf("only %d paths registered", len(oapi.Paths))
	}
	if v := httpx.TalentFieldViolations(oapi); len(v) > 0 {
		t.Fatalf("talent fields in the API:\n%s", strings.Join(v, "\n"))
	}
	if len(oapi.OnAddOperation) == 0 {
		t.Fatal("the talent field guard is not installed on the API")
	}
}

// TestWire_TalentEventsAdapter (BE-085): talent reads the events module's
// participation figures through talentEvents (both L8): a registration
// maps to its event and the event's figures carry its organiser and its
// confirmed registrations.
func TestWire_TalentEventsAdapter(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	mem := c.events.built.(*events.MemStore)
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	e := events.Event{
		ID: uuid.NewV7(), OrgID: uuid.NewV7(), CreatedBy: uuid.NewV7(), Title: "Talent Etkinliği", Slug: "talent",
		Category: events.CategoryTalk, Format: events.FormatInPerson, StartsAt: start, EndsAt: start.Add(time.Hour),
		Visibility: events.VisibilityPublic, Status: events.StatusPublished, PublishedAt: &start, RequiresRegistration: true,
		RegistrationMode: events.ModeAuto, CreatedAt: start, UpdatedAt: start,
	}
	if err := mem.PutEvent(e); err != nil {
		t.Fatal(err)
	}
	r, err := mem.InsertRegistration(ctx, events.Registration{ID: uuid.NewV7(), EventID: e.ID, OrgID: e.OrgID, UserID: uuid.NewV7(),
		Status: events.RegConfirmed, Source: events.SourceWeb, ConfirmedAt: &start, AnswersPurgeAt: start.Add(90 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	a := talentEvents{p: c.events.svc}
	of, err := a.EventsOf(ctx, []uuid.UUID{r.ID, uuid.NewV7()})
	if err != nil || len(of) != 1 || of[r.ID] != e.ID {
		t.Fatalf("EventsOf = %v, %v", of, err)
	}
	facts, err := a.EventFacts(ctx, []uuid.UUID{e.ID, uuid.NewV7()})
	if want := (talent.EventFacts{OrgID: e.OrgID, Confirmed: 1}); err != nil || len(facts) != 1 || facts[e.ID] != want {
		t.Fatalf("EventFacts = %+v, %v; want %+v", facts, err, want)
	}
}

// TestWire_TalentCollectionOffDestructionHook (BE-087, T-324): with
// collection off (the default) talent.recompute deletes the scores and
// cohorts and records them through hooks.destruction in kvkk's
// destruction record, under the talent_collection_off policy seeded by
// 000134 (the recorder checks that talent owns it); without the hook the
// purge fails closed and keeps the rows.
func TestWire_TalentCollectionOffDestructionHook(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	records, ok := c.kvkk.records.(*kvkk.MemRecords)
	if !ok {
		t.Fatalf("kvkk records = %T, want the in-memory store", c.kvkk.records)
	}
	p, ok := seedPolicies(t)[talent.PolicyCollectionOff]
	if !ok || p.OwnerModule != "talent" || p.Target != "talent.scores, talent.cohorts" || p.Method != privacy.MethodHardDelete || !p.Enabled {
		t.Fatalf("seeded policy %s = %+v, %v", talent.PolicyCollectionOff, p, ok)
	}
	records.PutPolicy(p)
	st, ok := c.talent.store.(*talent.MemStore)
	if !ok {
		t.Fatalf("talent store = %T, want the in-memory store", c.talent.store)
	}
	st.PutScore(uuid.NewV7(), talent.BandNeutral, false)

	hook := c.hooks.destruction
	c.hooks.destruction = nil
	if err := c.talent.mod.Recompute(t.Context()); err == nil {
		t.Fatal("the purge ran without kvkk's destruction record")
	}
	c.hooks.destruction = hook
	// The memory store does not roll back (db.NopTx): put the row back.
	st.PutScore(uuid.NewV7(), talent.BandNeutral, false)
	if err := c.talent.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.AllScores()) != 0 || len(st.Cohorts()) != 0 {
		t.Fatal("collection off kept score or cohort rows")
	}
	logs, err := records.DestructionLogs(t.Context(), kvkk.DestructionFilter{PolicyCode: talent.PolicyCollectionOff}, nil, 10)
	if err != nil || len(logs) != 1 || logs[0].Module != "talent" || logs[0].Target != p.Target || logs[0].Method != privacy.MethodHardDelete ||
		logs[0].RecordCount < 2 {
		t.Fatalf("destruction rows = %+v, %v", logs, err)
	}
}
