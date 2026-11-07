package feed_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/orgs"
)

// TestFollowedOrgIDs_OwnFollowsNewestFirst (BE-174): the recommendations
// read one user's own follows as org IDs, newest first, bounded by the
// limit; another user's follows are not theirs.
func TestFollowedOrgIDs_OwnFollowsNewestFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader, other := persona(t, "student"), persona(t, "student_minor")
	for _, org := range []string{"club_a", "club_b"} {
		if err := e.svc.FollowOrg(ctx, reader, orgID(org), nil); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(1)
	}
	if err := e.svc.FollowOrg(ctx, other, orgID("uni_a"), nil); err != nil {
		t.Fatal(err)
	}
	f, ok := e.svc.(feed.Follows)
	if !ok {
		t.Fatal("the feed service does not implement Follows")
	}
	got, err := f.FollowedOrgIDs(ctx, reader.UserID, 10)
	if err != nil || !slices.Equal(got, []uuid.UUID{orgID("club_b"), orgID("club_a")}) {
		t.Fatalf("FollowedOrgIDs = %v, %v; want club_b then club_a", got, err)
	}
	if got, err := f.FollowedOrgIDs(ctx, reader.UserID, 1); err != nil || !slices.Equal(got, []uuid.UUID{orgID("club_b")}) {
		t.Fatalf("limit 1 = %v, %v", got, err)
	}
	if got, err := f.FollowedOrgIDs(ctx, reader.UserID, 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = %v, %v", got, err)
	}
}

// putCall seeds a published opportunity call of org (a company's is
// commercial) with opp as its opportunity; edit adjusts the row (counts,
// audience, status) before it is stored.
func (e *env) putCall(t *testing.T, org string, kind feed.AuthorKind, opp string, edit func(*feed.Post)) feed.Post {
	t.Helper()
	at := e.clk.Now()
	e.clk.Advance(time.Second)
	title := "Fırsat"
	p := feed.Post{
		ID: uuid.NewV7(), OrgID: orgID(org), AuthorOrgKind: kind, AuthorUserID: uuid.NewV7(), Kind: feed.KindOpportunityCall,
		Title: &title, BodyJSON: doc("Başvurular açık"), Opportunity: feed.JSON(opp), AudienceKind: feed.AudiencePublic,
		AudienceKeys: []string{feed.KeyAll}, AudienceDimCount: 1, CommercialFlag: kind == feed.AuthorCompany,
		Status: feed.StatusPublished, PublishedAt: &at,
	}
	if edit != nil {
		edit(&p)
	}
	if err := e.mem.PutPost(p); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	clubCall    = `{"type":"volunteer","apply_via":"external_url","external_url":"https://a.example.test","deadline":"2026-10-04"}`
	yearCall    = `{"type":"internship","apply_via":"external_url","external_url":"https://b.example.test","eligible_years":[3]}`
	companyCall = `{"type":"internship","apply_via":"contact_request"}`
	overCall    = `{"type":"job","apply_via":"external_url","external_url":"https://c.example.test","deadline":"2026-10-02"}`
)

// segmentOf makes p a segment post addressed to keys (one dimension); a
// company's carries its estimate and quota unit.
func segmentOf(keys ...string) func(*feed.Post) {
	return func(p *feed.Post) {
		p.AudienceKind, p.AudienceKeys, p.AudienceDimCount, p.AudienceSnapshot = feed.AudienceSegment, keys, 1, feed.JSON(`{}`)
		if p.AuthorOrgKind == feed.AuthorCompany {
			n, unit := 40, uuid.NewV7()
			p.AudienceSizeEstimate, p.EntitlementUsageID = &n, &unit
		}
	}
}

func candidateIDs(cs []feed.OpportunityCandidate) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.PostID
	}
	return out
}

