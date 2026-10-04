package main

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/outreach"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
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
// and registers in tasks (01 §7): its service (Targeting for the feed) and
// outreach.recount_segments at 06:00 on the low queue.
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
	if want := []string{"outreach.recount_segments 0 6 * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("outreach jobs %q, want %q", jobs, want)
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
