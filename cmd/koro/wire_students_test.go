package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz/authztest"
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
// provider, the two late-bound reads of orgs, five subscriptions and two
// nightly jobs.
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
