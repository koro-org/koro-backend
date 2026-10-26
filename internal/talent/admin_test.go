package talent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/talent"
)

// accessReason is a valid X-Koro-Access-Reason.
const accessReason = "KVKK başvurusu 2026-114 incelemesi"

// testParams are the score parameters of the tests (test values, not
// product values: those come with createTalentConfigVersion).
func testParams() talent.ScoreParams {
	return talent.ScoreParams{ShrinkK: 5, CohortMin: 10, LookbackDays: 540,
		CategoryCapsCP: map[string]int{talent.CategoryParticipation: 4000, talent.CategoryIntent: 1500, talent.CategoryProfile: 1000,
			talent.CategoryCommunity: 600, talent.CategoryEngagement: 300},
		Bands: []talent.BandRule{{Band: 1, MinPctBP: 5000, MultBP: 10500}, {Band: 2, MinPctBP: 7500, MultBP: 11000},
			{Band: 3, MinPctBP: 9000, MultBP: 11500}}}
}

// configBody is a valid createTalentConfigVersion body.
func configBody(password string) string {
	b, _ := json.Marshal(map[string]any{
		"params": map[string]any{"shrink_k": 5, "cohort_min": 10, "lookback_days": 540,
			"category_caps_cp": map[string]int{"participation": 4000, "intent": 1500, "profile": 1000, "community": 600, "engagement": 300},
			"bands":            []map[string]int{{"band": 1, "min_pct_bp": 5000, "mult_bp": 10500}, {"band": 2, "min_pct_bp": 7500, "mult_bp": 11000}}},
		"signal_defs": []map[string]any{{"code": talent.SignalFeedLike, "base_points_cp": 5, "daily_cap_cp": 25, "half_life_days": 30, "enabled": true}},
		"notes":       "pilot ayarı",
		"reauth":      map[string]string{"password": password},
	})
	return string(b)
}

// adminFixtures are the rows the seven operations act on.
type adminFixtures struct {
	subject   uuid.UUID // a student with a check-in in the ledger and a score
	point     talent.PointEvent
	neutral   talent.Neutralization // an auditor neutralisation of another student
	objection talent.Neutralization // a KVKK objection of a third student
}

