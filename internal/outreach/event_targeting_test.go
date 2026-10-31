package outreach_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/outreach"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// fakeEvents is events.Targeting's TargetableEvents: the events a company
// may name (events applies the rule; the fake answers what it was told).
type fakeEvents struct {
	mu    sync.Mutex
	ok    map[uuid.UUID]bool
	calls int
	err   error
}

func (f *fakeEvents) TargetableEvents(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if v, known := f.ok[id]; known {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeEvents) set(id uuid.UUID, targetable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ok[id] = targetable
}

func withEvents(f *fakeEvents) envOption {
	return func(_ *env, d *outreach.Deps) { d.Events = f }
}

func eventKey(id uuid.UUID) string    { return "event:" + id.String() }
func attendedKey(id uuid.UUID) string { return "attended:" + id.String() }

// TestDefinition_EventDimension (BE-161): an event list is one dimension —
// its registrants (event:) or, with event_attended, its attendees
// (attended:) — sorted and de-duplicated like the others; event_attended
// without events, more than four dimensions at once or a nil event ID are
// 422 at their location.
func TestDefinition_EventDimension(t *testing.T) {
	t.Parallel()
	ev1, ev2 := uuid.NewV7(), uuid.NewV7()
	reg := outreach.Definition{Event: []uuid.UUID{ev1, ev2}, Year: []int16{3}}
	if got, want := reg.Keys(), sortedKeys(eventKey(ev1), eventKey(ev2), yearKey(3)); !slices.Equal(got, want) || reg.DimCount() != 2 {
		t.Fatalf("Keys = %v (dims %d), want %v", got, reg.DimCount(), want)
	}
	att := outreach.Definition{Event: []uuid.UUID{ev1}, EventAttended: true}
	if got := att.Keys(); !slices.Equal(got, []string{attendedKey(ev1)}) || att.DimCount() != 1 {
		t.Fatalf("attended Keys = %v (dims %d)", got, att.DimCount())
	}

	events := &fakeEvents{ok: map[uuid.UUID]bool{ev1: true, ev2: true}}
	e := newEnv(t, withEvents(events))
	cma := actor(t, "company_admin")
	for name, tc := range map[string]struct {
		def outreach.Definition
		loc string
	}{
		"attended without event": {outreach.Definition{Year: []int16{3}, EventAttended: true}, "body.definition.event_attended"},
		"five dimensions": {outreach.Definition{Uni: []uuid.UUID{uniA}, Dept: []uuid.UUID{uuid.NewV7()}, Year: []int16{3},
			Club: []uuid.UUID{clubA}, Event: []uuid.UUID{ev1}}, "body.definition"},
		"nil event": {outreach.Definition{Event: []uuid.UUID{uuid.Nil()}}, "body.definition.event"},
	} {
		_, err := e.svc.CountAudience(t.Context(), cma, companyA, tc.def)
		pe, ok := errors.AsType[*problem.Error](err)
		if !ok || pe.Status() != 422 || len(pe.Fields()) != 1 || pe.Fields()[0].Location != tc.loc {
			t.Errorf("%s: %v, want 422 at %s", name, err, tc.loc)
		}
	}
	if n := e.audience.callCount(); n != 0 {
		t.Fatalf("an invalid definition was counted %d times", n)
	}
}

func sortedKeys(keys ...string) []string {
	slices.Sort(keys)
	return keys
}

// TestCountAudience_EventDimension_CompanyOnly (BE-161, Q-39, 03 k16): a
// company (and the platform) counts the registrants or the attendees of
// targetable events; an event that is not targetable — unknown, draft,
// cancelled, members-only, sensitive, too old: one answer — is 422
// outreach.event_not_targetable before anything is counted, as is any
// event without the events reader (fail closed); a club or a university
// never names an event (422 outreach.target_out_of_scope).
func TestCountAudience_EventDimension_CompanyOnly(t *testing.T) {
	t.Parallel()
	talk, members := uuid.NewV7(), uuid.NewV7()
	events := &fakeEvents{ok: map[uuid.UUID]bool{talk: true, members: false}}
	e := newEnv(t, withEvents(events))
	ctx := t.Context()
	e.audience.add(23, true, uniKey(uniA), eventKey(talk))
	e.audience.add(14, true, uniKey(uniA), eventKey(talk), attendedKey(talk))
	e.audience.add(30, false, eventKey(talk), attendedKey(talk)) // not eligible: never counted

	for _, tc := range []struct {
		name  string
		actor authz.Actor
		org   uuid.UUID
		def   outreach.Definition
		want  int
	}{
		{"registrants", actor(t, "company_admin"), companyA, outreach.Definition{Event: []uuid.UUID{talk}}, 30},
		{"attendees", actor(t, "company_member"), companyA, outreach.Definition{Event: []uuid.UUID{talk}, EventAttended: true}, 10},
		{"platform", actor(t, "platform_admin"), platformOrg, outreach.Definition{Event: []uuid.UUID{talk}, Uni: []uuid.UUID{uniA}}, 30},
	} {
		got, err := e.svc.CountAudience(ctx, tc.actor, tc.org, tc.def)
		if err != nil || got.Count == nil || *got.Count != tc.want {
			t.Fatalf("%s: %+v, %v; want %d", tc.name, got, err, tc.want)
		}
	}

	before := e.audience.callCount()
	cma := actor(t, "company_admin")
	for name, def := range map[string]outreach.Definition{
		"not targetable":        {Event: []uuid.UUID{members}},
		"unknown":               {Event: []uuid.UUID{uuid.NewV7()}},
		"OR with a targetable":  {Event: []uuid.UUID{talk, members}},
		"attendees of the same": {Event: []uuid.UUID{members}, EventAttended: true},
	} {
		_, err := e.svc.CountAudience(ctx, cma, companyA, def)
		if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.event_not_targetable" {
			t.Errorf("%s: %v, want 422 outreach.event_not_targetable", name, err)
		}
		_, err = e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Etkinlik", Definition: def})
		problemtest.WantErr(t, err, outreach.ErrEventNotTargetable)
		_, err = e.svc.Compile(ctx, cma, specFor(companyA, def))
		problemtest.WantErr(t, err, outreach.ErrEventNotTargetable)
	}
	for _, tc := range []struct {
		name  string
		actor authz.Actor
		org   uuid.UUID
		def   outreach.Definition
	}{
		{"club_admin", actor(t, "club_admin"), clubA, outreach.Definition{Club: []uuid.UUID{clubA}, Event: []uuid.UUID{talk}}},
		{"university_rep", actor(t, "university_rep"), uniOrgA, outreach.Definition{Uni: []uuid.UUID{uniA}, Event: []uuid.UUID{talk}}},
	} {
		_, err := e.svc.CountAudience(ctx, tc.actor, tc.org, tc.def)
		problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)
	}
	if n := e.audience.callCount(); n != before {
		t.Fatalf("a refused event targeting was counted %d times", n-before)
	}

	// Without the events reader nothing is targetable.
	bare := newEnv(t)
	bare.audience.add(20, true, eventKey(talk))
	_, err := bare.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Event: []uuid.UUID{talk}})
	problemtest.WantErr(t, err, outreach.ErrEventNotTargetable)
	// A failing reader is an error, never a targetable event.
	events.err = errors.New("events unavailable")
	if _, err := e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Event: []uuid.UUID{talk}}); err == nil {
		t.Fatal("count with a failing events reader succeeded")
	}
}

