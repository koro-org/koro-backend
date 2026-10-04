package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/kvkk"
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
// provider, the two late-bound reads of orgs, six subscriptions (file.rejected
// since BE-025, next to files' own) and two nightly jobs.
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
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "students.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"students.audience_refresh 0 2 * * * low", "students.graduation_check 15 2 * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("students jobs %q, want %q", jobs, want)
	}
	for topic, name := range map[string]string{
		domainevent.TopicMembershipStarted:         students.AddClubKeyTaskName,
		domainevent.TopicMembershipEnded:           students.RemoveClubKeyTaskName,
		domainevent.TopicUserEduVerified:           students.ApplyEduVerifiedTaskName,
		domainevent.TopicUserEduVerificationLapsed: students.ApplyEduLapsedTaskName,
		domainevent.TopicClubSensitivityChanged:    students.DropSensitiveClubKeyTaskName,
		domainevent.TopicFileRejected:              students.DropRejectedFileTaskName,
	} {
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
