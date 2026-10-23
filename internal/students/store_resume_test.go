package students_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

func TestStore_GrantResumeExpiry_Mem(t *testing.T) {
	t.Parallel()
	grantResumeExpiryContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// grantResumeExpiryContract is the BE-088 part of the store contract (both
// stores): ResumeGrant reopens only the student's own open suspended grant
// (resumed_at set, suspended_at cleared) and a suspension older than the
// resume skips it; SuspendedStudentsBefore walks the students holding an
// open grant suspended before the cut-off; ExpireSuspendedGrants revokes
// exactly those grants of one student (system, suspension_expired);
// ExpiredRevocations pages the (student, revoked_at) pairs of the expiries
// since a time, nothing else.
func grantResumeExpiryContract(t *testing.T, newStore func(t *testing.T) (students.Store, world)) {
	t.Helper()
	t.Run("grants: resume", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, v := w.user(t), w.user(t)
		c1, c2 := w.company(t), w.company(t)
		grant := func(student, company uuid.UUID) students.AccessGrant {
			t.Helper()
			g, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: student, CompanyOrgID: company,
				Source: students.SourceContactRequest, SourceID: uuid.NewV7(), GrantedAt: t0})
			if err != nil || !ok {
				t.Fatalf("InsertGrant: ok=%v err=%v", ok, err)
			}
			return g
		}
		g1, g2 := grant(u, c1), grant(u, c2)
		if _, err := s.ResumeGrant(ctx, u, g1.ID, t0.Add(time.Minute)); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("resuming an active grant: %v, want ErrNotFound", err)
		}
		lapse := t0.Add(time.Hour)
		if got, err := s.SuspendGrants(ctx, u, lapse); err != nil || len(got) != 2 {
			t.Fatalf("SuspendGrants = %+v, %v", got, err)
		}
		if _, err := s.ResumeGrant(ctx, v, g1.ID, lapse.Add(time.Hour)); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("resuming another student's grant: %v, want ErrNotFound", err)
		}
		at := lapse.Add(24 * time.Hour)
		g, err := s.ResumeGrant(ctx, u, g1.ID, at)
		if err != nil || g.Status() != students.GrantActive || g.SuspendedAt != nil || g.ResumedAt == nil || !g.ResumedAt.Equal(at) ||
			!g.GrantedAt.Equal(t0) {
			t.Fatalf("ResumeGrant = %+v, %v", g, err)
		}
		if a, err := s.ActiveGrant(ctx, u, c1); err != nil || a.ID != g1.ID {
			t.Fatalf("ActiveGrant after the resume = %+v, %v", a, err)
		}
		if page, _ := s.CompanyGrants(ctx, c1, nil, 10); len(page) != 1 || page[0].ID != g1.ID {
			t.Fatalf("CompanyGrants after the resume = %+v", page)
		}
		if _, err := s.ResumeGrant(ctx, u, g1.ID, at.Add(time.Minute)); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("resuming twice: %v, want ErrNotFound", err)
		}
		// A suspension older than the resume (a late task) skips it; the
		// still suspended g2 is not suspended again.
		if got, err := s.SuspendGrants(ctx, u, at.Add(-time.Minute)); err != nil || len(got) != 0 {
			t.Fatalf("a late suspension = %+v, %v", got, err)
		}
		// A later one takes it (suspended_at ≥ resumed_at).
		later := at.Add(time.Hour)
		if got, err := s.SuspendGrants(ctx, u, later); err != nil || len(got) != 1 || got[0].ID != g1.ID ||
			!got[0].SuspendedAt.Equal(later) || !got[0].ResumedAt.Equal(at) {
			t.Fatalf("a later suspension = %+v, %v", got, err)
		}
		// A revoked grant is not resumed.
		if _, err := s.RevokeGrants(ctx, u, c2, students.RevokedByStudent, nil, later); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResumeGrant(ctx, u, g2.ID, later.Add(time.Minute)); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("resuming a revoked grant: %v, want ErrNotFound", err)
		}
	})

	t.Run("grants: expiry", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, v, x := w.user(t), w.user(t), w.user(t)
		c1, c2, c3 := w.company(t), w.company(t), w.company(t)
		grant := func(student, company uuid.UUID) students.AccessGrant {
			t.Helper()
			g, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: student, CompanyOrgID: company,
				Source: students.SourceContactRequest, SourceID: uuid.NewV7(), GrantedAt: t0})
			if err != nil || !ok {
				t.Fatalf("InsertGrant: ok=%v err=%v", ok, err)
			}
			return g
		}
		grant(u, c1)
		grant(u, c2)
		uRevoked := grant(u, c3)
		vGrant := grant(v, c1)
		grant(x, c1) // active: never expires
		if _, err := s.SuspendGrants(ctx, u, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RevokeGrants(ctx, u, c3, students.RevokedByCompany, nil, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SuspendGrants(ctx, v, t0.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}

		cutoff := t0.Add(2 * time.Hour) // u's suspension is before it, v's after
		ids, err := s.SuspendedStudentsBefore(ctx, cutoff, uuid.Nil(), 10)
		if err != nil || len(ids) != 1 || ids[0] != u {
			t.Fatalf("SuspendedStudentsBefore = %v, %v; want u only", ids, err)
		}
		both, err := s.SuspendedStudentsBefore(ctx, t0.Add(4*time.Hour), uuid.Nil(), 10)
		if err != nil || len(both) != 2 {
			t.Fatalf("SuspendedStudentsBefore (later cut-off) = %v, %v", both, err)
		}
		if page, _ := s.SuspendedStudentsBefore(ctx, t0.Add(4*time.Hour), both[0], 10); len(page) != 1 || page[0] != both[1] {
			t.Fatalf("SuspendedStudentsBefore after %v = %v", both[0], page)
		}
		if page, _ := s.SuspendedStudentsBefore(ctx, t0.Add(4*time.Hour), uuid.Nil(), 1); len(page) != 1 || page[0] != both[0] {
			t.Fatalf("SuspendedStudentsBefore limit 1 = %v", page)
		}

		at := t0.Add(100 * 24 * time.Hour)
		if got, err := s.ExpireSuspendedGrants(ctx, v, cutoff, at); err != nil || len(got) != 0 {
			t.Fatalf("expiring v before its cut-off = %+v, %v", got, err)
		}
		got, err := s.ExpireSuspendedGrants(ctx, u, cutoff, at)
		if err != nil || len(got) != 2 {
			t.Fatalf("ExpireSuspendedGrants = %+v, %v", got, err)
		}
		for _, g := range got {
			if g.Status() != students.GrantRevoked || *g.RevokedByKind != students.RevokedBySystem || !g.RevokedAt.Equal(at) ||
				g.RevokeReason == nil || *g.RevokeReason != students.ReasonSuspensionExpired || g.SuspendedAt == nil {
				t.Fatalf("expired row = %+v", g)
			}
		}
		if again, _ := s.ExpireSuspendedGrants(ctx, u, cutoff, at.Add(time.Hour)); len(again) != 0 {
			t.Fatalf("expiring again = %+v", again)
		}
		if g, _ := s.GetGrant(ctx, u, uRevoked.ID); *g.RevokedByKind != students.RevokedByCompany || g.RevokeReason != nil {
			t.Fatalf("the company's revocation changed: %+v", g)
		}
		if g, _ := s.GetGrant(ctx, v, vGrant.ID); g.Status() != students.GrantSuspended {
			t.Fatalf("v's suspension changed: %+v", g)
		}
		// v expires later, at another time.
		at2 := at.Add(time.Hour)
		if got, err := s.ExpireSuspendedGrants(ctx, v, t0.Add(4*time.Hour), at2); err != nil || len(got) != 1 {
			t.Fatalf("expiring v = %+v, %v", got, err)
		}

		rows, err := s.ExpiredRevocations(ctx, at.Add(-time.Hour), nil, 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("ExpiredRevocations = %+v, %v; want u's and v's", rows, err)
		}
		want := map[uuid.UUID]time.Time{u: at, v: at2}
		for _, r := range rows {
			if !want[r.StudentUserID].Equal(r.RevokedAt) {
				t.Fatalf("expiry = %+v", r)
			}
		}
		if page, _ := s.ExpiredRevocations(ctx, at.Add(-time.Hour), &rows[0], 10); len(page) != 1 || !sameExpiry(page[0], rows[1]) {
			t.Fatalf("ExpiredRevocations after %+v = %+v", rows[0], page)
		}
		if page, _ := s.ExpiredRevocations(ctx, at.Add(-time.Hour), nil, 1); len(page) != 1 || !sameExpiry(page[0], rows[0]) {
			t.Fatalf("ExpiredRevocations limit 1 = %+v", page)
		}
		if page, _ := s.ExpiredRevocations(ctx, at.Add(time.Minute), nil, 10); len(page) != 1 || page[0].StudentUserID != v {
			t.Fatalf("ExpiredRevocations since after u's expiry = %+v", page)
		}
	})
}

func sameExpiry(a, b students.ExpiryKey) bool {
	return a.StudentUserID == b.StudentUserID && a.RevokedAt.Equal(b.RevokedAt)
}