// TestCountAudience_EventIntersection_BelowK (BE-161; ticket: a single
// event narrowed by department and class year must not leak a small
// group): the event dimension is under the same k-anonymity as every
// other — 9 students of one event, one department and one class year are
// below_threshold with no number anywhere (count, error, saved segment,
// Compile, HTTP body); widened to the whole event the bucketed estimate
// appears.
func TestCountAudience_EventIntersection_BelowK(t *testing.T) {
	t.Parallel()
	ev, deptA, deptB := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	e := newEnv(t, withEvents(&fakeEvents{ok: map[uuid.UUID]bool{ev: true}}))
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(9, true, uniKey(uniA), deptKey(deptB), yearKey(3), eventKey(ev))
	e.audience.add(26, true, uniKey(uniA), deptKey(deptA), yearKey(2), eventKey(ev))
	small := outreach.Definition{Event: []uuid.UUID{ev}, Dept: []uuid.UUID{deptB}, Year: []int16{3}}

	got, err := e.svc.CountAudience(ctx, cma, companyA, small)
	if err != nil || got.Count != nil || !got.BelowThreshold || got.Dimensions != 3 {
		t.Fatalf("CountAudience(event ∩ dept ∩ year) = %+v, %v; want below the threshold, no number", got, err)
	}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Küçük grup", Definition: small})
	if err != nil || seg.Estimate.Count != nil || !seg.Estimate.BelowThreshold {
		t.Fatalf("CreateSegment = %+v, %v", seg.Estimate, err)
	}
	_, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)
	pe, _ := errors.AsType[*problem.Error](err)
	if len(pe.Fields()) != 0 || strings.ContainsAny(pe.Error()+pe.Title(), "0123456789") {
		t.Fatalf("the error leaks a number: %q %q %v", pe.Error(), pe.Title(), pe.Fields())
	}

	app := newHTTP(t, e)
	rec, out := call(t, app, "company_admin", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{
		"event": []string{ev.String()}, "dept": []string{deptB.String()}, "year": []int{3},
	}}, nil)
	if rec.Code != http.StatusOK || out["count"] != nil || out["below_threshold"] != true ||
		strings.Contains(rec.Body.String(), "9") {
		t.Fatalf("countAudience(event ∩ dept ∩ year) = %d %s", rec.Code, rec.Body.String())
	}
	rec, out = call(t, app, "company_admin", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{
		"event": []string{ev.String()},
	}}, nil)
	if rec.Code != http.StatusOK || out["count"] != float64(30) || out["dimensions"] != float64(1) {
		t.Fatalf("countAudience(event) = %d %v; want the bucket 30", rec.Code, out)
	}
	// The attendees of the same event: nobody checked in, below the threshold.
	rec, out = call(t, app, "company_admin", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{
		"event": []string{ev.String()}, "event_attended": true,
	}}, nil)
	if rec.Code != http.StatusOK || out["count"] != nil || out["below_threshold"] != true {
		t.Fatalf("countAudience(attended) = %d %v", rec.Code, out)
	}
	if rec, _ := call(t, app, "company_admin", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{
		"event": []string{"not-a-uuid"},
	}}, nil); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "body.definition.event[0]") {
		t.Fatalf("malformed event id = %d %s", rec.Code, rec.Body.String())
	}
}