// seedAdmin writes the fixtures (collection on, a config version in force).
func (e *env) seedAdmin(t *testing.T) adminFixtures {
	t.Helper()
	var f adminFixtures
	f.subject = e.student()
	e.send(t, domainevent.CheckinRecorded{CheckinID: uuid.NewV7(), UserID: f.subject, StaffUserID: uuid.NewV7()}, t0.Add(-time.Hour))
	rows := e.points(f.subject)
	if len(rows) != 1 {
		t.Fatalf("fixture ledger = %+v", rows)
	}
	f.point = rows[0]
	e.store.PutScore(f.subject, 2, false)
	other, third := e.student(), e.student()
	if err := e.mod.Auditor.Neutralize(t.Context(), other, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.mod.Auditor.Neutralize(t.Context(), third, talent.NeutralizeObjection, ptr(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	f.neutral, f.objection = e.active(other)[0], e.active(third)[0]
	return f
}

// adminEnv is an environment with collection on, a config version and the
// auditor API switched as given, plus its fixtures and HTTP server.
func adminEnv(t *testing.T, open bool) (*env, adminFixtures, http.Handler) {
	t.Helper()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: open, config: true})
	f := e.seedAdmin(t)
	e.audit.mu.Lock()
	e.audit.events = nil // the fixtures' system records
	e.audit.mu.Unlock()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(),
		Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	talent.Register(app.V1, e.mod.Admin)
	return e, f, app.Handler()
}

// adminCall is one request to a talent_auditor operation.
type adminCall struct {
	op, method, path, body string
	ok                     int // the talent_auditor's status with the API open
}

// adminCalls are valid requests to the seven operations of 04 §T.
func adminCalls(f adminFixtures) []adminCall {
	reauth := `"reauth":{"password":"` + reauthPassword + `"}`
	return []adminCall{
		{"getTalentReport", http.MethodGet, "/admin/talent/subjects/" + f.subject.String() + "/report", "", http.StatusOK},
		{"getTalentDistribution", http.MethodGet, "/admin/talent/distribution", "", http.StatusOK},
		{"createTalentNeutralization", http.MethodPost, "/admin/talent/subjects/" + f.subject.String() + "/neutralizations",
			`{"note":"çoklu hesapla etkinlik şişirme şüphesi",` + reauth + `}`, http.StatusCreated},
		{"liftTalentNeutralization", http.MethodPost, "/admin/talent/neutralizations/" + f.neutral.ID.String() + "/lift",
			`{"note":"inceleme kapandı",` + reauth + `}`, http.StatusOK},
		{"reverseTalentPointEvent", http.MethodPost, "/admin/talent/point-events/" + f.point.ID.String() + "/reversal",
			`{"reason":"kendi etkinliğinde okutma",` + reauth + `}`, http.StatusCreated},
		{"getTalentConfig", http.MethodGet, "/admin/talent/config", "", http.StatusOK},
		{"createTalentConfigVersion", http.MethodPost, "/admin/talent/config/versions", configBody(reauthPassword), http.StatusCreated},
	}
}

// do sends c as persona with the access reason header (none when reason
// is "") and returns the status and the decoded body.
func do(t *testing.T, h http.Handler, persona string, c adminCall, reason string) (int, map[string]any) {
	t.Helper()
	var body *bytes.Reader
	if c.body != "" {
		body = bytes.NewReader([]byte(c.body))
	} else {
		body = bytes.NewReader(nil)
	}
	req := httptest.NewRequestWithContext(t.Context(), c.method, httpx.APIPrefix+c.path, body)
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if reason != "" {
		req.Header.Set("X-Koro-Access-Reason", reason)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestAuditorAPI_FlagOff_404ForAll (03 §5.12 k-flag, §7.7 "Talent", 04 §T;
// DECISIONS-LOG BE-004): with talent.auditor_api_enabled false (the
// default) the seven operations are registered but closed: every
// signed-in persona — talent_auditor and platform_admin included — gets
// 404 whatever it sends (no access reason, an empty or a broken body, a
// malformed ID), the anonymous caller the auth layer's 401; nothing is
// read, written or audited.
func TestAuditorAPI_FlagOff_404ForAll(t *testing.T) {
	t.Parallel()
	e, f, h := adminEnv(t, false)
	ledger, neutral := len(e.store.AllPoints()), len(e.store.AllNeutralizations())
	personas := append(authztest.Personas(), authztest.Helpers()...)
	for _, c := range adminCalls(f) {
		bare := c
		if c.body != "" {
			bare.body = `{}`
		}
		bare.path = strings.Replace(c.path, f.subject.String(), "not-a-uuid", 1)
		for _, p := range personas {
			want := http.StatusNotFound
			if p.Name == "anonymous" {
				want = http.StatusUnauthorized
			}
			if code, body := do(t, h, p.Name, c, accessReason); code != want {
				t.Fatalf("%s as %s (flag off) = %d %v, want %d", c.op, p.Name, code, body, want)
			}
			if code, body := do(t, h, p.Name, bare, ""); code != want {
				t.Fatalf("%s as %s (flag off, no reason, bare body) = %d %v, want %d", c.op, p.Name, code, body, want)
			}
		}
	}
	if got := e.audit.actions(); len(got) != 0 {
		t.Fatalf("closed API audited %q", got)
	}
	if len(e.store.AllPoints()) != ledger || len(e.store.AllNeutralizations()) != neutral {
		t.Fatal("closed API changed the store")
	}
	if e.mod.AuditorAPIEnabled() {
		t.Fatal("auditor API reported open")
	}
}

// TestAuditorAPI_FlagOn_PlatformAdmin403 (03 §5.12 Y1, §7.7; 04 §T): with
// the API open the talent_auditor (MFA) reaches all seven operations, while
// platform_admin — not a super user: it never sees talent — and every other
// signed-in persona gets 403 before anything is read (admin surface), the
// subject of the report included; the denials write nothing.
func TestAuditorAPI_FlagOn_PlatformAdmin403(t *testing.T) {
	t.Parallel()
	for _, c := range adminCalls(adminFixtures{subject: uuid.NewV7(), point: talent.PointEvent{ID: uuid.NewV7()},
		neutral: talent.Neutralization{ID: uuid.NewV7()}}) {
		e, f, h := adminEnv(t, true)
		calls := adminCalls(f)
		call := calls[slices.IndexFunc(calls, func(x adminCall) bool { return x.op == c.op })]
		for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
			if p.Actor.HasPlatformRole(authz.PlatformTalentAuditor) {
				continue
			}
			want := http.StatusForbidden
			if p.Name == "anonymous" {
				want = http.StatusUnauthorized
			}
			if code, body := do(t, h, p.Name, call, accessReason); code != want {
				t.Fatalf("%s as %s (flag on) = %d %v, want %d", c.op, p.Name, code, body, want)
			}
		}
		if got := e.audit.actions(); len(got) != 0 {
			t.Fatalf("%s: denials audited %q", c.op, got)
		}
		code, body := do(t, h, "talent_auditor", call, accessReason)
		if code != call.ok {
			t.Fatalf("%s as talent_auditor = %d %v, want %d", c.op, code, body, call.ok)
		}
		// Every call is audited under its action with the reason (03 §6.5).
		e.audit.mu.Lock()
		last := e.audit.events[len(e.audit.events)-1]
		e.audit.mu.Unlock()
		auditor := persona(t, "talent_auditor").UserID
		if !strings.HasPrefix(last.Action, "talent.") || last.Outcome != audit.Success || last.Reason != accessReason ||
			last.ActorUserID == nil || *last.ActorUserID != auditor {
			t.Fatalf("%s audit = %+v", c.op, last)
		}
	}
}

// TestAuditorAPI_AccessReasonRequired (03 §5.12): an open API without a
// 10–500 character X-Koro-Access-Reason answers the talent_auditor 422
// and audits nothing; others still get 403 first.
func TestAuditorAPI_AccessReasonRequired(t *testing.T) {
	t.Parallel()
	e, f, h := adminEnv(t, true)
	for _, c := range adminCalls(f) {
		for _, reason := range []string{"", "  kısa  ", strings.Repeat("ç", 501)} {
			code, body := do(t, h, "talent_auditor", c, reason)
			if code != http.StatusUnprocessableEntity || body["code"] != "validation_failed" {
				t.Fatalf("%s with reason %q = %d %v, want 422", c.op, reason, code, body)
			}
		}
		if code, _ := do(t, h, "platform_admin", c, ""); code != http.StatusForbidden {
			t.Fatalf("%s as platform_admin without a reason = %d, want 403", c.op, code)
		}
	}
	if got := e.audit.actions(); len(got) != 0 {
		t.Fatalf("refused calls audited %q", got)
	}
}

// TestAuditorAPI_ReportCarriesNoNameOrEmail (04 §T getTalentReport): the
// report names the subject by user ID only and lists the ledger with each
// row's point_event_id, the last score and the neutralisations.
func TestAuditorAPI_ReportCarriesNoNameOrEmail(t *testing.T) {
	t.Parallel()
	_, f, h := adminEnv(t, true)
	code, body := do(t, h, "talent_auditor", adminCalls(f)[0], accessReason)
	if code != http.StatusOK {
		t.Fatalf("report = %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	for _, banned := range []string{"name", "email", "first_name", "last_name", "e_mail", "phone"} {
		if strings.Contains(string(raw), `"`+banned+`"`) {
			t.Fatalf("report carries %q: %s", banned, raw)
		}
	}
	signals, _ := body["signals"].([]any)
	if body["user_id"] != f.subject.String() || body["band"] != float64(2) || len(signals) != 1 {
		t.Fatalf("report = %s", raw)
	}
	sig, _ := signals[0].(map[string]any)
	ref, _ := sig["source_ref"].(map[string]any)
	if sig["point_event_id"] != f.point.ID.String() || sig["points_cp"] != float64(500) || ref["type"] != talent.SourceCheckin {
		t.Fatalf("signal = %v", sig)
	}
}

// TestReversal_AppendOnly (ADR-0015, 02 §15.3, 04 §T reverseTalentPointEvent):
// the auditor's reversal appends one row — points and raw points negated,
// the original's occurred_at and config version, reason_code
// auditor_reversal — and never changes the original; the ledger nets to
// zero; a second reversal (also a consumer's: checkin.voided) is refused
// or a duplicate, a reversal is not reversed, an unknown row is 404; the
// record names the subject, both rows and the proof's kind.
func TestReversal_AppendOnly(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true})
	f := e.seedAdmin(t)
	before := f.point
	auditor := persona(t, "talent_auditor")
	in := talent.ReversalInput{Reason: accessReason, Why: "organizatör kendi etkinliğinde okuttu"}
	rev, err := e.mod.Admin.ReversePoint(t.Context(), auditor, f.point.ID, in, goodProof)
	if err != nil {
		t.Fatal(err)
	}
	if rev.SourceType != talent.SourceReversal || rev.ReversalOf == nil || *rev.ReversalOf != f.point.ID || rev.SourceID != f.point.ID ||
		rev.PointsCP != -before.PointsCP || rev.RawPointsCP != -before.RawPointsCP || !rev.OccurredAt.Equal(before.OccurredAt) ||
		rev.ConfigVersionID != before.ConfigVersionID || rev.ReasonCode != talent.ReasonAuditorReversal || rev.UserID != f.subject {
		t.Fatalf("reversal = %+v of %+v", rev, before)
	}
	rows := e.points(f.subject)
	if len(rows) != 2 || !reflectEqualPoint(rows[0], before) || e.total(f.subject, "") != 0 {
		t.Fatalf("ledger after the reversal = %+v", rows)
	}
	if _, err := e.mod.Admin.ReversePoint(t.Context(), auditor, f.point.ID, in, goodProof); !errors.Is(err, talent.ErrAlreadyReversed) {
		t.Fatalf("second reversal = %v, want ErrAlreadyReversed", err)
	}
	if _, err := e.mod.Admin.ReversePoint(t.Context(), auditor, rev.ID, in, goodProof); !errors.Is(err, talent.ErrNotReversible) {
		t.Fatalf("reversal of a reversal = %v, want ErrNotReversible", err)
	}
	if _, err := e.mod.Admin.ReversePoint(t.Context(), auditor, uuid.NewV7(), in, goodProof); !errors.Is(err, talent.ErrNotFound) {
		t.Fatalf("unknown row = %v, want ErrNotFound", err)
	}
	e.send(t, domainevent.CheckinVoided{CheckinID: before.SourceID, UserID: f.subject}, t0)
	if rows := e.points(f.subject); len(rows) != 2 {
		t.Fatalf("a consumer's reversal after the auditor's wrote %d rows", len(rows))
	}
	e.audit.mu.Lock()
	var rec *audit.Event
	for i := range e.audit.events {
		if e.audit.events[i].Action == string(authz.TalentReversalCreate) {
			rec = &e.audit.events[i]
		}
	}
	e.audit.mu.Unlock()
	if rec == nil || rec.ResourceID == nil || *rec.ResourceID != f.subject || rec.Reason != accessReason ||
		rec.Metadata["point_event_id"] != f.point.ID.String() || rec.Metadata["reversal_id"] != rev.ID.String() ||
		rec.Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("reversal record = %+v", rec)
	}
}

func reflectEqualPoint(a, b talent.PointEvent) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// TestConfigVersion_RequiresReauth (03 §6.2, 04 §T createTalentConfigVersion):
// without the proof (403 identity.reauth_required) or with a wrong one
// (403 identity.reauth_failed) no version is created and no signal
// changes, and one denied record is written; an overload writes nothing;
// the right proof creates the version in force from now with the auditor
// as created_by, the catalogue snapshot in its params and the listed
// signal replaced — the gap of BE-047 ("no config version, nothing
// written") closes: the next signal is frozen with it.
func TestConfigVersion_RequiresReauth(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true})
	auditor := persona(t, "talent_auditor")
	five, quarter, month, on := 5, 25, 30, true
	in := talent.ConfigInput{Reason: accessReason, Params: testParams(), Notes: "pilot",
		Signals: []talent.SignalUpdate{{Code: talent.SignalFeedLike, BasePointsCP: &five, DailyCapCP: &quarter, HalfLifeDays: &month, Enabled: &on}}}
	user := e.student()
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	if e.count(talent.SignalFeedLike, talent.OutcomeNoConfig) != 1 || len(e.points(user)) != 0 {
		t.Fatal("a signal was written without a config version")
	}
	audittest.ReauthCase[identity.ReauthProof]{
		Call: func(ctx context.Context, proof identity.ReauthProof) error {
			_, err := e.mod.Admin.CreateConfigVersion(ctx, auditor, in, proof)
			return err
		},
		Rows:   func() []audittest.Row { return e.audit.rows(authz.TalentConfigManage) },
		Denied: audittest.RowOf(audit.Event{Action: string(authz.TalentConfigManage), ResourceType: "talent_config_version"}.By(auditor)),
		Late:   e.reauth,
		Wrong:  identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
		Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
		Good: goodProof,
	}.Run(t)

	var v talent.ConfigVersion
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		v, err = e.store.CurrentConfig(ctx, t0)
		return err
	})
	if v.CreatedBy != auditor.UserID || !v.EffectiveFrom.Equal(t0) || v.Notes != "pilot" {
		t.Fatalf("version = %+v", v)
	}
	p, err := talent.ParamsOf(v)
	if err != nil || p.CohortMin != 10 || len(p.Signals) != 10 || len(p.Bands) != 3 {
		t.Fatalf("params = %+v, %v", p, err)
	}
	var like talent.SignalDef
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		like, err = e.store.SignalDef(ctx, talent.SignalFeedLike)
		return err
	})
	if like.BasePointsCP != 5 || like.DailyCapCP == nil || *like.DailyCapCP != 25 || like.WeeklyCapCP != nil || !like.Enabled {
		t.Fatalf("feed_like after the version = %+v", like)
	}
	// Only one version exists: the refusals and the overload created none.
	if runs := len(configVersions(t, e)); runs != 1 {
		t.Fatalf("%d versions, want 1", runs)
	}
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	if rows := e.points(user); len(rows) != 1 || rows[0].ConfigVersionID != v.ID || rows[0].PointsCP != 5 {
		t.Fatalf("signal after the version = %+v", rows)
	}
}

