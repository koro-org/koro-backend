package identity

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
)

// UserStats implements StatsStore: one pass over users grouped by status
// and persona, one count of the student verifications in effect.
//
// cross-org: platform-wide account counts (identity owns no org-scoped row).
func (s gormStore) UserStats(ctx context.Context, from, to, now time.Time) (UserStats, error) {
	var rows []struct {
		Status   string
		Persona  string
		N        int
		Verified int
		Fresh    int
	}
	conn := db.Conn(ctx, s.orm)
	err := conn.Raw(`SELECT status, signup_persona AS persona, count(*) AS n,
	        count(*) FILTER (WHERE email_verified_at IS NOT NULL) AS verified,
	        count(*) FILTER (WHERE created_at >= ? AND created_at < ?) AS fresh
	   FROM users GROUP BY status, signup_persona`, from, to).Scan(&rows).Error
	if err != nil {
		return UserStats{}, fmt.Errorf("identity: user stats: %w", err)
	}
	st := newUserStats()
	for _, r := range rows {
		st.ByStatus[Status(r.Status)] += r.N
		st.NewInRange += r.Fresh
		if Status(r.Status) == StatusErased {
			continue
		}
		st.ByPersona[Persona(r.Persona)] += r.N
		st.EmailVerified += r.Verified
	}
	err = conn.Raw(`SELECT count(DISTINCT user_id) FROM edu_verifications WHERE status = ? AND expires_at > ?`,
		string(EduVerified), now).Scan(&st.VerifiedStudents).Error
	if err != nil {
		return UserStats{}, fmt.Errorf("identity: verified students: %w", err)
	}
	return st, nil
}
