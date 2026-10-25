package outreach_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/outreach"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

func inline(d outreach.Definition) outreach.SegmentSpec { return outreach.SegmentSpec{Definition: &d} }

func specFor(org uuid.UUID, d outreach.Definition) outreach.SegmentSpec {
	s := inline(d)
	s.OrgID = org
	return s
}

// TestCompile_Below10_TooSmallNoCount: a company targeting that reaches
// fewer than 10 eligible students fails with 422 outreach.audience_too_small
// and carries no number at all — not in the result, not in the error, not
// in its fields — whether it is inline or a saved segment; 10 is enough.
func TestCompile_Below10_TooSmallNoCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(9, true, uniKey(uniA), yearKey(4))
	e.audience.add(30, false, uniKey(uniA), yearKey(4)) // not eligible (minors, unverified): never counted
	def := outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{4}}

	got, err := e.svc.Compile(ctx, cma, specFor(companyA, def))
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)
	if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.audience_too_small" {
		t.Fatalf("problem = %d %s", status, code)
	}
	if got.Size != 0 || got.Keys != nil || got.DimCount != 0 {
		t.Fatalf("a too small targeting returned %+v", got)
	}
	pe, _ := errors.AsType[*problem.Error](err)
	if len(pe.Fields()) != 0 || strings.ContainsAny(pe.Error()+pe.Title(), "0123456789") {
		t.Fatalf("the error leaks a number: %q %q %v", pe.Error(), pe.Title(), pe.Fields())
	}

	// The same through a saved segment (count null, below the threshold).
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Dördüncü sınıflar", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	if seg.Estimate.Count != nil || !seg.Estimate.BelowThreshold {
		t.Fatalf("segment estimate %+v", seg.Estimate)
	}
	_, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)

	// The tenth eligible student makes it: the estimate is 10.
	e.audience.add(1, true, uniKey(uniA), yearKey(4))
	got, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	if err != nil || got.Size != 10 || got.SegmentID == nil || *got.SegmentID != seg.ID {
		t.Fatalf("Compile(10) = %+v, %v", got, err)
	}
	if !slices.Equal(got.Keys, []string{uniKey(uniA), yearKey(4)}) || got.DimCount != 2 {
		t.Fatalf("keys %v dims %d", got.Keys, got.DimCount)
	}
}

// TestCompile_CompanyTargetsSensitiveClub_Rejected: a company (and the
// platform) never targets a sensitive club (03 Y2), alone, OR-ed with
// another club or through a saved segment whose club became sensitive;
// nothing is counted and no budget is spent before the refusal.
func TestCompile_CompanyTargetsSensitiveClub_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(40, true, clubKey(clubA), clubKey(clubSensitive), yearKey(3))
	for _, tc := range []struct {
		name  string
		actor authz.Actor
		org   uuid.UUID
		def   outreach.Definition
	}{
		{"company alone", actor(t, "company_admin"), companyA, outreach.Definition{Club: []uuid.UUID{clubSensitive}}},
		{"recruiter OR another club", actor(t, "company_member"), companyA,
			outreach.Definition{Club: []uuid.UUID{clubA, clubSensitive}, Year: []int16{3}}},
		{"platform_admin", actor(t, "platform_admin"), platformOrg, outreach.Definition{Club: []uuid.UUID{clubSensitive}}},
	} {
		_, err := e.svc.Compile(ctx, tc.actor, specFor(tc.org, tc.def))
		if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.sensitive_target" {
			t.Fatalf("%s: %v, want 422 outreach.sensitive_target", tc.name, err)
		}
	}
	if n := e.audience.callCount(); n != 0 {
		t.Fatalf("a refused targeting was counted %d times", n)
	}

	// A saved segment whose club turns sensitive later no longer compiles.
	cma := actor(t, "company_admin")
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{
		Name: "Kulüp A", Definition: outreach.Definition{Club: []uuid.UUID{clubA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID}); err != nil {
		t.Fatalf("Compile before the change: %v", err)
	}
	e.catalog.markSensitive(clubA)
	_, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	problemtest.WantErr(t, err, outreach.ErrSensitiveTarget)
	// countAudience and createSegment refuse it the same way.
	_, err = e.svc.CountAudience(ctx, cma, companyA, outreach.Definition{Club: []uuid.UUID{clubA}})
	problemtest.WantErr(t, err, outreach.ErrSensitiveTarget)
	_, err = e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Yine", Definition: outreach.Definition{Club: []uuid.UUID{clubA}}})
	problemtest.WantErr(t, err, outreach.ErrSensitiveTarget)
}