// configVersions returns the versions in force at any time up to t0.
func configVersions(t *testing.T, e *env) []talent.ConfigVersion {
	t.Helper()
	var out []talent.ConfigVersion
	e.inTalentTx(t, func(ctx context.Context) error {
		v, err := e.store.CurrentConfig(ctx, t0)
		if errors.Is(err, talent.ErrNotFound) {
			return nil
		}
		out = append(out, v)
		// Earlier versions: walk back from just before each effective_from.
		for err == nil {
			v, err = e.store.CurrentConfig(ctx, v.EffectiveFrom.Add(-time.Microsecond))
			if err == nil {
				out = append(out, v)
			}
		}
		if errors.Is(err, talent.ErrNotFound) {
			return nil
		}
		return err
	})
	return out
}

// TestConfigVersion_Validation: a broken version is 422 with the field
// before the proof is asked for (an unknown signal after it, inside the
// transaction); the params are guards, not product values.
func TestConfigVersion_Validation(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true})
	auditor := persona(t, "talent_auditor")
	ten := 10
	for name, c := range map[string]struct {
		mutate func(*talent.ConfigInput)
		field  string
	}{
		"cohort below k":           {func(in *talent.ConfigInput) { in.Params.CohortMin = 9 }, "body.params.cohort_min"},
		"window beyond the ledger": {func(in *talent.ConfigInput) { in.Params.LookbackDays = 731 }, "body.params.lookback_days"},
		"missing category cap": {func(in *talent.ConfigInput) { delete(in.Params.CategoryCapsCP, talent.CategoryIntent) },
			"body.params.category_caps_cp.intent"},
		"lift above 15 %":    {func(in *talent.ConfigInput) { in.Params.Bands[2].MultBP = 11501 }, "body.params.bands[2].mult_bp"},
		"downward band":      {func(in *talent.ConfigInput) { in.Params.Bands[0].MultBP = 9500 }, "body.params.bands[0].mult_bp"},
		"bands out of order": {func(in *talent.ConfigInput) { in.Params.Bands[1].MinPctBP = 4000 }, "body.params.bands[1].min_pct_bp"},
		"band zero": {func(in *talent.ConfigInput) {
			in.Params.Bands = []talent.BandRule{{Band: 0, MinPctBP: 1, MultBP: 10001}}
		},
			"body.params.bands[0].band"},
		"signal without base points": {func(in *talent.ConfigInput) {
			in.Signals = []talent.SignalUpdate{{Code: talent.SignalFeedLike, Enabled: ptr(true)}}
		}, "body.signal_defs[0].base_points_cp"},
		"signal without enabled": {func(in *talent.ConfigInput) {
			in.Signals = []talent.SignalUpdate{{Code: talent.SignalFeedLike, BasePointsCP: &ten}}
		}, "body.signal_defs[0].enabled"},
		"zero cap": {func(in *talent.ConfigInput) {
			zero := 0
			in.Signals = []talent.SignalUpdate{{Code: talent.SignalFeedLike, BasePointsCP: &ten, DailyCapCP: &zero, Enabled: ptr(true)}}
		}, "body.signal_defs[0].daily_cap_cp"},
		"unknown signal": {func(in *talent.ConfigInput) {
			in.Signals = []talent.SignalUpdate{{Code: "no_such_signal", BasePointsCP: &ten, Enabled: ptr(true)}}
		}, "body.signal_defs[0].code"},
	} {
		in := talent.ConfigInput{Reason: accessReason, Params: testParams()}
		c.mutate(&in)
		_, err := e.mod.Admin.CreateConfigVersion(t.Context(), auditor, in, goodProof)
		pe, ok := errors.AsType[*problem.Error](err)
		if !ok || pe.Code() != "validation_failed" || !slices.ContainsFunc(pe.Fields(), func(f problem.FieldError) bool { return f.Location == c.field }) {
			t.Errorf("%s: %v, want 422 at %s", name, err, c.field)
		}
	}
	if n := len(configVersions(t, e)); n != 0 {
		t.Fatalf("%d versions after refused input", n)
	}
}

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): each talent R
// operation audits a refused proof — wrong or missing — as one denied
// reauth_failed record of its action (the subject when the path names it,
// the step's row ID otherwise; nothing else: nothing happened), writes
// nothing on an overload (the bare deadline, httpx 503) and succeeds with
// the right proof afterwards.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	auditor := persona(t, "talent_auditor")
	for _, op := range []string{"create", "lift", "reverse"} {
		e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true})
		f := e.seedAdmin(t)
		var call func(ctx context.Context, proof identity.ReauthProof) error
		var act authz.Action
		var denied audit.Event
		switch op {
		case "create":
			act, call = authz.TalentNeutralizationManage, func(ctx context.Context, proof identity.ReauthProof) error {
				_, err := e.mod.Admin.CreateNeutralization(ctx, auditor, f.subject, talent.NeutralizationInput{Reason: accessReason, Note: "şüpheli"}, proof)
				return err
			}
			subject := f.subject
			denied = audit.Event{Action: string(act), ResourceType: authz.TypeTalentSubject, ResourceID: &subject,
				Metadata: map[string]any{"change": "neutralized"}}
		case "lift":
			act, call = authz.TalentNeutralizationManage, func(ctx context.Context, proof identity.ReauthProof) error {
				_, err := e.mod.Admin.LiftNeutralization(ctx, auditor, f.neutral.ID, talent.NeutralizationInput{Reason: accessReason}, proof)
				return err
			}
			denied = audit.Event{Action: string(act), ResourceType: authz.TypeTalentSubject,
				Metadata: map[string]any{"change": "lifted", "neutralization_id": f.neutral.ID.String()}}
		case "reverse":
			act, call = authz.TalentReversalCreate, func(ctx context.Context, proof identity.ReauthProof) error {
				_, err := e.mod.Admin.ReversePoint(ctx, auditor, f.point.ID, talent.ReversalInput{Reason: accessReason, Why: "hatalı"}, proof)
				return err
			}
			denied = audit.Event{Action: string(act), ResourceType: authz.TypeTalentSubject,
				Metadata: map[string]any{"point_event_id": f.point.ID.String()}}
		}
		audittest.ReauthCase[identity.ReauthProof]{
			Call: call, Rows: func() []audittest.Row { return e.audit.rows(act) }, Denied: audittest.RowOf(denied.By(auditor)), Late: e.reauth,
			Wrong: identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
			Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
			Good: goodProof,
		}.Run(t)
	}
	// Without the re-authentication port every R operation fails closed.
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true, noReauth: true})
	f := e.seedAdmin(t)
	if _, err := e.mod.Admin.ReversePoint(t.Context(), auditor, f.point.ID, talent.ReversalInput{Reason: accessReason, Why: "x"},
		goodProof); !errors.Is(err, talent.ErrUnavailable) {
		t.Fatalf("no reauth port = %v, want 503", err)
	}
}

