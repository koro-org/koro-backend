//go:build integration

package students_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

func TestStore_GrantResumeExpiry_Gorm(t *testing.T) {
	t.Parallel()
	grantResumeExpiryContract(t, func(t *testing.T) (students.Store, world) {
		d := testutil.NewDB(t)
		return students.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestResume_Postgres_ResumeAndExpire (BE-088) over PostgreSQL: two grants
// of a student are suspended by the lapse; verified again, the student
// resumes one (resumed_at stored, the company reads the profile again) and
// a late gap task leaves it active; after the window the nightly job
// revokes the other (system, suspension_expired), keeps the resumed one
// and tells the student once; the export carries the reasons.
func TestResume_Postgres_ResumeAndExpire(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	mod, err := students.New(students.Deps{
		Store: students.NewGormStore(e.d.ORM), Tx: e.d.Tx, Outbox: outbox.NewMem(e.clk), Audit: e.audit, Clock: e.clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.d.Tx, Store: idempotency.NewGormStore(e.d.ORM), Clock: e.clk, Log: obs.Discard()}),
		Consent:     e.consent, Consents: e.consent, Catalog: e.catalog, Orgs: e.orgs, Users: e.users, Verifications: e.users,
		OrgNames: e.orgNames, Sender: e.notices,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, subs := mod.Service, outbox.NewRegistry()
	mod.Subscribe(subs)
	deliver := func(topic, name string, body []byte) {
		t.Helper()
		for _, s := range subs.For(topic) {
			if s.Name == name {
				if err := s.HandleTask(context.WithoutCancel(ctx), body); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return
			}
		}
		t.Fatalf("no subscription %s", name)
	}
	job := func() {
		t.Helper()
		for _, j := range mod.Jobs() {
			if j.Name == students.ExpireSuspendedGrantsJobName {
				if err := j.Handle(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("no expiry job")
	}

	a := e.pgStudent(t)
	if _, err := svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Headline: ptr("Veri bilimi öğrencisi")}); err != nil {
		t.Fatal(err)
	}
	w := pgWorld(e.d)
	keep, drop := w.company(t), w.company(t)
	e.orgNames.name(keep, "Acme")
	rec := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone,
		Memberships: []authz.Membership{{OrgID: keep, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}}
	for _, org := range []uuid.UUID{keep, drop} {
		if err := svc.Grant(ctx, a.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
	}
	e.users.lapse(a.UserID)
	e.clk.Advance(time.Hour)
	deliver(domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName,
		e.event(t, domainevent.UserEduVerificationLapsed{UserID: a.UserID, VerificationID: uuid.NewV7()}, e.clk.Now()))
	suspendedAt := e.clk.Now()

	e.clk.Advance(24 * time.Hour)
	e.users.verify(a.UserID, *a.UniversityID, e.clk.Now())
	gap := e.event(t, domainevent.UserEduVerified{UserID: a.UserID, UniversityID: *a.UniversityID,
		ExpiresAt: e.clk.Now().AddDate(1, 0, 0)}, e.clk.Now())
	e.clk.Advance(time.Minute)
	mine, err := svc.ListMyAccessGrants(ctx, a, nil, 0)
	if err != nil || len(mine.Items) != 2 {
		t.Fatalf("my grants = %+v, %v", mine, err)
	}
	var keepID uuid.UUID
	for _, v := range mine.Items {
		if v.Grant.CompanyOrgID == keep {
			keepID = v.Grant.ID
		}
		if v.ResumableUntil == nil || !v.ResumableUntil.Equal(suspendedAt.Add(students.DefaultSuspensionWindow)) {
			t.Fatalf("resumable_until = %v", v.ResumableUntil)
		}
	}
	if v, err := svc.ResumeMyAccessGrant(ctx, a, keepID); err != nil || v.CompanyName != "Acme" {
		t.Fatalf("resume = %+v, %v", v, err)
	}
	deliver(domainevent.TopicUserEduVerified, students.SuspendGapGrantsTaskName, gap) // late
	var resumedAt, susp *time.Time
	if err := e.d.Pool.QueryRow(ctx, `SELECT resumed_at, suspended_at FROM profile_access_grants WHERE id = $1`, keepID).
		Scan(&resumedAt, &susp); err != nil {
		t.Fatal(err)
	}
	if resumedAt == nil || susp != nil {
		t.Fatalf("stored resumed grant: resumed_at=%v suspended_at=%v", resumedAt, susp)
	}
	if _, err := svc.GetCompanyStudentProfile(ctx, rec, keep, a.UserID); err != nil {
		t.Fatalf("the company after the resume: %v", err)
	}

	e.clk.Set(suspendedAt.Add(students.DefaultSuspensionWindow + time.Microsecond))
	job()
	var by, reason string
	if err := e.d.Pool.QueryRow(ctx, `SELECT revoked_by_kind, revoke_reason FROM profile_access_grants
		WHERE student_user_id = $1 AND company_org_id = $2`, a.UserID, drop).Scan(&by, &reason); err != nil {
		t.Fatal(err)
	}
	if by != "system" || reason != students.ReasonSuspensionExpired {
		t.Fatalf("expired grant: by=%s reason=%s", by, reason)
	}
	if ok, err := svc.Has(ctx, a.UserID, keep); err != nil || !ok {
		t.Fatalf("the resumed grant after the expiry: Has = %v, %v", ok, err)
	}
	n := 0
	for _, m := range e.notices.messages() {
		if m.Template == "students.access_grants_expired" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expiry notices = %d, want 1", n)
	}
	sec, err := mod.Privacy.Export(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, it := range sec.Items {
		switch {
		case it["record"] == "access_grant" && it["company_org_id"] == drop.String() && it["revoke_reason"] == students.ReasonSuspensionExpired:
			found++
		case it["record"] == "access_grant" && it["company_org_id"] == keep.String() && it["resumed_at"] != nil:
			found++
		}
	}
	if found != 2 {
		t.Fatalf("export = %+v", sec.Items)
	}
}
