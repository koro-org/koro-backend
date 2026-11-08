package outreach_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/ratelimit"
)

// testNoise is the production noise over a fixed test key.
func testNoise(t *testing.T, secret byte) *outreach.KeyedNoise {
	t.Helper()
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "test1", Secret: bytes.Repeat([]byte{secret}, crypto.MinMACKeySize)}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := outreach.NewKeyedNoise(d)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fixedNoise offsets every estimate by off, within the allowed range.
type fixedNoise struct{ off int }

func (f fixedNoise) Offset(_ []byte, half int) (int, error) { return max(-half, min(half, f.off)), nil }

// fixedIDs returns n deterministic IDs (the tests that read a keyed offset
// stay the same from run to run).
func fixedIDs(prefix string, n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = authztest.ID(prefix + strconv.Itoa(i))
	}
	return out
}

// TestKeyedNoise_DeterministicBoundedAndSpread (BE-203): the offset of a
// seed never changes, stays in [-half, half] and is spread evenly over the
// range across seeds; another key draws other offsets; a zero range adds
// nothing; a negative range and a missing key are errors.
func TestKeyedNoise_DeterministicBoundedAndSpread(t *testing.T) {
	t.Parallel()
	n := testNoise(t, 7)
	const draws = 11_000
	seen, sum := map[int]int{}, 0
	for i := range draws {
		seed := []byte("seed-" + strconv.Itoa(i))
		a, err := n.Offset(seed, 5)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := n.Offset(seed, 5); a != b {
			t.Fatalf("seed %d: offset %d, then %d", i, a, b)
		}
		if a < -5 || a > 5 {
			t.Fatalf("offset %d outside [-5, 5]", a)
		}
		seen[a]++
		sum += a
	}
	for v := -5; v <= 5; v++ {
		if c := seen[v]; c < 800 || c > 1200 {
			t.Fatalf("offset %d drawn %d times in %d (about 1000 expected)", v, c, draws)
		}
	}
	if mean := float64(sum) / draws; math.Abs(mean) > 0.2 {
		t.Fatalf("mean offset %.3f", mean)
	}
	other, differ := testNoise(t, 8), 0
	for i := range 100 {
		seed := []byte("seed-" + strconv.Itoa(i))
		a, _ := n.Offset(seed, 25)
		b, _ := other.Offset(seed, 25)
		if a != b {
			differ++
		}
	}
	if differ < 80 {
		t.Fatalf("two keys agree on %d of 100 offsets", 100-differ)
	}
	if off, err := n.Offset([]byte("x"), 0); err != nil || off != 0 {
		t.Fatalf("Offset(half 0) = %d, %v", off, err)
	}
	if _, err := n.Offset([]byte("x"), -1); err == nil {
		t.Fatal("a negative range is accepted")
	}
	if _, err := outreach.NewKeyedNoise(nil); err == nil {
		t.Fatal("NewKeyedNoise(nil) accepted")
	}
	if _, err := outreach.New(outreach.Deps{}); err == nil || !strings.Contains(err.Error(), "Noise") {
		t.Fatalf("New without Noise = %v, want a missing-dependency error naming it", err)
	}
}

