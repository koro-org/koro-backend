package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

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
