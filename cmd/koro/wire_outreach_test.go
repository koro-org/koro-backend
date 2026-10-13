package main

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/students"
)

// useMemStore installs the in-memory outreach stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-030.
// Only tickets of the outreach module edit this file (BE-001 R4).
// updated_at uses the real clock (matrix fixtures put their own rows).
func (w *outreachWiring) useMemStore() {
	w.store = outreach.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_OutreachProvidesServiceAndJobs pins what outreach binds in build
// and registers in tasks (01 §7, §6.2): its service (Targeting for the
// feed), outreach.recount_segments at 06:00 on the low queue,
// outreach.resume_dispatches every 10 minutes, the dispatch consumers
// (outreach.dispatch_scheduled → outreach.run_dispatch, post.published →
// outreach.start_dispatch) and the outreach_dispatch retention sweeper
// (BE-035). The late adapters fail without notify and the feed.
func TestWire_OutreachProvidesServiceAndJobs(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.outreach.svc == nil {
		t.Fatal("outreach service not built")
	}
	var _ outreach.Targeting = c.outreach.svc
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "outreach.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	want := []string{"outreach.recount_segments 0 6 * * * low", "outreach.resume_dispatches */10 * * * * default"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("outreach jobs %q, want %q", jobs, want)
	}
	for topic, task := range map[string]string{
		domainevent.TopicOutreachDispatchScheduled: outreach.RunDispatchTaskName,
		domainevent.TopicPostPublished:             outreach.StartDispatchTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == task }) {
			t.Errorf("%s has no %s subscriber", topic, task)
		}
	}
	if !slices.ContainsFunc(c.sweepers, func(s privacy.RetentionSweeper) bool {
		return slices.Contains(s.PolicyCodes(), outreach.PolicyOutreachDispatch)
	}) {
		t.Fatal("the outreach_dispatch sweeper is not registered")
	}
	empty := &container{}
	if _, err := (outreachSender{c: empty}).Send(t.Context(), notify.Message{}); err == nil {
		t.Fatal("a sender without notify succeeded")
	}
	if _, err := (outreachPosts{c: empty}).DispatchPost(t.Context(), uuid.NewV7()); err == nil {
		t.Fatal("a post lookup without the feed succeeded")
	}
}