// TestCompile_ClubOwnMembers_NoKThreshold: a club_admin addresses its own
// members without k ≥ 10 (02 §12.2), its sensitive club too, but nothing
// beyond club:<own club> (03 k16); the size shown is still never below 10.
func TestCompile_ClubOwnMembers_NoKThreshold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cla := t.Context(), actor(t, "club_admin")
	e.audience.add(3, true, clubKey(clubA), yearKey(2))
	own := outreach.Definition{Club: []uuid.UUID{clubA}}

	got, err := e.svc.Compile(ctx, cla, specFor(clubA, own))
	if err != nil {
		t.Fatalf("Compile(3 members) = %v", err)
	}
	if !slices.Equal(got.Keys, []string{clubKey(clubA)}) || got.DimCount != 1 || got.Size != 0 {
		t.Fatalf("Compile(3 members) = %+v (size must stay unpublished below 10)", got)
	}
	e.audience.add(22, true, clubKey(clubA))
	if got, err = e.svc.Compile(ctx, cla, specFor(clubA, own)); err != nil || got.Size != 20 {
		t.Fatalf("Compile(25 members) = %+v, %v", got, err)
	}

	// The sensitive club's own admin: allowed, never counted.
	e.audience.add(50, true, clubKey(clubSensitive))
	got, err = e.svc.Compile(ctx, actor(t, "club_admin_sensitive"), specFor(clubSensitive, outreach.Definition{Club: []uuid.UUID{clubSensitive}}))
	if err != nil || got.Size != 0 || !slices.Equal(got.Keys, []string{clubKey(clubSensitive)}) {
		t.Fatalf("sensitive club's own members = %+v, %v", got, err)
	}

	// k16: nothing but the own club.
	for name, def := range map[string]outreach.Definition{
		"another club":         {Club: []uuid.UUID{clubB}},
		"own club and another": {Club: []uuid.UUID{clubA, clubB}},
		"narrowed by year":     {Club: []uuid.UUID{clubA}, Year: []int16{2}},
		"a university":         {Uni: []uuid.UUID{uniA}},
	} {
		_, err := e.svc.Compile(ctx, cla, specFor(clubA, def))
		if status, code := problemtest.StatusCode(err); status != 422 || code != "outreach.target_out_of_scope" {
			t.Fatalf("%s: %v, want 422 outreach.target_out_of_scope", name, err)
		}
	}
	// A club_admin acting for another org has no scope there.
	_, err = e.svc.Compile(ctx, cla, specFor(clubB, outreach.Definition{Club: []uuid.UUID{clubB}}))
	problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)

	// The university_rep: uni:<own> (+ dept, year), never a club or another university.
	rep := actor(t, "university_rep")
	e.audience.add(12, true, uniKey(uniA), yearKey(4))
	if got, err := e.svc.Compile(ctx, rep, specFor(uniOrgA, outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{4}})); err != nil || got.Size != 10 {
		t.Fatalf("university_rep own university = %+v, %v", got, err)
	}
	for name, def := range map[string]outreach.Definition{
		"another university": {Uni: []uuid.UUID{uniB}},
		"two universities":   {Uni: []uuid.UUID{uniA, uniB}},
		"no university":      {Year: []int16{4}},
		"a club":             {Uni: []uuid.UUID{uniA}, Club: []uuid.UUID{clubA}},
		"unknown university": {Uni: []uuid.UUID{uuid.NewV7()}},
	} {
		if _, err := e.svc.Compile(ctx, rep, specFor(uniOrgA, def)); !errors.Is(err, outreach.ErrTargetOutOfScope) {
			t.Fatalf("university_rep, %s: %v, want outreach.target_out_of_scope", name, err)
		}
	}
	// A university targeting below 10 needs k.
	_, err = e.svc.Compile(ctx, rep, specFor(uniOrgA, outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{1}}))
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)
}