// rows returns the recorded events of act in audittest's terms.
func (f *fakeAudit) rows(act authz.Action) []audittest.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []audittest.Row
	for _, e := range f.events {
		if e.Action == string(act) {
			out = append(out, audittest.RowOf(e))
		}
	}
	return out
}

// TestAuditorAPI_Neutralization: an auditor neutralisation needs a note,
// makes the band 0 at once and is one per user (409); the record carries
// the reason and the proof's kind.
func TestAuditorAPI_Neutralization(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true})
	f := e.seedAdmin(t)
	auditor := persona(t, "talent_auditor")
	if _, err := e.mod.Admin.CreateNeutralization(t.Context(), auditor, f.subject, talent.NeutralizationInput{Reason: accessReason, Note: "  "},
		goodProof); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("empty note = %v, want 422", err)
	}
	bands, _ := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{f.subject})
	if len(bands) != 1 {
		t.Fatal("fixture subject has no band")
	}
	n, err := e.mod.Admin.CreateNeutralization(t.Context(), auditor, f.subject, talent.NeutralizationInput{Reason: accessReason, Note: "şüpheli"}, goodProof)
	if err != nil || n.Reason != talent.NeutralizeAdmin || n.CreatedBy == nil || *n.CreatedBy != auditor.UserID || n.Note != "şüpheli" {
		t.Fatalf("neutralisation = %+v, %v", n, err)
	}
	if bands, _ := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{f.subject}); len(bands) != 0 {
		t.Fatal("a neutralised subject kept its band")
	}
	if _, err := e.mod.Admin.CreateNeutralization(t.Context(), auditor, f.subject, talent.NeutralizationInput{Reason: accessReason, Note: "yine"},
		goodProof); !errors.Is(err, talent.ErrAlreadyNeutralized) {
		t.Fatalf("second neutralisation = %v, want 409", err)
	}
	if _, err := e.mod.Admin.LiftNeutralization(t.Context(), auditor, f.objection.ID, talent.NeutralizationInput{Reason: accessReason},
		goodProof); !errors.Is(err, talent.ErrObjectionPermanent) {
		t.Fatalf("lift of an objection = %v, want 409", err)
	}
	rows := e.audit.rows(authz.TalentNeutralizationManage)
	last := rows[len(rows)-1]
	if last.Outcome != audittest.Success || last.Reason != accessReason || last.Metadata[audittest.MetaReauth] != "password" ||
		last.Metadata["reason"] != "admin" || last.ResourceID == nil || *last.ResourceID != f.subject {
		t.Fatalf("record = %+v", last)
	}
}