// TestSegment_EventDefinitionRoundTrip (BE-161): a saved segment keeps its
// event dimension (definition, audit shape without values), an unchanged
// definition is not counted again, and the HTTP DTO returns it.
func TestSegment_EventDefinitionRoundTrip(t *testing.T) {
	t.Parallel()
	ev := uuid.NewV7()
	e := newEnv(t, withEvents(&fakeEvents{ok: map[uuid.UUID]bool{ev: true}}))
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(12, true, eventKey(ev), attendedKey(ev), yearKey(4))
	def := outreach.Definition{Event: []uuid.UUID{ev, ev}, EventAttended: true, Year: []int16{4}}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Katılımcılar", Definition: def})
	if err != nil || seg.Estimate.Count == nil || *seg.Estimate.Count != 10 {
		t.Fatalf("CreateSegment = %+v, %v", seg, err)
	}
	if !slices.Equal(seg.Definition.Event, []uuid.UUID{ev}) || !seg.Definition.EventAttended {
		t.Fatalf("stored definition = %+v", seg.Definition)
	}
	if ev0 := e.audit.Last(); ev0.Metadata["event"] != 1 || ev0.Metadata["event_attended"] != true {
		t.Fatalf("audit shape = %v", ev0.Metadata)
	} else if strings.Contains(fmtAny(ev0.Metadata), ev.String()) {
		t.Fatalf("the audit record names the event: %v", ev0.Metadata)
	}
	calls := e.audience.callCount()
	if _, err := e.svc.UpdateSegment(ctx, cma, companyA, seg.ID, seg.ETag, outreach.SegmentPatch{Definition: &def}); err != nil {
		t.Fatal(err)
	}
	if e.audience.callCount() != calls {
		t.Fatal("an unchanged event definition was counted again")
	}
	app := newHTTP(t, e)
	rec, out := call(t, app, "company_member", http.MethodPost, segmentsPath(companyA), map[string]any{"name": "HTTP",
		"definition": map[string]any{"event": []string{ev.String()}, "event_attended": true, "year": []int{4}}}, nil)
	d, _ := out["definition"].(map[string]any)
	if evs, _ := d["event"].([]any); rec.Code != http.StatusCreated || len(evs) != 1 || evs[0] != ev.String() ||
		d["event_attended"] != true || out["dimensions"] != float64(2) {
		t.Fatalf("createSegment = %d %v", rec.Code, out)
	}
	back, err := outreach.SegmentDefinitionDTO{Event: []string{ev.String()}, EventAttended: true}.Definition("body.definition")
	if err != nil || !slices.Equal(back.Event, []uuid.UUID{ev}) || !back.EventAttended {
		t.Fatalf("SegmentDefinitionDTO.Definition = %+v, %v", back, err)
	}
}

func fmtAny(v any) string { return fmt.Sprint(v) }

// TestScheduleDispatch_EventNoLongerTargetable (BE-161): a targeted post's
// dispatch re-checks the events its keys name, like a sensitive club (03
// Y2): an event that left the targeting since the post was compiled is 422
// outreach.event_not_targetable; a university's post never names one.
func TestScheduleDispatch_EventNoLongerTargetable(t *testing.T) {
	t.Parallel()
	ev := uuid.NewV7()
	events := &fakeEvents{ok: map[uuid.UUID]bool{ev: true}}
	e := newEnv(t, withEvents(events))
	ctx := t.Context()
	e.grantDispatches(t, companyA, 3)
	sched := func(a authz.Actor, p outreach.PostInfo) error {
		_, err := e.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: a, Post: p, Channels: []outreach.Channel{outreach.ChannelPush}})
		return err
	}
	cma := actor(t, "company_admin")
	if err := sched(cma, e.targetedPost(companyA, outreach.AuthorCompany, attendedKey(ev))); err != nil {
		t.Fatalf("dispatch of a targetable event's attendees: %v", err)
	}
	events.set(ev, false)
	for _, keys := range [][]string{{eventKey(ev)}, {attendedKey(ev), uniKey(uniA)}} {
		err := sched(cma, e.targetedPost(companyA, outreach.AuthorCompany, keys...))
		if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.event_not_targetable" {
			t.Fatalf("dispatch of %v: %v, want 422 outreach.event_not_targetable", keys, err)
		}
	}
	err := sched(actor(t, "university_rep"), e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA), eventKey(ev)))
	problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)
}
