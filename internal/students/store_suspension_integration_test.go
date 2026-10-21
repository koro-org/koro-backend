//go:build integration

package students_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

func TestStore_GrantSuspension_Gorm(t *testing.T) {
	t.Parallel()
	grantSuspensionContract(t, func(t *testing.T) (students.Store, world) {
		d := testutil.NewDB(t)
		return students.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestSuspend_Postgres_LapseCutsCompanyAccess (BE-077) over PostgreSQL:
// the company reads the student's level 3 profile; the verification ends
// and the read is 404 at once; user.edu_verification_lapsed suspends the
// grant in the database (suspended_at, still open) and the company list
// leaves the student out; a new verification (user.edu_verified, both
// students consumers) does not reopen it; the student, unverified, lists
// the suspended grant and revokes it (by the student, suspended_at kept).
func TestSuspend_Postgres_LapseCutsCompanyAccess(t *testing.T) {
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
	deliver := func(topic, name string, p domainevent.Payload) {
		t.Helper()
		body := e.event(t, p, e.clk.Now())
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

	a := e.pgStudent(t)
	if _, err := svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Headline: ptr("Veri bilimi öğrencisi")}); err != nil {
		t.Fatal(err)
	}
	company := pgWorld(e.d).company(t)
	e.orgNames.name(company, "Acme")
	rec := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone,
		Memberships: []authz.Membership{{OrgID: company, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}}
	if err := svc.Grant(ctx, a.UserID, company, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetCompanyStudentProfile(ctx, rec, company, a.UserID); err != nil {
		t.Fatalf("before the lapse: %v", err)
	}
	hidden := func(step string) {
		t.Helper()
		if _, err := svc.GetCompanyStudentProfile(ctx, rec, company, a.UserID); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("%s: company profile %v, want 404", step, err)
		}
		if p, err := svc.ListCompanyStudents(ctx, rec, company, students.CompanyStudentQuery{}); err != nil || len(p.Items) != 0 {
			t.Fatalf("%s: company list = %+v, %v", step, p, err)
		}
		if ok, err := svc.Has(ctx, a.UserID, company); err != nil || ok {
			t.Fatalf("%s: Has = %v, %v", step, ok, err)
		}
	}
	state := func() (suspended, revoked *time.Time, by *string) {
		t.Helper()
		if err := e.d.Pool.QueryRow(ctx, `SELECT suspended_at, revoked_at, revoked_by_kind FROM profile_access_grants
			WHERE student_user_id = $1`, a.UserID).Scan(&suspended, &revoked, &by); err != nil {
			t.Fatal(err)
		}
		return suspended, revoked, by
	}

	e.users.lapse(a.UserID)
	hidden("verification ended")
	e.clk.Advance(time.Hour)
	deliver(domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName,
		domainevent.UserEduVerificationLapsed{UserID: a.UserID, VerificationID: uuid.NewV7()})
	if sus, rev, _ := state(); sus == nil || !sus.Equal(e.clk.Now()) || rev != nil {
		t.Fatalf("stored grant after the lapse: suspended_at=%v revoked_at=%v", sus, rev)
	}
	hidden("suspended")
	if n := len(e.notices.messages()); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}

	e.clk.Advance(24 * time.Hour)
	uni := *a.UniversityID
	e.users.verify(a.UserID, uni, e.clk.Now())
	verified := domainevent.UserEduVerified{UserID: a.UserID, UniversityID: uni, ExpiresAt: e.clk.Now().AddDate(1, 0, 0)}
	deliver(domainevent.TopicUserEduVerified, students.ApplyEduVerifiedTaskName, verified)
	deliver(domainevent.TopicUserEduVerified, students.SuspendGapGrantsTaskName, verified)
	hidden("verified again")
	if sus, rev, _ := state(); sus == nil || rev != nil {
		t.Fatalf("stored grant after the new verification: suspended_at=%v revoked_at=%v", sus, rev)
	}

	lapsed := authz.Actor{Kind: authz.KindUser, UserID: a.UserID, StudentStatus: authz.StudentLapsed}
	mine, err := svc.ListMyAccessGrants(ctx, lapsed, nil, 0)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].Grant.Status() != students.GrantSuspended || mine.Items[0].CompanyName != "Acme" {
		t.Fatalf("my grants = %+v, %v", mine, err)
	}
	if err := svc.RevokeMyAccessGrant(ctx, lapsed, mine.Items[0].Grant.ID, ""); err != nil {
		t.Fatal(err)
	}
	if sus, rev, by := state(); sus == nil || rev == nil || by == nil || *by != "student" {
		t.Fatalf("stored grant after the revocation: suspended_at=%v revoked_at=%v by=%v", sus, rev, by)
	}
}