// TestCompile_OrWithinDimAndAcrossDims: values of one dimension are ORed,
// dimensions are ANDed (02 §9.1); the keys and the dimension count come
// from the definition, normalized (sorted, without duplicates).
func TestCompile_OrWithinDimAndAcrossDims(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cmm := t.Context(), actor(t, "company_member")
	uniC, deptX := uuid.NewV7(), uuid.NewV7()
	e.audience.add(12, true, uniKey(uniA), yearKey(3), deptKey(deptX))
	e.audience.add(15, true, uniKey(uniA), yearKey(4))
	e.audience.add(13, true, uniKey(uniB), yearKey(3), clubKey(clubA), clubKey(clubB))
	e.audience.add(30, true, uniKey(uniA), yearKey(2), clubKey(clubB))
	e.audience.add(40, true, uniKey(uniC), yearKey(3))
	for _, tc := range []struct {
		name string
		def  outreach.Definition
		size int // the bucket of the exact count
		dims int16
	}{
		{"(A or B) and (3 or 4) = 12+15+13", outreach.Definition{Uni: []uuid.UUID{uniB, uniA, uniA}, Year: []int16{4, 3}}, 40, 2},
		{"A alone = 12+15+30", outreach.Definition{Uni: []uuid.UUID{uniA}}, 50, 1},
		{"3 alone = 12+13+40", outreach.Definition{Year: []int16{3}}, 60, 1},
		{"A or B = 70", outreach.Definition{Uni: []uuid.UUID{uniA, uniB}}, 70, 1},
		{"A and 3 = 12", outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}}, 10, 2},
		{"A and 3 and deptX = 12", outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}, Dept: []uuid.UUID{deptX}}, 10, 3},
		{"club A or club B = 13+30", outreach.Definition{Club: []uuid.UUID{clubB, clubA}}, 40, 1},
		{"(club A or B) and A = 30", outreach.Definition{Club: []uuid.UUID{clubA, clubB}, Uni: []uuid.UUID{uniA}}, 30, 2},
	} {
		got, err := e.svc.Compile(ctx, cmm, specFor(companyA, tc.def))
		if err != nil || got.Size != tc.size || got.DimCount != tc.dims {
			t.Fatalf("%s: %+v, %v; want size %d dims %d", tc.name, got, err, tc.size, tc.dims)
		}
		if !slices.IsSorted(got.Keys) || len(slices.Compact(slices.Clone(got.Keys))) != len(got.Keys) {
			t.Fatalf("%s: keys not normalized: %v", tc.name, got.Keys)
		}
		if !slices.Equal(got.Keys, got.Definition.Keys()) || got.Definition.DimCount() != tc.dims {
			t.Fatalf("%s: keys %v do not follow the definition %+v", tc.name, got.Keys, got.Definition)
		}
	}
	// The normalized definition of the first case (posts.audience_snapshot).
	got, err := e.svc.Compile(ctx, cmm, specFor(companyA, outreach.Definition{Uni: []uuid.UUID{uniB, uniA, uniA}, Year: []int16{4, 3}}))
	if err != nil {
		t.Fatal(err)
	}
	wantUni := []uuid.UUID{uniA, uniB}
	slices.SortFunc(wantUni, uuid.UUID.Compare)
	if !slices.Equal(got.Definition.Uni, wantUni) || !slices.Equal(got.Definition.Year, []int16{3, 4}) {
		t.Fatalf("snapshot %+v", got.Definition)
	}
	if want := []string{uniKey(uniA), uniKey(uniB), yearKey(3), yearKey(4)}; !slices.Equal(got.Keys, slices.Sorted(slices.Values(want))) {
		t.Fatalf("keys %v", got.Keys)
	}

	// Malformed targetings.
	for name, spec := range map[string]outreach.SegmentSpec{
		"empty definition": specFor(companyA, outreach.Definition{}),
		"year 9":           specFor(companyA, outreach.Definition{Year: []int16{9}}),
		"nil uuid":         specFor(companyA, outreach.Definition{Uni: []uuid.UUID{uuid.Nil()}}),
		"neither":          {OrgID: companyA},
		"both":             {OrgID: companyA, SegmentID: ptr(uuid.NewV7()), Definition: &outreach.Definition{Year: []int16{3}}},
	} {
		_, err := e.svc.Compile(ctx, cmm, spec)
		if status, code := problemtest.StatusCode(err); status != 422 || code != "validation_failed" {
			t.Fatalf("%s: %v, want 422 validation_failed", name, err)
		}
	}
	// Too many values in one dimension.
	many := make([]int16, 0)
	for range 101 {
		many = append(many, 3)
	}
	if _, err := e.svc.Compile(ctx, cmm, specFor(companyA, outreach.Definition{Year: many})); err == nil {
		t.Fatal("101 years accepted")
	}
}