// TestCountAudience_NoiseFixedPerDefinition (BE-203): the offset belongs to
// the definition and its audience — counting again, writing the same
// definition in another order or with duplicates, another org's count, the
// saved segment and the nightly recount all show the same number — while
// definitions of the same size get different offsets, never more than one
// bucket away from the plain bucket.
func TestCountAudience_NoiseFixedPerDefinition(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withNoise(testNoise(t, 7)), withDailyLimit(200))
	ctx, cma, other := t.Context(), actor(t, "company_admin"), actor(t, "other_company_member")
	depts := fixedIDs("be203_noise_dept_", 20)
	for _, d := range depts {
		e.audience.add(40, true, uniKey(uniA), deptKey(d), yearKey(4))
	}
	shown := map[int]int{}
	var first0 int
	for i, d := range depts {
		got, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Dept: []uuid.UUID{d}, Year: []int16{4}})
		if err != nil || got.Count == nil {
			t.Fatalf("count of 40 students = %+v, %v", got, err)
		}
		again, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Year: []int16{4, 4}, Dept: []uuid.UUID{d, d}})
		if err != nil || again.Count == nil || *again.Count != *got.Count {
			t.Fatalf("the same definition again = %+v, %v; first %d", again, err, *got.Count)
		}
		if c := *got.Count; c != 30 && c != 40 {
			t.Fatalf("40 students shown as %d: more than one bucket away", c)
		}
		shown[*got.Count]++
		if i == 0 {
			first0 = *got.Count
		}
	}
	if len(shown) != 2 {
		t.Fatalf("20 definitions of 40 students all show %v: the offset does not follow the definition", shown)
	}
	def := outreach.Definition{Dept: []uuid.UUID{depts[0]}, Year: []int16{4}}
	got, err := e.svc.CountAudience(ctx, other, companyB, def)
	if err != nil || got.Count == nil || *got.Count != first0 {
		t.Fatalf("another org's count = %+v, %v; want %d", got, err, first0)
	}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Bölüm", Definition: def})
	if err != nil || seg.Estimate.Count == nil || *seg.Estimate.Count != first0 {
		t.Fatalf("segment estimate = %+v, %v; want %d", seg.Estimate, err, first0)
	}
	e.recount(t)
	v, err := e.svc.GetSegment(ctx, cma, companyA, seg.ID)
	if err != nil || v.Estimate.Count == nil || *v.Estimate.Count != first0 {
		t.Fatalf("estimate after the recount = %+v, %v; want %d", v.Estimate, err, first0)
	}
	c, err := e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	if err != nil || c.Size != first0 {
		t.Fatalf("Compile = %+v, %v; want size %d", c, err, first0)
	}
}

// TestCountAudience_NoiseBelowThreshold (BE-203): a size the offset takes
// below 10 is shown as below the threshold (a compile that needs k is
// refused); a size below 10 never gets a number, whatever the offset.
func TestCountAudience_NoiseBelowThreshold(t *testing.T) {
	t.Parallel()
	ctx, cma := t.Context(), actor(t, "company_admin")
	def := outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{4}}

	down := newEnv(t, withNoise(fixedNoise{off: -5}))
	down.audience.add(12, true, uniKey(uniA), yearKey(4))
	got, err := down.svc.CountAudience(ctx, cma, companyA, def)
	if err != nil || got.Count != nil || !got.BelowThreshold {
		t.Fatalf("12 students offset by -5 = %+v, %v; want below the threshold", got, err)
	}
	_, err = down.svc.Compile(ctx, cma, specFor(companyA, def))
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)

	up := newEnv(t, withNoise(fixedNoise{off: 5}))
	up.audience.add(12, true, uniKey(uniA), yearKey(4))
	up.audience.add(9, true, uniKey(uniB), yearKey(4))
	if got, err := up.svc.CountAudience(ctx, cma, companyA, def); err != nil || got.Count == nil || *got.Count != 10 {
		t.Fatalf("12 students offset by +5 = %+v, %v; want 10", got, err)
	}
	got, err = up.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Uni: []uuid.UUID{uniB}})
	if err != nil || got.Count != nil || !got.BelowThreshold {
		t.Fatalf("9 students offset by +5 = %+v, %v; want below the threshold", got, err)
	}
}

// wantValueRefused checks a value floor refusal: 422
// outreach.audience_value_too_small at loc, without a number.
func wantValueRefused(t *testing.T, err error, loc string) {
	t.Helper()
	if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.audience_value_too_small" {
		t.Fatalf("%v, want 422 outreach.audience_value_too_small", err)
	}
	pe, _ := errors.AsType[*problem.Error](err)
	f := pe.Fields()
	if len(f) != 1 || f[0].Location != loc {
		t.Fatalf("fields %+v, want one at %s", f, loc)
	}
	if strings.ContainsAny(pe.Title()+pe.Error()+f[0].Message, "0123456789") {
		t.Fatalf("the refusal carries a number: %q %q", pe.Error(), f[0].Message)
	}
}

