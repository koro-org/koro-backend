package identity_test

import (
	"testing"
	"time"

	"koro/internal/identity"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(*testing.T) eduWorld { return memEduWorld() })
}

// statsContract is the account counts of the platform panel (BE-163): by
// status (tombstones included), by persona and verified e-mail (live
// accounts), created in a range, and the users with a student
// verification in effect. A fresh store may hold seed rows, so the
// contract checks what its own rows add.
func statsContract(t *testing.T, newWorld func(t *testing.T) eduWorld) {
	t.Helper()
	t.Run("user stats", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		s, ctx := w.s, t.Context()
		wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		now := contractBase.Add(time.Hour)
		before, err := s.UserStats(ctx, wideFrom, wideTo, now)
		if err != nil {
			t.Fatal(err)
		}
		verified := contractUser("stats-a@example.test")
		verified.EmailVerifiedAt = &contractBase
		plain := contractUser("stats-b@example.test")
		company := contractUser("stats-c@example.test")
		company.Status, company.Persona, company.BirthDate = identity.StatusPendingEmail, identity.PersonaCompany, nil
		for _, u := range []identity.User{verified, plain, company} {
			if err := s.InsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		uni := w.uni(t)
		w.insertAt(t, eduEmailRow(verified.ID, uni, "a@uni.edu.tr", identity.EduVerified, contractBase), contractBase)
		w.insertAt(t, eduEmailRow(plain.ID, uni, "b@uni.edu.tr", identity.EduVerified, contractBase.AddDate(-2, 0, 0)),
			contractBase.AddDate(-2, 0, 0))

		after, err := s.UserStats(ctx, wideFrom, wideTo, now)
		if err != nil {
			t.Fatal(err)
		}
		delta := func(name string, got, was, want int) {
			t.Helper()
			if got-was != want {
				t.Errorf("%s: +%d, want +%d", name, got-was, want)
			}
		}
		delta("active", after.ByStatus[identity.StatusActive], before.ByStatus[identity.StatusActive], 2)
		delta("pending_email", after.ByStatus[identity.StatusPendingEmail], before.ByStatus[identity.StatusPendingEmail], 1)
		delta("student persona", after.ByPersona[identity.PersonaStudent], before.ByPersona[identity.PersonaStudent], 2)
		delta("company persona", after.ByPersona[identity.PersonaCompany], before.ByPersona[identity.PersonaCompany], 1)
		delta("email verified", after.EmailVerified, before.EmailVerified, 1)
		delta("new in range", after.NewInRange, before.NewInRange, 3)
		// The second verification expired a year after 2024-10: only one is in effect.
		delta("verified students", after.VerifiedStudents, before.VerifiedStudents, 1)

		empty, err := s.UserStats(ctx, wideFrom, wideFrom.Add(time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		if empty.NewInRange != 0 {
			t.Errorf("new in an empty range = %d", empty.NewInRange)
		}
	})
}