// TestCompile_SavedSegment_ScopedArchivedAndBudget: a saved segment is
// found only in its org (another org's is 404), an archived one is 409,
// and every compile spends the org's daily count budget.
func TestCompile_SavedSegment_ScopedArchivedAndBudget(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(4))
	ctx, cma := t.Context(), actor(t, "company_admin")
	e.audience.add(20, true, uniKey(uniA))
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Seg A", Definition: outreach.Definition{Uni: []uuid.UUID{uniA}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Compile(ctx, actor(t, "other_company_member"), outreach.SegmentSpec{OrgID: companyB, SegmentID: &seg.ID})
	problemtest.WantErr(t, err, outreach.ErrNotFound)
	if _, err := e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ArchiveSegment(ctx, cma, companyA, seg.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	problemtest.WantErr(t, err, outreach.ErrInvalidState)
	// create (1) + compile (2) spent; two more, then 429.
	for range 2 {
		if _, err := e.svc.Compile(ctx, cma, specFor(companyA, outreach.Definition{Uni: []uuid.UUID{uniA}})); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.svc.Compile(ctx, cma, specFor(companyA, outreach.Definition{Uni: []uuid.UUID{uniA}}))
	if status, code := problemtest.StatusCode(err); status != 429 || code != "rate_limited" {
		t.Fatalf("budget spent: %v, want 429 rate_limited", err)
	}
	// An actor without a targeting role in the org has no scope.
	_, err = e.svc.Compile(ctx, actor(t, "student"), specFor(companyB, outreach.Definition{Uni: []uuid.UUID{uniA}}))
	problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)
}

// TestBucket pins the rounding of published estimates.
func TestBucket(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]int{
		9: 0, 10: 10, 19: 10, 20: 20, 99: 90, 100: 100, 149: 100, 150: 150, 999: 950, 1000: 1000, 1099: 1000,
		9999: 9900, 10000: 10000, 10999: 10000, 123456: 123000,
	} {
		if got := outreach.Bucket(n); got != want {
			t.Fatalf("Bucket(%d) = %d, want %d", n, got, want)
		}
	}
	if e := outreach.PublicEstimate(9); e.Count != nil || !e.BelowThreshold {
		t.Fatalf("PublicEstimate(9) = %+v", e)
	}
	if e := outreach.PublicEstimate(57); e.Count == nil || *e.Count != 50 || e.BelowThreshold {
		t.Fatalf("PublicEstimate(57) = %+v", e)
	}
	// Two definitions that differ by a small group mostly show the same number.
	same := 0
	for base := 100; base < 1000; base++ {
		if outreach.Bucket(base) == outreach.Bucket(base+3) {
			same++
		}
	}
	if same < 800 {
		t.Fatalf("a 3-student difference is visible in %d of 900 cases", 900-same)
	}
}

// TestScopeOf pins the targeting scope of each role (03 k16).
func TestScopeOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		persona string
		org     uuid.UUID
		want    outreach.Scope
	}{
		{"club_admin", clubA, outreach.ScopeClub},
		{"club_member", clubA, outreach.ScopeNone},
		{"university_rep", uniOrgA, outreach.ScopeUniversity},
		{"company_member", companyA, outreach.ScopeCompany},
		{"company_admin", companyA, outreach.ScopeCompany},
		{"company_owner_a", companyA, outreach.ScopeCompany},
		{"company_admin", companyB, outreach.ScopeNone},
		{"platform_admin", companyA, outreach.ScopePlatform},
		{"moderator", companyA, outreach.ScopeNone},
		{"anonymous", companyA, outreach.ScopeNone},
	} {
		if got := outreach.ScopeOf(actor(t, tc.persona), tc.org); got != tc.want {
			t.Fatalf("ScopeOf(%s, %s) = %q, want %q", tc.persona, tc.org, got, tc.want)
		}
	}
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: uniOrgA, OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	if got := outreach.ScopeOf(cs, uniOrgA); got != outreach.ScopeUniversity {
		t.Fatalf("career_staff scope %q", got)
	}
	minor := actor(t, "club_admin")
	minor.IsMinor = true // 03 Y4: a minor is never a club_admin
	if got := outreach.ScopeOf(minor, clubA); got != outreach.ScopeNone {
		t.Fatalf("minor club_admin scope %q", got)
	}
}