// seedStudent adds a verified student of university uni (class year) to
// identity and students, in memory; birth decides adult or minor.
func seedStudent(t *testing.T, c *container, uni uuid.UUID, birth civil.Date, year int16) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	email := "ogrenci-" + id.String() + "@matrix.koro.test"
	bd := birth
	if err := c.identity.store.InsertUser(ctx, identity.User{
		ID: id, Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		FirstName: "Öğrenci", LastName: "Test", BirthDate: &bd, Persona: identity.PersonaStudent,
		Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	exp, addr := now.AddDate(1, 0, 0), "o-"+id.String()+"@matrix.edu.tr"
	if err := c.identity.store.InsertEduVerification(ctx, identity.EduRecord{
		ID: uuid.NewV7(), UserID: id, Method: identity.EduMethodEmail, EduEmail: addr, EduEmailNormalized: addr,
		EmailDomain: "matrix.edu.tr", UniversityID: &uni, Status: identity.EduVerified, VerifiedAt: &now,
		ExpiresAt: &exp, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	y := year
	if _, _, err := c.students.store.InsertProfile(ctx, students.Profile{
		UserID: id, UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive, ClassYear: &y,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDispatch_EndToEnd_ClubNoticeAndCompanyOffer runs the real modules in
// memory (identity → orgs → students → feed → outreach → notify, consent):
// a club_admin's targeted post with "notify on publish" reaches the club's
// adult members in-app once published (the minor member never), exactly
// once even when the event comes twice; a company's offer reaches nobody
// without a commercial consent, keeps only published counts and spent one
// targeted_dispatch unit.
func TestDispatch_EndToEnd_ClubNoticeAndCompanyOffer(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := obs.WithTaskID(t.Context(), "task-dispatch") // the worker's context (notify refuses a request's)
	now := c.infra.clock.Now().UTC()
	uni := authztest.UniversityID("uni_a")
	adult, _ := civil.New(2000, time.January, 1)
	minor, _ := civil.New(now.Year()-16, time.January, 1)
	var adults, minors []uuid.UUID
	for range 12 {
		adults = append(adults, seedStudent(t, c, uni, adult, 3))
	}
	for range 2 {
		minors = append(minors, seedStudent(t, c, uni, minor, 3))
	}
	runJob(t, c, students.AudienceRefreshJobName)

	club, company := authztest.ID("club_a"), authztest.ID("company_a")
	for id, kind := range map[uuid.UUID]orgs.Kind{club: orgs.KindClub, company: orgs.KindCompany} {
		if _, ok, err := c.orgs.store.InsertOrg(ctx, orgs.Org{ID: id, Kind: kind, Name: "Uçtan uca " + string(kind),
			Slug: "uctan-uca-" + string(kind), Status: orgs.StatusActive, VerifiedAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil || !ok {
			t.Fatalf("org %s: %v %v", kind, ok, err)
		}
	}
	members := []uuid.UUID{adults[0], adults[1], adults[2], minors[0]}
	for _, u := range members {
		if _, err := c.orgs.store.InsertMember(ctx, orgs.Membership{ID: uuid.NewV7(), OrgID: club, UserID: u,
			Role: authz.MemberMember, Status: orgs.MemberActive, Source: orgs.SourceJoinRequest, JoinedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	fs, ok := c.feed.built.(*feed.MemStore)
	if !ok {
		t.Fatalf("feed store %T", c.feed.built)
	}
	fs.PutSettings(feed.OrgSettings{OrgID: club, TrustLevel: feed.TrustTrusted})
	fs.PutSettings(feed.OrgSettings{OrgID: company, TrustLevel: feed.TrustTrusted})
	worker := func(d uuid.UUID, org, post uuid.UUID) {
		t.Helper()
		env, err := domainevent.New(domainevent.OutreachDispatchScheduled{DispatchID: d, OrgID: org, PostID: post},
			"outreach_dispatch", d)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range c.infra.subs.For(env.Topic) {
			if err := s.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
	}
	inApp := func(u uuid.UUID) int {
		t.Helper()
		rows, err := c.notify.store.ListInApp(ctx, u, false, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	// The club's notice to its own members.
	cla, _ := authztest.Get("club_admin")
	v, err := c.feed.svc.CreateTargetedPost(ctx, cla.Actor, club, feed.TargetedInput{
		Post:       feed.PostInput{Kind: feed.KindText, Title: "Genel kurul", Body: []byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Yarın"}]}]}`)},
		Definition: &outreach.Definition{Club: []uuid.UUID{club}},
		Dispatch:   &outreach.DispatchRequest{Channels: []outreach.Channel{outreach.ChannelInApp}},
	})
	if err != nil || v.DispatchID == nil {
		t.Fatalf("club targeted post = %v, %v", v.DispatchID, err)
	}
	worker(*v.DispatchID, club, v.Post.ID) // a draft: it waits
	if inApp(adults[0]) != 0 {
		t.Fatal("a draft's dispatch notified")
	}
	if _, err := c.feed.svc.PublishPost(ctx, cla.Actor, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	worker(*v.DispatchID, club, v.Post.ID)
	worker(*v.DispatchID, club, v.Post.ID) // the event again: nothing more
	for _, u := range members[:3] {
		if n := inApp(u); n != 1 {
			t.Fatalf("adult member got %d notices, want 1", n)
		}
	}
	if inApp(minors[0]) != 0 || inApp(adults[5]) != 0 {
		t.Fatal("a minor member or a non-member was notified")
	}
	d, err := c.outreach.svc.GetDispatch(ctx, cla.Actor, club, *v.DispatchID)
	if err != nil || d.Status != outreach.DispatchCompleted || d.Counts.Eligible != nil {
		t.Fatalf("club dispatch = %s %v, %v (3 < 10: no number)", d.Status, d.Counts.Eligible, err)
	}

	// The company's offer: nobody consented to commercial push.
	by := authztest.ID("platform_admin")
	for _, f := range []entitlements.Feature{entitlements.FeatureTargetedPost, entitlements.FeatureTargetedDispatch} {
		n := 2
		if _, err := c.entitlements.svc.Grant(ctx, entitlements.GrantInput{OrgID: company, Feature: f, Quantity: &n,
			ValidFrom: now.Add(-time.Hour), ValidTo: now.Add(24 * time.Hour), Status: entitlements.StatusActive,
			SourceType: entitlements.SourceAdminGrant, GrantedBy: &by}); err != nil {
			t.Fatal(err)
		}
	}
	cma, _ := authztest.Get("company_admin")
	cv, err := c.feed.svc.CreateTargetedPost(ctx, cma.Actor, company, feed.TargetedInput{
		Post:       feed.PostInput{Kind: feed.KindText, Title: "Yaz stajı", Body: []byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Başvur"}]}]}`)},
		Definition: &outreach.Definition{Uni: []uuid.UUID{uni}},
		Dispatch:   &outreach.DispatchRequest{Channels: []outreach.Channel{outreach.ChannelPush}},
	})
	if err != nil || cv.DispatchID == nil {
		t.Fatalf("company targeted post = %v, %v", cv.DispatchID, err)
	}
	if _, err := c.feed.svc.PublishPost(ctx, cma.Actor, cv.Post.ID); err != nil {
		t.Fatal(err)
	}
	worker(*cv.DispatchID, company, cv.Post.ID)
	cd, err := c.outreach.svc.GetDispatch(ctx, cma.Actor, company, *cv.DispatchID)
	switch {
	case err != nil:
		t.Fatal(err)
	case cd.Status != outreach.DispatchCompleted || !cd.Commercial:
		t.Fatalf("company dispatch %s commercial %v", cd.Status, cd.Commercial)
	case cd.Counts.Eligible == nil || *cd.Counts.Eligible != 10 || cd.Counts.Consented != nil || cd.Counts.Sent != nil:
		t.Fatalf("counts = %+v (12 adults → 10; nobody consenting → null)", cd.Counts)
	}
	for _, u := range append(slices.Clone(adults), minors...) {
		rows, err := c.notify.store.ListOfUser(ctx, u, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Template == "outreach.commercial" {
				t.Fatal("a commercial notification went to a student without consent")
			}
		}
	}
	av, err := c.entitlements.svc.Available(ctx, company, entitlements.FeatureTargetedDispatch)
	if err != nil || av.Remaining != 1 {
		t.Fatalf("targeted_dispatch left = %d, %v", av.Remaining, err)
	}
}

// TestCompile_ExcludesMinors runs the real modules end to end (identity →
// students → outreach, in memory): verified students under 18 never enter
// an audience. 25 adults and 12 minors of one university and class year
// compile to the estimate of the 25 (20, not 30); 6 adults and 12 minors
// are too small to target (18 would pass if minors were counted).
func TestCompile_ExcludesMinors(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	uni := authztest.UniversityID("uni_a")
	adult, err := civil.New(2000, time.January, 1)
	if err != nil {
		t.Fatal(err)
	}
	minor, err := civil.New(now.Year()-16, time.January, 1)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	student := func(birth civil.Date, year int16) {
		t.Helper()
		n++
		id, email := uuid.NewV7(), "ogrenci"+strconv.Itoa(n)+"@matrix.koro.test"
		bd := birth
		if err := c.identity.store.InsertUser(ctx, identity.User{
			ID: id, Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
			FirstName: "Öğrenci", LastName: "Test", BirthDate: &bd, Persona: identity.PersonaStudent,
			Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		exp, addr := now.AddDate(1, 0, 0), "o"+strconv.Itoa(n)+"@matrix.edu.tr"
		if err := c.identity.store.InsertEduVerification(ctx, identity.EduRecord{
			ID: uuid.NewV7(), UserID: id, Method: identity.EduMethodEmail, EduEmail: addr, EduEmailNormalized: addr,
			EmailDomain: "matrix.edu.tr", UniversityID: &uni, Status: identity.EduVerified, VerifiedAt: &now,
			ExpiresAt: &exp, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		y := year
		if _, _, err := c.students.store.InsertProfile(ctx, students.Profile{
			UserID: id, UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive, ClassYear: &y,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 25 {
		student(adult, 3)
	}
	for range 12 {
		student(minor, 3)
	}
	for range 6 {
		student(adult, 2)
	}
	for range 12 {
		student(minor, 2)
	}
	// students.audience_refresh derives audience_eligible from identity
	// (active verification, adult, active account).
	refreshed := false
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == students.AudienceRefreshJobName {
			if err := j.Handle(ctx); err != nil {
				t.Fatal(err)
			}
			refreshed = true
		}
	}
	if !refreshed {
		t.Fatal("students.audience_refresh is not registered")
	}

	company, _ := authztest.Get("company_admin")
	org := authztest.ID("company_a")
	got, err := c.outreach.svc.Compile(ctx, company.Actor, outreach.SegmentSpec{OrgID: org, Definition: &outreach.Definition{
		Uni: []uuid.UUID{uni}, Year: []int16{3},
	}})
	if err != nil || got.Size != 20 {
		t.Fatalf("25 adults + 12 minors = %+v, %v; want the estimate of the 25 adults (20)", got, err)
	}
	_, err = c.outreach.svc.Compile(ctx, company.Actor, outreach.SegmentSpec{OrgID: org, Definition: &outreach.Definition{
		Uni: []uuid.UUID{uni}, Year: []int16{2},
	}})
	if !errors.Is(err, outreach.ErrAudienceTooSmall) {
		t.Fatalf("6 adults + 12 minors = %v, want outreach.audience_too_small", err)
	}
}