// TestRecommendableOpportunities_SignalsYearAndPopular (BE-191): the
// candidates are the open calls the student sees of the orgs asked, open
// to the student's class year (from the profile keys) or popular (10 likes
// or 10 saves), newest first; each carries its decoded opportunity and the
// ForYourYear / Popular flags, never a count. A minor gets no company call.
func TestRecommendableOpportunities_SignalsYearAndPopular(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	student := persona(t, "student")
	e.viewers.keys[student.UserID] = []string{"uni:" + uniA.String(), "year:3"}
	since := e.clk.Now()
	asked := e.putCall(t, "club_b", feed.AuthorClub, clubCall, nil)
	forYear := e.putCall(t, "club_unclaimed", feed.AuthorClub, yearCall, nil)
	likes := e.putCall(t, "company_a", feed.AuthorCompany, companyCall, func(p *feed.Post) { p.LikeCount = feed.PopularThreshold })
	saves := e.putCall(t, "company_b", feed.AuthorCompany, companyCall, func(p *feed.Post) { p.SaveCount = feed.PopularThreshold })
	e.putCall(t, "company_b", feed.AuthorCompany, companyCall, func(p *feed.Post) { // 9 + 9 is not popular
		p.LikeCount, p.SaveCount = feed.PopularThreshold-1, feed.PopularThreshold-1
	})
	e.putCall(t, "club_a", feed.AuthorClub, overCall, nil)                 // the deadline is over (today is 2026-10-03)
	e.putCall(t, "club_a", feed.AuthorClub, clubCall, func(p *feed.Post) { // published before since
		old := since.Add(-time.Hour)
		p.PublishedAt = &old
	})
	o, ok := e.svc.(feed.Opportunities)
	if !ok {
		t.Fatal("the feed service does not implement Opportunities")
	}
	q := feed.OpportunityQuery{OrgIDs: []uuid.UUID{orgID("club_b"), orgID("club_a")}, Since: since, Limit: 10}
	got, err := o.RecommendableOpportunities(ctx, student, q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(candidateIDs(got), []uuid.UUID{saves.ID, likes.ID, forYear.ID, asked.ID}) {
		t.Fatalf("candidates = %v", candidateIDs(got))
	}
	flags := map[uuid.UUID][2]bool{}
	for _, c := range got {
		flags[c.PostID] = [2]bool{c.ForYourYear, c.Popular}
		if c.OrgID == uuid.Nil() || c.PublishedAt.IsZero() || c.Opportunity.Type == "" {
			t.Fatalf("candidate without its data: %+v", c)
		}
	}
	if flags[forYear.ID] != [2]bool{true, false} || flags[likes.ID] != [2]bool{false, true} ||
		flags[saves.ID] != [2]bool{false, true} || flags[asked.ID] != [2]bool{false, false} {
		t.Fatalf("flags = %v", flags)
	}
	minor := persona(t, "student_minor")
	e.viewers.keys[minor.UserID] = []string{"uni:" + uniA.String(), "year:3"}
	got, err = o.RecommendableOpportunities(ctx, minor, q)
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{forYear.ID, asked.ID}) {
		t.Fatalf("minor = %v, %v (no commercial call)", candidateIDs(got), err)
	}
	// Without a class year in the profile, only the orgs and popularity.
	got, err = o.RecommendableOpportunities(ctx, persona(t, "student_b"), q)
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{saves.ID, likes.ID, asked.ID}) {
		t.Fatalf("no class year = %v, %v", candidateIDs(got), err)
	}
	if got, err := o.RecommendableOpportunities(ctx, student, feed.OpportunityQuery{Since: since}); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = %v, %v", got, err)
	}
	if _, err := o.RecommendableOpportunities(ctx, authz.Anonymous(), q); err == nil {
		t.Fatal("an anonymous caller got candidates")
	}
}

// TestRecommendableOpportunities_OnlyVisible (BE-191, 03 §3.6): a call is a
// candidate only when the feed would show it to the student: not a
// followers call of an org the student neither follows nor belongs to,
// not a segment call outside the student's keys, not of a muted org, not
// hidden, liked or saved, not of an org that is not active.
func TestRecommendableOpportunities_OnlyVisible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	student := persona(t, "student")
	since := e.clk.Now()
	followers := e.putCall(t, "club_b", feed.AuthorClub, clubCall, func(p *feed.Post) { p.AudienceKind = feed.AudienceFollowers })
	segment := e.putCall(t, "club_b", feed.AuthorClub, clubCall, segmentOf("year:4"))
	muted := e.putCall(t, "club_unclaimed", feed.AuthorClub, clubCall, nil)
	hidden := e.putCall(t, "club_a", feed.AuthorClub, clubCall, nil)
	liked := e.putCall(t, "club_a", feed.AuthorClub, clubCall, nil)
	saved := e.putCall(t, "club_a", feed.AuthorClub, clubCall, nil)
	suspended := e.putCall(t, "company_b", feed.AuthorCompany, companyCall, nil)
	shown := e.putCall(t, "company_a", feed.AuthorCompany, companyCall, nil)
	e.mem.PutMute(student.UserID, orgID("club_unclaimed"))
	e.mem.PutHide(student.UserID, hidden.ID)
	e.mem.PutLike(student.UserID, liked.ID)
	e.mem.PutSave(student.UserID, saved.ID)
	e.orgs.setStatus(orgID("company_b"), orgs.StatusSuspended)
	o := e.svc.(feed.Opportunities)
	all := []uuid.UUID{orgID("club_a"), orgID("club_b"), orgID("club_unclaimed"), orgID("company_a"), orgID("company_b")}
	got, err := o.RecommendableOpportunities(ctx, student, feed.OpportunityQuery{OrgIDs: all, Since: since, Limit: 10})
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{shown.ID}) {
		t.Fatalf("candidates = %v, %v; want only %s (followers %s, segment %s, muted %s, hidden %s, liked %s, saved %s, suspended %s)",
			candidateIDs(got), err, shown.ID, followers.ID, segment.ID, muted.ID, hidden.ID, liked.ID, saved.ID, suspended.ID)
	}
	e.mem.PutFollow(student.UserID, orgID("club_b"))
	e.viewers.keys[student.UserID] = []string{"year:4"}
	got, err = o.RecommendableOpportunities(ctx, student, feed.OpportunityQuery{OrgIDs: all, Since: since, Limit: 10})
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{shown.ID, segment.ID, followers.ID}) {
		t.Fatalf("after the follow and the keys = %v, %v", candidateIDs(got), err)
	}
}