// TestCountAudience_ValueFloor (BE-203): every value of a dimension with
// several values must reach 10 students with the other dimensions; the
// refusal points at the value where the request wrote it. A dimension with
// one value and an audience below 10 as a whole are left to k (no number,
// no refusal). Each refusal spends a unit and raises the value_floor alarm.
func TestCountAudience_ValueFloor(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(50))
	ctx, cma := t.Context(), actor(t, "company_admin")
	ids := fixedIDs("be203_floor_dept_", 3)
	d1, d2, d3 := ids[0], ids[1], ids[2]
	e.audience.add(15, true, uniKey(uniA), deptKey(d1), yearKey(4))
	e.audience.add(3, true, uniKey(uniA), deptKey(d2), yearKey(4))
	e.audience.add(20, true, uniKey(uniA), deptKey(d2), yearKey(3))

	for _, tc := range []struct {
		depts []uuid.UUID
		loc   string
	}{
		{[]uuid.UUID{d1, d2}, "body.definition.dept[1]"},
		{[]uuid.UUID{d2, d1, d2}, "body.definition.dept[0]"},
	} {
		_, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Dept: tc.depts, Year: []int16{4}})
		wantValueRefused(t, err, tc.loc)
	}
	_, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Dept: []uuid.UUID{d2}, Year: []int16{3, 4}})
	wantValueRefused(t, err, "body.definition.year[1]")

	for name, def := range map[string]outreach.Definition{
		"one value":         {Dept: []uuid.UUID{d2}, Year: []int16{4}},
		"below 10 in total": {Dept: []uuid.UUID{d2, d3}, Year: []int16{4}},
	} {
		got, err := e.svc.CountAudience(ctx, cma, companyA, def)
		if err != nil || got.Count != nil || !got.BelowThreshold {
			t.Fatalf("%s = %+v, %v; want below the threshold", name, got, err)
		}
	}
	// Every value large enough: 15 + 3 + 20.
	got, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Dept: []uuid.UUID{d1, d2}, Year: []int16{3, 4}})
	if err != nil || got.Count == nil || *got.Count != 30 {
		t.Fatalf("every value large enough = %+v, %v; want 30", got, err)
	}
	if a := alarmOf(e, outreach.AlarmValueFloor); a != 3 {
		t.Fatalf("value_floor alarms = %d, want 3", a)
	}

	// The HTTP answer names the value too.
	rec, body := call(t, newHTTP(t, e), "company_admin", http.MethodPost, countPath(companyA),
		map[string]any{"definition": map[string]any{"dept": []string{d1.String(), d2.String()}, "year": []int{4}}}, nil)
	errs, _ := body["errors"].([]any)
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "outreach.audience_value_too_small" || len(errs) != 1 ||
		errs[0].(map[string]any)["location"] != "body.definition.dept[1]" {
		t.Fatalf("HTTP refusal = %d %v", rec.Code, body)
	}
}

