package outreach_test

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/idempotency"
)

// TestSegment_LastCountNullWhenBelow10: a segment below k = 10 is saved
// with last_count NULL and shown as count null + below_threshold (service,
// store and JSON); from 10 on the stored and shown number is the bucket,
// never the exact count. The store refuses a last_count below 10.
func TestSegment_LastCountNullWhenBelow10(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cmm := t.Context(), actor(t, "company_member")
	e.audience.add(7, true, uniKey(uniA), yearKey(3))
	e.audience.add(23, true, uniKey(uniA), yearKey(4))

	small, err := e.svc.CreateSegment(ctx, cmm, companyA, outreach.SegmentInput{
		Name: "Üçüncü sınıflar", Definition: outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.store.Get(ctx, companyA, small.ID)
	if err != nil || row.LastCount != nil || row.LastCountedAt == nil || !row.LastCountedAt.Equal(t0) {
		t.Fatalf("stored small segment = %+v, %v", row, err)
	}
	if small.Estimate.Count != nil || !small.Estimate.BelowThreshold {
		t.Fatalf("small estimate %+v", small.Estimate)
	}

	big, err := e.svc.CreateSegment(ctx, cmm, companyA, outreach.SegmentInput{
		Name: "Dördüncü sınıflar", Definition: outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := e.store.Get(ctx, companyA, big.ID); row.LastCount == nil || *row.LastCount != 20 {
		t.Fatalf("stored big segment last_count = %v (23 → bucket 20)", row.LastCount)
	}

	// JSON: count null and below_threshold true; the other one 20.
	app := newHTTP(t, e)
	rec, body := call(t, app, "company_member", http.MethodGet, segmentsPath(companyA)+"/"+small.ID.String(), nil, nil)
	cnt, _ := body["count"].(map[string]any)
	if rec.Code != http.StatusOK || cnt == nil || cnt["count"] != nil || cnt["below_threshold"] != true {
		t.Fatalf("getSegment(small) = %d %v", rec.Code, body)
	}
	if v, has := cnt["count"]; !has || v != nil {
		t.Fatalf("count must be present as null: %v", cnt)
	}
	rec, body = call(t, app, "company_member", http.MethodGet, segmentsPath(companyA)+"/"+big.ID.String(), nil, nil)
	cnt, _ = body["count"].(map[string]any)
	if rec.Code != http.StatusOK || cnt["count"] != float64(20) || cnt["below_threshold"] != false || rec.Header().Get("ETag") != big.ETag {
		t.Fatalf("getSegment(big) = %d %v etag %q", rec.Code, body, rec.Header().Get("ETag"))
	}

	// countAudience: the same rule without saving.
	rec, body = call(t, app, "company_member", http.MethodPost, countPath(companyA),
		map[string]any{"definition": defBody(outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}})}, nil)
	if rec.Code != http.StatusOK || body["count"] != nil || body["below_threshold"] != true || body["dimensions"] != float64(2) {
		t.Fatalf("countAudience(7) = %d %v", rec.Code, body)
	}
	if !strings.Contains(rec.Body.String(), `"count":null`) {
		t.Fatalf("count must be null, not omitted: %s", rec.Body.String())
	}

	// The CHECK segments_last_count_chk: never a number below 10.
	bad := row
	bad.LastCount = ptr(9)
	if _, err := e.store.Update(ctx, bad); err == nil {
		t.Fatal("store accepted last_count 9")
	}
}

// TestSegment_CRUD: create (audited, idempotent), list (active/archived,
// org-scoped), get (IDOR 404), update with If-Match (428, 412, unchanged
// patch keeps the ETag, a new definition is counted again), archive
// (idempotent, then updates are 409).
func TestSegment_CRUD(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(15, true, uniKey(uniA), yearKey(3))
	e.audience.add(40, true, uniKey(uniB), yearKey(3))
	def := outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}}

	ictx := idempotency.WithKey(authz.WithActor(ctx, cma), idempotency.Key{
		Scope: cma.UserID.String() + ":POST /orgs/{org_id}/segments", Value: "seg-create-1", Status: 201,
	})
	first, err := e.svc.CreateSegment(ictx, cma, companyA, outreach.SegmentInput{Name: "  Hedef   kitle ", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateSegment(ictx, cma, companyA, outreach.SegmentInput{Name: "  Hedef   kitle ", Definition: def})
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %v, %v (want the first segment)", again.ID, err)
	}
	if first.Name != "Hedef kitle" || first.CreatedBy != cma.UserID || first.OrgID != companyA ||
		!slices.Equal(first.AudienceKeys, def.Keys()) || first.AudienceDimCount != 2 {
		t.Fatalf("created %+v", first.Segment)
	}

	// Another org's path never finds it.
	if _, err := e.svc.GetSegment(ctx, actor(t, "other_company_member"), companyB, first.ID); err == nil {
		t.Fatal("company_b read company_a's segment")
	}

	// Update: If-Match is required and must be current.
	name := "Yeni ad"
	_, err = e.svc.UpdateSegment(ctx, cma, companyA, first.ID, "", outreach.SegmentPatch{Name: &name})
	wantErr(t, err, outreach.ErrPreconditionRequired)
	_, err = e.svc.UpdateSegment(ctx, cma, companyA, first.ID, `"stale"`, outreach.SegmentPatch{Name: &name})
	wantErr(t, err, outreach.ErrPreconditionFailed)
	_, err = e.svc.UpdateSegment(ctx, cma, companyA, first.ID, first.ETag, outreach.SegmentPatch{})
	wantErr(t, err, outreach.ErrEmptyPatch)
	same := first.Name
	unchanged, err := e.svc.UpdateSegment(ctx, cma, companyA, first.ID, first.ETag, outreach.SegmentPatch{Name: &same, Definition: &def})
	if err != nil || unchanged.ETag != first.ETag {
		t.Fatalf("no-op patch = %v, %v (the ETag must stay)", unchanged.ETag, err)
	}
	e.clk.Advance(time.Minute)
	wider := outreach.Definition{Uni: []uuid.UUID{uniA, uniB}, Year: []int16{3}}
	upd, err := e.svc.UpdateSegment(ctx, cma, companyA, first.ID, first.ETag, outreach.SegmentPatch{Name: &name, Definition: &wider})
	if err != nil || upd.Name != name || upd.Estimate.Count == nil || *upd.Estimate.Count != 50 || upd.ETag == first.ETag ||
		!upd.LastCountedAt.Equal(t0.Add(time.Minute)) || !slices.Equal(upd.AudienceKeys, wider.Keys()) {
		t.Fatalf("update = %+v, %v", upd, err)
	}

	// List: active only by default, archived on request; newest first.
	second, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "İkinci", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSegment(ctx, actor(t, "other_company_member"), companyB, outreach.SegmentInput{Name: "Seg B", Definition: def}); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.ListSegments(ctx, cma, companyA, outreach.ListQuery{Limit: 1})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != second.ID || p.Next == nil {
		t.Fatalf("list page 1 = %+v, %v", p, err)
	}
	p, err = e.svc.ListSegments(ctx, cma, companyA, outreach.ListQuery{Limit: 10, After: p.Next})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != first.ID || p.Next != nil {
		t.Fatalf("list page 2 = %+v, %v", p, err)
	}

	// Archive: idempotent; an archived segment does not change.
	arch, err := e.svc.ArchiveSegment(ctx, cma, companyA, first.ID)
	if err != nil || arch.ArchivedAt == nil {
		t.Fatalf("archive = %+v, %v", arch, err)
	}
	arch2, err := e.svc.ArchiveSegment(ctx, cma, companyA, first.ID)
	if err != nil || arch2.ETag != arch.ETag {
		t.Fatalf("second archive = %v, %v", arch2.ETag, err)
	}
	_, err = e.svc.UpdateSegment(ctx, cma, companyA, first.ID, arch.ETag, outreach.SegmentPatch{Name: &name})
	wantErr(t, err, outreach.ErrInvalidState)
	p, _ = e.svc.ListSegments(ctx, cma, companyA, outreach.ListQuery{Limit: 10})
	if len(p.Items) != 1 || p.Items[0].ID != second.ID {
		t.Fatalf("active list %+v", p.Items)
	}
	p, _ = e.svc.ListSegments(ctx, cma, companyA, outreach.ListQuery{Archived: true, Limit: 10})
	if len(p.Items) != 1 || p.Items[0].ID != first.ID {
		t.Fatalf("archived list %+v", p.Items)
	}
	_, err = e.svc.ArchiveSegment(ctx, cma, companyA, uuid.NewV7())
	wantErr(t, err, outreach.ErrNotFound)

	// Every write is audited once, without a count.
	want := []string{"outreach.segment.create", "outreach.segment.update", "outreach.segment.create",
		"outreach.segment.create", "outreach.segment.archive"}
	if got := e.audit.actions(); !slices.Equal(got, want) {
		t.Fatalf("audit %v, want %v", got, want)
	}
	for _, ev := range e.audit.events {
		for k := range ev.Metadata {
			if strings.Contains(k, "count") {
				t.Fatalf("audit metadata carries a count: %v", ev.Metadata)
			}
		}
	}
}

// TestSegment_OrgStatus: writes and counts for a suspended org are 403
// orgs.org_suspended (a platform_admin excepted), for a company awaiting
// verification 403 orgs.org_not_verified, for a closed org 409; reads stay
// open; a missing org is 404.
func TestSegment_OrgStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cma := t.Context(), actor(t, "company_admin")
	def := outreach.Definition{Uni: []uuid.UUID{uniA}}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Seg A", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status orgs.Status
		code   string
	}{
		{orgs.StatusSuspended, "orgs.org_suspended"}, {orgs.StatusPendingReview, "orgs.org_not_verified"},
		{orgs.StatusClosed, "orgs.invalid_state"},
	} {
		e.orgs.set(companyA, orgs.KindCompany, tc.status)
		_, err := e.svc.CountAudience(ctx, cma, companyA, def)
		if _, code := problemOf(err); code != tc.code {
			t.Fatalf("%s: count = %v, want %s", tc.status, err, tc.code)
		}
		_, err = e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Seg B", Definition: def})
		if _, code := problemOf(err); code != tc.code {
			t.Fatalf("%s: create = %v, want %s", tc.status, err, tc.code)
		}
		_, err = e.svc.ArchiveSegment(ctx, cma, companyA, seg.ID)
		if _, code := problemOf(err); code != tc.code {
			t.Fatalf("%s: archive = %v, want %s", tc.status, err, tc.code)
		}
		if _, err := e.svc.GetSegment(ctx, cma, companyA, seg.ID); err != nil {
			t.Fatalf("%s: read = %v", tc.status, err)
		}
	}
	// platform_admin counts for a suspended org.
	e.orgs.set(companyA, orgs.KindCompany, orgs.StatusSuspended)
	if _, err := e.svc.CountAudience(ctx, actor(t, "platform_admin"), companyA, def); err != nil {
		t.Fatalf("platform_admin count for a suspended org: %v", err)
	}
	_, err = e.svc.CountAudience(ctx, actor(t, "platform_admin"), uuid.NewV7(), def)
	wantErr(t, err, outreach.ErrNotFound)
}