// failingClubs is directory's catalog when the sensitivity cannot be read.
type failingClubs struct{ feed.Clubs }

func (failingClubs) SensitiveOrgIDs(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error) {
	return nil, errors.New("directory unavailable")
}

// TestRecommendableOpportunities_NeverSensitive (BE-191, 03 Y2): a call of
// a sensitive club, or addressed to a sensitive club's members (the
// student holds the key), is never a candidate, even of an org asked for;
// without the catalog every club counts as sensitive and an error reading
// the sensitivity is returned (fail closed).
func TestRecommendableOpportunities_NeverSensitive(t *testing.T) {
	t.Parallel()
	sens := orgID("club_sensitive")
	seed := func(e *env) (own, toMembers, plain, company feed.Post) {
		own = e.putCall(t, "club_sensitive", feed.AuthorClub, clubCall, nil)
		toMembers = e.putCall(t, "company_a", feed.AuthorCompany, companyCall, segmentOf("club:"+sens.String()))
		plain = e.putCall(t, "club_a", feed.AuthorClub, clubCall, nil)
		company = e.putCall(t, "company_b", feed.AuthorCompany, companyCall, nil)
		return own, toMembers, plain, company
	}
	member := persona(t, "club_member") // member of club_a and club_sensitive
	q := func(since time.Time) feed.OpportunityQuery {
		return feed.OpportunityQuery{OrgIDs: []uuid.UUID{sens, orgID("club_a"), orgID("company_a"), orgID("company_b")}, Since: since, Limit: 10}
	}

	e := newEnv(t)
	e.clubs.set(sens, true)
	e.viewers.keys[member.UserID] = []string{"club:" + sens.String(), "club:" + orgID("club_a").String()}
	since := e.clk.Now()
	_, _, plain, company := seed(e)
	got, err := e.svc.(feed.Opportunities).RecommendableOpportunities(t.Context(), member, q(since))
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{company.ID, plain.ID}) {
		t.Fatalf("candidates = %v, %v; the sensitive club's call and the call to its members are never ones", candidateIDs(got), err)
	}

	nocat := newEnvWith(t, func(d *feed.Deps) { d.Clubs = nil })
	nocat.viewers.keys[member.UserID] = []string{"club:" + sens.String()}
	since = nocat.clk.Now()
	_, _, _, company = seed(nocat)
	got, err = nocat.svc.(feed.Opportunities).RecommendableOpportunities(t.Context(), member, q(since))
	if err != nil || !slices.Equal(candidateIDs(got), []uuid.UUID{company.ID}) {
		t.Fatalf("without the catalog = %v, %v; want the company call only (every club sensitive)", candidateIDs(got), err)
	}

	broken := newEnvWith(t, func(d *feed.Deps) { d.Clubs = failingClubs{d.Clubs} })
	since = broken.clk.Now()
	seed(broken)
	if got, err := broken.svc.(feed.Opportunities).RecommendableOpportunities(t.Context(), member, q(since)); err == nil {
		t.Fatalf("without the sensitivity = %v, want an error", candidateIDs(got))
	}
}

// TestOpportunityCards_ReaderViewsOfTheCandidates (BE-191): the cards are
// getFeed's reader views of the candidates, in their order, with the
// reader's state; a candidate the read did not return is left out.
func TestOpportunityCards_ReaderViewsOfTheCandidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	student := persona(t, "student")
	since := e.clk.Now()
	first := e.putCall(t, "club_a", feed.AuthorClub, yearCall, nil)
	second := e.putCall(t, "company_a", feed.AuthorCompany, companyCall, nil)
	o := e.svc.(feed.Opportunities)
	got, err := o.RecommendableOpportunities(ctx, student, feed.OpportunityQuery{
		OrgIDs: []uuid.UUID{orgID("club_a"), orgID("company_a")}, Since: since, Limit: 10})
	if err != nil || len(got) != 2 {
		t.Fatalf("candidates = %v, %v", candidateIDs(got), err)
	}
	rows := []feed.OpportunityCandidate{got[1], {PostID: uuid.NewV7(), OrgID: orgID("club_a")}, got[0]}
	views, err := o.OpportunityCards(ctx, student, rows)
	if err != nil || len(views) != 2 || views[0].Post.ID != first.ID || views[1].Post.ID != second.ID {
		t.Fatalf("cards = %+v, %v", views, err)
	}
	for _, v := range views {
		d := feed.PostDTOOf(v)
		if v.Viewer == nil || d.Kind != "opportunity_call" || d.Opportunity == nil || d.Author.OrgID == "" {
			t.Fatalf("card = %+v", d)
		}
	}
	if !feed.PostDTOOf(views[1]).CommercialLabel {
		t.Fatal("the company call lost its commercial label")
	}
	if views, err := o.OpportunityCards(ctx, student, nil); err != nil || len(views) != 0 {
		t.Fatalf("no rows = %v, %v", views, err)
	}
}