// TestAuditorAPI_Distribution_KThreshold (04 §T, §1.11): the counts by band
// are null below 10, neutralised users are left out, university_id narrows
// to that university's cohorts and a malformed one is 422; the read is
// audited with the filter.
func TestAuditorAPI_Distribution_KThreshold(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true})
	uni := uuid.NewV7()
	scores := []talent.Score{}
	for i := range 23 {
		id := e.student()
		key, band := "uni:"+uni.String()+":year:2", talent.Band(1)
		switch {
		case i < 12:
			band = 0
		case i == 22:
			key = "global" // another university, fallen back
		}
		scores = append(scores, talent.Score{UserID: id, Band: band, CohortKey: key, PctBP: 5000, NSignals: 1,
			ConfigVersionID: e.cfg.ID, RunID: uuid.NewV7(), CalculatedAt: t0})
	}
	neutral := scores[0].UserID
	e.inTalentTx(t, func(ctx context.Context) error { return e.store.PutScores(ctx, scores) })
	if err := e.mod.Auditor.Neutralize(t.Context(), neutral, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	auditor := persona(t, "talent_auditor")
	d, err := e.mod.Admin.Distribution(t.Context(), auditor, talent.DistributionQuery{Reason: accessReason})
	if err != nil || len(d.Buckets) != 4 {
		t.Fatalf("distribution = %+v, %v", d, err)
	}
	if c := d.Buckets[0].Count; c == nil || *c != 11 { // 12 minus the neutralised one
		t.Fatalf("band 0 = %v", c)
	}
	if c := d.Buckets[1].Count; c == nil || *c != 11 {
		t.Fatalf("band 1 = %v", c)
	}
	if d.Buckets[2].Count != nil || d.Buckets[3].Count != nil {
		t.Fatal("empty bands have a count")
	}
	d, err = e.mod.Admin.Distribution(t.Context(), auditor, talent.DistributionQuery{Reason: accessReason, UniversityID: uni.String()})
	if err != nil || d.UniversityID == nil || *d.UniversityID != uni || d.Buckets[1].Count == nil || *d.Buckets[1].Count != 10 {
		t.Fatalf("university distribution = %+v, %v", d, err)
	}
	if _, err := e.mod.Admin.Distribution(t.Context(), auditor, talent.DistributionQuery{Reason: accessReason, UniversityID: "x"}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("malformed university = %v, want 422", err)
	}
	rows := e.audit.rows(authz.TalentDistributionRead)
	if len(rows) != 2 || rows[1].Metadata["university_id"] != uni.String() || rows[0].Reason != accessReason {
		t.Fatalf("distribution records = %+v", rows)
	}
}

