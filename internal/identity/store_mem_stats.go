package identity

import (
	"context"
	"time"
	"uuid"
)

// UserStats implements StatsStore.
func (m *MemStore) UserStats(_ context.Context, from, to, now time.Time) (UserStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := newUserStats()
	for _, u := range m.users {
		st.ByStatus[u.Status]++
		if !u.CreatedAt.Before(from) && u.CreatedAt.Before(to) {
			st.NewInRange++
		}
		if u.Status == StatusErased {
			continue
		}
		st.ByPersona[u.Persona]++
		if u.EmailVerifiedAt != nil {
			st.EmailVerified++
		}
	}
	students := map[uuid.UUID]bool{}
	for _, r := range m.edu {
		if r.activeAt(now) {
			students[r.UserID] = true
		}
	}
	st.VerifiedStudents = len(students)
	return st, nil
}
