package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/kvkk"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/students"
)

// useMemStore installs the in-memory students stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-023.
// Only tickets of the students module edit this file (BE-001 R4).
func (w *studentsWiring) useMemStore() {
	w.store = students.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_StudentsProvidesProviderHooksAndTasks pins what students binds
// in build and registers in tasks (01 §6.2, §7): its service, its KVKK
// provider, its retention sweeper (BE-046), the two late-bound reads of orgs, eight subscriptions (file.rejected
// since BE-025, next to files' own; the two grant suspensions of BE-077) and three nightly jobs (the expiry of
// suspended grants since BE-088).
func TestWire_StudentsProvidesProviderHooksAndTasks(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.students.svc == nil {
		t.Fatal("students service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "students" }) {
		t.Fatal("students privacy provider not registered")
	}
	if c.orgs.graduatedUsers == nil || c.orgs.avatarURLs == nil {
		t.Fatal("orgs' late-bound students reads are not set")
	}
	if !slices.ContainsFunc(c.sweepers, func(s privacy.RetentionSweeper) bool {
		return slices.Equal(s.PolicyCodes(), []string{students.PolicyAccessGrantRevoked})
	}) {
		t.Fatal("the access_grant_revoked sweeper is not registered (BE-046)")
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "students.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"students.audience_refresh 0 2 * * * low", "students.graduation_check 15 2 * * * default",
		"students.expire_suspended_grants 30 2 * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("students jobs %q, want %q", jobs, want)
	}
	for _, sub := range [][2]string{
		{domainevent.TopicMembershipStarted, students.AddClubKeyTaskName},
		{domainevent.TopicMembershipEnded, students.RemoveClubKeyTaskName},
		{domainevent.TopicUserEduVerified, students.ApplyEduVerifiedTaskName},
		{domainevent.TopicUserEduVerificationLapsed, students.ApplyEduLapsedTaskName},
		{domainevent.TopicClubSensitivityChanged, students.DropSensitiveClubKeyTaskName},
		{domainevent.TopicFileRejected, students.DropRejectedFileTaskName},
		{domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName},
		{domainevent.TopicUserEduVerified, students.SuspendGapGrantsTaskName},
	} {
		topic, name := sub[0], sub[1]
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s is not subscribed to %s", name, topic)
		}
	}
}

// TestWire_StudentsGraduatedReachesOrgs: the graduated profile a students
// store holds is what orgs' admin sweep reads through the late-bound hook
// (T-210); an unknown user has no avatar.
func TestWire_StudentsGraduatedReachesOrgs(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC()
	grad, active := authztest.ID("student"), authztest.ID("student_b")
	for _, p := range []students.Profile{
		{UserID: grad, UniversityID: authztest.UniversityID("uni_a"), DegreeLevel: students.DegreeBachelor,
			Status: students.ProfileGraduated, GraduatedAt: &now},
		{UserID: active, UniversityID: authztest.UniversityID("uni_a"), DegreeLevel: students.DegreeBachelor,
			Status: students.ProfileActive},
	} {
		if _, ok, err := c.students.store.InsertProfile(ctx, p); err != nil || !ok {
			t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
		}
	}
	got, err := c.orgs.graduatedUsers(ctx, []uuid.UUID{grad, active, uuid.NewV7()})
	if err != nil || len(got) != 1 || !got[grad] {
		t.Fatalf("graduatedUsers = %v, %v", got, err)
	}
	if urls, err := c.orgs.avatarURLs(ctx, []uuid.UUID{grad}); err != nil || len(urls) != 0 {
		t.Fatalf("avatarURLs = %v, %v", urls, err)
	}
}

// TestWire_StudentsSensitivityChangedThroughTheSubscription delivers
// club.sensitivity_changed through the registered handler (as the worker
// does) and the club's key leaves the stored audience (T-80).
func TestWire_StudentsSensitivityChangedThroughTheSubscription(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	club := authztest.ID("club_a")
	user := authztest.ID("student")
	if _, ok, err := c.students.store.InsertProfile(ctx, students.Profile{
		UserID: user, UniversityID: authztest.UniversityID("uni_a"), DegreeLevel: students.DegreeBachelor,
		Status: students.ProfileActive, AudienceKeys: []string{"all", students.ClubKey(club)}, AudienceEligible: true,
	}); err != nil || !ok {
		t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
	}
	env, err := domainevent.New(domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: true}, "club", club)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), c.infra.clock.Now()
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	subs := c.infra.subs.For(domainevent.TopicClubSensitivityChanged)
	i := slices.IndexFunc(subs, func(s outbox.Subscription) bool { return s.Name == students.DropSensitiveClubKeyTaskName })
	if i < 0 {
		t.Fatal("no students subscription")
	}
	if err := subs[i].HandleTask(ctx, payload); err != nil {
		t.Fatal(err)
	}
	p, err := c.students.store.GetProfile(ctx, user)
	if err != nil || slices.Contains(p.AudienceKeys, students.ClubKey(club)) {
		t.Fatalf("profile keys after the event = %v, %v", p.AudienceKeys, err)
	}
}