// TestValueFloor_SegmentsCompileAndRecount (BE-203): createSegment,
// updateSegment and Compile (inline or saved) refuse a small value the same
// way and write nothing; a university's own targeting too; a saved segment
// whose value shrinks below 10 gets a NULL estimate overnight and no longer
// compiles.
func TestValueFloor_SegmentsCompileAndRecount(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(50))
	ctx, cma := t.Context(), actor(t, "company_admin")
	ids := fixedIDs("be203_seg_dept_", 3)
	d1, d2, d3 := ids[0], ids[1], ids[2]
	e.audience.add(15, true, deptKey(d1), yearKey(4))
	e.audience.add(12, true, deptKey(d2), yearKey(4))
	def := outreach.Definition{Dept: []uuid.UUID{d1, d2}, Year: []int16{4}}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "İki bölüm", Definition: def})
	if err != nil || seg.Estimate.Count == nil || *seg.Estimate.Count != 20 {
		t.Fatalf("segment = %+v, %v", seg.Estimate, err)
	}

	e.audience.add(2, true, deptKey(d3), yearKey(4))
	small := outreach.Definition{Dept: []uuid.UUID{d1, d2, d3}, Year: []int16{4}}
	_, err = e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Üç bölüm", Definition: small})
	wantValueRefused(t, err, "body.definition.dept[2]")
	page, err := e.svc.ListSegments(ctx, cma, companyA, outreach.ListQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("segments after the refusal = %d, %v", len(page.Items), err)
	}
	_, err = e.svc.UpdateSegment(ctx, cma, companyA, seg.ID, seg.ETag, outreach.SegmentPatch{Definition: &small})
	wantValueRefused(t, err, "body.definition.dept[2]")
	if v, err := e.svc.GetSegment(ctx, cma, companyA, seg.ID); err != nil || !slices.Equal(v.Definition.Dept, seg.Definition.Dept) {
		t.Fatalf("segment after the refused update = %+v, %v", v.Definition, err)
	}
	_, err = e.svc.Compile(ctx, cma, specFor(companyA, small))
	wantValueRefused(t, err, "body.audience.definition.dept[2]")

	// A university's own targeting: year 2 has 2 students of the university.
	e.audience.add(14, true, uniKey(uniA), yearKey(3))
	e.audience.add(2, true, uniKey(uniA), yearKey(2))
	_, err = e.svc.Compile(ctx, actor(t, "university_rep"), specFor(uniOrgA, outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3, 2}}))
	wantValueRefused(t, err, "body.audience.definition.year[1]")

	// Overnight d2 shrinks to 4.
	e.audience.reset()
	e.audience.add(15, true, deptKey(d1), yearKey(4))
	e.audience.add(4, true, deptKey(d2), yearKey(4))
	e.clk.Advance(time.Hour)
	e.recount(t)
	r, err := e.store.Get(ctx, companyA, seg.ID)
	if err != nil || r.LastCount != nil {
		t.Fatalf("recounted segment = %v, %v; want NULL", r.LastCount, err)
	}
	_, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	wantValueRefused(t, err, "body.audience.segment_id.dept["+strconv.Itoa(slices.Index(r.Definition.Dept, d2))+"]")
}

// TestDefinition_EventCap (BE-203): a definition ORs at most ten events
// (duplicates aside); the eleventh is 422 validation_failed at the event
// field, in the service and in the request schema.
func TestDefinition_EventCap(t *testing.T) {
	t.Parallel()
	ev := &fakeEvents{ok: map[uuid.UUID]bool{}}
	e := newEnv(t, withEvents(ev))
	ctx, cma := t.Context(), actor(t, "company_admin")
	events := fixedIDs("be203_event_", outreach.MaxEvents+1)
	for _, id := range events {
		ev.set(id, true)
	}
	_, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Event: events})
	pe, _ := errors.AsType[*problem.Error](err)
	if status, code := problemtest.StatusCode(err); status != 422 || code != "validation_failed" || pe == nil ||
		len(pe.Fields()) != 1 || pe.Fields()[0].Location != "body.definition.event" {
		t.Fatalf("eleven events = %v", err)
	}
	ten := events[:outreach.MaxEvents]
	for _, def := range []outreach.Definition{{Event: ten}, {Event: append(slices.Clone(ten), ten[0])}} {
		if got, err := e.svc.CountAudience(ctx, cma, companyA, def); err != nil || !got.BelowThreshold {
			t.Fatalf("ten events = %+v, %v", got, err)
		}
	}
	strs := make([]string, len(events))
	for i, id := range events {
		strs[i] = id.String()
	}
	rec, body := call(t, newHTTP(t, e), "company_admin", http.MethodPost, countPath(companyA),
		map[string]any{"definition": map[string]any{"event": strs}}, nil)
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "validation_failed" {
		t.Fatalf("HTTP eleven events = %d %v", rec.Code, body)
	}
}

// alarmOf is the count alarm counter of reason.
func alarmOf(e *env, reason string) int64 {
	for _, a := range e.mod.CountAlarms() {
		if a.Reason == reason {
			return a.Count
		}
	}
	return -1
}

// familyDown fails the family watch only (an unavailable limiter for it).
type familyDown struct{ outreach.Limiter }