// TestTalentStatus_SideEffectFree (BE-129): getTalentStatus tells the
// talent_auditor whether the API is open — with the flag off too, without
// an access reason — and writes nothing; everyone else, platform_admin
// included, gets 403 (the anonymous caller 401), so a client tells a closed
// API from a refused call without probing getTalentConfig.
func TestTalentStatus_SideEffectFree(t *testing.T) {
	t.Parallel()
	status := adminCall{op: "getTalentStatus", method: http.MethodGet, path: "/admin/talent/status"}
	for _, open := range []bool{false, true} {
		e, _, h := adminEnv(t, open)
		for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
			code, body := do(t, h, p.Name, status, "")
			want := http.StatusForbidden
			switch p.Name {
			case "anonymous":
				want = http.StatusUnauthorized
			case "talent_auditor":
				want = http.StatusOK
				if body["auditor_api_enabled"] != open {
					t.Fatalf("open %v: status = %v", open, body)
				}
			}
			if code != want {
				t.Fatalf("open %v: getTalentStatus as %s = %d %v, want %d", open, p.Name, code, body, want)
			}
		}
		if got := e.audit.actions(); len(got) != 0 {
			t.Fatalf("open %v: the status read audited %q", open, got)
		}
	}
}

// TestTalentContract_BoundsAndNullableConfig (BE-129, BE-117): the
// auditor's texts carry the service's 500-character bound in the contract
// (the requirement stays the service's: 422 after the flag and authz), and
// active_version is nullable (null before the first version).
func TestTalentContract_BoundsAndNullableConfig(t *testing.T) {
	t.Parallel()
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := huma.SchemaFromType(reg, reflect.TypeFor[talent.NeutralizationBody]())
	if n := s.Properties["note"]; n == nil || n.MaxLength == nil || *n.MaxLength != 500 || n.MinLength != nil {
		t.Fatalf("note = %+v", n)
	}
	huma.SchemaFromType(reg, reflect.TypeFor[talent.TalentConfigDTO]())
	v := reg.Map()["TalentConfigVersionDTO"]
	if v == nil || !v.Nullable {
		t.Fatalf("TalentConfigVersionDTO = %+v, want a nullable object", v)
	}
	// Over HTTP an over-long reversal reason is refused by the contract.
	_, f, h := adminEnv(t, true)
	long := adminCall{op: "reverseTalentPointEvent", method: http.MethodPost,
		path: "/admin/talent/point-events/" + f.point.ID.String() + "/reversal",
		body: `{"reason":"` + strings.Repeat("ç", 501) + `","reauth":{"password":"` + reauthPassword + `"}}`}
	if code, body := do(t, h, "talent_auditor", long, accessReason); code != http.StatusUnprocessableEntity {
		t.Fatalf("501-character reason = %d %v", code, body)
	}
}
