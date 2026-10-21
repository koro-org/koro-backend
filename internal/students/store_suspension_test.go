package students_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

func TestStore_GrantSuspension_Mem(t *testing.T) {
	t.Parallel()
	grantSuspensionContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// grantSuspensionContract is the BE-077 part of the store contract (both
// stores): SuspendGrants takes the student's active grants given at or
// before the time (not later ones, not revoked ones, not another
// student's) and suspends each once; CountSuspendedAt counts one
// suspension's open grants; a suspended grant is no active grant, is
// left out of the company list, stays in the student's list and is
// revoked like an active one (only open grants count as suspended).
func grantSuspensionContract(t *testing.T, newStore func(t *testing.T) (students.Store, world)) {
	t.Helper()
	t.Run("grants: suspension", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, v := w.user(t), w.user(t)
		c1, c2, c3, c4 := w.company(t), w.company(t), w.company(t), w.company(t)
		grant := func(student, company uuid.UUID, at time.Time) students.AccessGrant {
			t.Helper()
			g, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: student, CompanyOrgID: company,
				Source: students.SourceContactRequest, SourceID: uuid.NewV7(), GrantedAt: at})
			if err != nil || !ok {
				t.Fatalf("InsertGrant: ok=%v err=%v", ok, err)
			}
			return g
		}
		g1 := grant(u, c1, t0)
		g2 := grant(u, c2, t0.Add(time.Minute))
		g3 := grant(u, c3, t0.Add(2*time.Minute))
		late := grant(u, c4, t0.Add(time.Hour)) // after the lapse
		gv := grant(v, c1, t0)
		if _, err := s.RevokeGrants(ctx, u, c3, students.RevokedByCompany, nil, t0.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}

		at := t0.Add(30 * time.Minute)
		got, err := s.SuspendGrants(ctx, u, at)
		if err != nil || len(got) != 2 {
			t.Fatalf("SuspendGrants = %+v, %v; want g1 and g2", got, err)
		}
		for _, g := range got {
			if (g.ID != g1.ID && g.ID != g2.ID) || g.Status() != students.GrantSuspended || !g.SuspendedAt.Equal(at) ||
				g.Active() || !g.Open() || g.UpdatedAt.Before(g.CreatedAt) {
				t.Fatalf("suspended row = %+v", g)
			}
		}
		if again, err := s.SuspendGrants(ctx, u, at.Add(time.Minute)); err != nil || len(again) != 0 {
			t.Fatalf("suspending again = %+v, %v (the late grant is after the cutoff)", again, err)
		}
		if n, err := s.CountSuspendedAt(ctx, u, at); err != nil || n != 2 {
			t.Fatalf("CountSuspendedAt = %d, %v", n, err)
		}
		if n, _ := s.CountSuspendedAt(ctx, u, at.Add(time.Second)); n != 0 {
			t.Fatalf("CountSuspendedAt of another time = %d", n)
		}
		if g, _ := s.GetGrant(ctx, u, g3.ID); g.Status() != students.GrantRevoked || g.SuspendedAt != nil {
			t.Fatalf("a revoked grant was suspended: %+v", g)
		}
		if g, _ := s.GetGrant(ctx, u, late.ID); g.Status() != students.GrantActive {
			t.Fatalf("a grant after the cutoff was suspended: %+v", g)
		}
		if g, _ := s.GetGrant(ctx, v, gv.ID); g.Status() != students.GrantActive {
			t.Fatalf("another student's grant was suspended: %+v", g)
		}

		// Reads: no active grant, no company card; the student still sees it.
		if _, err := s.ActiveGrant(ctx, u, c1); !errors.Is(err, students.ErrNotFound) {
			t.Fatalf("ActiveGrant of a suspended grant: %v", err)
		}
		if page, _ := s.CompanyGrants(ctx, c1, nil, 10); len(page) != 1 || page[0].ID != gv.ID {
			t.Fatalf("CompanyGrants = %+v", page)
		}
		page, err := s.StudentGrants(ctx, u, nil, 10)
		if err != nil || len(page) != 3 || page[0].ID != late.ID || page[1].ID != g2.ID || page[2].ID != g1.ID {
			t.Fatalf("StudentGrants = %+v, %v", page, err)
		}
		if page[1].Status() != students.GrantSuspended {
			t.Fatalf("listed status = %s", page[1].Status())
		}

		// A suspended grant is revoked like an active one.
		revoked, err := s.RevokeGrants(ctx, u, c1, students.RevokedByStudent, nil, at.Add(time.Hour))
		if err != nil || len(revoked) != 1 || revoked[0].ID != g1.ID || revoked[0].Status() != students.GrantRevoked ||
			!revoked[0].SuspendedAt.Equal(at) {
			t.Fatalf("RevokeGrants of a suspended grant = %+v, %v", revoked, err)
		}
		if n, _ := s.CountSuspendedAt(ctx, u, at); n != 1 {
			t.Fatalf("CountSuspendedAt after a revocation = %d, want 1 (open only)", n)
		}
		all, err := s.AllGrants(ctx, u)
		if err != nil || len(all) != 4 {
			t.Fatalf("AllGrants = %+v, %v", all, err)
		}
	})
}