// TestWire_StudentsDSRsReadKvkkAtCallTime (03 k11): the dpo's request is
// read through kvkk (built after students) at call time: an open request
// names its subject, a closed one and an unknown one are no request, and a
// caller kvkk does not let read requests (support) gets none either.
func TestWire_StudentsDSRsReadKvkkAtCallTime(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	subject := authztest.ID("student")
	at := c.infra.clock.Now().UTC()
	open, closed := uuid.NewV7(), uuid.NewV7()
	for id, st := range map[uuid.UUID]kvkk.Status{open: kvkk.StatusInProgress, closed: kvkk.StatusCompleted} {
		d := kvkk.DSR{ID: id, UserID: &subject, Type: kvkk.TypeAccess, Channel: kvkk.ChannelInApp, Status: st,
			ReceivedAt: at, DueAt: at.Add(kvkk.LegalPeriod)}
		if st == kvkk.StatusCompleted {
			done := at.Add(time.Hour)
			d.CompletedAt, d.ResponseSummary = &done, "Yanıtlandı"
		}
		if err := c.kvkk.store.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	dpo, _ := authztest.Get("dpo")
	sup, _ := authztest.Get("support")
	dsrs := studentsDSRs{c: c}
	if got, ok, err := dsrs.OpenSubject(ctx, dpo.Actor, open); err != nil || !ok || got != subject {
		t.Fatalf("open request = %v, %v, %v", got, ok, err)
	}
	for name, id := range map[string]uuid.UUID{"closed": closed, "unknown": uuid.NewV7()} {
		if _, ok, err := dsrs.OpenSubject(ctx, dpo.Actor, id); err != nil || ok {
			t.Fatalf("%s request = %v, %v", name, ok, err)
		}
	}
	if _, ok, err := dsrs.OpenSubject(ctx, sup.Actor, open); err != nil || ok {
		t.Fatalf("support reading a request = %v, %v", ok, err)
	}
	if _, ok, err := (studentsDSRs{c: &container{}}).OpenSubject(ctx, dpo.Actor, open); err != nil || ok {
		t.Fatalf("without kvkk = %v, %v", ok, err)
	}
}

// TestWire_StudentsLapseSuspendsGrantsAndNotifies (BE-077) through the
// built container: identity's verification feeds the read-time check (the
// lapse hides the student at once), user.edu_verification_lapsed through
// the registered subscription suspends the grant, and notify (read at call
// time) stores the student's in-app notice of the embedded template.
func TestWire_StudentsLapseSuspendsGrantsAndNotifies(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	student, company, uni := uuid.NewV7(), uuid.NewV7(), authztest.UniversityID("uni_a")
	adult, _ := civil.New(2000, time.March, 14)
	email := "lapse-" + student.String()[:8] + "@example.test"
	if err := c.identity.store.InsertUser(ctx, identity.User{ID: student, Status: identity.StatusActive, Email: email,
		EmailNormalized: email, EmailVerifiedAt: &now, FirstName: "Ayla", LastName: "Lapse", Persona: identity.PersonaStudent,
		Locale: identity.LocaleTR, BirthDate: &adult}); err != nil {
		t.Fatal(err)
	}
	exp, addr := now.AddDate(1, 0, 0), "lapse-"+student.String()[:8]+"@matrix.edu.tr"
	v := identity.EduRecord{ID: uuid.NewV7(), UserID: student, Method: identity.EduMethodEmail, EduEmail: addr,
		EduEmailNormalized: addr, EmailDomain: "matrix.edu.tr", UniversityID: &uni, Status: identity.EduVerified,
		VerifiedAt: &now, ExpiresAt: &exp}
	if err := c.identity.store.InsertEduVerification(ctx, v); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.students.store.InsertProfile(ctx, students.Profile{UserID: student, UniversityID: uni,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil || !ok {
		t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
	}
	if err := c.students.svc.Grant(ctx, student, company, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.students.svc.Has(ctx, student, company); err != nil || !ok {
		t.Fatalf("Has before the lapse = %v, %v", ok, err)
	}

	v.Status = identity.EduExpired
	if err := c.identity.store.UpdateEduVerification(ctx, v); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.students.svc.Has(ctx, student, company); err != nil || ok {
		t.Fatalf("Has once the verification ended = %v, %v", ok, err)
	}
	env, err := domainevent.New(domainevent.UserEduVerificationLapsed{UserID: student, VerificationID: v.ID}, "edu_verification", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), now
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	subs := c.infra.subs.For(domainevent.TopicUserEduVerificationLapsed)
	i := slices.IndexFunc(subs, func(s outbox.Subscription) bool { return s.Name == students.SuspendLapsedGrantsTaskName })
	if i < 0 {
		t.Fatal("no grant suspension subscription")
	}
	if err := subs[i].HandleTask(ctx, payload); err != nil {
		t.Fatal(err)
	}
	gs, err := c.students.store.StudentGrants(ctx, student, nil, 10)
	if err != nil || len(gs) != 1 || gs[0].Status() != students.GrantSuspended {
		t.Fatalf("grants after the lapse = %+v, %v", gs, err)
	}
	notes, err := c.notify.store.ListInApp(ctx, student, false, nil, 10)
	if err != nil || len(notes) != 1 || notes[0].Kind != "students.access_grants_suspended" || notes[0].LinkPath != "/me/access-grants" ||
		notes[0].Title == "" || notes[0].SourceOrgID != nil {
		t.Fatalf("in-app notices = %+v, %v", notes, err)
	}
}

// TestWire_StudentsExpiryJobUsesConfiguredWindow (BE-088) through the built
// container: the window comes from config (STUDENTS_GRANT_SUSPENSION_WINDOW,
// here 48 hours); the scheduled students.expire_suspended_grants keeps a
// grant suspended inside the window and revokes it after (by the system,
// suspension_expired), and notify (read at call time) stores the student's
// in-app notice of the embedded template without a company.
func TestWire_StudentsExpiryJobUsesConfiguredWindow(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.infra.cfg.Students.GrantSuspensionWindow = 48 * time.Hour
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	clk := c.infra.clock.(*clock.Fake)
	now := clk.Now().UTC().Truncate(time.Microsecond)
	student, company, uni := uuid.NewV7(), uuid.NewV7(), authztest.UniversityID("uni_a")
	adult, _ := civil.New(2000, time.March, 14)
	email := "expiry-" + student.String()[:8] + "@example.test"
	if err := c.identity.store.InsertUser(ctx, identity.User{ID: student, Status: identity.StatusActive, Email: email,
		EmailNormalized: email, EmailVerifiedAt: &now, FirstName: "Eren", LastName: "Expiry", Persona: identity.PersonaStudent,
		Locale: identity.LocaleTR, BirthDate: &adult}); err != nil {
		t.Fatal(err)
	}
	exp, addr := now.AddDate(1, 0, 0), "expiry-"+student.String()[:8]+"@matrix.edu.tr"
	v := identity.EduRecord{ID: uuid.NewV7(), UserID: student, Method: identity.EduMethodEmail, EduEmail: addr,
		EduEmailNormalized: addr, EmailDomain: "matrix.edu.tr", UniversityID: &uni, Status: identity.EduVerified,
		VerifiedAt: &now, ExpiresAt: &exp}
	if err := c.identity.store.InsertEduVerification(ctx, v); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.students.store.InsertProfile(ctx, students.Profile{UserID: student, UniversityID: uni,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil || !ok {
		t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
	}
	if err := c.students.svc.Grant(ctx, student, company, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if got, err := c.students.store.SuspendGrants(ctx, student, now); err != nil || len(got) != 1 {
		t.Fatalf("SuspendGrants = %+v, %v", got, err)
	}
	run := func() {
		t.Helper()
		for _, j := range c.infra.jobs.Jobs() {
			if j.Name == students.ExpireSuspendedGrantsJobName {
				if err := j.Handle(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("students.expire_suspended_grants is not scheduled")
	}
	clk.Advance(47 * time.Hour)
	run()
	if gs, _ := c.students.store.StudentGrants(ctx, student, nil, 10); len(gs) != 1 || gs[0].Status() != students.GrantSuspended {
		t.Fatalf("inside the 48-hour window = %+v", gs)
	}
	clk.Advance(2 * time.Hour)
	run()
	all, err := c.students.store.AllGrants(ctx, student)
	if err != nil || len(all) != 1 || all[0].Status() != students.GrantRevoked || *all[0].RevokedByKind != students.RevokedBySystem ||
		*all[0].RevokeReason != students.ReasonSuspensionExpired {
		t.Fatalf("after the window = %+v, %v", all, err)
	}
	notes, err := c.notify.store.ListInApp(ctx, student, false, nil, 10)
	if err != nil || len(notes) != 1 || notes[0].Kind != "students.access_grants_expired" || notes[0].LinkPath != "/me/access-grants" ||
		notes[0].Title == "" || notes[0].SourceOrgID != nil {
		t.Fatalf("in-app notices = %+v, %v", notes, err)
	}
}

// TestWire_ConsentWithdrawalClearsContactShares (BE-104, T-355) through the
// built container: the real consent module writes consent.recorded for
// every purpose, so a contact_info_share withdrawal recorded through
// recordMyConsent reaches students.clear_contact_shares over the outbox;
// consent.submit_iys passes the non-İYS events without a submission or an
// error (drain fails on a task error). The share is gone, a consent given
// again revives nothing, and only a new acceptance shares again (and
// survives the redelivered withdrawal). The topic's consumers are exactly
// the two that filter by purpose.
func TestWire_ConsentWithdrawalClearsContactShares(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range c.infra.subs.For(domainevent.TopicConsentRecorded) {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	// A new consumer must filter by purpose (every consent act is an event).
	if want := []string{consent.SubmitIYSTaskName, students.ClearContactSharesTaskName}; !slices.Equal(names, want) {
		t.Fatalf("consent.recorded consumers %q, want %q", names, want)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	student, company, uni := uuid.NewV7(), uuid.NewV7(), authztest.UniversityID("uni_a")
	adult, _ := civil.New(2001, time.May, 9)
	email := "share-" + student.String()[:8] + "@example.test"
	if err := c.identity.store.InsertUser(ctx, identity.User{ID: student, Status: identity.StatusActive, Email: email,
		EmailNormalized: email, EmailVerifiedAt: &now, FirstName: "Deniz", LastName: "Paylaşım", Persona: identity.PersonaStudent,
		Locale: identity.LocaleTR, BirthDate: &adult}); err != nil {
		t.Fatal(err)
	}
	exp, addr := now.AddDate(1, 0, 0), "share-"+student.String()[:8]+"@matrix.edu.tr"
	if err := c.identity.store.InsertEduVerification(ctx, identity.EduRecord{ID: uuid.NewV7(), UserID: student,
		Method: identity.EduMethodEmail, EduEmail: addr, EduEmailNormalized: addr, EmailDomain: "matrix.edu.tr", UniversityID: &uni,
		Status: identity.EduVerified, VerifiedAt: &now, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.students.store.InsertProfile(ctx, students.Profile{UserID: student, UniversityID: uni,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil || !ok {
		t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
	}
	if err := c.students.svc.Grant(ctx, student, company, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	purpose, body := consent.PurposeContactInfoShare, "İletişim bilgimin paylaşılmasına açık rıza veriyorum.\n"
	text := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.Audience("student"),
		Locale: consent.LocaleTR, Version: "2026.10.25", Title: "İletişim paylaşımı", BodyMD: body,
		BodySHA256: consent.BodyDigest(body), CreatedAt: now.Add(-2 * time.Hour)}
	if err := c.consent.store.InsertNotice(ctx, text); err != nil {
		t.Fatal(err)
	}
	if err := c.consent.store.Publish(ctx, text.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	me := authz.Actor{Kind: authz.KindUser, UserID: student, StudentStatus: authz.StudentVerified}
	shared := func() bool {
		t.Helper()
		gs, err := c.students.store.StudentGrants(ctx, student, nil, 10)
		if err != nil || len(gs) != 1 {
			t.Fatalf("grants = %+v, %v", gs, err)
		}
		return gs[0].ContactSharedAt != nil
	}
	mine := func(action consent.Action) consent.Consent {
		t.Helper()
		in := consent.MyConsent{Purpose: purpose, Action: action, Channel: consent.ChannelWeb}
		if action == consent.Granted {
			in.NoticeVersionID, in.TextSHA256 = &text.ID, text.BodySHA256
		}
		rec, err := c.consent.svc.RecordMine(ctx, me, in)
		if err != nil {
			t.Fatalf("recordMyConsent %s: %v", action, err)
		}
		return rec
	}
	share := func() {
		t.Helper()
		pr := students.ConsentProof{NoticeVersionID: text.ID, TextSHA256: text.BodySHA256}
		if err := c.students.svc.ShareContactWithCompany(ctx, me, company, pr, consent.ChannelWeb, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
	}

	clk := c.infra.clock.(*clock.Fake)
	share() // the acceptance records the consent and shares
	drain(t, c)
	if !shared() {
		t.Fatal("the acceptance did not share")
	}
	clk.Advance(time.Second)
	w := mine(consent.Withdrawn)
	drain(t, c)
	if shared() {
		t.Fatal("the withdrawal through the consent module left the share")
	}
	if _, err := c.consent.store.SubmissionOf(ctx, w.ID); !errors.Is(err, consent.ErrNotFound) {
		t.Fatalf("İYS submission of a contact_info_share withdrawal: %v", err)
	}
	clk.Advance(time.Second)
	mine(consent.Granted)
	drain(t, c)
	if on, err := c.consent.svc.HasActive(ctx, student, purpose, nil); err != nil || !on || shared() {
		t.Fatalf("consent again: in force %v (%v), shared %v (must not revive)", on, err, shared())
	}
	clk.Advance(time.Second)
	share()
	drain(t, c) // drain runs every queued task again: the old withdrawal is redelivered
	if !shared() {
		t.Fatal("a new acceptance under the new consent did not share (or a redelivered withdrawal cleared it)")
	}
}