// TestCountAudience_RateLimitedPerOrg: countAudience (with createSegment
// and updateSegment) is the audience_count class of 04 §1.9 — 30 per minute
// per org: the 31st request of company_a is 429 with Retry-After while
// company_b is untouched. Behind it every fresh count spends the org's
// daily budget (difference attacks): spent → 429, again per org; an
// unavailable limiter fails closed (503).
func TestCountAudience_RateLimitedPerOrg(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	body := map[string]any{"definition": defBody(outreach.Definition{Uni: []uuid.UUID{uniA}})}
	for i := range 30 {
		if rec, _ := call(t, app, "company_member", http.MethodPost, countPath(companyA), body, nil); rec.Code != http.StatusOK {
			t.Fatalf("count %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec, out := call(t, app, "company_admin", http.MethodPost, countPath(companyA), body, nil)
	if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("31st count of company_a = %d %v", rec.Code, out)
	}
	// The class is per org and shared with segment writes of that org.
	rec, _ = call(t, app, "company_admin", http.MethodPost, segmentsPath(companyA),
		map[string]any{"name": "Yeni", "definition": body["definition"]}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("createSegment after 30 counts = %d", rec.Code)
	}
	if rec, _ := call(t, app, "other_company_member", http.MethodPost, countPath(companyB), body, nil); rec.Code != http.StatusOK {
		t.Fatalf("company_b count = %d", rec.Code)
	}
	// Reads are not in the class.
	if rec, _ := call(t, app, "company_member", http.MethodGet, segmentsPath(companyA), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("listSegments = %d", rec.Code)
	}

	// The daily budget of the service (here 3 per org).
	b := newEnv(t, withDailyLimit(3))
	ctx, def := t.Context(), outreach.Definition{Uni: []uuid.UUID{uniA}}
	for range 3 {
		if _, err := b.svc.CountAudience(ctx, actor(t, "company_member"), companyA, def); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.svc.CountAudience(ctx, actor(t, "company_admin"), companyA, def)
	if status, code := problemOf(err); status != 429 || code != "rate_limited" {
		t.Fatalf("4th count of the day = %v", err)
	}
	_, err = b.svc.CreateSegment(ctx, actor(t, "company_admin"), companyA, outreach.SegmentInput{Name: "Seg X", Definition: def})
	wantErr(t, err, outreach.ErrRateLimited)
	if _, err := b.svc.CountAudience(ctx, actor(t, "other_company_member"), companyB, def); err != nil {
		t.Fatalf("company_b's budget: %v", err)
	}
	calls := b.audience.callCount()
	if calls != 4 {
		t.Fatalf("students counted %d times, want 4 (refused counts never reach count_segment)", calls)
	}
	// A day later the budget is back.
	b.clk.Advance(25 * time.Hour)
	if _, err := b.svc.CountAudience(ctx, actor(t, "company_admin"), companyA, def); err != nil {
		t.Fatalf("next day: %v", err)
	}

	// Without the limiter nothing is counted (fail closed).
	f := newEnv(t, withLimiter(failingLimiter{}))
	_, err = f.svc.CountAudience(ctx, actor(t, "company_member"), companyA, def)
	if status, _ := problemOf(err); status != http.StatusServiceUnavailable {
		t.Fatalf("limiter down: %v, want 503", err)
	}
	if f.audience.callCount() != 0 {
		t.Fatal("counted without a budget")
	}
}

// TestRecountSegments_Updates: outreach.recount_segments refreshes every
// active segment's estimate (k < 10 → NULL, otherwise the bucket),
// including a segment whose club became sensitive (→ NULL), leaves an
// archived one alone, spends no budget and can run twice.
func TestRecountSegments_Updates(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(3))
	ctx, cma, cla := t.Context(), actor(t, "company_admin"), actor(t, "club_admin")
	e.audience.add(25, true, uniKey(uniA), yearKey(4))
	e.audience.add(12, true, clubKey(clubA))
	grow, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Büyüyen", Definition: outreach.Definition{Uni: []uuid.UUID{uniA}}})
	if err != nil {
		t.Fatal(err)
	}
	shrink, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Küçülen", Definition: outreach.Definition{Year: []int16{4}}})
	if err != nil {
		t.Fatal(err)
	}
	club, err := e.svc.CreateSegment(ctx, cla, clubA, outreach.SegmentInput{Name: "Üyeler", Definition: outreach.Definition{Club: []uuid.UUID{clubA}}})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := e.svc.CreateSegment(ctx, cla, clubA, outreach.SegmentInput{Name: "Eski", Definition: outreach.Definition{Club: []uuid.UUID{clubA}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ArchiveSegment(ctx, cla, clubA, archived.ID); err != nil {
		t.Fatal(err)
	}
	last := func(org, id uuid.UUID) (*int, time.Time) {
		t.Helper()
		r, err := e.store.Get(ctx, org, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.LastCount, *r.LastCountedAt
	}
	for _, c := range []struct {
		org, id uuid.UUID
		want    int
	}{{companyA, grow.ID, 20}, {companyA, shrink.ID, 20}, {clubA, club.ID, 10}} {
		if n, _ := last(c.org, c.id); n == nil || *n != c.want {
			t.Fatalf("initial last_count of %s = %v, want %d", c.id, n, c.want)
		}
	}

	// The audience changes overnight: uni A grows, year 4 shrinks below 10
	// (the 25 leave, 5 come), club A turns sensitive.
	e.audience.reset()
	e.audience.add(160, true, uniKey(uniA))
	e.audience.add(5, true, uniKey(uniB), yearKey(4))
	e.audience.add(30, true, clubKey(clubA))
	e.catalog.markSensitive(clubA)
	e.clk.Advance(21 * time.Hour)
	e.recount(t)

	if n, at := last(companyA, grow.ID); n == nil || *n != 150 || !at.Equal(t0.Add(21*time.Hour)) {
		t.Fatalf("grown segment = %v at %v, want 150", n, at)
	}
	if n, _ := last(companyA, shrink.ID); n != nil {
		t.Fatalf("shrunk segment = %d, want NULL", *n)
	}
	if n, _ := last(clubA, club.ID); n != nil {
		t.Fatalf("segment of a now sensitive club = %d, want NULL", *n)
	}
	if n, at := last(clubA, archived.ID); n == nil || !at.Equal(t0) {
		t.Fatalf("archived segment touched: %v at %v", n, at)
	}
	// The job spends no budget: a second run (companyA has one unit left
	// for the day) recounts every segment again.
	e.recount(t)
	if n, _ := last(companyA, grow.ID); n == nil || *n != 150 {
		t.Fatalf("second run = %v", n)
	}
	// The shown estimate follows the stored one.
	v, err := e.svc.GetSegment(ctx, cma, companyA, shrink.ID)
	if err != nil || v.Estimate.Count != nil || !v.Estimate.BelowThreshold {
		t.Fatalf("getSegment after the recount = %+v, %v", v.Estimate, err)
	}
}

// TestHTTP_SegmentRoundTrip: the JSON surface of the segment operations
// (04 §2.8): 201 with ETag, PATCH with If-Match, 428 without, archive.
func TestHTTP_SegmentRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.audience.add(31, true, uniKey(uniA), yearKey(3))
	app := newHTTP(t, e)
	def := defBody(outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3, 3}})
	rec, out := call(t, app, "company_member", http.MethodPost, segmentsPath(companyA), map[string]any{"name": "ODTÜ 3", "definition": def}, nil)
	if rec.Code != http.StatusCreated || rec.Header().Get("ETag") == "" {
		t.Fatalf("createSegment = %d %v", rec.Code, out)
	}
	id, _ := out["segment_id"].(string)
	d, _ := out["definition"].(map[string]any)
	cnt, _ := out["count"].(map[string]any)
	if years, _ := d["year"].([]any); len(years) != 1 || cnt["count"] != float64(30) || out["dimensions"] != float64(2) {
		t.Fatalf("created %v", out)
	}
	path := segmentsPath(companyA) + "/" + id
	if rec, out := call(t, app, "company_member", http.MethodPatch, path, map[string]any{"name": "Yeni"}, nil); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PATCH without If-Match = %d %v", rec.Code, out)
	}
	etag := rec.Header().Get("ETag")
	if rec, out := call(t, app, "company_admin", http.MethodPatch, path, map[string]any{"name": "Yeni"}, map[string]string{"If-Match": etag}); rec.Code != http.StatusOK || out["name"] != "Yeni" {
		t.Fatalf("PATCH = %d %v", rec.Code, out)
	}
	if rec, out := call(t, app, "company_admin", http.MethodPost, path+"/archive", nil, nil); rec.Code != http.StatusOK || out["archived_at"] == nil {
		t.Fatalf("archive = %d %v", rec.Code, out)
	}
	// Malformed IDs: 404 after the org surface; a bad year or UUID: 422.
	if rec, _ := call(t, app, "company_member", http.MethodGet, segmentsPath(companyA)+"/not-a-uuid", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed segment id = %d", rec.Code)
	}
	if rec, _ := call(t, app, "company_member", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{"uni": []string{"x"}}}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed uuid = %d", rec.Code)
	}
	if rec, _ := call(t, app, "company_member", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{}}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty definition = %d", rec.Code)
	}
	// An unknown dimension (an age, a talent band) is not part of the contract.
	if rec, _ := call(t, app, "company_member", http.MethodPost, countPath(companyA), map[string]any{"definition": map[string]any{"talent_band": []string{"top"}, "year": []int{3}}}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown dimension = %d", rec.Code)
	}
}

// TestDTO_NoTalentNoIdentity: the outreach DTOs carry no talent value and
// no person: a count, a definition and the segment's own fields only.
func TestDTO_NoTalentNoIdentity(t *testing.T) {
	t.Parallel()
	banned := []string{"talent", "score", "band", "point", "rank", "user", "student", "email", "name_of", "sample"}
	for _, v := range []any{outreach.SegmentDTO{}, outreach.AudienceCountResultDTO{}, outreach.SegmentDefinitionDTO{}} {
		var walk func(rt reflect.Type)
		walk = func(rt reflect.Type) {
			for i := range rt.NumField() {
				f := rt.Field(i)
				if f.Anonymous {
					walk(f.Type)
					continue
				}
				tag := strings.ToLower(f.Tag.Get("json"))
				for _, b := range banned {
					if strings.Contains(tag, b) {
						t.Fatalf("%s.%s (%s) looks like a banned field", rt.Name(), f.Name, tag)
					}
				}
			}
		}
		walk(reflect.TypeOf(v))
	}
}