func (f familyDown) Allow(ctx context.Context, r ratelimit.Rule, key string) (ratelimit.Result, error) {
	if r.Name == "outreach_count_family" {
		return ratelimit.Result{}, ratelimit.ErrUnavailable
	}
	return f.Limiter.Allow(ctx, r, key)
}

// TestCount_Alarms (BE-203): 30 counts of one definition family in a day
// are routine, the 31st raises the family alarm (another family stays
// quiet); the watch never refuses a count and an unavailable limiter only
// skips it; a spent budget raises the budget alarm.
func TestCount_Alarms(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(100))
	ctx, cma := t.Context(), actor(t, "company_admin")
	if got := e.mod.CountAlarms(); len(got) != 3 || got[0].Reason != outreach.AlarmFamily ||
		got[1].Reason != outreach.AlarmValueFloor || got[2].Reason != outreach.AlarmBudget {
		t.Fatalf("CountAlarms = %+v", got)
	}
	depts := fixedIDs("be203_family_dept_", 31)
	for i, d := range depts {
		if _, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Dept: []uuid.UUID{d}, Year: []int16{4}}); err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if i == 30 {
			want = 1
		}
		if a := alarmOf(e, outreach.AlarmFamily); a != want {
			t.Fatalf("after %d counts of one family: %d alarms, want %d", i+1, a, want)
		}
	}
	if _, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Uni: []uuid.UUID{uniA}}); err != nil {
		t.Fatal(err)
	}
	if a := alarmOf(e, outreach.AlarmFamily); a != 1 {
		t.Fatalf("another family raised an alarm: %d", a)
	}

	down := newEnv(t, withDailyLimit(2), withLimiter(familyDown{ratelimit.NewMem(clock.NewFake(t0))}))
	for range 2 {
		if _, err := down.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Uni: []uuid.UUID{uniA}}); err != nil {
			t.Fatalf("a count with the family watch down = %v", err)
		}
	}
	_, err := down.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Uni: []uuid.UUID{uniA}})
	problemtest.WantErr(t, err, outreach.ErrRateLimited)
	if a := alarmOf(down, outreach.AlarmBudget); a != 1 {
		t.Fatalf("budget alarms = %d, want 1", a)
	}
}

// TestOutreachStats_StoredEstimateNotOffsetTwice (BE-190 × BE-203): the
// company panel shows a segment's stored, already offset estimate as it is
// and counts nobody again.
func TestOutreachStats_StoredEstimateNotOffsetTwice(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withNoise(testNoise(t, 7)))
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(47, true, uniKey(uniA), yearKey(3))
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Üçüncü sınıflar",
		Definition: outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}}})
	if err != nil || seg.Estimate.Count == nil {
		t.Fatalf("segment = %+v, %v", seg.Estimate, err)
	}
	calls := e.audience.callCount()
	stats, ok := e.svc.(outreach.Statistics)
	if !ok {
		t.Fatal("the service is not outreach.Statistics")
	}
	uni := uniA
	st, err := stats.OrgOutreachStats(ctx, companyA, t0.Add(-time.Hour), t0.Add(time.Hour), outreach.StatsFilter{Uni: &uni})
	if err != nil || len(st.Segments) != 1 || st.Segments[0].Estimate.Count == nil || *st.Segments[0].Estimate.Count != *seg.Estimate.Count {
		t.Fatalf("panel = %+v, %v; want the stored %d", st.Segments, err, *seg.Estimate.Count)
	}
	if e.audience.callCount() != calls {
		t.Fatal("the panel counted students again")
	}
}

// TestBucket_PublishedValueIsItsOwnBucket (BE-203): a published value
// passes through PublicEstimate unchanged, so a stored estimate is never
// rounded or shifted twice.
func TestBucket_PublishedValueIsItsOwnBucket(t *testing.T) {
	t.Parallel()
	for n := outreach.KThreshold; n < 60_000; n++ {
		b := outreach.Bucket(n)
		if e := outreach.PublicEstimate(b); e.Count == nil || *e.Count != b {
			t.Fatalf("PublicEstimate(Bucket(%d) = %d) = %+v", n, b, e)
		}
	}
}
